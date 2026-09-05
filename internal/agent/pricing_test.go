package agent

import (
	"testing"

	"cracked/internal/agentapi"
)

// The API reports the model id it resolved a request to, which may carry a
// dated suffix the caller never wrote. A first-match lookup would resolve
// "claude-opus-4-5-20251101" against the shorter "claude-opus-4" entry and bill
// an Opus 4.5 turn at Opus 4 rates -- three times over, silently, forever.
func TestPricingPrefixMatchIsLongest(t *testing.T) {
	got, ok := rateFor("claude-opus-4-5-20251101")
	if !ok {
		t.Fatal("a dated Opus 4.5 id did not resolve at all")
	}
	if want := priceTable["claude-opus-4-5"]; got != want {
		t.Errorf("rate = %+v, want %+v (matched a shorter prefix)", got, want)
	}
	if got == priceTable["claude-opus-4"] {
		t.Error("Opus 4.5 resolved to Opus 4 rates, which are 3x higher")
	}
}

// The alias the profiles actually ask for has to price, or every VM this
// product ships reports zero.
func TestTheShippedModelIsPriced(t *testing.T) {
	for _, id := range []string{"claude-sonnet-5", "claude-sonnet-5-20260115", "claude-haiku-4-5"} {
		if _, ok := rateFor(id); !ok {
			t.Errorf("no price for %q, which a shipped profile can request", id)
		}
	}
}

// A model the table has not caught up with contributes zero, and zero is
// exactly what a free turn looks like -- the bug this pricing path exists to
// end. It must be reported, not absorbed.
func TestUnpricedModelIsReportedNotZeroed(t *testing.T) {
	if _, ok := rateFor("claude-something-7"); ok {
		t.Fatal("an unknown model resolved to a price")
	}
	got := Price(agentapi.UsageReport{ByModel: []agentapi.ModelUsage{
		{Model: "claude-something-7", Usage: agentapi.Usage{InputTokens: 1_000_000}},
	}})
	if len(got.UnpricedModels) != 1 || got.UnpricedModels[0] != "claude-something-7" {
		t.Errorf("unpriced models = %v; a zero cost must say why", got.UnpricedModels)
	}
	if got.InputTokens != 1_000_000 {
		t.Error("tokens must still be counted for a model that cannot be priced")
	}
}

// Cache reads are a tenth of input and cache writes a quarter more, so a
// conversation that is mostly cache hits must not be billed at the input rate.
// Getting this wrong overstates a long browsing session several-fold.
func TestCachePricingUsesTheMultipliers(t *testing.T) {
	const million = 1_000_000
	in := priceTable["claude-sonnet-5"].in
	cost, ok := costOf("claude-sonnet-5", agentapi.Usage{CacheReadInputTokens: million})
	if !ok {
		t.Fatal("sonnet-5 did not price")
	}
	if want := in * cacheReadMultiple; !near(cost, want) {
		t.Errorf("1M cache reads = $%v, want $%v", cost, want)
	}
	cost, _ = costOf("claude-sonnet-5", agentapi.Usage{CacheCreationInputTokens: million})
	if want := in * cacheWriteMultiple; !near(cost, want) {
		t.Errorf("1M cache writes = $%v, want $%v", cost, want)
	}
}

// A worked example from the published pricing page, so the arithmetic is
// pinned against a number someone else computed.
func TestWorkedExampleMatchesThePublishedFigure(t *testing.T) {
	// 50,000 input + 15,000 output on Opus 5 is $0.25 + $0.375 = $0.625.
	cost, ok := costOf("claude-opus-5", agentapi.Usage{InputTokens: 50_000, OutputTokens: 15_000})
	if !ok || !near(cost, 0.625) {
		t.Errorf("cost = $%v, want $0.625", cost)
	}
}

