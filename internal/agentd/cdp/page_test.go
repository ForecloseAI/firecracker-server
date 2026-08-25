package cdp

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Before this, Call blocked until the reply landed or the caller's context
// ended -- and the context a tool handler gets is the agent's Run context,
// cancelled only on eviction or interrupt. So one unanswered command parked
// that goroutine for the life of the process and stalled Supervisor.Close's
// wg.Wait() until systemd resorted to SIGKILL. An open JavaScript dialog is
// exactly what stops Chrome answering.
func TestCallTimesOutWhenChromeNeverAnswers(t *testing.T) {
	old := callTimeout
	callTimeout = 100 * time.Millisecond
	t.Cleanup(func() { callTimeout = old })

	c := dialFake(t, fakeChrome(t, func(_ *websocket.Conn, _ message) {})) // never answers
	err := c.Call(context.Background(), "S", "Page.navigate", nil, nil)
	if err == nil {
		t.Fatal("a command with no reply and no deadline returned nil")
	}
}

// A caller that sets its own deadline must keep it: the floor in Call is a
// backstop, not a policy, and an action that wants fifteen seconds should not
// be given thirty.
func TestAnExplicitDeadlineBeatsTheFloor(t *testing.T) {
	old := callTimeout
	callTimeout = time.Hour
	t.Cleanup(func() { callTimeout = old })

	c := dialFake(t, fakeChrome(t, func(_ *websocket.Conn, _ message) {}))
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Call(ctx, "S", "Page.navigate", nil, nil); err == nil {
		t.Fatal("expected the caller's deadline to fire")
	}
	if time.Since(start) > 5*time.Second {
		t.Error("the floor overrode the caller's shorter deadline")
	}
}

// Navigate is the sharpest instance of the interleaving hole, not an exception
// to it: another agent computing an element's quads and then dispatching a
// press across this call would click those coordinates on a different page
// entirely, with nothing reporting an error.
func TestNavigateWaitsForTheActionLock(t *testing.T) {
	b, rec := fakeBrowser(t, answers(nil))
	b.act <- struct{}{} // another action is in flight
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := b.Navigate(ctx, "https://example.com")
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v, want the busy refusal", err)
	}
	if rec.count("Page.navigate") != 0 {
		t.Error("Navigate reached Chrome while another action held the lock")
	}
}

// Take is deliberately OUTSIDE the action lock. Serialising every click behind
// a 500 KB accessibility tree would be a real cost for no correctness gain: a
// snapshot landing mid-action only bumps the generation, and a backendNodeId
// already resolved still names the same node.
func TestSnapshotsAreNotSerialisedBehindActions(t *testing.T) {
	b, _ := fakeBrowser(t, answers(map[string]string{
		"Accessibility.getFullAXTree": `{"nodes":[]}`}))
	b.act <- struct{}{}
	defer func() { <-b.act }()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := b.Take(ctx); err != nil {
		t.Errorf("Take blocked on the action lock: %v", err)
	}
}
