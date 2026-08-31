package agentd

import (
	"context"
	"slices"

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
	files, err := fileTools(r, d.reload)
	if err != nil {
		return nil, err
	}
	rest, err := buildTools(
		func() (anthropic.BetaTool, error) { return bashTool(r.workspace, d.gate) },
		func() (anthropic.BetaTool, error) { return askTool(d.gate) },
		func() (anthropic.BetaTool, error) { return rememberPersonTool(d) },
		func() (anthropic.BetaTool, error) { return createSkillTool(r, d) },
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
	sched, err := scheduleTools(d)
	if err != nil {
		return nil, err
	}
	// One outbox shared by both send tools, so their numbers cannot collide. It
	// resumes from disk, which is what lets a client group by sequence across a
	// restart.
	send, err := sendTools(r, d, newOutbox(r.own))
	if err != nil {
		return nil, err
	}
	all := slices.Concat(files, rest, team, browser, sched, send)
	return keepAllowed(all, permitted(allow, d.browser)), nil
}

// alwaysAllowed are the tools every profile gets whether or not it names them.
// Anything about the person belongs to all of them equally, and requiring six
// profile files -- and every future one -- to remember a name is how a tool ends
// up missing from exactly the agent that needed it.
// Scheduling is here for the same reason: an agent that cannot put itself on a
// timer has to ask the boss to do it, and the tool creating one is gated anyway,
// so the person still agrees to every job whichever agent proposed it.
// create_skill is here for the same reason: an agent that cannot record what it
// just worked out has to rediscover it every time, and no profile should have to
// remember to ask for the ability to learn.
// send_file is here for the same reason again: every agent produces documents,
// and one that cannot hand its work over has done the work for nobody.
// send_screenshot is deliberately NOT here -- it rides the browser switch below,
// because only an agent driving the screen has one worth photographing.
// ownBrowserTools are OUR browser-gated tools. They are kept out of
// browserAllowed because that map also filters the MCP server's own tools/list,
// where a name the server never advertises would look like a tool that went
// missing. They ride the same single switch.
var ownBrowserTools = []string{"send_screenshot"}

var alwaysAllowed = []string{
	"remember_about_person", "schedule_task", "list_schedules", "cancel_schedule",
	"create_skill", "send_file",
}

// permitted expands what a profile allows with the tools it does not have to ask
// for: the always-on ones, and the browser surface when it declares a browser.
//
// One switch for the browser, not two. `browser: true` and a tools: list naming
// every browser tool would be two gates that can disagree, and the failure is
// silent in both directions: an unknown name in a profile is dropped without an
// error, and a profile that sets the flag but forgets a name just quietly loses
// that tool. The flag is the single source of truth; tools: stays authoritative
// for everything else.
//
// The MCP names come straight from browserAllowed, which is also what filters
// the server's tools/list -- so the allow list and the surface cannot drift
// apart. Our own browser-gated tools are named in ownBrowserTools instead, and
// gated on the same flag where they are built.
func permitted(allow []string, browser bool) []string {
	if len(allow) == 0 {
		return allow // an empty list already means everything
	}
	out := append(append([]string(nil), allow...), alwaysAllowed...)
	if !browser {
		return out
	}
	for name := range browserAllowed {
		out = append(out, name)
	}
	return append(out, ownBrowserTools...)
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
	// stateDir is the machine's, for the one file about the person that every
	// agent here reads and writes.
	stateDir string
	// reload is set by a tool whose effect only reaches the model through a
	// freshly composed prompt, so the agent knows to recycle itself when the
	// turn ends. A pointer: deps is copied by value into every tool closure.
	reload *reloadFlag
	chrome *browserServer
	snaps  *snapshotStore
	log    *Log
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

// rememberInput is the remember_about_person tool's argument.
type rememberInput struct {
	Fact string `json:"fact" jsonschema:"required,description=One sentence about the person that will still be true next month"`
}

// rememberPersonTool records something durable about the person.
//
// Every agent gets this and they all write the same file, because they all work
// for the same person: a name learned in one conversation should not be missing
// from the next. It appends rather than rewrites, so two agents recording
// something at once cannot lose each other's line.
func rememberPersonTool(d toolDeps) (anthropic.BetaTool, error) {
	return toolrunner.NewBetaToolFromJSONSchema[rememberInput](
		"remember_about_person",
		"Record something durable you have learned about the person, so every agent "+
			"here knows it from their next conversation on. Their name, their work, how "+
			"they like things done. Not what they asked for today.",
		func(ctx context.Context, in rememberInput) (anthropic.BetaToolResultBlockParamContentUnion, error) {
			if d.stateDir == "" {
				return toolText("there is nowhere to record that on this machine"), nil
			}
			if err := AppendAboutPerson(d.stateDir, in.Fact, d.self, personNow(d.stateDir)); err != nil {
				return toolText("could not record that: " + err.Error()), nil
			}
			return toolText("Recorded. Every agent here will have it from their next start."), nil
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
