package chat

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

// beatLabel gives a tool a human label, or "" for tools not worth narrating.
func beatLabel(tool string) string {
	return beats[tool]
}
