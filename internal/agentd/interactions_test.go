package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"cracked/internal/agentapi"
)

// gateFor builds a gate belonging to a named agent, sharing one hub.
func gateFor(t *testing.T, hub *Interactions, agent string) *Gate {
	t.Helper()
	log, err := OpenLog(t.TempDir(), agent)
	if err != nil {
		t.Fatal(err)
	}
	return NewGate(log, hub, t.TempDir())
}

// raiseOne asks something and returns the id it was given, without waiting for
// an answer that is not coming.
func raiseOne(t *testing.T, g *Gate, hub *Interactions) string {
	t.Helper()
	go g.Check(context.Background(), "Bash", "rm -rf /tmp", nil)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := hub.List(); len(got) > 0 {
			for _, r := range got {
				if strings.HasPrefix(r.ID, g.log.agent+".") {
					return r.ID
				}
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("nothing was raised")
	return ""
}

// seq is per-gate and gates are per-agent, so without a namespace the boss's
// first approval and a worker's first are BOTH "ap_001". Any client keying on
// the id would have one card overwrite the other, and answering the visible one
// would answer the wrong agent's prompt.
func TestApprovalIDsAreUniqueAcrossAgents(t *testing.T) {
	hub := NewInteractions()
	boss := raiseOne(t, gateFor(t, hub, "boss"), hub)
	coder := raiseOne(t, gateFor(t, hub, "coder"), hub)
	if boss == coder {
		t.Fatalf("both agents minted %q", boss)
	}
	if !strings.HasPrefix(coder, "coder.") || !strings.HasPrefix(boss, "boss.") {
		t.Errorf("ids do not name their agent: %q and %q", boss, coder)
	}
}

// The point of the hub. A specialist that needs the person asks on its own
// behalf; the boss is not involved and must not have to be.
func TestWorkerRaisesDirectlyAndIsAttributed(t *testing.T) {
	hub := NewInteractions()
	id := raiseOne(t, gateFor(t, hub, "coder"), hub)
	raised := hub.List()
	if len(raised) != 1 {
		t.Fatalf("hub holds %d raised hands, want 1", len(raised))
	}
	if raised[0].Agent != "coder" {
		t.Errorf("attributed to %q, want the agent that actually asked", raised[0].Agent)
	}
	if raised[0].ID != id || raised[0].Preview == "" || raised[0].Kind != "approval_required" {
		t.Errorf("card is missing what a person needs to answer it: %+v", raised[0])
	}
}

// Answering must reach the agent that asked, and take the card down. A hub that
// kept the entry would leave a button that does nothing.
func TestResolvingReachesTheAskingAgentAndClears(t *testing.T) {
	hub := NewInteractions()
	g := gateFor(t, hub, "coder")
	id := raiseOne(t, g, hub)
	if !g.Resolve(id, Decision{Decision: "allow"}) {
		t.Fatal("the answer did not reach the waiting agent")
	}
	if got := hub.List(); len(got) != 0 {
		t.Errorf("card survived being answered: %+v", got)
	}
}

// A gate that gave up must not leave an answerable card behind. This is the
// failure the old replay-the-log approach produced in reverse: a card the
// person could still see for an agent that had stopped waiting half an hour ago.
func TestHubClearsWhenTheGateGivesUp(t *testing.T) {
	hub := NewInteractions()
	g := gateFor(t, hub, "coder")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.Check(ctx, "Bash", "rm -rf /tmp", nil); close(done) }()
	for len(hub.List()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if got := hub.List(); len(got) != 0 {
		t.Errorf("a cancelled wait left %d cards up", len(got))
	}
}

// Cards render oldest first, so a person answers in the order asked rather than
// in Go's map order, which changes on every read.
func TestRaisedHandsComeBackInTheOrderAsked(t *testing.T) {
	hub := NewInteractions()
	first := raiseOne(t, gateFor(t, hub, "boss"), hub)
	time.Sleep(2 * time.Millisecond)
	second := raiseOne(t, gateFor(t, hub, "coder"), hub)
	got := hub.List()
	if len(got) != 2 || got[0].ID != first || got[1].ID != second {
		t.Errorf("order = %v, want %q then %q", ids(got), first, second)
	}
}

// ids is the id list, for a readable failure message.
func ids(rs []agentapi.Raised) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// A retried approval must not be able to boot the roster. Get STARTS an agent,
// so resolving through it would let a client that keeps clicking "allow" spawn
// every agent on the machine into memory -- the same hazard List and /health
// are already careful about.
func TestResolveDoesNotStartAnAgent(t *testing.T) {
	sup := newTestSupervisor(t)
	if _, err := sup.Roster().Add(Record{Type: "coder", Name: "Coder"}); err != nil {
		t.Fatal(err)
	}
	s := NewServer(sup)
	before := sup.LiveCount()

	w := do(t, s, "POST", "/approvals/coder.ap_001", `{"decision":"allow"}`)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for an interaction nobody is waiting on", w.Code)
	}
	if got := sup.LiveCount(); got != before {
		t.Errorf("live agents went %d -> %d; answering must never start one", before, got)
	}
}

// An id that names no agent is a malformed request, not a reason to scan the
// roster looking for something that might match it.
func TestResolveRejectsAnIDThatNamesNoAgent(t *testing.T) {
	sup := newTestSupervisor(t)
	if do(t, NewServer(sup), "POST", "/approvals/ap_001", `{"decision":"allow"}`).Code != http.StatusNotFound {
		t.Error("a bare id without an agent prefix should not resolve")
	}
}

// The person is answering the team, so one call has to show every raised hand.
func TestPendingListsEveryAgentsRaisedHand(t *testing.T) {
	sup := newTestSupervisor(t)
	hub := sup.Interactions()
	raiseOne(t, gateFor(t, hub, "boss"), hub)
	raiseOne(t, gateFor(t, hub, "coder"), hub)

	w := do(t, NewServer(sup), "GET", "/pending", "")
	var got []agentapi.Raised
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("/pending returned %d, want both agents: %s", len(got), w.Body)
	}
	seen := map[string]bool{got[0].Agent: true, got[1].Agent: true}
	if !seen["boss"] || !seen["coder"] {
		t.Errorf("agents = %v, want boss and coder", seen)
	}
}

// The person addresses the boss directly, so it must always be startable.
// Refusing because every specialist is busy would lock them out of their own
// conversation precisely when the work they asked for is running -- and the
// only symptom is a send that fails with a 503.
func TestTheBossStartsEvenWhenEveryWorkerIsBusy(t *testing.T) {
	sup := supervisorWith(t, 1)
	if _, err := sup.Create("coder", "Ada"); err != nil {
		t.Fatal(err)
	}
	ada, err := sup.Get("ada")
	if err != nil {
		t.Fatal(err)
	}
	ada.setState("working") // the ceiling is 1, and it is now full and busy
	if _, err := sup.Get(BossID); err != nil {
		t.Fatalf("the boss could not start: %v", err)
	}
}
