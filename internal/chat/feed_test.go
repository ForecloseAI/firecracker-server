package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"cracked/internal/agentapi"
)

// ev builds one guest event.
func ev(id int, typ string) agentapi.Event {
	return agentapi.Event{ID: id, Agent: "coder", Type: typ, TS: time.Now().UTC()}
}

// The client appends its own message from the POST response, so echoing it back
// would render a duplicate bubble. Their contract calls this out by name.
func TestOwnMessageIsNotEchoedButIsInHistory(t *testing.T) {
	user := ev(1, "user")
	user.Text = "do the thing"

	var out strings.Builder
	newFeed("m1", noHandoff).emitMessage(&out, "coder", user)
	if out.Len() != 0 {
		t.Errorf("the person's own message was echoed: %s", out.String())
	}

	th := buildThread("coder", []agentapi.Event{user}, "")
	if len(th.Messages) != 1 || th.Messages[0].From != fromMe {
		t.Fatalf("history = %+v, want the person's own line", th.Messages)
	}
}

// One mapper for both paths, so a line cannot render one way live and another
// way after a reload.
func TestStreamAndHistoryAgreeOnAMessage(t *testing.T) {
	e := ev(7, "text")
	e.Text = "done"

	var out strings.Builder
	newFeed("m1", noHandoff).emitMessage(&out, "coder", e)
	var frame feedFrame
	if err := json.Unmarshal(payload(t, out.String()), &frame); err != nil {
		t.Fatal(err)
	}
	th := buildThread("coder", []agentapi.Event{e}, "")
	if len(th.Messages) != 1 {
		t.Fatal("history dropped the message")
	}
	if *frame.Message != th.Messages[0] {
		t.Errorf("stream %+v != history %+v", *frame.Message, th.Messages[0])
	}
}

// payload pulls the JSON out of one SSE frame.
func payload(t *testing.T, s string) []byte {
	t.Helper()
	line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "data:"))
	if line == "" {
		t.Fatal("no frame written")
	}
	return []byte(line)
}

// Typing is how the app shows an agent is working, and it must only fire on a
// change or a reconnect would re-announce the same state.
func TestTypingOnlyOnChange(t *testing.T) {
	f := newFeed("m1", noHandoff)
	working, idle := ev(1, "state"), ev(2, "state")
	working.SessionState, idle.SessionState = "working", "idle"

	var out strings.Builder
	f.forward(&out, "coder", []agentapi.Event{working, working, idle})
	frames := strings.Count(out.String(), "data:")
	if frames != 2 {
		t.Fatalf("%d typing frames, want 2 (one per change)\n%s", frames, out.String())
	}
	if !strings.Contains(out.String(), `"typing":true`) {
		t.Error("no typing:true frame")
	}
}

// An ask carries what the person needs to decide, and its id is the event id --
// the same id the approval endpoint will be called with.
func TestAskCarriesDetailAndTheEventID(t *testing.T) {
	ask := ev(42, "approval_required")
	ask.Tool, ask.Preview, ask.ApprovalID = "Bash", "rm -rf /tmp/x", "coder.ap_001"
	m, ok := projectMessage(ask)
	if !ok || m.Kind != kindAsk {
		t.Fatalf("message = %+v, ok = %v", m, ok)
	}
	if m.ID != "42" {
		t.Errorf("id = %q, want the event id", m.ID)
	}
	if m.Detail != "rm -rf /tmp/x" || !strings.Contains(m.Text, "Bash") {
		t.Errorf("message = %+v", m)
	}
}

// History shows a settled ask with its outcome on the card.
func TestHistoryFillsTheVerdict(t *testing.T) {
	ask := ev(10, "approval_required")
	ask.Tool, ask.ApprovalID = "Bash", "coder.ap_001"
	text := ev(11, "text")
	text.Text = "ran it"
	decision := ev(12, "decision")
	decision.ApprovalID, decision.Decision = "coder.ap_001", "allow"

	th := buildThread("coder", []agentapi.Event{ask, text, decision}, "")
	if len(th.Messages) != 2 {
		t.Fatalf("messages = %+v", th.Messages)
	}
	if th.Messages[0].Verdict != "approved" || th.Messages[0].VerdictTime.IsZero() {
		t.Errorf("ask = %+v, want an approved verdict", th.Messages[0])
	}
	if th.Messages[1].Verdict != "" {
		t.Error("the verdict landed on the wrong message")
	}
}

