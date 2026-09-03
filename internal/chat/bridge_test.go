package chat

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cracked/internal/agentapi"
)

// deadControl stands in for the control plane, 404ing so consume() returns
// immediately instead of reaching for a real guest.
func deadControl(t *testing.T) *Control {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no such vm", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return NewControl(srv.URL, "test-token")
}

// testBridge is a bridge over that dead control plane, stopped when the test
// ends. Every bridge test starts here.
func testBridge(t *testing.T) *Bridge {
	t.Helper()
	b := newBridge("alice", deadControl(t), nil)
	t.Cleanup(b.stop)
	return b
}

// TestBridgeRevivesAfterIdleStop is the regression guard: a bridge that idled
// out stays in the server's map, so Subscribe must restart its consumer or
// every later page load gets a connected-looking stream that never delivers.
func TestBridgeRevivesAfterIdleStop(t *testing.T) {
	b := testBridge(t)
	if b.ctx.Err() != nil {
		t.Fatal("a fresh bridge must be running")
	}
	ch := b.Subscribe()
	b.Unsubscribe(ch)
	b.stopIfEmpty()
	if b.ctx.Err() == nil {
		t.Fatal("stopIfEmpty must stop the consumer once the last browser left")
	}
	b.Subscribe()
	if b.ctx.Err() != nil {
		t.Fatal("Subscribe must revive a stopped bridge")
	}
}

// TestBridgeKeepsLastAcrossRestart checks the revived stream resumes from the
// same event id, so the idle gap produces neither a hole nor duplicates.
func TestBridgeKeepsLastAcrossRestart(t *testing.T) {
	b := testBridge(t)
	b.onEvent(agentapi.Event{ID: 42, Type: "text", Text: "hello"})
	b.stopIfEmpty()
	b.Subscribe()
	if got := b.since(); got != 42 {
		t.Errorf("resume point = %d, want 42", got)
	}
}

// TestBridgeIdleTimerStopsConsumer checks the timer path end to end, not just
// the stopIfEmpty call the other tests make directly.
func TestBridgeIdleTimerStopsConsumer(t *testing.T) {
	old := idleGrace
	idleGrace = 10 * time.Millisecond
	defer func() { idleGrace = old }()
	b := testBridge(t)
	b.Unsubscribe(b.Subscribe())
	if !eventually(func() bool { return b.ctx.Err() != nil }) {
		t.Fatal("idle timer never stopped the consumer")
	}
	b.Subscribe()
	if b.ctx.Err() != nil {
		t.Fatal("Subscribe must revive after the idle timer fired")
	}
}

// eventually polls a condition for up to a second.
func eventually(cond func() bool) bool {
	for i := 0; i < 100; i++ {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
