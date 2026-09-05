package agentd

import (
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// usageFrom parses a raw usage block the way the SDK does, so a test sees
// exactly what a response would hand reportedCost.
func usageFrom(t *testing.T, raw string) anthropic.BetaUsage {
	t.Helper()
	var u anthropic.BetaUsage
	if err := json.Unmarshal([]byte(raw), &u); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	return u
}

// Real usage blocks, captured from OpenRouter on 2026-09-06 and copied verbatim
// rather than composed. An invented pair got this wrong once: written from the
// documented semantics, it agreed with code written from the same reading, and
// the two were wrong together. See reportedCost for what the fields mean.
const (
	byokUsage = `{"input_tokens":8,"output_tokens":16,
		"cache_creation_input_tokens":0,"cache_read_input_tokens":0,
		"cost":0,"is_byok":true,"cost_details":{"upstream_inference_cost":0.000088,
		"upstream_inference_prompt_cost":0.000008,"upstream_inference_completions_cost":0.00008}}`

	creditsUsage = `{"input_tokens":1,"output_tokens":10,
		"cache_creation_input_tokens":0,"cache_read_input_tokens":0,
		"cost":0.0000253,"is_byok":false,"cost_details":{"upstream_inference_cost":0.0000253,
		"upstream_inference_prompt_cost":3E-7,"upstream_inference_completions_cost":0.000025}}`
)

// Taking cost alone here would report the turn as free, confidently, because a
// figure was found.
func TestABYOKTurnIsBilledForBothHalves(t *testing.T) {
	if got := reportedCost(usageFrom(t, byokUsage)); got != 0.000088 {
		t.Errorf("reportedCost = %v, want 0.000088", got)
	}
}

// And the mirror, which is the one that bites: adding the two here -- which the
// documented semantics invite -- doubles every turn.
func TestACreditsTurnIsNotDoubleCounted(t *testing.T) {
	got := reportedCost(usageFrom(t, creditsUsage))
	if got != 0.0000253 {
		t.Errorf("reportedCost = %v, want 0.0000253", got)
	}
	if got == 0.0000506 {
		t.Error("cost and upstream_inference_cost were added; they are the same money")
	}
}

// Absent fields must not be read as a bill of zero when they are really a bill
// of nothing-said. Zero is the signal the host falls back to its table on.
func TestAPartialCostBlockIsStillRead(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  string
		want float64
	}{
		{"byok, no cost_details", `{"cost":0.10,"is_byok":true}`, 0.10},
		{"byok, null upstream", `{"cost":0.10,"is_byok":true,"cost_details":{"upstream_inference_cost":null}}`, 0.10},
		{"credits, no cost_details", `{"cost":0.42,"is_byok":false}`, 0.42},
		{"no is_byok at all", `{"cost":0.42,"cost_details":{"upstream_inference_cost":0.42}}`, 0.42},
	} {
		if got := reportedCost(usageFrom(t, c.raw)); got != c.want {
			t.Errorf("%s: reportedCost = %v, want %v", c.name, got, c.want)
		}
	}
}

// Anthropic returns token counts and nothing else. Zero here has to mean "no
// figure", so the host falls back to its price table; a number invented at this
// point would be an estimate wearing a receipt's clothes.
func TestAnAnthropicResponseReportsNoCost(t *testing.T) {
	u := usageFrom(t, `{"input_tokens":100,"output_tokens":20,
		"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`)
	if got := reportedCost(u); got != 0 {
		t.Errorf("reportedCost = %v, want 0 for a response that priced nothing", got)
	}
}

// usageOf is the only place a response becomes a wire Usage, so a cost that is
// read but never copied across is a silent zero all the way to the bill.
func TestUsageOfCarriesTheCostOntoTheWire(t *testing.T) {
	u := usageFrom(t, `{"input_tokens":7,"output_tokens":3,
		"cost":0.05,"is_byok":true,"cost_details":{"upstream_inference_cost":0.95}}`)
	got := usageOf(u)
	if got.CostUSD != 1.00 {
		t.Errorf("Usage.CostUSD = %v, want 1.00", got.CostUSD)
	}
	if got.InputTokens != 7 || got.OutputTokens != 3 {
		t.Errorf("tokens = %d/%d, want 7/3", got.InputTokens, got.OutputTokens)
	}
}

// The shape that actually happens in production, and the one a bare-usage test
// cannot vouch for.
//
// msg.Usage is filled by unmarshalling the whole MESSAGE, not the usage block on
// its own. If the SDK kept raw JSON only for the root of a decode, RawJSON()
// would come back empty here, every real turn would price at zero, and every
// other test in this file would still pass.
func TestCostIsReadableFromAUsageNestedInAMessage(t *testing.T) {
	raw := `{"id":"msg_1","type":"message","role":"assistant",
		"model":"anthropic/claude-haiku-4.5","content":[{"type":"text","text":"hi"}],
		"stop_reason":"end_turn","usage":` + byokUsage + `}`
	var msg anthropic.BetaMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if got := reportedCost(msg.Usage); got != 0.000088 {
		t.Errorf("reportedCost off a nested usage = %v, want 0.000088", got)
	}
}
