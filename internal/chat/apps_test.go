package chat

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
)

// Stored rows are caller-writable by design, because RLS lets each person own
// their row. The broker must not add the project-wide key until the destination
// has been pinned to the provider's exact origin.
func TestOnlyComposioCanReceiveTheProjectKey(t *testing.T) {
	valid := []string{
		"https://backend.composio.dev/mcp/sess_1",
		"https://backend.composio.dev/api/mcp?session=sess_1",
	}
	for _, raw := range valid {
		if err := validateComposioSessionURL(raw); err != nil {
			t.Errorf("valid URL %q rejected: %v", raw, err)
		}
	}

	invalid := []string{
		"https://attacker.example/mcp",
		"http://backend.composio.dev/mcp",
		"https://backend.composio.dev.attacker.example/mcp",
		"https://backend.composio.dev:443/mcp",
		"https://user@backend.composio.dev/mcp",
	}
	for _, raw := range invalid {
		if err := validateComposioSessionURL(raw); err == nil {
			t.Errorf("untrusted URL %q was accepted", raw)
		}
	}
}

// The app opens by fetching every agent's thread in parallel and connecting the
// stream, so first sign-in is exactly when a dozen requests arrive at once. A
// check-then-mark would have all of them pass the check and mint a dozen
// sessions for one person, on the highest-traffic path there is.
func TestOnlyOneCallerMintsPerMachine(t *testing.T) {
	s := &Server{appsClaims: map[string]appsClaim{}}
	var claimed atomic.Int64
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.claimApps("m1") {
				claimed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := claimed.Load(); got != 1 {
		t.Errorf("%d callers each minted a session", got)
	}
}

// A failed push releases the claim, so the next request tries again rather than
// leaving the machine without app tools for the life of the process.
func TestAFailedPushIsRetried(t *testing.T) {
	s := &Server{appsClaims: map[string]appsClaim{}}
	if !s.claimApps("m1") {
		t.Fatal("the first caller did not get the claim")
	}
	s.forgetApps("m1")
	if !s.claimApps("m1") {
		t.Error("a released claim was not available again")
	}
}

// A failure is retried, but not on the very next request. Without the cooldown a
// provider having a bad ten minutes costs a full mint attempt per machine per
// request, and the app opens by fetching every agent's thread at once.
func TestAFailedPushWaitsBeforeTryingAgain(t *testing.T) {
	s := &Server{appsClaims: map[string]appsClaim{}}
	if !s.claimApps("m1") {
		t.Fatal("the first caller did not get the claim")
	}
	s.failApps("m1")
	if s.claimApps("m1") {
		t.Error("a failure was retried immediately")
	}
	// A machine that is created or erased holds nothing at all, so it is not made
	// to sit out a cooldown earned by the machine that used to have that id.
	s.forgetApps("m1")
	if !s.claimApps("m1") {
		t.Error("a recreated machine was kept waiting")
	}
}

// Every way into a machine must provision its connected-apps session, and the
// bug this guards against is a SECOND way that quietly does not.
//
// listAgents reached the guest through its own copy of "ensureMachine then
// agent.New", so the app's very first call after signing in booted a machine and
// left it with no session -- and because that path never failed, nothing was
// logged and nothing looked wrong. A structural check rather than a behavioural
// one, because reproducing it needs a control plane and a live guest: what went
// wrong was a call site, so a call site is what this counts.
func TestOnlyClientForBuildsAGuestClient(t *testing.T) {
	// The operator surface, gated on the fleet token. It takes a VM id from the
	// request and has no signed-in person, so there is nobody to mint a session
	// for -- and minting one on the operator's behalf would attach a person's
	// connected accounts to whoever is looking at the fleet.
	allowed := map[string]bool{"api.go": true, "bridge.go": true}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range entries {
		name := f.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || allowed[name] {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		inClientFor := false
		for i, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "func ") {
				inClientFor = strings.Contains(line, ") clientFor(")
			}
			if strings.Contains(line, "agent.New(") && !inClientFor {
				t.Errorf("%s:%d reaches a guest outside clientFor, so that path "+
					"would boot a machine with no connected-apps session: %s",
					name, i+1, strings.TrimSpace(line))
			}
		}
	}
}

// A malformed broker address must stop the service coming up rather than turn
// connected apps off in silence: NewAppsGateway returns nil for one, which opens
// no listener and short-circuits every push, with a valid key set.
func TestAMalformedBrokerAddressIsRefusedAtStartup(t *testing.T) {
	base := Config{
		Origin: "https://chat.example.com", VNCOrigin: "https://vnc.example.com",
		Token: "t", SupabaseURL: "https://p.supabase.co",
	}
	off := base
	off.AppsAddr = "8092" // a port with no host, which is the likely typo
	if err := off.validate(); err != nil {
		t.Fatalf("a bad address was refused with no provider configured: %v", err)
	}
	on := off
	on.ComposioKey, on.SupabasePublishable = "ak_x", "sb_publishable_x"
	if err := on.validate(); err == nil {
		t.Fatal("a malformed CHAT_APPS_ADDR was accepted with the feature on")
	}
	good := on
	good.AppsAddr = "0.0.0.0:8092"
	if err := good.validate(); err != nil {
		t.Fatalf("a good address was refused: %v", err)
	}
}

// heldAppsStore answers with one already-minted session, so a push can be
// driven without a provider or a database.
type heldAppsStore struct{ held agentapi.Apps }

func (h *heldAppsStore) Get(context.Context, string) (agentapi.Apps, error) { return h.held, nil }
func (h *heldAppsStore) Put(context.Context, string, agentapi.Apps) error   { return nil }
func (h *heldAppsStore) Delete(context.Context, string) error               { return nil }

// The read-only set is resolved BEFORE the ticket is minted, not between the
// ticket and the push.
//
// On a cold cache that fetch is a round trip to the provider. pushApps runs
// detached, so a machine erased and recreated while it is in flight leaves the
// old goroutine holding a ticket forgetApps has already dropped -- which it then
// pushes over the replacement's good one, and the replacement's claim is latched
// pushed, so nothing tries again. Ordering is not what closes that window, but a
// provider round trip inside it is this PR's to not add.
//
// Pinned by what the gateway holds WHILE the fetch runs, so moving the call back
// after Register fails this rather than merely reordering two lines.
func TestTheReadOnlySetIsResolvedBeforeTheTicketExists(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer guest.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(guest.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	gw := NewAppsGateway("the-project-key", "0.0.0.0:8092")
	var ticketsWhenFetched int
	reads := &appReads{fetch: func(_ context.Context, slug string) ([]string, error) {
		gw.mu.Lock()
		ticketsWhenFetched = len(gw.routes)
		gw.mu.Unlock()
		return []string{slug + "_GET"}, nil
	}}
	s := &Server{gw: gw, reads: reads, appsClaims: map[string]appsClaim{},
		apps: &heldAppsStore{held: agentapi.Apps{
			SessionURL: "https://backend.composio.dev/mcp/sess_1", SessionID: "sess_1"}}}

	guestPort, _ := strconv.Atoi(port)
	cl := agent.New(host, guestPort)
	if err := s.pushApps(context.Background(), testUserID, vmView{ID: "m1", GuestIP: host}, cl); err != nil {
		t.Fatalf("push failed: %v", err)
	}
	if ticketsWhenFetched != 0 {
		t.Errorf("the provider was asked with %d ticket(s) already minted -- the fetch "+
			"sits between Register and SetApps, widening the window where an erased "+
			"machine's push lands on its replacement", ticketsWhenFetched)
	}
}
