package chat

import (
	"fmt"
	"testing"

	"cracked/internal/agentapi"
)

// approvalEvent is a guest approval waiting on a human decision.
func approvalEvent(id int, approvalID string) agentapi.Event {
	return agentapi.Event{ID: id, Type: "approval_required", ApprovalID: approvalID,
		Tool: "Bash", Preview: "Run shell command: rm -rf /tmp/x"}
}

// noisyLog is `n` ordinary frames, enough to blow past the replay window.
func noisyLog(from, n int) []agentapi.Event {
	out := make([]agentapi.Event, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, agentapi.Event{ID: from + i, Type: "text", Text: fmt.Sprintf("line %d", i)})
	}
	return out
}

// pendingFrames picks out the cards in a replay result.
func pendingFrames(frames []Frame) []Frame {
	var out []Frame
	for _, f := range frames {
		if f.Kind == "pending" {
			out = append(out, f)
		}
	}
	return out
}

// A card older than the replay window must still reach the page. The bridge goes
// on accepting an answer for its id, so without this the agent sits blocked on a
// question nobody can see until the gate times out half an hour later.
func TestPendingSurvivesTruncation(t *testing.T) {
	b := &Bridge{pending: map[string]*Pending{}}
	evs := append([]agentapi.Event{approvalEvent(1, "ap_001")}, noisyLog(2, ringSize+50)...)

	frames := b.replay(evs)

	cards := pendingFrames(frames)
	if len(cards) != 1 {
		t.Fatalf("got %d cards, want 1 -- the card was truncated away", len(cards))
	}
	if cards[0].PendingID != "ap_001" {
		t.Errorf("PendingID = %q, want ap_001", cards[0].PendingID)
	}
	// ID 0 keeps emitFrame's resume watermark untouched; a real id here would be
	// silently swallowed as already-sent.
	if cards[0].ID != 0 {
		t.Errorf("re-emitted card ID = %d, want 0", cards[0].ID)
	}
	if frames[len(frames)-1].Kind != "pending" {
		t.Error("the card must land last, where the newest thing belongs")
	}
}

// Within the window the card is already there, and a second copy would render as
// a duplicate: chat.html appends a new node per pending frame.
func TestPendingNotDuplicatedWhenInWindow(t *testing.T) {
	b := &Bridge{pending: map[string]*Pending{}}

	frames := b.replay([]agentapi.Event{approvalEvent(1, "ap_001"), {ID: 2, Type: "text", Text: "hi"}})

	if got := len(pendingFrames(frames)); got != 1 {
		t.Fatalf("got %d cards, want exactly 1", got)
	}
}

// An answered card must not come back on the next page load.
func TestResolvedPendingNotReemitted(t *testing.T) {
	b := &Bridge{pending: map[string]*Pending{}}
	evs := append([]agentapi.Event{
		approvalEvent(1, "ap_001"),
		{ID: 2, Type: "decision", ApprovalID: "ap_001", Decision: "allow"},
	}, noisyLog(3, ringSize+50)...)

	frames := b.replay(evs)

	if got := len(pendingFrames(frames)); got != 0 {
		t.Fatalf("got %d cards, want 0 -- a resolved card was re-emitted", got)
	}
}

// Several stranded cards must come back in a stable order, not map order.
func TestUnshownPendingIsOrdered(t *testing.T) {
	b := &Bridge{pending: map[string]*Pending{}}
	evs := append([]agentapi.Event{
		approvalEvent(1, "ap_003"), approvalEvent(2, "ap_001"), approvalEvent(3, "ap_002"),
	}, noisyLog(4, ringSize+50)...)

	for i := 0; i < 5; i++ {
		cards := pendingFrames(b.replay(evs))
		if len(cards) != 3 {
			t.Fatalf("got %d cards, want 3", len(cards))
		}
		got := []string{cards[0].PendingID, cards[1].PendingID, cards[2].PendingID}
		want := []string{"ap_001", "ap_002", "ap_003"}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("order = %v, want %v", got, want)
			}
		}
	}
}
