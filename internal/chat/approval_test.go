package chat

import (
	"encoding/json"
	"net/http"
	"testing"

	"cracked/internal/agentapi"
)

// ui is one ask's answering shape.
func ui(kind string, options ...string) *AskUI {
	return &AskUI{Kind: kind, Options: options}
}

// THE test. agentd reads any non-empty answer as consent, so a client string
// arriving at a tool gate would approve a gated shell command with nobody
// having pressed anything. It must be refused, not quietly dropped.
func TestFreeTextIsRefusedAtAToolGate(t *testing.T) {
	for _, answer := range []string{"yes go ahead", "allow", "y", " "} {
		body, ok := decisionBody(ui(askApproval), approvalReq{Verdict: verdictApproved, Answer: answer})
		if ok {
			t.Fatalf("answer %q was accepted at an approval and produced %v", answer, body)
		}
	}
}

// The same rule for the other two button-only kinds.
func TestFreeTextIsRefusedOnButtonAsks(t *testing.T) {
	for _, kind := range []string{askConfirm, askHandoff} {
		if _, ok := decisionBody(ui(kind), approvalReq{Verdict: verdictApproved, Answer: "x"}); ok {
			t.Errorf("%s accepted a free-text answer", kind)
		}
	}
}

// Each kind produces exactly the body the guest expects.
func TestDecisionBodies(t *testing.T) {
	cases := []struct {
		name   string
		ui     *AskUI
		req    approvalReq
		want   map[string]any
		refuse bool
	}{
		{"approval allow", ui(askApproval), approvalReq{Verdict: verdictApproved},
			map[string]any{"decision": "allow"}, false},
		{"approval deny", ui(askApproval), approvalReq{Verdict: verdictDenied},
			map[string]any{"decision": "deny", "reason": declined}, false},
		{"confirm yes", ui(askConfirm), approvalReq{Verdict: verdictApproved},
			map[string]any{"answer": "yes"}, false},
		{"confirm no", ui(askConfirm), approvalReq{Verdict: verdictDenied},
			map[string]any{"answer": "no"}, false},
		{"handoff done", ui(askHandoff), approvalReq{Verdict: verdictApproved},
			map[string]any{"answer": "done"}, false},
		{"handoff cancel", ui(askHandoff), approvalReq{Verdict: verdictDenied},
			map[string]any{"answer": "not now"}, false},
		{"text answered", ui(askText), approvalReq{Verdict: verdictApproved, Answer: "Lisbon"},
			map[string]any{"answer": "Lisbon"}, false},
		{"text declined", ui(askText), approvalReq{Verdict: verdictDenied},
			map[string]any{"decision": "deny", "reason": declined}, false},
		{"choice picked", ui(askChoice, "Delta", "United"),
			approvalReq{Verdict: verdictApproved, Answer: "United"},
			map[string]any{"answer": "United"}, false},

		// An empty answer reads as a refusal to the guest, so it would tell the
		// agent "no" on behalf of someone who meant to say something.
		{"text with no answer", ui(askText), approvalReq{Verdict: verdictApproved}, nil, true},
		{"choice not offered", ui(askChoice, "Delta"),
			approvalReq{Verdict: verdictApproved, Answer: "Ryanair"}, nil, true},
		{"unknown verdict", ui(askApproval), approvalReq{Verdict: "maybe"}, nil, true},
		{"empty verdict", ui(askApproval), approvalReq{}, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := decisionBody(c.ui, c.req)
			if c.refuse {
				if ok {
					t.Fatalf("accepted, producing %v", got)
				}
				return
			}
			if !ok {
				t.Fatal("refused a valid decision")
			}
			if !sameBody(got, c.want) {
				t.Errorf("body = %v, want %v", got, c.want)
			}
		})
	}
}

