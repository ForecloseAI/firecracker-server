package chat

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
	"cracked/internal/composio"
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

// Everything that crosses the internet happens BEFORE the ticket is minted, not
// between the ticket and the push.
//
// On a cold cache that is now several round trips: this person's connections,
// and then each of those apps' actions. pushApps runs detached, so a machine
// erased and recreated while it is in flight leaves the old goroutine holding a
// ticket forgetApps has already dropped -- which it then pushes over the
// replacement's good one, and the replacement's claim is latched pushed, so
// nothing tries again. Ordering is not what closes that window, but every round
// trip moved inside it widens it.
//
// Pinned by what the gateway holds WHILE each call runs, so moving one back
// after Register fails this rather than merely reordering two lines. Both are
// checked: reading the connections was added after this test was written, and a
// test that only watched the capability fetch would not have noticed.
func TestTheActionsAreResolvedBeforeTheTicketExists(t *testing.T) {
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer guest.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(guest.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}

	gw := NewAppsGateway("the-project-key", "0.0.0.0:8092")
	var mu sync.Mutex
	tickets := map[string]int{}
	note := func(what string) {
		gw.mu.Lock()
		held := len(gw.routes)
		gw.mu.Unlock()
		mu.Lock()
		tickets[what] = held
		mu.Unlock()
	}
	kinds, _ := stubCaps(func(app string) (map[string]string, error) {
		note("actions")
		return map[string]string{app + "_GET": composio.CapRead}, nil
	})
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		note("connections")
		w.Write([]byte(`{"items":[{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`))
	}))
	defer provider.Close()
	s := &Server{gw: gw, kinds: kinds, appsClaims: map[string]appsClaim{},
		composio: composio.New("k", provider.URL),
		apps: &heldAppsStore{held: agentapi.Apps{
			SessionURL: "https://backend.composio.dev/mcp/sess_1", SessionID: "sess_1"}}}

	guestPort, _ := strconv.Atoi(port)
	cl := agent.New(host, guestPort)
	if _, err := s.pushApps(context.Background(), testUserID, vmView{ID: "m1", GuestIP: host}, cl); err != nil {
		t.Fatalf("push failed: %v", err)
	}
	for _, what := range []string{"connections", "actions"} {
		held, asked := tickets[what]
		if !asked {
			t.Fatalf("the %s were never read, so this pins nothing", what)
		}
		if held != 0 {
			t.Errorf("the %s were read with %d ticket(s) already minted -- that call "+
				"sits between Register and SetApps, widening the window where an "+
				"erased machine's push lands on its replacement", what, held)
		}
	}
}

// THE test for this fix. A pushed claim used to latch forever, so the TTL
// governed only what the NEXT machine to boot was told: a machine already
// running kept whatever set it was handed until the host process restarted.
//
// The permissive half is what makes it worth fixing -- a tool the provider stops
// annotating readOnlyHint stays read-only on every live machine -- so expiry has
// to reach machines that already hold a copy, not just the host cache.
func TestAMachineIsPushedAgainOnceItsSetGoesStale(t *testing.T) {
	s := &Server{appsClaims: map[string]appsClaim{}}
	if !s.claimApps("m1") {
		t.Fatal("the first caller did not get the claim")
	}

	// A set that is still good keeps the machine out: re-pushing rotates its
	// ticket, so doing it per request would 404 anything in flight for nothing.
	s.doneApps("m1", pushed{until: time.Now().Add(appCapsTTL)})
	if s.claimApps("m1") {
		t.Error("a machine holding a fresh set was pushed again anyway")
	}

	// Once the set it was handed is stale, it is due another push.
	s.doneApps("m1", pushed{until: time.Now().Add(-time.Second)})
	if !s.claimApps("m1") {
		t.Error("a machine holding a stale set was never pushed again, so a tool " +
			"the provider stopped calling read-only stays read-only there until restart")
	}
}

