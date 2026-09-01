package chat

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"cracked/internal/composio"
)

// appReadsTTL is how long the read-only set is kept. An hour, matching the
// catalogue: the answer is the same for everyone on the fleet, and it moves only
// when the provider ships or re-annotates a tool.
const appReadsTTL = time.Hour

// appReads is which of the featured apps' actions only read, as the PROVIDER
// annotates them. Nothing here is a list this project maintains.
//
// Kept separate from appCatalog rather than folded into it, though the shape is
// the same. Their failures are not worth the same: a blurb that will not load
// costs an app a nice description, and this failing to load costs a person a
// prompt on every read they make. Letting either invalidate the other would mean
// paying one of those costs for the other's bad minute.
type appReads struct {
	// fetch is a field so a test can answer without a provider.
	fetch func(context.Context, string) ([]string, error)

	mu      sync.Mutex
	held    []string
	expires time.Time
}

// newAppReads prepares the cache. It fetches nothing until asked.
func newAppReads(c *composio.Client) *appReads {
	return &appReads{fetch: c.ReadOnly}
}

// slugs is every action the featured apps expose that only reads.
//
// Never fails, and an incomplete answer is never cached. What is missing from
// this set asks a person, so a bad minute at the provider costs prompts rather
// than silence -- and caching a partial answer would spend an hour asking about
// reads that are perfectly safe.
func (a *appReads) slugs(ctx context.Context) []string {
	if held, ok := a.fresh(); ok {
		return held
	}
	got, whole := a.fetchAll(ctx)
	if whole {
		a.keep(got)
	}
	return got
}

// fresh returns the cached set while it is still good. The bool is what says so:
// a legitimately empty answer is not the same as nothing cached.
func (a *appReads) fresh() ([]string, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held, time.Now().Before(a.expires)
}

// keep stores a complete set and starts its clock.
func (a *appReads) keep(held []string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held, a.expires = held, time.Now().Add(appReadsTTL)
}

// fetchAll reads every featured app at once, reporting whether all of them
// answered. An app that did not answer contributes nothing, so its tools ask.
func (a *appReads) fetchAll(ctx context.Context) ([]string, bool) {
	out := make([][]string, len(featured))
	var missed atomic.Bool
	var wg sync.WaitGroup
	for i, slug := range featured {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := a.fetch(ctx, slug)
			if err != nil {
				missed.Store(true)
				return
			}
			out[i] = got
		}()
	}
	wg.Wait()
	return slices.Concat(out...), !missed.Load()
}
