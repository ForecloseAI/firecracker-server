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

// appReadsRetry is how long a machine keeps an INCOMPLETE set before it is
// pushed again.
//
// Far shorter than the TTL, because an incomplete set is one an outage made:
// its missing tools ask a person, and healing that should not wait an hour. Far
// longer than appsRetryAfter, because it is not free -- an incomplete answer is
// never cached, so every machine coming due re-fans-out across six apps, and a
// 30-second cadence would aim that at a provider already having a bad day.
const appReadsRetry = 5 * time.Minute

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

// slugs is every action the featured apps expose that only reads, with the
// moment the answer stops being good.
//
// That deadline is the caller's, not this cache's: a machine is pushed a COPY
// and keeps it until it is pushed again, so what the deadline governs is when
// the machine holding it is due another push. Returning it here is what lets
// the claim expire on the same clock as the set, rather than latching for the
// life of the host process.
//
// Never fails, and an incomplete answer is never cached: caching one would
// spend an hour asking about reads that are perfectly safe.
func (a *appReads) slugs(ctx context.Context) ([]string, time.Time) {
	if held, until, ok := a.fresh(); ok {
		return held, until
	}
	got, whole := a.fetchAll(ctx)
	if !whole {
		// Said out loud because the cost lands somewhere else entirely: the
		// machine pushed this set keeps it until it is pushed again, and every
		// read outside it asks a person. Silence here reads as a chatty gate.
		log.Printf("chat: read-only set is incomplete, %d actions; some reads will ask", len(got))
		// Not cached, and the machine given it comes back on the short clock
		// rather than the full TTL -- an outage's set must not outlive the outage.
		return got, time.Now().Add(appReadsRetry)
	}
	return got, a.keep(got)
}

// fresh returns the cached set while it is still good, with its deadline. The
// bool is what says so: a legitimately empty answer is not the same as nothing
// cached.
func (a *appReads) fresh() ([]string, time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held, a.expires, time.Now().Before(a.expires)
}

// keep stores a complete set and starts its clock, reporting when it runs out.
func (a *appReads) keep(held []string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held, a.expires = held, time.Now().Add(appReadsTTL)
	return a.expires
}

// fetchAll reads every featured app. One that did not answer contributes
// nothing, so its tools fall outside the set and ask.
func (a *appReads) fetchAll(ctx context.Context) ([]string, bool) {
	out, whole := fanOut(ctx, a.fetch, func(string) []string { return nil })
	return slices.Concat(out...), whole
}