// near compares dollars, which are floats and so never exactly equal.
func near(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

// Every agent's spend must reach the VM total. A delegated worker running on a
// different model is the whole reason the report is split by model at all.
func TestEveryModelsSpendIsInTheTotal(t *testing.T) {
	got := Price(agentapi.UsageReport{Turns: 3, ByModel: []agentapi.ModelUsage{
		{Model: "claude-sonnet-5", Usage: agentapi.Usage{InputTokens: 1_000_000}},
		{Model: "claude-haiku-4-5", Usage: agentapi.Usage{InputTokens: 1_000_000}},
	}})
	want := priceTable["claude-sonnet-5"].in + priceTable["claude-haiku-4-5"].in
	if !near(got.CostUSD, want) {
		t.Errorf("cost = $%v, want $%v (a model was dropped)", got.CostUSD, want)
	}
	if got.InputTokens != 2_000_000 || got.Turns != 3 {
		t.Errorf("totals = %+v", got)
	}
}

// An endpoint that priced its own call knows two things this table cannot: the
// fees it added, and which provider it actually routed to. When it reports a
// figure, that figure is the bill -- and it has to beat the table rather than
// merely fill in for it, or every OpenRouter turn would be re-estimated at
// Anthropic list prices and quietly disagree with the invoice.
func TestAReportedCostBeatsTheTable(t *testing.T) {
	// A million input tokens, which the table would price at its own rate --
	// so a fall-through would give a different number, not merely a wrong one.
	u := agentapi.Usage{InputTokens: 1_000_000, CostUSD: 7.50}
	if table, _ := costOf("claude-sonnet-5", agentapi.Usage{InputTokens: 1_000_000}); table == 7.50 {
		t.Fatal("the table happens to agree with the reported figure; this test proves nothing")
	}
	got, ok := costOf("claude-sonnet-5", u)
	if !ok {
		t.Fatal("a call that came with a price was reported as unpriceable")
	}
	if got != 7.50 {
		t.Errorf("cost = %v, want 7.50 (the table overrode the reported figure)", got)
	}
}

// A model this table has never heard of is priced anyway when the endpoint said
// what it cost -- which is the whole point of reading the figure. Gemini is the
// real case: reachable now that the fleet goes through a gateway, and no version
// of an Anthropic price table will ever have a rate for it.
func TestAModelTheTableNeverKnewIsPricedFromItsOwnReport(t *testing.T) {
	if _, ok := rateFor("google/gemini-2.5-flash"); ok {
		t.Fatal("the table priced a Gemini model; this test no longer proves anything")
	}
	got := Price(agentapi.UsageReport{ByModel: []agentapi.ModelUsage{
		{Model: "google/gemini-2.5-flash", Usage: agentapi.Usage{InputTokens: 100, CostUSD: 0.25}},
	}})
	if len(got.UnpricedModels) != 0 {
		t.Errorf("unpriced = %v; a call that reported its cost is priced", got.UnpricedModels)
	}
	if got.CostUSD != 0.25 {
		t.Errorf("CostUSD = %v, want 0.25", got.CostUSD)
	}
}

// Rows written before any endpoint reported a figure -- and every turn against
// Anthropic direct -- carry no cost. Zero must mean "ask the table", not "free",
// or the fallback this whole path depends on would be dead code.
func TestNoReportedCostStillFallsBackToTheTable(t *testing.T) {
	const million = 1_000_000
	want := priceTable["claude-sonnet-5"].in
	got, ok := costOf("claude-sonnet-5", agentapi.Usage{InputTokens: million})
	if !ok || got != want {
		t.Errorf("cost = %v (ok=%v), want %v from the table", got, ok, want)
	}
}

// The reported cost is the authority, but it must not be the only thing between
// this table and a fleet reporting $0. Every model the profiles ask for is
// provider-prefixed now, so without stripping that the table matches nothing
// the fleet runs -- and the day a response omits its cost, every VM would read
// as free with one log line to say why.
func TestAProviderPrefixDoesNotHideAKnownModel(t *testing.T) {
	const million = 1_000_000
	want := priceTable["claude-sonnet-5"].in
	got, ok := costOf("anthropic/claude-sonnet-5", agentapi.Usage{InputTokens: million})
	if !ok || got != want {
		t.Errorf("cost = %v (ok=%v), want %v from the table", got, ok, want)
	}
	// The stripping must not invent prices for models the table never knew.
	if _, ok := rateFor("openai/gpt-4o"); ok {
		t.Error("a model with no table entry was priced anyway")
	}
	// And the longest-prefix rule still has to survive it.
	if r, _ := rateFor("anthropic/claude-opus-4-5-20251101"); r != priceTable["claude-opus-4-5"] {
		t.Errorf("a dated prefixed Opus 4.5 resolved to %+v", r)
	}
}
