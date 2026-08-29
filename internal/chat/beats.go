package chat

import (
	"encoding/json"
	"strings"

	"cracked/internal/agentapi"
)

// Beat labels come from tool names, not from model text: no extra model call,
// no cost, no latency, and they are what fill the 10-60s a turn can take.
// The browser names are bare: the daemon drives Chrome through an MCP server
// but exposes its tools under their own names, not under an mcp__chrome__
// prefix. A tool missing from this map renders as SILENCE, not as a generic
// line -- the page drops a beat with no label -- so an absent entry is not a
// cosmetic gap, it is a turn the person watches go by with nothing said.
var beats = map[string]string{
	"navigate_page":   "Opening a page",
	"take_snapshot":   "Reading the page",
	"click":           "Clicking",
	"fill":            "Typing",
	"fill_form":       "Filling a form",
	"press_key":       "Pressing a key",
	"wait_for":        "Waiting for the page",
	"take_screenshot": "Taking a screenshot",
	"list_pages":      "Checking open tabs",
	"handle_dialog":   "Answering a dialog",
	"new_page":        "Opening a tab",
	"select_page":     "Switching tabs",
	"close_page":      "Closing a tab",
	"drag":            "Dragging",
	"upload_file":     "Uploading a file",
	"hover":           "Hovering",

	"Bash":  "Running a command",
	"Read":  "Reading a file",
	"Write": "Writing a file",
	"Edit":  "Editing a file",
	"Glob":  "Looking for files",
	"Grep":  "Searching files",

	// Working as a team is most of what a boss does, and none of it was
	// narrated: the person watched an idle-looking page while several agents
	// worked.
	"delegate":         "Handing work to a specialist",
	"start_task":       "Opening a task folder",
	"message_agent":    "Messaging a teammate",
	"list_agents":      "Checking the team",
	"list_agent_types": "Checking what it can hire",
	"create_agent":     "Hiring a specialist",
	"delete_agent":     "Letting a specialist go",
}

// Where an agent keeps its own state, relative to its own directory. Each agent
// has one, under <state-dir>/agents/<id>/, so these are matched as trailing
// segments rather than as whole paths -- a fixed path would only ever match one
// agent, and would silently relabel every other agent's memory write as an
// ordinary file save.
const (
	memorySegment      = "/memory/"
	instructionsSuffix = "/instructions.md"
)

// filePath pulls the path out of a tool input, or "" when it is absent or the
// input is not an object. Never an error: an unreadable input just means the
// generic label.
func filePath(input json.RawMessage) string {
	var in struct {
		Path string `json:"path"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	return in.Path
}

// memoryBeat labels a write that lands in the agent's own state, and returns
// the memory-relative path for the detail line.
func memoryBeat(path string) (label, detail string, ok bool) {
	if strings.HasSuffix(path, instructionsSuffix) {
		return "Updating its instructions", "", true
	}
	if i := strings.LastIndex(path, memorySegment); i >= 0 {
		return "Updating memory", path[i+len(memorySegment):], true
	}
	return "", "", false
}

// beatLabel gives a tool a human label plus an optional detail, or "" for tools
// not worth narrating. Write and Edit are relabelled when they land in the
// memory tree: remembering something should not read like saving a scratch file.
func beatLabel(tool string, input json.RawMessage) (label, detail string) {
	if strings.HasPrefix(tool, agentapi.MCPToolPrefix) {
		return mcpBeat(tool), ""
	}
	if tool == "Write" || tool == "Edit" {
		if label, detail, ok := memoryBeat(filePath(input)); ok {
			return label, detail
		}
	}
	return beats[tool], ""
}

// mcpBeat narrates a call into a server the person registered.
//
// The map above cannot hold an entry for these: the names are minted on the
// guest when somebody registers a server, and are not knowable when this file is
// compiled. The map's own rule is that a missing entry renders as SILENCE rather
// than as a generic line -- so without this, every call to every server the
// person went to the trouble of adding is a turn they watch go by with nothing
// said at all.
//
// Everything goes in the LABEL. The page reads only the label and drops an empty
// one, so a detail here would be invisible.
func mcpBeat(tool string) string {
	rest := strings.TrimPrefix(tool, agentapi.MCPToolPrefix)
	server, action, ok := strings.Cut(rest, "__")
	if !ok || server == "" || action == "" {
		return "Using a connected app"
	}
	return displayID(server) + ": " + spaced(action)
}

// displayID renders a server id the way a person named it: notion -> Notion.
// Ids are lowercase ASCII by construction, so indexing the first byte is safe.
func displayID(id string) string {
	return strings.ToUpper(id[:1]) + spaced(id[1:])
}

// spaced turns a tool name into words.
func spaced(s string) string {
	return strings.NewReplacer("_", " ", "-", " ").Replace(s)
}