// A decision on the stream settles the card the client is already showing.
func TestDecisionEmitsAskResolved(t *testing.T) {
	f := newFeed("m1", noHandoff)
	ask := ev(10, "approval_required")
	ask.Tool, ask.ApprovalID = "Bash", "coder.ap_001"
	decision := ev(12, "decision")
	decision.ApprovalID, decision.Decision = "coder.ap_001", "deny"

	var out strings.Builder
	f.forward(&out, "coder", []agentapi.Event{ask, decision})
	if !strings.Contains(out.String(), `"type":"ask_resolved"`) {
		t.Fatalf("no ask_resolved frame:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"messageId":"10"`) {
		t.Errorf("ask_resolved does not name the ask's message id:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"verdict":"denied"`) {
		t.Errorf("wrong verdict:\n%s", out.String())
	}
}

// A timed-out ask appends NO decision event -- the gate just forgets it -- so
// leaving the hub is the only evidence. Without this the card sits on the phone
// looking live and fails when tapped.
func TestExpiredAskIsSettled(t *testing.T) {
	f := newFeed("m1", noHandoff)
	ask := ev(10, "approval_required")
	ask.Tool, ask.ApprovalID = "Bash", "coder.ap_001"
	var seed strings.Builder
	f.forward(&seed, "coder", []agentapi.Event{ask})

	var out strings.Builder
	f.expire(&out, "coder.ap_001", "10", map[string]bool{}) // no longer raised
	if !strings.Contains(out.String(), `"type":"ask_resolved"`) {
		t.Fatalf("an expired ask was never settled:\n%s", out.String())
	}
	if !strings.Contains(out.String(), `"agentId":"coder"`) {
		t.Errorf("expiry did not name the agent:\n%s", out.String())
	}
	if len(f.pending) != 0 {
		t.Error("the expired ask is still tracked")
	}
}

// An ask still outstanding must not be reported as expired.
func TestLiveAskIsNotExpired(t *testing.T) {
	f := newFeed("m1", noHandoff)
	f.pending["coder.ap_001"] = "10"
	var out strings.Builder
	f.expire(&out, "coder.ap_001", "10", map[string]bool{"coder.ap_001": true})
	if out.Len() != 0 {
		t.Errorf("a live ask was reported expired: %s", out.String())
	}
}

// A fresh connection starts from now: replaying an old transcript down the
// stream would double every line the client just fetched from the thread.
func TestNewConnectionStartsFromNow(t *testing.T) {
	f := newFeed("m1", noHandoff)
	var out strings.Builder
	f.emitEvents(&out, nil, agentapi.Status{ID: "coder", LastEventID: 99})
	if out.Len() != 0 {
		t.Errorf("first tick replayed history: %s", out.String())
	}
	if f.cursor["coder"] != 99 {
		t.Errorf("cursor = %d, want 99", f.cursor["coder"])
	}
}

// A roster row is sent once, then only when it actually changes.
func TestAgentFrameOnlyOnChange(t *testing.T) {
	f := newFeed("m1", noHandoff)
	roster := []agentapi.Status{{ID: "coder", Name: "Coder", Type: "coder"}}
	var first, second strings.Builder
	f.emitRoster(&first, roster)
	f.emitRoster(&second, roster)
	if !strings.Contains(first.String(), `"type":"agent"`) {
		t.Fatal("a new agent was never announced")
	}
	if second.Len() != 0 {
		t.Errorf("an unchanged roster re-announced itself: %s", second.String())
	}
	roster[0].Task = &agentapi.Task{Title: "Reconciling invoices"}
	var third strings.Builder
	f.emitRoster(&third, roster)
	if !strings.Contains(third.String(), "Reconciling invoices") {
		t.Error("a changed row was not re-sent")
	}
}

// History is bounded, and it must keep the NEWEST -- the end of a conversation
// is the part someone is looking at.
func TestHistoryKeepsTheNewest(t *testing.T) {
	var events []agentapi.Event
	for i := 1; i <= historyCap+40; i++ {
		e := ev(i, "text")
		e.Text = strings.Repeat("x", 3)
		events = append(events, e)
	}
	th := buildThread("coder", events, "")
	if len(th.Messages) != historyCap {
		t.Fatalf("kept %d messages, want %d", len(th.Messages), historyCap)
	}
	if th.Messages[len(th.Messages)-1].ID != "240" {
		t.Errorf("last id = %q, want the newest", th.Messages[len(th.Messages)-1].ID)
	}
}

// The preview is what the roster list shows under the agent's name.
func TestThreadCarriesAPreview(t *testing.T) {
	e := ev(3, "text")
	e.Text = "all done"
	th := buildThread("coder", []agentapi.Event{e}, "")
	if th.LastMessage != "all done" || th.LastTime.IsZero() {
		t.Errorf("thread = %+v", th)
	}
	if th.Unread != 0 {
		t.Error("unread should be 0 until a read watermark exists")
	}
}

// A login has to happen on the machine's own screen, so a handoff ask carries
// the link rather than asking for a password through this service.
func TestHandoffAskCarriesTheURL(t *testing.T) {
	ask := ev(5, "question")
	ask.Question, ask.Kind, ask.ApprovalID = "Sign in?", "handoff", "coder.q_001"
	th := buildThread("coder", []agentapi.Event{ask}, "https://vnc.example/v/m1?k=abc")
	if th.Messages[0].URL == "" {
		t.Error("a handoff ask has no screen link")
	}
	plain := buildThread("coder", []agentapi.Event{ev(6, "text")}, "https://vnc.example/v/m1?k=abc")
	if len(plain.Messages) > 0 && plain.Messages[0].URL != "" {
		t.Error("an ordinary message carries a screen link")
	}
}

// Events the app has no way to render must not reach it. The default arm is
// what keeps this surface small: a guest that learns a new event type must not
// start pushing it at an app that cannot draw it.
func TestNoiseIsDropped(t *testing.T) {
	for _, typ := range []string{"tool_use", "usage", "ready", "memory", "state", "turn_complete"} {
		if _, ok := projectMessage(ev(1, typ)); ok {
			t.Errorf("%s reached the client", typ)
		}
	}
}

// The client upserts on an agent frame, so one that arrived without a role or
// capabilities would wipe what GET /agents had already told it.
func TestAgentFrameCarriesTheProfile(t *testing.T) {
	f := newFeed("m1", noHandoff)
	f.profiles["coder"] = agentapi.Profile{Key: "coder", Description: "Writes code"}
	var out strings.Builder
	f.emitRoster(&out, []agentapi.Status{{ID: "coder", Name: "Coder", Type: "coder"}})
	var frame feedFrame
	if err := json.Unmarshal(payload(t, out.String()), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Agent.Role != "Writes code" {
		t.Errorf("role = %q; an upsert would blank it out", frame.Agent.Role)
	}
}

// "stopped working" must be sent as an explicit false. Dropped by omitempty it
// reads as undefined, and the spinner never goes away.
func TestTypingFalseIsExplicit(t *testing.T) {
	f := newFeed("m1", noHandoff)
	f.typing["coder"] = true
	var out strings.Builder
	f.emitTyping(&out, "coder", false)
	if !strings.Contains(out.String(), `"typing":false`) {
		t.Errorf("typing:false was dropped from the frame: %s", out.String())
	}
}

// noHandoff stands in for the capability minter in tests that do not exercise a
// handoff card.
func noHandoff() string { return "" }
