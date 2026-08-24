package chat

import (
	"encoding/json"
	"testing"
)

// A memory write must read as remembering, not as saving a file, and a write
// anywhere else must keep the label it has always had.
func TestBeatLabel(t *testing.T) {
	cases := []struct {
		name   string
		tool   string
		input  string
		label  string
		detail string
	}{
		{"memory concept", "Write", `{"file_path":"/home/agent/agent-state/memory/people/naman.md"}`,
			"Updating memory", "people/naman.md"},
		{"memory index edit", "Edit", `{"file_path":"/home/agent/agent-state/memory/index.md"}`,
			"Updating memory", "index.md"},
		{"instructions", "Write", `{"file_path":"/home/agent/agent-state/instructions.md"}`,
			"Updating its instructions", ""},
		{"ordinary file", "Write", `{"file_path":"/home/agent/workspace/notes.txt"}`,
			"Writing a file", ""},
		{"reading memory is still reading", "Read", `{"file_path":"/home/agent/agent-state/memory/index.md"}`,
			"Reading a file", ""},
		{"prefix must not match a sibling dir", "Write", `{"file_path":"/home/agent/agent-state/memory-notes/x.md"}`,
			"Writing a file", ""},
		// The Task tools are enabled in the guest but deliberately absent from
		// `beats`: they are the agent's own planning aid, and an empty label is
		// what keeps them out of the chat transcript (renderBeat drops labelless
		// frames). Giving any of them a label here would silently make task
		// tracking user-facing.
		{"TaskCreate never reaches the transcript", "TaskCreate",
			`{"subject":"x","description":"y","activeForm":"Doing x"}`, "", ""},
		{"TaskUpdate never reaches the transcript", "TaskUpdate",
			`{"taskId":"t1","status":"completed"}`, "", ""},
		{"TaskList never reaches the transcript", "TaskList", `{}`, "", ""},
		{"unknown tool stays silent", "SomeFutureTool", `{}`, "", ""},
		{"malformed input degrades", "Write", `not json`, "Writing a file", ""},
		{"absent input degrades", "Write", ``, "Writing a file", ""},
		{"browser tool ignores input", "mcp__chrome__click", `{"uid":"e1"}`, "Clicking", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			label, detail := beatLabel(c.tool, json.RawMessage(c.input))
			if label != c.label || detail != c.detail {
				t.Fatalf("beatLabel(%q, %q) = (%q, %q), want (%q, %q)",
					c.tool, c.input, label, detail, c.label, c.detail)
			}
		})
	}
}
