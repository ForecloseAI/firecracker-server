package chat

import (
	"fmt"
	"time"

	"cracked/internal/agentapi"
)

// Option is one button on a pending card. The body that actually goes to the
// guest is NOT here: see optionBody.
type Option struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Tone  string `json:"tone"`
	Opens bool   `json:"opens,omitempty"`
}

// UI tells the page how to render a pending interaction.
type UI struct {
	Kind    string   `json:"kind"`
	Options []Option `json:"options"`
	URL     string   `json:"url,omitempty"`
}

// Pending is a server-side pending interaction: what the page sees, plus the
// option bodies it must never see.
type Pending struct {
	ID     string
	Agent  string
	Since  time.Time
	Prompt string
	Detail string
	UI     UI
	bodies map[string]map[string]any
}

// batchUses is how many follow-up commands one "yes to the next few" covers.
const batchUses = 10

// buildPending turns a raised hand into a renderable card.
//
// Built from the hub rather than from an event, because the transcript carries
// only the boss's log: a worker's question is never in it, and neither is the
// decision that ends one.
func buildPending(r agentapi.Raised) *Pending {
	p := questionPending(r)
	if r.Kind == "approval_required" {
		p = confirmPending(r)
	}
	p.Agent, p.Since = r.Agent, r.Since
	return p
}

// confirmPending renders a gated tool call. The batch option is offered only
// for Bash, because gate.applyScope hardcodes a Bash grant.
func confirmPending(r agentapi.Raised) *Pending {
	p := &Pending{
		ID: r.ID, Prompt: promptFor(r.Tool), Detail: r.Preview,
		bodies: map[string]map[string]any{
			"once": {"decision": "allow"},
			"deny": {"decision": "deny", "reason": "the person declined"},
		},
	}
	p.UI = UI{Kind: "confirm", Options: []Option{{ID: "once", Label: "Yes", Tone: "ok"}}}
	// Offered for every gated tool, not only Bash. The old restriction existed
	// because the TypeScript gate hardcoded a Bash grant whatever was approved;
	// this gate scopes the grant to the tool actually being asked about.
	p.bodies["batch"] = map[string]any{
		"decision": "allow", "scope": "batch",
		"max_uses": batchUses, "ttl_seconds": 3600,
	}
	p.UI.Options = append(p.UI.Options, Option{
		ID: "batch", Label: fmt.Sprintf("Yes, next %d for an hour", batchUses), Tone: "warn"})
	p.UI.Options = append(p.UI.Options, Option{ID: "deny", Label: "No", Tone: "bad"})
	return p
}

// promptFor gives a gated call a one-line human title.
func promptFor(tool string) string {
	if tool == "Bash" {
		return "Run a shell command?"
	}
	return "Allow " + tool + "?"
}

// questionPending renders an ask_human call by its declared kind.
func questionPending(r agentapi.Raised) *Pending {
	p := &Pending{ID: r.ID, Prompt: r.Question, bodies: map[string]map[string]any{}}
	switch uiKind(r) {
	case "confirm":
		p.UI = UI{Kind: "confirm", Options: yesNo()}
		p.bodies["yes"] = map[string]any{"answer": "yes"}
		p.bodies["no"] = map[string]any{"answer": "no"}
	case "choice":
		p.UI = UI{Kind: "choice", Options: choiceOptions(r, p.bodies)}
	case "handoff":
		p.UI = UI{Kind: "handoff", Options: handoffOptions()}
		p.bodies["done"] = map[string]any{"answer": "done"}
		p.bodies["cancel"] = map[string]any{"answer": "not now"}
	default:
		p.UI = UI{Kind: "text", Options: []Option{}}
	}
	return p
}

// uiKind is how a question should render, defaulting to a free-text answer --
// which is what ask_human itself defaults to when the model says nothing.
func uiKind(r agentapi.Raised) string {
	if r.UI == nil || r.UI.Kind == "" {
		return "text"
	}
	return r.UI.Kind
}

// yesNo is the two-button set shared by confirm cards.
func yesNo() []Option {
	return []Option{{ID: "yes", Label: "Yes", Tone: "ok"}, {ID: "no", Label: "No", Tone: "bad"}}
}

// handoffOptions offers the VNC takeover and a decline.
func handoffOptions() []Option {
	return []Option{
		{ID: "done", Label: "Take over", Tone: "ok", Opens: true},
		{ID: "cancel", Label: "Not now", Tone: "bad"},
	}
}

// choiceOptions turns the tool's labels into buttons and their answer bodies.
func choiceOptions(r agentapi.Raised, bodies map[string]map[string]any) []Option {
	// The daemon sends UI as a typed block, so the options arrive already
	// parsed. This used to hand-unmarshal a raw blob, because the host kept its
	// own copy of the event shape and had typed the field json.RawMessage.
	var labels []string
	if r.UI != nil {
		labels = r.UI.Options
	}
	out := make([]Option, 0, len(labels))
	for i, label := range labels {
		id := fmt.Sprintf("c%d", i)
		bodies[id] = map[string]any{"answer": label}
		out = append(out, Option{ID: id, Label: label, Tone: "ok"})
	}
	return out
}

// Body returns the guest-bound body for a chosen option. Unknown ids are
// refused: the page picks from a server-authored list and never authors the
// body, so it cannot grant itself an unbounded standing approval.
func (p *Pending) Body(option string) (map[string]any, bool) {
	b, ok := p.bodies[option]
	return b, ok
}
