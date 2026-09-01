package chat

import (
	"context"
	"log"
	"slices"
	"sync"
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
// A separate cache entry from appCatalog, sharing its fan-out. Separate because
// the failures are not worth the same -- a blurb that will not load costs an app
// a nice description, this costs a person a prompt on every read they make -- so
// neither may invalidate the other's deadline. That is an argument about the two
// ENTRIES, not the mechanism, which is why fanOut is shared.
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
// Never fails, and an incomplete answer is never cached: caching one would
// spend an hour asking about reads that are perfectly safe.
func (a *appReads) slugs(ctx context.Context) []string {
	if held, ok := a.fresh(); ok {
		return held
	}
	got, whole := a.fetchAll(ctx)
	if !whole {
		// Said out loud because the cost lands somewhere else entirely: the
		// machine pushed this set keeps it until the host restarts, and every
		// read outside it asks a person. Silence here reads as a chatty gate.
		log.Printf("chat: read-only set is incomplete, %d actions; some reads will ask", len(got))
		return got
	}
	a.keep(got)
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

// fetchAll reads every featured app. One that did not answer contributes
// nothing, so its tools fall outside the set and ask.
func (a *appReads) fetchAll(ctx context.Context) ([]string, bool) {
	out, whole := fanOut(ctx, a.fetch, func(string) []string { return nil })
	return slices.Concat(out...), whole
}
