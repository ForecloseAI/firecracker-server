package chat

import (
	"context"
	"log"
	"sync"
	"time"

	"cracked/internal/agentapi"
	"cracked/internal/composio"
)

// appCapsTTL is how long the capability map is kept. An hour, matching the
// catalogue and the read-only set: it is the provider's answer, the same for
// everyone on the fleet, and it moves only when they ship or re-annotate a tool.
const appCapsTTL = time.Hour

// appsRetry is how long a machine keeps an INCOMPLETE answer before it is pushed
// again.
//
// Far shorter than the TTL, because an incomplete answer is one an outage made:
// what is missing from it asks a person, and healing that should not wait an
// hour. Far longer than appsRetryAfter, because it is not free -- an incomplete
// answer is never cached, so every machine coming due re-fans-out across six
// apps, and a 30-second cadence would aim that at a provider already having a
// bad day.
const appsRetry = 5 * time.Minute

// appCaps is what kind of thing each of the featured apps' actions is, as the
// PROVIDER annotates it. No catalogue of our own -- 910 tools we would otherwise
// keep in step with somebody else's release.
//
// Fleet-wide and cached, unlike the policy it is resolved against, which is one
// person's and stored. Keeping the expensive half shared is the whole reason the
// two are separate: a person changing a setting must not cost six round trips.
type appCaps struct {
	// fetch is a field so a test can answer without a provider.
	fetch func(context.Context, string) (map[string]string, error)

	mu      sync.Mutex
	held    map[string]map[string]string
	expires time.Time
}

// newAppCaps prepares the cache. It fetches nothing until asked.
func newAppCaps(c *composio.Client) *appCaps {
	return &appCaps{fetch: c.Capabilities}
}

// resolved is what each action needs from this person: auto to run, ask to raise
// a card, never to refuse. Flattened by slug, so the guest looks up one string
// and holds no vocabulary of its own.
//
// The deadline is the caller's, not this cache's: a machine is pushed a COPY and
// keeps it until pushed again, so what it governs is when that machine is due
// another push.
func (a *appCaps) resolved(ctx context.Context,
	policy map[string]map[string]string) (map[string]string, time.Time) {
	held, until := a.capabilities(ctx)
	out := make(map[string]string)
	for app, slugs := range held {
		for slug, capability := range slugs {
			out[slug] = actionFor(capability, policy[app][capability])
		}
	}
	return out, until
}

// actionFor is what one action needs, given what it is and what the person said.
//
// Reading is always allowed and has no control on the screen, because a
// connected app can already read and a switch that only ever said yes would be a
// promise we could not keep.
//
// Every other way of being uncertain -- no answer, an answer we do not know, a
// capability we do not know -- resolves to asking. This is the first thing that
// can make a machine LESS capable than intended, so nothing here may resolve to
// auto by accident, and nothing may resolve to never by accident either.
func actionFor(capability, chosen string) string {
	switch capability {
	case composio.CapRead:
		return agentapi.ActionAuto
	case composio.CapWrite, composio.CapDelete:
		if chosen == agentapi.ActionAuto || chosen == agentapi.ActionNever {
			return chosen
		}
	}
	return agentapi.ActionAsk
}

// capabilities is every featured app's actions and what kind each is.
//
// Never fails, and an incomplete answer is never cached: what is missing from it
// asks a person, and caching that would spend an hour asking about reads that
// are perfectly safe.
func (a *appCaps) capabilities(ctx context.Context) (map[string]map[string]string, time.Time) {
	if held, until, ok := a.fresh(); ok {
		return held, until
	}
	got, whole := a.fetchAll(ctx)
	if !whole {
		// Said out loud because the cost lands somewhere else entirely: the
		// machine pushed this keeps it until pushed again, and every action
		// missing from it asks. Silence here reads as a chatty gate.
		log.Printf("chat: capability map is incomplete, %d apps; some reads will ask", len(got))
		// Not cached, and the machine given it comes back on the short clock
		// rather than the full TTL -- an outage's answer must not outlive it.
		return got, time.Now().Add(appsRetry)
	}
	return got, a.keep(got)
}

// fresh returns the cached map while it is still good, with its deadline. The
// bool is what says so: a legitimately empty answer is not nothing cached.
func (a *appCaps) fresh() (map[string]map[string]string, time.Time, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.held, a.expires, time.Now().Before(a.expires)
}

// keep stores a complete map and starts its clock, reporting when it runs out.
func (a *appCaps) keep(held map[string]map[string]string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held, a.expires = held, time.Now().Add(appCapsTTL)
	return a.expires
}

// fetchAll reads every featured app. One that did not answer contributes
// nothing, so its actions fall outside the map and ask.
func (a *appCaps) fetchAll(ctx context.Context) (map[string]map[string]string, bool) {
	out, whole := fanOut(ctx, a.fetch, func(string) map[string]string { return nil })
	held := make(map[string]map[string]string, len(featured))
	for i, app := range featured {
		if out[i] != nil {
			ourView(out[i])
			held[app] = out[i]
		}
	}
	return held, whole
}

// ourView applies the handful of annotations we disagree with, before anything
// is cached -- so what a machine is handed is already the answer and no later
// consumer can forget to.
func ourView(got map[string]string) {
	for slug := range deniedReads {
		if _, ok := got[slug]; !ok {
			continue
		}
		// Said out loud: without it nobody could tell whether the provider still
		// annotates it the way we disagreed with, or quietly stopped.
		log.Printf("chat: %s is annotated read-only and we do not accept it", slug)
		got[slug] = composio.CapWrite
	}
}

// deniedReads are actions the provider calls read-only and we do not.
//
// GMAIL_CREATE_PROMPT_POST is tagged readOnlyHint, carries not even
// openWorldHint, and posts text to an unrelated third party -- MCP's "annotations
// are untrusted hints" with a name on it. Host-side so a disagreement is fixed by
// deploying rather than rebuilding a rootfs. Growing past a handful would mean
// the annotations have drifted, which is worth saying rather than curating around.
var deniedReads = map[string]bool{"GMAIL_CREATE_PROMPT_POST": true}