// sameBody compares two authored bodies.
func sameBody(a, b map[string]any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// A tool gate is always allow-or-deny; a question reports its own kind and, for
// a choice, the options the person may pick from.
func TestAskUIKinds(t *testing.T) {
	gate := agentapi.Event{Type: "approval_required", Tool: "Bash"}
	if got := askUIOf(gate); got.Kind != askApproval {
		t.Errorf("tool gate ui = %+v", got)
	}
	choice := agentapi.Event{Type: "question", Kind: askChoice,
		UI: &agentapi.UI{Kind: askChoice, Options: []string{"Delta", "United"}}}
	got := askUIOf(choice)
	if got.Kind != askChoice || len(got.Options) != 2 {
		t.Errorf("choice ui = %+v", got)
	}
	// ask_human defaults to a text question, so a kindless one must not become
	// a two-button card that can only answer it "yes".
	bare := agentapi.Event{Type: "question"}
	if got := askUIOf(bare); got.Kind != askText {
		t.Errorf("bare question ui = %+v, want text", got)
	}
}

// The ask a client sees carries its answering shape, or two buttons are the
// only thing it can render.
func TestAskMessageCarriesItsUI(t *testing.T) {
	e := ev(18, "question")
	e.Question, e.Kind, e.ApprovalID = "Which city?", askText, "coder.q_001"
	m, ok := projectMessage(e)
	if !ok || m.UI == nil {
		t.Fatalf("message = %+v", m)
	}
	if m.UI.Kind != askText {
		t.Errorf("ui.kind = %q", m.UI.Kind)
	}
	if plain, _ := projectMessage(ev(19, "text")); plain.UI != nil {
		t.Error("an ordinary message carries an answering shape")
	}
}

// A message id that is not an ask must not settle anything.
func TestApprovalRejectsANonAsk(t *testing.T) {
	g := &fakeGuest{roster: []agentapi.Status{{ID: "coder", Name: "Coder", Type: "coder"}}}
	g.events = []agentapi.Event{ev(4, "text")}
	s, u := serverOver(t, g)
	w := call(t, s, u, "POST", "/v1/threads/coder/messages/4/approval", `{"verdict":"approved"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if len(g.resolved) != 0 {
		t.Errorf("a non-ask reached the guest: %v", g.resolved)
	}
}

// The end-to-end shape: an ask is found, the body is authored, 204 comes back.
func TestApprovalDelivers(t *testing.T) {
	ask := ev(18, "approval_required")
	ask.Tool, ask.Preview, ask.ApprovalID = "Bash", "rm -rf /tmp/x", "coder.ap_001"
	g := &fakeGuest{roster: []agentapi.Status{{ID: "coder", Name: "Coder", Type: "coder"}}}
	g.events = []agentapi.Event{ask}
	s, u := serverOver(t, g)

	w := call(t, s, u, "POST", "/v1/threads/coder/messages/18/approval", `{"verdict":"approved"}`)
	if w.Code != http.StatusNoContent || w.Body.Len() != 0 {
		t.Fatalf("status = %d, body = %q", w.Code, w.Body.String())
	}
	if len(g.resolved) != 1 || g.resolved[0].id != "coder.ap_001" {
		t.Fatalf("guest received %+v", g.resolved)
	}
	if g.resolved[0].body["decision"] != "allow" {
		t.Errorf("body = %v", g.resolved[0].body)
	}
}

// An ask answered elsewhere, timed out, or revoked is a stale card -- 409 tells
// the client to re-fetch rather than that the route was wrong.
func TestSettledAskIs409(t *testing.T) {
	ask := ev(18, "approval_required")
	ask.Tool, ask.ApprovalID = "Bash", "coder.ap_001"
	g := &fakeGuest{roster: []agentapi.Status{{ID: "coder", Name: "Coder", Type: "coder"}}}
	g.events, g.resolveStatus = []agentapi.Event{ask}, http.StatusNotFound
	s, u := serverOver(t, g)
	w := call(t, s, u, "POST", "/v1/threads/coder/messages/18/approval", `{"verdict":"approved"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
}

// Free text at a tool gate must be refused over HTTP too, not just in the
// authoring function -- and the guest must never hear about it.
func TestFreeTextAtAToolGateIsRefusedOverHTTP(t *testing.T) {
	ask := ev(18, "approval_required")
	ask.Tool, ask.ApprovalID = "Bash", "coder.ap_001"
	g := &fakeGuest{roster: []agentapi.Status{{ID: "coder", Name: "Coder", Type: "coder"}}}
	g.events = []agentapi.Event{ask}
	s, u := serverOver(t, g)
	w := call(t, s, u, "POST", "/v1/threads/coder/messages/18/approval",
		`{"verdict":"approved","answer":"yes go ahead"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if len(g.resolved) != 0 {
		t.Fatalf("the guest was told to allow: %+v", g.resolved)
	}
}