// The in-flight guard survives the deadline being added. A claim is taken before
// the push, so it has no set to expire on yet -- and dating it now would let the
// dozen requests that arrive at first sign-in all pass and mint a dozen sessions.
func TestAnInFlightPushStillBlocksTheNextCaller(t *testing.T) {
	s := &Server{appsClaims: map[string]appsClaim{}}
	if !s.claimApps("m1") {
		t.Fatal("the first caller did not get the claim")
	}
	if s.claimApps("m1") {
		t.Error("a push still in flight did not hold the claim")
	}
	// It is not held forever either: a goroutine that dies without reporting
	// either way frees the machine on the same bound the push runs under.
	s.mu.Lock()
	held := s.appsClaims["m1"]
	s.mu.Unlock()
	if held.expires.IsZero() || held.expires.After(time.Now().Add(appsMintTimeout+time.Second)) {
		t.Errorf("an in-flight claim is dated %v, which strands the machine if the "+
			"goroutine dies without calling doneApps or failApps", held.expires)
	}
}

// pushingServer is a host wired to a stub guest, ready to run a real push.
func pushingServer(t *testing.T) (*Server, *agent.Client, *[]byte) {
	t.Helper()
	var body []byte
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(guest.Close)
	host, port, err := net.SplitHostPort(strings.TrimPrefix(guest.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	kinds, _ := stubCaps(func(string) (map[string]string, error) { return nil, nil })
	s := &Server{gw: NewAppsGateway("the-project-key", "0.0.0.0:8092"), composio: connectedTo(t, "gmail"),
		kinds: kinds, appsClaims: map[string]appsClaim{}, apps: &heldAppsStore{held: agentapi.Apps{
			SessionURL: "https://backend.composio.dev/mcp/sess_1", SessionID: "sess_1"}}}
	guestPort, _ := strconv.Atoi(port)
	return s, agent.New(host, guestPort), &body
}

// connectedTo stands up a provider that reports these apps as connected, which
// is what a push now resolves over.
func connectedTo(t *testing.T, apps ...string) *composio.Client {
	t.Helper()
	rows := make([]string, 0, len(apps))
	for _, app := range apps {
		rows = append(rows, fmt.Sprintf(
			`{"id":"ca_%s","status":"ACTIVE","toolkit":{"slug":%q}}`, app, app))
	}
	body := `{"items":[` + strings.Join(rows, ",") + `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return composio.New("k", srv.URL)
}

// A push that landed has to say so on the SET's clock, not the one the claim was
// taken with.
//
// The claim starts dated appsMintTimeout out, which is only the in-flight guard.
// Left at that, a machine would come due twenty seconds after every successful
// push -- a re-fan-out and a fresh ticket per machine, three times a minute,
// forever. That is worse than the latch it replaced, and it is invisible in a
// unit test that only ever calls claimApps and doneApps by hand: this drives the
// real mintApps and reads back what it recorded.
func TestASuccessfulPushIsDueAgainOnItsSetsClockNotTheMintTimeout(t *testing.T) {
	s, cl, _ := pushingServer(t)
	if !s.claimApps("m1") {
		t.Fatal("the first caller did not get the claim")
	}
	s.mintApps(context.Background(), testUserID, vmView{ID: "m1", GuestIP: "127.0.0.1"}, cl)

	s.mu.Lock()
	held := s.appsClaims["m1"]
	s.mu.Unlock()
	if !held.pushed {
		t.Fatal("a push that landed did not leave a pushed claim")
	}
	if wait := time.Until(held.expires); wait < appCapsTTL-time.Minute {
		t.Errorf("a machine that was just pushed comes due again in %v, not on its "+
			"set's hour -- every machine re-tickets on the mint timeout forever", wait)
	}
}

// The failure path still wins over the deadline: a push that did not land leaves
// a cooldown, never a claim dated an hour out that nothing ever retries.
func TestAFailedPushDoesNotRecordASetDeadline(t *testing.T) {
	s, _, _ := pushingServer(t)
	// No guest to answer, so SetApps fails after the ticket is minted.
	if !s.claimApps("m1") {
		t.Fatal("the first caller did not get the claim")
	}
	s.mintApps(context.Background(), testUserID, vmView{ID: "m1", GuestIP: "127.0.0.1"},
		agent.New("127.0.0.1", 1))

	s.mu.Lock()
	held := s.appsClaims["m1"]
	s.mu.Unlock()
	if held.pushed {
		t.Error("a push that failed was recorded as done, so the machine waits an " +
			"hour before anything tries again")
	}
	if s.claimApps("m1") {
		t.Error("a failure was retried immediately rather than on the cooldown")
	}
}

// THE test for what makes opening the catalogue usable rather than merely
// possible. An app connected after a machine was pushed is an app whose reads
// raise a card until that machine next comes due -- up to an hour of the feature
// working and looking broken. The claim remembers WHICH apps its answer was
// about, so the next read of somebody's connections notices.
func TestAnAppConnectedAfterThePushBringsTheMachineBackEarly(t *testing.T) {
	machine := machineFor(testUserID)
	s := &Server{appsClaims: map[string]appsClaim{}}
	s.doneApps(machine, pushed{until: time.Now().Add(appCapsTTL), apps: "gmail", known: true})

	s.noteApps(machine, "gmail")
	if _, held := s.appsClaims[machine]; !held {
		t.Fatal("an unchanged set dropped the claim, so every screen re-pushes")
	}
	s.noteApps(machine, "gmail,notion")
	if _, held := s.appsClaims[machine]; held {
		t.Error("a newly connected app left the machine on its old answer")
	}
}

// THE case the empty mark used to swallow, and the one opening the catalogue is
// for: somebody with nothing connected who connects their FIRST app.
//
// Their mark is the empty string, which is also what a push that could not read
// anybody's connections falls back to -- so while those two were the same value,
// this person's claim read as a guess and nothing ever brought it back. Every
// read in the app they had just connected raised a card for the full hour.
func TestTheFirstAppSomebodyEverConnectsBringsTheMachineBackEarly(t *testing.T) {
	machine := machineFor(testUserID)
	s := &Server{appsClaims: map[string]appsClaim{}}
	s.doneApps(machine, pushed{until: time.Now().Add(appCapsTTL), apps: "", known: true})
	s.noteApps(machine, "notion")
	if _, held := s.appsClaims[machine]; held {
		t.Error("the first app they ever connected left the machine on its old answer")
	}
}

// A guessed-at answer is not compared against anything. A push that could not
// read somebody's connections resolves the floor instead, and letting that stand
// as a real mark would make the first route to see the true list take it for a
// change and re-push over nothing.
func TestAGuessedAnswerIsNotRememberedAsTheSet(t *testing.T) {
	machine := machineFor(testUserID)
	s := &Server{appsClaims: map[string]appsClaim{}}
	s.doneApps(machine, pushed{until: time.Now().Add(appCapsTTL)})
	s.noteApps(machine, "gmail,notion")
	if _, held := s.appsClaims[machine]; !held {
		t.Error("a guess was compared against the real list and lost the claim")
	}
}

// A claim taken but not yet answered has no mark, and nothing may compare
// against one. Otherwise the first screen to open during a mint would decide the
// in-flight push was stale and release the claim it exists to hold -- which is
// how one person's sign-in mints a session per request.
func TestAnInFlightClaimIsNotJudgedStale(t *testing.T) {
	machine := machineFor(testUserID)
	s := &Server{appsClaims: map[string]appsClaim{}}
	if !s.claimApps(machine) {
		t.Fatal("the claim was not taken")
	}
	s.noteApps(machine, "gmail")
	if !s.appsClaims[machine].pushed {
		t.Error("an in-flight claim was released, so the next request mints again")
	}
}

// Bringing a machine up for another push must not take its ticket with it.
//
// Nothing about who the machine is or which session is theirs has changed --
// only which apps the answer covers. Dropping the route leaves the guest dialling
// something the broker refuses, in the middle of the retry the person connected
// an app for. Register rotates it on the next push.
func TestAStaleAnswerDoesNotCostTheMachineItsSession(t *testing.T) {
	machine := machineFor(testUserID)
	gw := NewAppsGateway("the-project-key", "0.0.0.0:8092")
	if _, err := gw.Register(machine, "127.0.0.1", "127.0.0.1",
		"https://backend.composio.dev/mcp/sess_1"); err != nil {
		t.Fatal(err)
	}
	s := &Server{gw: gw, appsClaims: map[string]appsClaim{}}
	s.doneApps(machine, pushed{until: time.Now().Add(appCapsTTL), apps: "gmail", known: true})

	s.noteApps(machine, "gmail,notion")
	gw.mu.Lock()
	held := len(gw.routes)
	gw.mu.Unlock()
	if held != 1 {
		t.Error("the ticket went with the claim, so a running agent dials a route " +
			"the broker now refuses until something pushes")
	}
	// Erasing a machine is the other thing entirely, and there the route MUST go:
	// slots are recycled, and one left behind is how one person's agent ends up
	// acting as another.
	s.forgetApps(machine)
	gw.mu.Lock()
	held = len(gw.routes)
	gw.mu.Unlock()
	if held != 0 {
		t.Error("an erased machine kept its route")
	}
}

// What a push resolves over: this person's own apps, deduplicated, in any
// status. INITIATED is the useful half -- a row exists at the provider from the
// moment a link is minted, so an app somebody is signing into right now is
// already in the next push rather than asking about its reads until the machine
// comes due.
func TestThePushResolvesTheAppsThisPersonActuallyHolds(t *testing.T) {
	got := appsIn([]composio.Connection{
		{ID: "ca_1", Toolkit: "gmail", Status: composio.StatusActive},
		{ID: "ca_2", Toolkit: "gmail", Status: "EXPIRED"},
		{ID: "ca_3", Toolkit: "notion", Status: "INITIATED"},
		{ID: "ca_4", Toolkit: ""},
	})
	if !slices.Equal(got, []string{"gmail", "notion"}) {
		t.Errorf("got %v, want each app once and the nameless row dropped", got)
	}
}

// The fan-out is bounded. Every app's actions are a round trip, they go out at
// once, and they all have to land inside appsMintTimeout alongside minting the
// session -- so somebody who has connected two hundred apps must not aim two
// hundred simultaneous requests at the provider every time a machine boots.
func TestTheFanOutIsBoundedByWhatOnePushMayAsk(t *testing.T) {
	held := make([]composio.Connection, 0, appsResolveCap*2)
	for i := range appsResolveCap * 2 {
		held = append(held, composio.Connection{Toolkit: fmt.Sprintf("app%d", i)})
	}
	if got := appsIn(held); len(got) != appsResolveCap {
		t.Errorf("resolving %d apps, want at most %d", len(got), appsResolveCap)
	}
	// What actually leaves at once is that plus the floor, which is the number
	// worth bounding -- the fan-out is simultaneous.
	if got := withFeatured(appsIn(held)); len(got) != appsResolveCap+len(featured) {
		t.Errorf("%d requests go out at once, want %d", len(got), appsResolveCap+len(featured))
	}
}

// The mark says only WHICH apps, so it does not churn on things that change
// nothing: the provider's ordering is not stable, a second account for an app
// already connected adds no actions, and a connection expiring reclassifies
// none of them.
func TestTheMarkIgnoresEverythingThatChangesNoActions(t *testing.T) {
	one := appsMark([]composio.Connection{
		{ID: "ca_1", Toolkit: "notion", Status: composio.StatusActive},
		{ID: "ca_2", Toolkit: "gmail", Status: composio.StatusActive},
	})
	two := appsMark([]composio.Connection{
		{ID: "ca_3", Toolkit: "gmail", Status: "EXPIRED"},
		{ID: "ca_4", Toolkit: "gmail", Status: composio.StatusActive},
		{ID: "ca_5", Toolkit: "notion", Status: composio.StatusActive},
	})
	if one != two {
		t.Errorf("%q and %q differ, so a machine re-pushes over a reordered list", one, two)
	}
	if same := appsMark(nil); same == one {
		t.Error("holding nothing marks the same as holding two apps")
	}
}

// The floor under the resolved set, and it is there for a flow this service
// never sees: an agent mints its own connect link through the provider, and the
// page a person lands on afterwards is deliberately anonymous. Without the
// floor, connecting Gmail that way and immediately asking an agent to read it
// raises a card on every read until the Apps screen is next opened or the claim
// runs out -- up to an hour of the feature working and looking broken.
func TestTheFeaturedAppsAreResolvedWhetherOrNotTheyAreConnected(t *testing.T) {
	got := withFeatured([]string{"notion"})
	if got[0] != "notion" {
		t.Errorf("got %v, want this person's own apps first -- they are what "+
			"survives if the push fills up", got)
	}
	for _, app := range featured {
		if !slices.Contains(got, app) {
			t.Errorf("%s is missing, so connecting it mid-conversation asks about "+
				"its reads until the machine next comes due", app)
		}
	}
	// A featured app they have already connected is not resolved twice: that is
	// a second round trip for the same answer.
	if two := withFeatured([]string{"gmail"}); len(two) != len(featured) {
		t.Errorf("got %v, want gmail counted once", two)
	}
}

// THE race between a push and the connection made during it.
//
// A push reads somebody's connections, and while it is crossing the internet
// they finish connecting an app. A route reads the new set, finds a claim still
// in flight with nothing to compare against, and used to throw the reading away.
// The push then landed and latched the set IT read -- already out of date --
// for the rest of the hour, with nothing afterwards guaranteed to look again.
func TestAConnectionMadeDuringAPushIsNotBuriedByIt(t *testing.T) {
	machine := machineFor(testUserID)
	s := &Server{appsClaims: map[string]appsClaim{}}
	if !s.claimApps(machine) {
		t.Fatal("the claim was not taken")
	}
	// The push is in flight and read only gmail. They finish connecting notion,
	// and the Apps screen refreshing is what sees it.
	s.noteApps(machine, "gmail,notion")
	s.doneApps(machine, pushed{until: time.Now().Add(appCapsTTL), apps: "gmail", known: true})

	if _, held := s.appsClaims[machine]; held {
		t.Error("the older answer was latched over a newer reading, so the app " +
			"they just connected asks about its reads for the rest of the hour")
	}
}

// The other half: a reading that AGREES with what was pushed latches normally.
//
// Without this the claim would be released on any read during a push, and a
// client that fetches the Apps screen while one is in flight -- which is exactly
// what opening the app does -- would push again every time. That is the stampede
// the claim exists to prevent.
func TestAReadingThatAgreesWithThePushStillLatches(t *testing.T) {
	machine := machineFor(testUserID)
	s := &Server{appsClaims: map[string]appsClaim{}}
	s.claimApps(machine)
	s.noteApps(machine, "gmail")
	s.doneApps(machine, pushed{until: time.Now().Add(appCapsTTL), apps: "gmail", known: true})

	held, ok := s.appsClaims[machine]
	if !ok || !held.pushed {
		t.Fatal("a push nothing disagreed with was not recorded")
	}
	if !held.known || held.apps != "gmail" {
		t.Errorf("claim is %+v", held)
	}
}

// A push that fell back to the floor is not re-pushed at once on the strength of
// a reading it never made. It already comes due on the short clock, and retrying
// immediately would aim it at a provider that has just failed.
func TestAGuessedPushIsLeftOnItsCooldownRatherThanRetriedAtOnce(t *testing.T) {
	machine := machineFor(testUserID)
	s := &Server{appsClaims: map[string]appsClaim{}}
	s.claimApps(machine)
	s.noteApps(machine, "gmail,notion")
	s.doneApps(machine, pushed{until: time.Now().Add(appsRetry)})

	if _, held := s.appsClaims[machine]; !held {
		t.Error("a failed read was retried at once instead of waiting out its clock")
	}
}
