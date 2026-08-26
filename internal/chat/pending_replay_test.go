package chat

import (
	"testing"
	"time"

	"cracked/internal/agentapi"
)

// raise pushes one hand up through the bridge, as the guest's hub would.
func raise(b *Bridge, id, agent string) {
	b.onPending(agentapi.PendingChange{Raised: &agentapi.Raised{
		ID: id, Agent: agent, Kind: "approval_required", Tool: "Bash", Preview: "rm -rf /tmp/x",
	}})
}

// cardsIn picks the pending frames out of a batch.
func cardsIn(fs []Frame) []Frame {
	out := []Frame{}
	for _, f := range fs {
		if f.Kind == "pending" {
			out = append(out, f)
		}
	}
	return out
}

// Cards come from the hub, not from the transcript. This is the whole point of
// the phase: a WORKER's approval is never in the boss's log, so a page that
// derived cards from that log could not show one at all -- and the person would
// never learn the agent was blocked.
func TestCardsComeFromTheHubNotTheLog(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	raise(b, "cody.ap_001", "cody")
	got := cardsIn(b.withPending(nil))
	if len(got) != 1 {
		t.Fatalf("got %d cards, want the worker's", len(got))
	}
	if got[0].Agent != "cody" || got[0].PendingID != "cody.ap_001" {
		t.Errorf("card = %+v, want it attributed to cody", got[0])
	}
}

// A reload must show one card, not two. The old path re-emitted stranded cards
// alongside any the replay window still held, and chat.html appends a new node
// per pending frame -- so duplicates piled up with live-looking buttons, while
// only the newest ever greyed out.
func TestReconnectDoesNotDuplicateCards(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	raise(b, "boss.ap_001", "boss")
	first := cardsIn(b.withPending(nil))
	second := cardsIn(b.withPending(nil))
	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("first %d cards, second %d; want exactly 1 each", len(first), len(second))
	}
}

// An answered card must not come back on the next page load.
func TestResolvedCardIsNotReemitted(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	raise(b, "boss.ap_001", "boss")
	b.onPending(agentapi.PendingChange{ClearedID: "boss.ap_001"})
	if got := cardsIn(b.withPending(nil)); len(got) != 0 {
		t.Errorf("an answered card came back: %+v", got)
	}
}

// A gate that gives up logs NO decision event -- it just stops waiting. The old
// path only dropped a card when it saw one, so a timed-out card became a
// permanent orphan that every later page load re-rendered, and clicking it hit
// an id the guest had already forgotten.
func TestTimedOutCardDisappears(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	raise(b, "cody.ap_001", "cody")
	b.onPending(agentapi.PendingChange{ClearedID: "cody.ap_001"})
	if got := cardsIn(b.withPending(nil)); len(got) != 0 {
		t.Errorf("a timed-out card survived: %+v", got)
	}
	if _, live := b.Pending("cody.ap_001"); live {
		t.Error("the bridge would still accept an answer for a card nobody is waiting on")
	}
}

// Clearing must reach the browser, or the card stays live-looking forever. It
// cannot come from a `decision` event: a worker's decision lands in the
// worker's log, which chat does not stream.
func TestClearingEmitsResolved(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	ch := b.Subscribe()
	defer b.Unsubscribe(ch)
	raise(b, "cody.ap_001", "cody")
	b.onPending(agentapi.PendingChange{ClearedID: "cody.ap_001"})
	<-ch // the pending frame
	if f := <-ch; f.Kind != "resolved" || f.PendingID != "cody.ap_001" {
		t.Errorf("frame = %+v, want a resolved for cody.ap_001", f)
	}
}

// Cards render in a stable order, not Go's map order, so they do not shuffle on
// every reload.
func TestCardsComeBackInAStableOrder(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	for _, id := range []string{"cody.ap_003", "boss.ap_001", "cody.ap_002"} {
		raise(b, id, "x")
	}
	got := cardsIn(b.withPending(nil))
	if len(got) != 3 || got[0].PendingID != "boss.ap_001" || got[2].PendingID != "cody.ap_003" {
		t.Errorf("order = %v", []string{got[0].PendingID, got[1].PendingID, got[2].PendingID})
	}
}

// The hub replays its whole current set every time a stream opens, so the same
// card arrives again on every blip. Without dedup the page stacks a duplicate
// node per card per reconnect, and because its map keeps only the newest, the
// older copies keep live-looking buttons that can never grey out.
func TestRaisedTwiceEmitsOneCard(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	ch := b.Subscribe()
	defer b.Unsubscribe(ch)
	raise(b, "cody.ap_001", "cody")
	raise(b, "cody.ap_001", "cody")
	if f := <-ch; f.Kind != "pending" {
		t.Fatalf("first frame = %+v, want the card", f)
	}
	select {
	case f := <-ch:
		t.Errorf("a second frame was emitted for the same card: %+v", f)
	default:
	}
}

// A card answered while every browser was away is invisible to the stream: the
// hub replays `raised` on connect but never `cleared`. Without reconciling
// against the snapshot the card comes back on the next page load with buttons
// that 404 — the stale orphan this phase exists to end, returning through the
// one door the stream cannot cover.
func TestResyncClearsCardsSettledWhileStopped(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	raise(b, "cody.ap_001", "cody")
	b.resync(nil) // the hub no longer holds it
	if got := cardsIn(b.withPending(nil)); len(got) != 0 {
		t.Errorf("a card settled while away came back: %+v", got)
	}
	if _, live := b.Pending("cody.ap_001"); live {
		t.Error("the bridge would still accept an answer for it")
	}
}

// Cards render in the order the person was asked. Ids are namespaced, so
// sorting by id would group by agent — a worker's newer question would jump
// above the boss's older one purely on the letter it starts with.
func TestCardsAreOrderedByWhenAsked(t *testing.T) {
	b := newBridge("alice", deadControl(t), nil)
	defer b.stop()
	base := time.Now().UTC()
	for i, id := range []string{"zeta.ap_001", "alpha.ap_001"} {
		b.onPending(agentapi.PendingChange{Raised: &agentapi.Raised{
			ID: id, Agent: "x", Kind: "approval_required", Tool: "Bash",
			Since: base.Add(time.Duration(i) * time.Second),
		}})
	}
	got := cardsIn(b.withPending(nil))
	if len(got) != 2 || got[0].PendingID != "zeta.ap_001" {
		t.Errorf("order = %v, want the earlier question first", got)
	}
}
