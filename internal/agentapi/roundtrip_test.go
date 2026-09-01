// Package agentapi_test is external on purpose: it imports the daemon, which
// imports agentapi, and only an external test package can do that without a
// cycle. Reaching the daemon's own alias is the point -- it is what fails if
// someone gives agentd a private copy of the wire format again.
package agentapi_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"cracked/internal/agentapi"
	"cracked/internal/agentd"
)

// fullEvent is an Event with every field set to something non-zero, built
// through the DAEMON's alias so the two names must stay one type.
func fullEvent() agentd.Event {
	return agentd.Event{
		ID: 42, Agent: "coder", Type: "usage",
		TS:   time.Date(2026, 8, 26, 9, 0, 0, 0, time.UTC),
		Text: "some text", From: "boss", To: "analyst", Tool: "Bash",
		Input: json.RawMessage(`{"command":"ls"}`),
		Model: "claude-sonnet-5",
		Usage: &agentapi.Usage{
			InputTokens: 1, OutputTokens: 2, CacheReadInputTokens: 3,
			CacheCreationInputTokens: 4, ClearedInputTokens: 5, ClearedToolUses: 6,
		},
		DurationMS: 1500, SessionState: "working", Message: "a message", IsError: true,
		ApprovalID: "coder.ap_001", Preview: "a preview", Question: "a question",
		File: &agentapi.File{Name: "n.pdf", Path: "/home/agent/workspace/uploads/n.pdf", Size: 12},
		Shot: "handoff-001-thumb.png", Kind: "choice",
		Attachment: &agentapi.Attachment{
			Seq: 7, Name: "0007-screen.png", Display: "screen.png",
			Kind: agentapi.KindImage, Size: 2048,
			Thumb: "0007-screen-thumb.png",
		},
		UI:       &agentapi.UI{Kind: "choice", Options: []string{"a", "b"}},
		Decision: "allow",
		TaskSlug: "a-task", TaskTitle: "A task", TaskDir: "/home/agent/workspace/a-task",
	}
}

// This is the test the package exists for.
//
// Event used to be declared twice -- once in agentd, once in the host's client
// -- and kept in sync by hand. They drifted: the daemon stamped `model` and
// `agent` on every event and the host's copy had neither field, so both were
// dropped on decode. Nothing errored, because a JSON field with nowhere to go
// is simply discarded, and the visible consequence was a VM that appeared never
// to have cost a cent.
//
// Encoding what the daemon emits and decoding it as the host does is now
// lossless by construction. Asserting it means a future re-split cannot pass.
func TestEveryEmittedFieldSurvivesTheRoundTrip(t *testing.T) {
	sent := fullEvent()
	buf, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var got agentapi.Event
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sent, got) {
		t.Errorf("the wire round trip lost something:\n sent %+v\n  got %+v", sent, got)
	}
}

// Every field must reach the wire under a name the other side reads. A field
// with no tag, or one whose tag does not match, decodes to a zero value in
// silence -- precisely how Model and Agent were lost before.
func TestNoEventFieldIsUnreachableOnTheWire(t *testing.T) {
	buf, err := json.Marshal(fullEvent())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf, &raw); err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(agentapi.Event{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" {
			t.Errorf("field %s has no json tag, so the other side cannot read it", f.Name)
			continue
		}
		if _, ok := raw[name]; !ok {
			t.Errorf("field %s (json %q) did not reach the wire", f.Name, name)
		}
	}
}

// The daemon and the host must be the SAME type, not two structurally identical
// ones -- a copy that matches today drifts the moment either side gains a field.
//
// In practice the compiler gets there first: every shared struct that embeds an
// Event (EventsPage, and anything the host decodes into) rejects a differently
// named type outright, so a re-split fails to build before it fails a test.
// This asserts the property directly anyway, so the reason is stated somewhere
// a person will actually read when that build error appears.
func TestTheDaemonAndTheHostShareOneType(t *testing.T) {
	if reflect.TypeOf(agentd.Event{}) != reflect.TypeOf(agentapi.Event{}) {
		t.Error("agentd.Event is no longer an alias of agentapi.Event; the wire format has been split in two again")
	}
}
