package agentd

import (
	"slices"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// withThinking is an assistant message shaped like Gemini's: an opaque
// reasoning block, then the tool call. Captured from OpenRouter 2026-09-06.
func withThinking() anthropic.BetaMessageParam {
	return anthropic.BetaMessageParam{
		Role: anthropic.BetaMessageParamRoleAssistant,
		Content: []anthropic.BetaContentBlockParamUnion{
			anthropic.NewBetaRedactedThinkingBlock("AY89a1+WbCuYfdKRYogit6zut"),
			{OfToolUse: &anthropic.BetaToolUseBlockParam{
				ID: "t1", Name: "take_screenshot", Input: map[string]any{"uid": "5_73"}}},
		},
	}
}

// The bug this exists for, in one sentence: an assistant message carrying a
// redacted_thinking block, followed by a tool_result carrying an image, makes
// the gateway drop the tool_result on its way to Google -- which then rejects
// the whole request because it ends on a model turn. Every screenshot a Gemini
// agent took killed its turn.
func TestOpaqueThinkingIsNotReplayedToAGatewayModel(t *testing.T) {
	msgs := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("look")),
		withThinking(),
	}
	got := withoutOpaqueThinking(msgs)
	for _, b := range got[1].Content {
		if b.OfRedactedThinking != nil {
			t.Error("a redacted_thinking block survived and will kill the next screenshot")
		}
	}
	if len(got[1].Content) != 1 || got[1].Content[0].OfToolUse == nil {
		t.Fatalf("the tool call did not survive the strip: %+v", got[1].Content)
	}
}

// Anthropic is the one service those blocks mean something to -- it requires
// them passed back -- and the gateway bug is not on its path. Stripping by
// accident there would be a silent downgrade of every Claude agent.
func TestAnthropicKeepsItsThinkingBlocks(t *testing.T) {
	for _, c := range []struct {
		model string
		strip bool
	}{
		{"anthropic/claude-sonnet-5", false},
		{"anthropic/claude-haiku-4.5", false},
		{"google/gemini-3.8-flash", true},
		{"openai/gpt-4o", true},
	} {
		if got := !anthropicModel(c.model); got != c.strip {
			t.Errorf("%s: strip = %v, want %v", c.model, got, c.strip)
		}
	}
}

// The history a turn is given is a shallow clone of the agent's own, so an
// edit in place would reach back into the conversation this turn is still
// entitled to roll back to. A failed turn must leave nothing behind.
func TestStrippingDoesNotReachIntoTheStoredConversation(t *testing.T) {
	stored := []anthropic.BetaMessageParam{withThinking()}
	candidate := slices.Clone(stored)
	_ = withoutOpaqueThinking(candidate)
	if len(stored[0].Content) != 2 || stored[0].Content[0].OfRedactedThinking == nil {
		t.Error("stripping a candidate history mutated the stored one")
	}
}

// A brokered turn on the fleet's own model must be untouched, and one on a
// gateway model must not carry the block into its request.
func TestParamsStripOnlyForAGatewayModel(t *testing.T) {
	hist := []anthropic.BetaMessageParam{withThinking()}
	claude := (&Agent{system: "p", ep: endpoint{model: "anthropic/claude-sonnet-5"}}).params(hist)
	if len(claude.Messages[0].Content) != 2 {
		t.Error("a Claude agent lost its thinking block")
	}
	gemini := (&Agent{system: "p", ep: endpoint{model: "google/gemini-3.8-flash"}}).params(hist)
	if len(gemini.Messages[0].Content) != 1 {
		t.Errorf("a Gemini agent kept it: %+v", gemini.Messages[0].Content)
	}
}
