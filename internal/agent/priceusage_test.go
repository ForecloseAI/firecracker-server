package agent

import (
	"testing"

	"cracked/internal/agentapi"
)

// PriceUsage is what the gateway prices per-agent spend with. It must agree
// with the table, and say so when it cannot, rather than answer zero: zero is
// what a free turn looks like.
func TestPriceUsageAgreesWithTheTable(t *testing.T) {
	usd, ok := PriceUsage("claude-sonnet-5", agentapi.Usage{InputTokens: 1_000_000})
	if !ok || usd != 2 {
		t.Fatalf("a million sonnet input tokens priced at %v, %v; want 2, true", usd, ok)
	}
	if usd, ok := PriceUsage("claude-something-7", agentapi.Usage{InputTokens: 1}); ok || usd != 0 {
		t.Errorf("an unknown model priced at %v, %v; want 0, false", usd, ok)
	}
}
