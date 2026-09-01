package chat

import (
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

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
