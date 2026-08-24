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
func Tools(root string, gate *Gate) ([]anthropic.BetaTool, error) {
	files, err := fileTools(root)
	if err != nil {
		return nil, err
	}
	rest, err := buildTools(
		func() (anthropic.BetaTool, error) { return bashTool(root, gate) },
		func() (anthropic.BetaTool, error) { return askTool(gate) },
	)
	if err != nil {
		return nil, err
	}
	return append(files, rest...), nil
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
