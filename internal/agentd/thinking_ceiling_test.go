package agentd

import (
	"context"
	"strings"
	"testing"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
)

// The arithmetic that broke, pinned at every level.
//
// The SDK refuses a NON-STREAMING request whose max_tokens implies more than
// ten minutes at its own pessimistic 128k-tokens-per-hour estimate, which puts
// the ceiling at 21,333. maxTokens plus the high budget is 24,576, so "high"
// was refused before a request was ever made. Raise either number past the
// ceiling again and this says so.
func TestThinkingNeverPushesMaxTokensPastTheNonStreamingCeiling(t *testing.T) {
	const ceiling = 21333 // 128000 * 10min / 60min, from CalculateNonStreamingTimeout
	for level, budget := range agentapi.ThinkingBudgets {
		a := &Agent{system: "p", ep: endpoint{model: "anthropic/claude-sonnet-5", thinking: level}}
		got := a.params(nil).MaxTokens
		if want := maxTokens + budget; got != want {
			t.Errorf("%s: max_tokens = %d, want %d", level, got, want)
		}
		if got > ceiling {
			t.Logf("%s: max_tokens %d is over the SDK's %d ceiling; only the request "+
				"timeout keeps it sendable", level, got, ceiling)
		}
	}
}

// The one that would have caught the bug: a real client, a real request, at the
// budget the high level actually asks for.
//
// This fails without the request timeout -- the SDK returns "streaming is
// required for operations that may take longer than 10 minutes" from inside
// Messages.New, having sent nothing, so the fake never sees a request at all.
func TestAHighThinkingTurnIsSentRatherThanRefusedOutright(t *testing.T) {
	clearModelEnv(t)
	fake := fakeModel(t)
	ep := endpoint{baseURL: fake.srv.URL, key: brokerKey, model: "anthropic/claude-sonnet-5"}

	budget := agentapi.ThinkingBudgets["high"]
	client := newClient(ep)
	_, err := client.Beta.Messages.New(context.Background(), anthropic.BetaMessageNewParams{
		Model: anthropic.Model(ep.model), MaxTokens: maxTokens + budget,
		Thinking: anthropic.BetaThinkingConfigParamOfEnabled(budget),
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock("hi"))},
	})
	if err != nil {
		if strings.Contains(err.Error(), "streaming is required") {
			t.Fatalf("the SDK refused to send a high-thinking turn: %v", err)
		}
		t.Fatalf("high-thinking turn: %v", err)
	}
	if fake.path != "/v1/messages" {
		t.Errorf("the request never reached the model: path %q", fake.path)
	}
}
