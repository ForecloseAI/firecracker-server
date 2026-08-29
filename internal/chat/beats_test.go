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
		{"memory concept", "Write", `{"path":"/home/agent/agent-state/agentd/agents/boss/memory/people/naman.md"}`,
			"Updating memory", "people/naman.md"},
		{"memory index edit", "Edit", `{"path":"/home/agent/agent-state/agentd/agents/boss/memory/index.md"}`,
			"Updating memory", "index.md"},
		{"instructions", "Write", `{"path":"/home/agent/agent-state/agentd/agents/boss/instructions.md"}`,
			"Updating its instructions", ""},
		{"ordinary file", "Write", `{"path":"/home/agent/workspace/notes.txt"}`,
			"Writing a file", ""},
		{"reading memory is still reading", "Read", `{"path":"/home/agent/agent-state/agentd/agents/boss/memory/index.md"}`,
			"Reading a file", ""},
		{"prefix must not match a sibling dir", "Write", `{"path":"/home/agent/agent-state/agentd/agents/boss/memory-notes/x.md"}`,
			"Writing a file", ""},
		// ask_human is deliberately absent from `beats`: the card it raises IS its
		// rendering, so a beat would narrate the same question twice, immediately
		// above the card asking it. An empty label is what keeps it out --
		// renderBeat drops labelless frames.
		{"ask_human is the card, not a beat", "ask_human", `{"question":"which city?"}`, "", ""},
		// Working as a team is most of what a boss does, and none of it used to be
		// narrated: the person watched an idle-looking page while agents worked.
		{"delegating is narrated", "delegate", `{"to":"cody","title":"fix it"}`,
			"Handing work to a specialist", ""},
		{"messaging a teammate is narrated", "message_agent", `{"to":"cody"}`,
			"Messaging a teammate", ""},
		{"unknown tool stays silent", "SomeFutureTool", `{}`, "", ""},
		{"malformed input degrades", "Write", `not json`, "Writing a file", ""},
		{"absent input degrades", "Write", ``, "Writing a file", ""},
		{"browser tool ignores input", "click", `{"uid":"e1"}`, "Clicking", ""},
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

// TestARegisteredToolIsNarratedRatherThanWatchedInSilence is the one that
// matters here. The beats map cannot name a tool that did not exist when this
// was compiled, and a name it does not hold renders as nothing at all -- so the
// person would watch a turn go by while their own connected app was being used.
func TestARegisteredToolIsNarratedRatherThanWatchedInSilence(t *testing.T) {
	cases := []struct{ tool, want string }{
		{"mcp__notion__search_pages", "Notion: search pages"},
		{"mcp__linear-eu__create_issue", "Linear eu: create issue"},
		{"mcp__notion__", "Using a connected app"},
		{"mcp__", "Using a connected app"},
		{"mcp__nounderscores", "Using a connected app"},
	}
	for _, c := range cases {
		got, _ := beatLabel(c.tool, nil)
		if got != c.want {
			t.Errorf("beatLabel(%q) = %q, want %q", c.tool, got, c.want)
		}
	}
}

// TestNoRegisteredToolEverRendersAsSilence states the invariant directly, so a
// future change to the parsing cannot reintroduce the empty label.
func TestNoRegisteredToolEverRendersAsSilence(t *testing.T) {
	for _, tool := range []string{"mcp__", "mcp____", "mcp__a__b", "mcp__x", "mcp__ __ "} {
		if got, _ := beatLabel(tool, nil); got == "" {
			t.Errorf("beatLabel(%q) rendered as silence", tool)
		}
	}
}

// TestBrowserBeatsAreStillBare guards the decision not to namespace the browser,
// which would have invalidated every entry in the map above.
func TestBrowserBeatsAreStillBare(t *testing.T) {
	if got, _ := beatLabel("take_snapshot", nil); got != "Reading the page" {
		t.Errorf("the browser lost its label: %q", got)
	}
}
