package chat

import (
	"testing"

	"cracked/internal/agentapi"
)

// ids lists an option set for comparison.
func ids(p *Pending) []string {
	out := make([]string, 0, len(p.UI.Options))
	for _, o := range p.UI.Options {
		out = append(out, o.ID)
	}
	return out
}

// TestApprovalOffersBatchForEveryTool checks the three-option shape. Batch used
// to be Bash-only because the TypeScript gate granted Bash whatever was
// approved; this gate scopes the grant to the tool actually asked about, so
// withholding the option from other tools only made them more tedious to allow.
func TestApprovalOffersBatchForEveryTool(t *testing.T) {
	for _, tool := range []string{"Bash", "Write"} {
		p := buildPending(agentapi.Raised{
			Kind: "approval_required", ID: "cody.ap_001", Agent: "cody",
			Tool: tool, Preview: "rm -rf /tmp/x",
		})
		if got := ids(p); len(got) != 3 || got[1] != "batch" {
			t.Fatalf("%s options = %v, want once/batch/deny", tool, got)
		}
		body, ok := p.Body("batch")
		if !ok || body["scope"] != "batch" || body["max_uses"] != batchUses {
			t.Fatalf("%s batch body = %v", tool, body)
		}
	}
}

// A card must name the agent that raised it. Any agent can ask, and the person
// answers THAT agent -- a card with no byline reads as though the boss asked.
func TestCardNamesTheAskingAgent(t *testing.T) {
	p := buildPending(agentapi.Raised{
		Kind: "approval_required", ID: "cody.ap_001", Agent: "cody", Tool: "Bash"})
	if p.Agent != "cody" {
		t.Errorf("agent = %q, want the worker that asked", p.Agent)
	}
}

// TestUnknownOptionRejected is the important one: the page names an option and
// never authors the body, so an invented name must not reach the guest.
func TestUnknownOptionRejected(t *testing.T) {
	p := buildPending(agentapi.Raised{Kind: "approval_required", ID: "boss.ap_003", Tool: "Bash"})
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
		p := buildPending(agentapi.Raised{Kind: "question", ID: "boss.q_1", Question: "?",
			UI: &agentapi.UI{Kind: kind}})
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
	p := buildPending(agentapi.Raised{
		Kind: "question", ID: "boss.q_2", Question: "Which?",
		UI: &agentapi.UI{Kind: "choice", Options: []string{"Delta", "United"}},
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
	p := buildPending(agentapi.Raised{Kind: "question", ID: "boss.q_3", Question: "Sign in?",
		UI: &agentapi.UI{Kind: "handoff"}})
	if !p.UI.Options[0].Opens {
		t.Fatal("handoff must open the VNC link")
	}
	if body, ok := p.Body("done"); !ok || body["answer"] != "done" {
		t.Fatalf("done body = %v", body)
	}
}
