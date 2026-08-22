package chat

import (
	"encoding/json"
	"testing"

	"cracked/internal/agent"
)

// ids lists an option set for comparison.
func ids(p *Pending) []string {
	out := make([]string, 0, len(p.UI.Options))
	for _, o := range p.UI.Options {
		out = append(out, o.ID)
	}
	return out
}

// TestBashApprovalOffersBatch checks the three-option shape for a gated shell
// command. Batch exists only here because gate.applyScope grants Bash.
func TestBashApprovalOffersBatch(t *testing.T) {
	p := buildPending(agent.Event{
		Type: "approval_required", ApprovalID: "ap_001", Tool: "Bash", Preview: "rm -rf /tmp/x",
	})
	if got := ids(p); len(got) != 3 || got[1] != "batch" {
		t.Fatalf("options = %v, want once/batch/deny", got)
	}
	body, ok := p.Body("batch")
	if !ok || body["scope"] != "batch" || body["max_uses"] != batchUses {
		t.Fatalf("batch body = %v", body)
	}
}

// TestNonBashApprovalHasNoBatch guards against handing out a standing grant
// for a tool the guest would not apply it to.
func TestNonBashApprovalHasNoBatch(t *testing.T) {
	p := buildPending(agent.Event{Type: "approval_required", ApprovalID: "ap_002", Tool: "Write"})
	if got := ids(p); len(got) != 2 {
		t.Fatalf("options = %v, want once/deny only", got)
	}
	if _, ok := p.Body("batch"); ok {
		t.Fatal("non-Bash approval must not offer a batch grant")
	}
}

// TestUnknownOptionRejected is the important one: the page names an option and
// never authors the body, so an invented name must not reach the guest.
func TestUnknownOptionRejected(t *testing.T) {
	p := buildPending(agent.Event{Type: "approval_required", ApprovalID: "ap_003", Tool: "Bash"})
	for _, bad := range []string{"", "allow", "../../etc", "BATCH"} {
		if _, ok := p.Body(bad); ok {
			t.Fatalf("option %q should be unknown", bad)
		}
	}
}

// TestQuestionKinds covers each ask_human rendering.
func TestQuestionKinds(t *testing.T) {
	cases := map[string]int{"text": 0, "confirm": 2, "handoff": 2}
	for kind, want := range cases {
		p := buildPending(agent.Event{Type: "question", ApprovalID: "q_1", Kind: kind, Question: "?"})
		if p.UI.Kind != kind {
			t.Errorf("kind %s rendered as %s", kind, p.UI.Kind)
		}
		if len(p.UI.Options) != want {
			t.Errorf("kind %s: %d options, want %d", kind, len(p.UI.Options), want)
		}
	}
}

// TestChoiceOptionsFromUI checks that choice labels become buttons with
// matching answer bodies.
func TestChoiceOptionsFromUI(t *testing.T) {
	ui, _ := json.Marshal(map[string][]string{"options": {"Delta", "United"}})
	p := buildPending(agent.Event{
		Type: "question", ApprovalID: "q_2", Kind: "choice", Question: "Which?", UI: ui,
	})
	if len(p.UI.Options) != 2 || p.UI.Options[1].Label != "United" {
		t.Fatalf("options = %+v", p.UI.Options)
	}
	if body, ok := p.Body("c1"); !ok || body["answer"] != "United" {
		t.Fatalf("c1 body = %v", body)
	}
}

// TestHandoffOpensAndAnswers checks the login path: one button opens the VNC
// tab, and resolving sends a plain answer.
func TestHandoffOpensAndAnswers(t *testing.T) {
	p := buildPending(agent.Event{Type: "question", ApprovalID: "q_3", Kind: "handoff", Question: "Sign in?"})
	if !p.UI.Options[0].Opens {
		t.Fatal("handoff must open the VNC link")
	}
	if body, ok := p.Body("done"); !ok || body["answer"] != "done" {
		t.Fatalf("done body = %v", body)
	}
}
