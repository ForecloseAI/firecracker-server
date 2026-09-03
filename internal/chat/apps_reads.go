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

// appReads is which of the featured apps' actions only read: the PROVIDER's
// annotations, less the handful we reject (see deniedReads). No catalogue of our
// own -- 910 tools we would have to keep in step with somebody else's release.
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
		return got, time.Now().Add(appsRetry)
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
	// Subtracted before the answer is cached, so what a machine is handed is
	// already the answer and no later consumer can forget to.
	return slices.DeleteFunc(slices.Concat(out...), denied), whole
}

// denied reports an action we will not pass on as read-only, saying so when it
// fires. Silence here would leave nobody able to tell whether the provider still
// annotates it the way we disagreed with, or quietly stopped.
func denied(slug string) bool {
	if !deniedReads[slug] {
		return false
	}
	log.Printf("chat: %s is annotated read-only and we do not accept it", slug)
	return true
}
