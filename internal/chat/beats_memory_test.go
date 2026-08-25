package chat

import (
	"encoding/json"
	"testing"

	"cracked/internal/agentapi"
)

// Captured verbatim from a live VM the first time an agent wrote its memory.
// Keeping the real shape here guards against the guest's event format drifting
// away from what the bridge expects -- the failure mode is a silent fallback to
// "Writing a file", which nobody would notice.
const liveMemoryWrite = `{"id":11,"type":"tool_use","ts":"2026-08-23T22:11:37.235Z",` +
	`"tool":"Write","input":{"file_path":"/home/agent/agent-state/memory/people/naman.md",` +
	`"content":"---\ntype: person\ntitle: Naman\n---\n\n# Naman\n"}}`

// A real memory write must reach the browser as a memory beat carrying only the
// path -- never the file body, which for a 16k memory file would be kilobytes
// of JSON on every keystroke of the agent's work.
func TestFrameForRealMemoryWrite(t *testing.T) {
	var ev agentapi.Event
	if err := json.Unmarshal([]byte(liveMemoryWrite), &ev); err != nil {
		t.Fatalf("decode live event: %v", err)
	}

	frame, ok := (&Bridge{}).frameFor(ev)
	if !ok {
		t.Fatal("tool_use event produced no frame")
	}
	if frame.Label != "Updating memory" {
		t.Errorf("Label = %q, want %q", frame.Label, "Updating memory")
	}
	if frame.Detail != "people/naman.md" {
		t.Errorf("Detail = %q, want %q", frame.Detail, "people/naman.md")
	}

	body, err := json.Marshal(frame)
	if err != nil {
		t.Fatalf("marshal frame: %v", err)
	}
	if len(body) > 200 {
		t.Errorf("frame is %d bytes; the file body must not ride along: %s", len(body), body)
	}
}
