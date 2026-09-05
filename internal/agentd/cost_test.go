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

// The whole reason this function is not one field read.
//
// Under BYOK, OpenRouter bills the inference to the person's own provider
// account and charges only its 5% fee itself. So `cost` is the small number and
// `cost_details.upstream_inference_cost` is the bill. Reading `cost` alone would
// report a $2.10 turn as $0.10 -- and report it confidently, with no warning,
// because a figure was found.
func TestABYOKTurnIsBilledForBothHalves(t *testing.T) {
	u := usageFrom(t, `{"input_tokens":100,"output_tokens":20,
		"cost":0.10,"cost_details":{"upstream_inference_cost":2.00}}`)
	if got := reportedCost(u); got != 2.10 {
		t.Errorf("reportedCost = %v, want 2.10 (one of the two terms was dropped)", got)
	}
}

// Not every response has both halves, and each one alone must still be the
// answer rather than a reason to fall through to the table.
func TestEitherCostFieldAloneIsStillTheCost(t *testing.T) {
	for _, c := range []struct {
		name string
		raw  string
		want float64
	}{
		{"openrouter credits, no byok", `{"cost":0.42,"cost_details":{"upstream_inference_cost":0}}`, 0.42},
		{"upstream only", `{"cost":0,"cost_details":{"upstream_inference_cost":1.25}}`, 1.25},
		{"null upstream", `{"cost":0.42,"cost_details":{"upstream_inference_cost":null}}`, 0.42},
		{"no cost_details at all", `{"cost":0.42}`, 0.42},
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
		"cost":0.05,"cost_details":{"upstream_inference_cost":0.95}}`)
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
		"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,
		"cost":0.10,"cost_details":{"upstream_inference_cost":2.00}}}`
	var msg anthropic.BetaMessage
	if err := json.Unmarshal([]byte(raw), &msg); err != nil {
		t.Fatalf("unmarshal message: %v", err)
	}
	if got := reportedCost(msg.Usage); got != 2.10 {
		t.Errorf("reportedCost off a nested usage = %v, want 2.10", got)
	}
}

// Compaction books through the same usageOf, and it is the call the person never
// sees any output from -- so a cost lost here is spend that /usage cannot
// account for and nobody would think to look for.
func TestACompactionSummaryBooksItsCost(t *testing.T) {
	a := newTestAgent(t)
	a.team = &Supervisor{meter: OpenMeter(t.TempDir())}

	u := usageFrom(t, `{"input_tokens":4000,"output_tokens":300,
		"cost":0.01,"cost_details":{"upstream_inference_cost":0.19}}`)
	a.bookUsage(summaryModel, usageOf(u))

	for _, row := range a.meter().Report().ByModel {
		if row.Model == summaryModel {
			if row.CostUSD != 0.20 {
				t.Errorf("summary CostUSD = %v, want 0.20", row.CostUSD)
			}
			return
		}
	}
	t.Fatal("the summary model is absent from the meter")
}
