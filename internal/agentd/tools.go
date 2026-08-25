package agentd

import (
	"context"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/toolrunner"
)

// askInput is the ask_human tool's argument.
type askInput struct {
	Question string   `json:"question" jsonschema:"required,description=One short sentence for the person to answer"`
	Kind     string   `json:"kind" jsonschema:"description=One of text confirm choice or handoff - defaults to text"`
	Options  []string `json:"options" jsonschema:"description=Labels to choose between - for kind choice"`
}

// Tools assembles the whole tool surface for one agent, in one place so the
// surface can be read at a glance.
//
// Every tool is our own handler, which is what makes the gate a real
// chokepoint. The TypeScript agent could only gate Bash, because Read, Write
// and Edit were the SDK's built-ins and its permission callback saw them only
// as names.
func Tools(r roots, d toolDeps, allow []string) ([]anthropic.BetaTool, error) {
	files, err := fileTools(r)
	if err != nil {
		return nil, err
	}
	rest, err := buildTools(
		func() (anthropic.BetaTool, error) { return bashTool(r.workspace, d.gate) },
		func() (anthropic.BetaTool, error) { return askTool(d.gate) },
	)
	if err != nil {
		return nil, err
	}
	team, err := teamTools(d)
	if err != nil {
		return nil, err
	}
	browser, err := browserTools(d)
	if err != nil {
		return nil, err
	}
	all := append(append(append(files, rest...), team...), browser...)
	return keepAllowed(all, withBrowser(allow, d.browser)), nil
}

// withBrowser adds the browser tools to what a profile allows when it declares
// browser access.
//
// One switch, not two. `browser: true` and a tools: list naming every browser
// tool would be two gates that can disagree, and the failure is silent in both
// directions: an unknown name in a profile is dropped without an error, and a
// profile that sets the flag but forgets a name just quietly loses that tool.
// The flag is the single source of truth; tools: stays authoritative for
// everything else.
//
// The names come straight from browserAllowed, which is also what filters the
// server's tools/list -- so the allow list and the surface cannot drift apart.
func withBrowser(allow []string, browser bool) []string {
	if !browser || len(allow) == 0 {
		return allow
	}
	out := append([]string(nil), allow...)
	for name := range browserAllowed {
		out = append(out, name)
	}
	return out
}

// toolDeps is what tool construction needs beyond the roots: the approval
// gate, the supervisor the team tools reach through, which agent these tools
// belong to, and the shared browser with the store that keeps its snapshots
// out of the conversation.
type toolDeps struct {
	gate    *Gate
	team    *Supervisor
	self    string
	browser bool
	chrome  *browserServer
	snaps   *snapshotStore
	log     *Log
}

// keepAllowed narrows the surface to what a profile declares. An empty list
// means everything, so a profile that says nothing about tools is not silently
// crippled.
//
// This is a real restriction, not a prompt instruction: a tool that is not in
// the list is not sent to the model at all, so it cannot be called. That is how
// boss-only powers will be enforced when delegation arrives.
func keepAllowed(tools []anthropic.BetaTool, allow []string) []anthropic.BetaTool {
	if len(allow) == 0 {
		return tools
	}
	wanted := make(map[string]bool, len(allow))
	for _, name := range allow {
		wanted[name] = true
	}
	out := make([]anthropic.BetaTool, 0, len(tools))
	for _, tool := range tools {
		if wanted[tool.Name()] {
			out = append(out, tool)
		}
	}
	return out
}

// askTool is the model's only channel to the person.
func askTool(gate *Gate) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[askInput](
		"ask_human",
		"Ask the person a question and wait for their answer. Use kind 'handoff' when a password or "+
			"sign-in is needed: it hands them the machine so they type it themselves. Never ask for a secret as text.",
		func(ctx context.Context, in askInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			answer, err := gate.Ask(ctx, in.Question, UI{Kind: orDefault(in.Kind, "text"), Options: in.Options})
			if err != nil {
				return toolText(err.Error()), nil
			}
			return toolText(orDefault(answer, "(no answer)")), nil
		})
}

// buildTools runs a list of constructors, failing on the first error. Tool
// construction reflects over struct tags, so a mistake there is a programming
// error worth failing the process for rather than starting a crippled agent.
func buildTools(ctors ...func() (anthropic.BetaTool, error)) ([]anthropic.BetaTool, error) {
	out := make([]anthropic.BetaTool, 0, len(ctors))
	for _, ctor := range ctors {
		tool, err := ctor()
		if err != nil {
			return nil, err
		}
		out = append(out, tool)
	}
	return out, nil
}
