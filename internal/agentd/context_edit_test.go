package agentd

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// Betas is a HEADER field, tagged json:"-". A test that marshals the params and
// greps the JSON for the beta name passes cleanly while the header is entirely
// absent and the API rejects the edit -- so the struct field is asserted
// directly, and this test exists to stop the obvious weaker version replacing it.
func TestContextEditingCarriesItsBetaHeader(t *testing.T) {
	p := contextManagement()
	params := anthropic.BetaMessageNewParams{
		ContextManagement: p,
		Betas:             []anthropic.AnthropicBeta{anthropic.AnthropicBetaContextManagement2025_06_27},
	}
	if !slices.Contains(params.Betas, anthropic.AnthropicBetaContextManagement2025_06_27) {
		t.Fatal("the context-management beta is not on the params")
	}
	buf, err := json.Marshal(params)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(buf), "context-management-2025-06-27") {
		t.Error("the beta name appeared in the JSON body; it is meant to be a header, " +
			"so a JSON assertion here would prove nothing about the request")
	}
	if !strings.Contains(string(buf), "clear_tool_uses_20250919") {
		t.Error("the edit itself is missing from the request body")
	}
}

// A person's answer is the one tool result that cannot be recovered by running
// the tool again: they have walked away, and re-asking spends them twice.
// Everything else is re-derivable. Snapshots are deliberately NOT protected --
// they are the fattest thing in history, so excluding them would defeat the
// entire point of turning this on.
func TestOnlyAskHumanIsProtectedFromClearing(t *testing.T) {
	edit := contextManagement().Edits[0].OfClearToolUses20250919
	if edit == nil {
		t.Fatal("no clear_tool_uses edit configured")
	}
	if !slices.Contains(edit.ExcludeTools, "ask_human") {
		t.Error("a human's answer is clearable; it cannot be recovered by re-running the tool")
	}
	if slices.Contains(edit.ExcludeTools, "take_snapshot") {
		t.Error("snapshots are protected from clearing, which defeats the point of clearing")
	}
}

// The thresholds are the whole behaviour: a trigger below the cost of a couple
// of snapshots would clear constantly, and clearing for a trivial saving costs
// more in rewritten prefix than it saves.
func TestClearingThresholdsAreSane(t *testing.T) {
	edit := contextManagement().Edits[0].OfClearToolUses20250919
	if edit.Trigger.OfInputTokens == nil || edit.Trigger.OfInputTokens.Value != clearTrigger {
		t.Errorf("trigger = %+v, want %d input tokens", edit.Trigger, clearTrigger)
	}
	if edit.Keep.Value != clearKeep {
		t.Errorf("keep = %d, want %d", edit.Keep.Value, clearKeep)
	}
	if edit.ClearAtLeast.Value >= clearTrigger {
		t.Errorf("clearAtLeast %d is not below the trigger %d, so a clear can never pay",
			edit.ClearAtLeast.Value, clearTrigger)
	}
}

// Clearing rewrites the prefix below the clear point, so a cache breakpoint
// sitting in the message array would be invalidated on every clear. There is
// exactly one breakpoint today and it is on the last system block, above the
// messages -- which is what makes clearing free. This is the test that fails if
// someone later adds a second one without revisiting that decision.
func TestSystemBlockIsTheOnlyCacheBreakpoint(t *testing.T) {
	a := &Agent{system: "some prompt", ep: endpoint{model: "claude-sonnet-5"}}
	p := a.params(nil)
	if p.BetaMessageNewParams.ContextManagement.Edits == nil {
		t.Fatal("params carry no context management")
	}
	// Asserted on the wire form, because that is what the API sees and because
	// the marker is a constant-typed field with no useful zero value in Go.
	msg := anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hello"))
	body, err := json.Marshal(a.params([]anthropic.BetaMessageParam{msg}).BetaMessageNewParams)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(body), `"cache_control"`); n != 1 {
		t.Errorf("%d cache breakpoints in the request, want exactly 1 (on the system block)", n)
	}
	// The one breakpoint must sit inside the system array. Field order in the
	// JSON is Go's declaration order, not prompt order, so position alone proves
	// nothing -- the system array is sliced out and searched instead.
	sys := string(body[strings.Index(string(body), `"system":`):])
	if !strings.Contains(sys, `"cache_control"`) {
		t.Error("the cache breakpoint is not on the system block, so clearing would invalidate it")
	}
}
