package chat

import (
	"fmt"

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
	Prompt string
	Detail string
	UI     UI
	bodies map[string]map[string]any
}

// batchUses is how many follow-up commands one "yes to the next few" covers.
const batchUses = 10

// buildPending turns a guest approval or question into a renderable card.
func buildPending(ev agentapi.Event) *Pending {
	if ev.Type == "approval_required" {
		return confirmPending(ev)
	}
	return questionPending(ev)
}

// confirmPending renders a gated tool call. The batch option is offered only
// for Bash, because gate.applyScope hardcodes a Bash grant.
func confirmPending(ev agentapi.Event) *Pending {
	p := &Pending{
		ID: ev.ApprovalID, Prompt: promptFor(ev), Detail: ev.Preview,
		bodies: map[string]map[string]any{
			"once": {"decision": "allow"},
			"deny": {"decision": "deny", "reason": "the person declined"},
		},
	}
	p.UI = UI{Kind: "confirm", Options: []Option{{ID: "once", Label: "Yes", Tone: "ok"}}}
	if ev.Tool == "Bash" {
		p.bodies["batch"] = map[string]any{
			"decision": "allow", "scope": "batch",
			"max_uses": batchUses, "ttl_seconds": 3600,
		}
		p.UI.Options = append(p.UI.Options, Option{
			ID: "batch", Label: fmt.Sprintf("Yes, next %d for an hour", batchUses), Tone: "warn"})
	}
	p.UI.Options = append(p.UI.Options, Option{ID: "deny", Label: "No", Tone: "bad"})
	return p
}

// promptFor gives a gated call a one-line human title.
func promptFor(ev agentapi.Event) string {
	if ev.Tool == "Bash" {
		return "Run a shell command?"
	}
	return "Allow " + ev.Tool + "?"
}

// questionPending renders an ask_human call by its declared kind.
func questionPending(ev agentapi.Event) *Pending {
	p := &Pending{ID: ev.ApprovalID, Prompt: ev.Question, bodies: map[string]map[string]any{}}
	switch ev.Kind {
	case "confirm":
		p.UI = UI{Kind: "confirm", Options: yesNo()}
		p.bodies["yes"] = map[string]any{"answer": "yes"}
		p.bodies["no"] = map[string]any{"answer": "no"}
	case "choice":
		p.UI = UI{Kind: "choice", Options: choiceOptions(ev, p.bodies)}
	case "handoff":
		p.UI = UI{Kind: "handoff", Options: handoffOptions()}
		p.bodies["done"] = map[string]any{"answer": "done"}
		p.bodies["cancel"] = map[string]any{"answer": "not now"}
	default:
		p.UI = UI{Kind: "text", Options: []Option{}}
	}
	return p
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
func choiceOptions(ev agentapi.Event, bodies map[string]map[string]any) []Option {
	// The daemon sends UI as a typed block, so the options arrive already
	// parsed. This used to hand-unmarshal a raw blob, because the host kept its
	// own copy of the event shape and had typed the field json.RawMessage.
	var labels []string
	if ev.UI != nil {
		labels = ev.UI.Options
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
