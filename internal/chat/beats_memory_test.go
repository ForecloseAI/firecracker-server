package chat

import (
	"encoding/json"
	"testing"

	"cracked/internal/agentapi"
)

// Captured verbatim from a live VM, from the boss writing its own memory.
// Keeping the real shape here guards against the guest's event format drifting
// away from what the bridge expects -- the failure mode is a silent fallback to
// "Writing a file", which nobody would notice.
//
// Note the path: memory is per-agent, under agents/<id>/, so matching a fixed
// location would relabel every agent's memory write but one.
const liveMemoryWrite = `{"id":31,"agent":"boss","type":"tool_use",` +
	`"ts":"2026-08-25T21:17:39.345442998Z","tool":"Write","input":{` +
	`"content":"---\ntype: person\ntitle: Naman\n---\n\n# Naman\n\nThe person this agent works for.\n",` +
	`"path":"/home/agent/agent-state/agentd/agents/boss/memory/people/naman.md"}}`

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
