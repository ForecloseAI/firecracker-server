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

// Both of these are real usage blocks, captured from OpenRouter on 2026-09-06.
// They are copied rather than composed because an invented pair got this wrong:
// the docs say upstream_inference_cost is "0 or null" for non-BYOK requests, and
// it is neither -- it repeats cost. A fixture written from the docs agreed with
// the code and both were wrong together.
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

// On BYOK the inference is billed to our own provider account, so cost is only
// what OpenRouter charged on top -- zero while under the free monthly allowance
// -- and the bill is the two added. Taking cost alone would report this turn as
// free, confidently, because a figure was found.
func TestABYOKTurnIsBilledForBothHalves(t *testing.T) {
	if got := reportedCost(usageFrom(t, byokUsage)); got != 0.000088 {
		t.Errorf("reportedCost = %v, want 0.000088", got)
	}
}

// And the mirror, which is the one that bites: off BYOK, cost IS the bill and
// upstream_inference_cost is the same money described a second way. Adding them
// here -- which the documented semantics invite -- doubles every turn.
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

// Compaction books through the same usageOf, and it is the call the person never
// sees any output from -- so a cost lost here is spend that /usage cannot
// account for and nobody would think to look for.
func TestACompactionSummaryBooksItsCost(t *testing.T) {
	a := newTestAgent(t)
	a.team = &Supervisor{meter: OpenMeter(t.TempDir())}

	u := usageFrom(t, `{"input_tokens":4000,"output_tokens":300,
		"cost":0.01,"is_byok":true,"cost_details":{"upstream_inference_cost":0.19}}`)
	a.bookUsage(summaryOpenRouter, usageOf(u))

	for _, row := range a.meter().Report().ByModel {
		if row.Model == summaryOpenRouter {
			if row.CostUSD != 0.20 {
				t.Errorf("summary CostUSD = %v, want 0.20", row.CostUSD)
			}
			return
		}
	}
	t.Fatal("the summary model is absent from the meter")
}
