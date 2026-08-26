package chat

import (
	"hash/fnv"
	"time"
	"unicode"

	"cracked/internal/agentapi"
)

// Agent is one roster row as the app renders it. The field set is the client's,
// not the daemon's: everything here feeds a specific piece of UI, which is why
// display-only values like hue and shape are computed server-side rather than
// left for the client to invent differently on each screen.
type Agent struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Initial string `json:"initial"`
	Hue     int    `json:"hue"`
	Shape   string `json:"shape"`
	Online  bool   `json:"online"`
	Task    string `json:"task"`
	// State is idle, working or waiting. Task alone was not enough for the roster:
	// it says what the last job was called and stays set after that job ends, so a
	// client showing it could not tell work in progress from work long finished.
	State string `json:"state"`
	// Machine is a label, not an address. There is no phone field: nothing in
	// this system can place a call, and a made-up number would be worse than a
	// missing one.
	Machine      string       `json:"machine"`
	Stats        AgentStats   `json:"stats"`
	Capabilities Capabilities `json:"capabilities"`
}

// AgentStats is the three tiles on the detail screen. Zero for now: the real
// numbers come from the event logs, which this pass does not read.
type AgentStats struct {
	TasksDone      int    `json:"tasksDone"`
	ApprovalsAsked int    `json:"approvalsAsked"`
	TimeSaved      string `json:"timeSaved"`
}

// Capabilities is what an agent may do without asking first. Only browse has an
// implementation; the other three are reported false rather than omitted so the
// client can render four switches and grey out what this build cannot do.
type Capabilities struct {
	Browse bool `json:"browse"`
	Email  bool `json:"email"`
	Call   bool `json:"call"`
	Spend  bool `json:"spend"`
}

// Template is one kind of agent a person can add, as the gallery renders it.
// It carries the same avatar recipe as a roster row, so the card you pick looks
// like the agent you get -- which works exactly because a type can be activated
// once, making the agent's id the template's id.
type Template struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Role    string `json:"role"`
	Initial string `json:"initial"`
	Hue     int    `json:"hue"`
	Shape   string `json:"shape"`
	// Active says this type is already on the roster. Reported rather than
	// filtered out, so the client can grey the card or hide it; omitting the row
	// would take that choice away.
	Active       bool         `json:"active"`
	Capabilities Capabilities `json:"capabilities"`
}

// Message is one line of a conversation as the app renders it. One shape for
// all three kinds -- text, event and ask -- with the ask fields omitted when
// empty, so the client switches on Kind and nothing else.
type Message struct {
	Kind string    `json:"kind"`
	ID   string    `json:"id"`
	Time time.Time `json:"time"`
	From string    `json:"from,omitempty"`
	Text string    `json:"text"`

	// Detail is what an ask is asking about: the command, the amount, the page.
	Detail string `json:"detail,omitempty"`
	// File is something the person attached, kept on the message so the bubble
	// still shows it after a reload rather than only in the session that sent it.
	File *agentapi.File `json:"file,omitempty"`
	// Shot is where to fetch a picture of the screen a handoff leads to.
	Shot string `json:"shot,omitempty"`
	// UI tells the client what kind of answer this ask wants. Without it two
	// buttons are the only option, and a question like "which city?" gets
	// answered "yes" -- which unblocks the agent while telling it something the
	// person never said.
	UI *AskUI `json:"ui,omitempty"`
	// URL sends the person to the machine's own screen for a login handoff, so
	// the credential never passes through this service.
	URL         string    `json:"url,omitempty"`
	Verdict     string    `json:"verdict,omitempty"`
	VerdictTime time.Time `json:"verdictTime,omitzero"`
}

// AskUI is how an ask should be answered. One vocabulary across both sources: a
// gated tool becomes "approval", and a question reports its own kind.
type AskUI struct {
	Kind    string   `json:"kind"` // approval | confirm | text | choice | handoff
	Options []string `json:"options,omitempty"`
}

// Ask kinds, as the client switches on them.
const (
	askApproval = "approval"
	askText     = "text"
	askChoice   = "choice"
	askConfirm  = "confirm"
	askHandoff  = "handoff"
)

// projectTemplates turns the catalog into a gallery.
//
// The boss is excluded: every machine is created with one, it cannot be deleted,
// and a second would be an agent nobody could act on.
func projectTemplates(profiles []agentapi.Profile, roster []agentapi.Status) []Template {
	active := map[string]bool{}
	for _, st := range roster {
		active[st.Type] = true
	}
	out := make([]Template, 0, len(profiles))
	for _, p := range profiles {
		if p.Key == agentapi.BossID {
			continue
		}
		out = append(out, projectTemplate(p, active[p.Key]))
	}
	return out
}

// projectTemplate builds one gallery card.
func projectTemplate(p agentapi.Profile, active bool) Template {
	return Template{
		ID: p.Key, Name: p.Title, Role: p.Description,
		Initial: initialOf(p.Title), Hue: hueOf(p.Key), Shape: shapeOf(p.Key),
		Active: active, Capabilities: Capabilities{Browse: p.Browser},
	}
}

// shapes gives each profile a stable avatar shape. An unknown profile falls back
// to a circle rather than an empty string, which the client would not render.
var shapes = map[string]string{
	"boss": "diamond", "coder": "square", "researcher": "ring",
	"analyst": "circle", "accountant": "square", "marketer": "ring",
}

// projectRoster turns the daemon's roster into the app's agent list. online is
// passed in rather than read off Status.Live: Live means "holds a goroutine right
// now", and an idle agent is evicted to save memory, so using it would show a
// perfectly healthy agent as offline.
func projectRoster(roster []agentapi.Status, profiles []agentapi.Profile, machine string, online bool) []Agent {
	byKey := map[string]agentapi.Profile{}
	for _, p := range profiles {
		byKey[p.Key] = p
	}
	out := make([]Agent, 0, len(roster))
	for _, st := range roster {
		out = append(out, projectAgent(st, byKey[st.Type], machine, online))
	}
	return out
}

// projectAgent builds one row.
func projectAgent(st agentapi.Status, p agentapi.Profile, machine string, online bool) Agent {
	return Agent{
		ID: st.ID, Name: st.Name, Role: p.Description,
		Initial: initialOf(st.Name), Hue: hueOf(st.ID), Shape: shapeOf(st.Type),
		Online: online, Task: taskOf(st), State: stateOf(st), Machine: machine + " · Linux",
		Capabilities: Capabilities{Browse: p.Browser},
	}
}

// stateOf is what the agent is doing. An agent that has never been started
// reports nothing, which is idle.
func stateOf(st agentapi.Status) string {
	if st.State == "" {
		return "idle"
	}
	return st.State
}

// taskOf is what the agent is working on, or "" when it is between jobs.
func taskOf(st agentapi.Status) string {
	if st.Task == nil {
		return ""
	}
	return st.Task.Title
}

// shapeOf picks an avatar shape for a profile.
func shapeOf(profileKey string) string {
	if s, ok := shapes[profileKey]; ok {
		return s
	}
	return "circle"
}

// initialOf is the avatar letter: the first rune of the name, uppercased. A
// rune, not a byte, so a non-ASCII name does not render as half a character.
func initialOf(name string) string {
	for _, r := range name {
		return string(unicode.ToUpper(r))
	}
	return "?"
}

// hueOf derives a stable colour from an agent id. Hashed rather than assigned so
// it survives restarts and never depends on roster order -- an agent whose colour
// changed between two screens would read as a different agent.
func hueOf(id string) int {
	h := fnv.New32a()
	h.Write([]byte(id))
	return int(h.Sum32() % 360)
}
