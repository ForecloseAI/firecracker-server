package agent

import (
	"log"
	"strings"
	"sync"

	"cracked/internal/agentapi"
)

// Cache pricing is a multiplier on a model's base input rate rather than a
// separate column: a 5-minute cache write costs 1.25x input and a cache read
// 0.1x, uniformly across the range. Deriving both means the table cannot become
// internally inconsistent, which a hand-maintained four-column table can.
const (
	cacheWriteMultiple = 1.25
	cacheReadMultiple  = 0.10
)

// rate is one model's price in US dollars per million tokens.
type rate struct{ in, out float64 }

// priceTable is keyed by model-id PREFIX, because the API reports the id it
// resolved a request to and that may carry a dated suffix the caller never
// wrote. Longest match wins, and that is load-bearing rather than tidy:
// "claude-opus-4-5" and "claude-opus-4" differ by a factor of three, so a
// first-match lookup would bill an Opus 4.5 turn at Opus 4 rates.
//
// Rates from platform.claude.com/docs/en/about-claude/pricing, August 2026.
var priceTable = map[string]rate{
	"claude-fable-5":    {10, 50},
	"claude-mythos-5":   {10, 50},
	"claude-opus-5":     {5, 25},
	"claude-opus-4-8":   {5, 25},
	"claude-opus-4-7":   {5, 25},
	"claude-opus-4-6":   {5, 25},
	"claude-opus-4-5":   {5, 25},
	"claude-opus-4-1":   {15, 75},
	"claude-opus-4":     {15, 75},
	"claude-sonnet-5":   {2, 10},
	"claude-sonnet-4-6": {3, 15},
	"claude-sonnet-4-5": {3, 15},
	"claude-sonnet-4":   {3, 15},
	"claude-haiku-4-5":  {1, 5},
	"claude-haiku-3-5":  {0.8, 4},
}

// rateFor finds a model's price by longest matching prefix.
func rateFor(model string) (rate, bool) {
	best, found := "", rate{}
	for prefix, r := range priceTable {
		if strings.HasPrefix(model, prefix) && len(prefix) > len(best) {
			best, found = prefix, r
		}
	}
	return found, best != ""
}

// costOf prices one model's token totals, reporting whether it could.
func costOf(model string, u agentapi.Usage) (float64, bool) {
	r, ok := rateFor(model)
	if !ok {
		return 0, false
	}
	perToken := func(n int64, dollarsPerMillion float64) float64 {
		return float64(n) * dollarsPerMillion / 1_000_000
	}
	return perToken(u.InputTokens, r.in) +
		perToken(u.OutputTokens, r.out) +
		perToken(u.CacheCreationInputTokens, r.in*cacheWriteMultiple) +
		perToken(u.CacheReadInputTokens, r.in*cacheReadMultiple), true
}

// warned remembers which unpriced models have already been reported, so an
// unknown id costs one log line rather than one per dashboard poll.
var warned sync.Map

// warnUnpriced reports a model the table cannot price, once.
//
// This exists because the failure it describes is invisible: an unpriced model
// contributes zero, and zero is exactly what a free turn looks like. That is
// the bug this whole pricing path was written to end, so it must not be
// reintroduced by a model id the table has not caught up with.
func warnUnpriced(model string) {
	if _, seen := warned.LoadOrStore(model, true); !seen {
		log.Printf("agent: no price for model %q; its spend is being reported as zero", model)
	}
}
