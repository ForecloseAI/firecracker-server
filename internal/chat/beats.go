package chat

import (
	"encoding/json"
	"strings"
)

// Beat labels come from tool names, not from model text: no extra model call,
// no cost, no latency, and they are what fill the 10-60s a turn can take.
var beats = map[string]string{
	"mcp__chrome__navigate_page":   "Opening a page",
	"mcp__chrome__take_snapshot":   "Reading the page",
	"mcp__chrome__click":           "Clicking",
	"mcp__chrome__fill":            "Typing",
	"mcp__chrome__fill_form":       "Filling a form",
	"mcp__chrome__press_key":       "Pressing a key",
	"mcp__chrome__wait_for":        "Waiting for the page",
	"mcp__chrome__take_screenshot": "Taking a screenshot",
	"mcp__chrome__evaluate_script": "Inspecting the page",
	"mcp__chrome__list_pages":      "Checking open tabs",
	"Bash":                         "Running a command",
	"Read":                         "Reading a file",
	"Write":                        "Writing a file",
	"Edit":                         "Editing a file",
	"Glob":                         "Looking for files",
	"Grep":                         "Searching files",
}

// Where the guest keeps the agent's own state. Compared as strings here; the
// host never opens these paths.
const (
	guestMemoryDir    = "/home/agent/agent-state/memory"
	guestInstructions = "/home/agent/agent-state/instructions.md"
)

// filePath pulls file_path out of a tool input, or "" when it is absent or the
// input is not an object. Never an error: an unreadable input just means the
// generic label.
func filePath(input json.RawMessage) string {
	var in struct {
		FilePath string `json:"file_path"`
	}
	if json.Unmarshal(input, &in) != nil {
		return ""
	}
	return in.FilePath
}

// memoryBeat labels a write that lands in the agent's own state, and returns
// the memory-relative path for the detail line.
func memoryBeat(path string) (label, detail string, ok bool) {
	if path == guestInstructions {
		return "Updating its instructions", "", true
	}
	if rel, cut := strings.CutPrefix(path, guestMemoryDir+"/"); cut {
		return "Updating memory", rel, true
	}
	return "", "", false
}

// beatLabel gives a tool a human label plus an optional detail, or "" for tools
// not worth narrating. Write and Edit are relabelled when they land in the
// memory tree: remembering something should not read like saving a scratch file.
func beatLabel(tool string, input json.RawMessage) (label, detail string) {
	if tool == "Write" || tool == "Edit" {
		if label, detail, ok := memoryBeat(filePath(input)); ok {
			return label, detail
		}
	}
	return beats[tool], ""
}
