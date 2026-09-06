package chat

import (
	"context"
	"log"
	"sync"
	"time"

	"cracked/internal/agentapi"
	"cracked/internal/composio"
)

// appCapsTTL is how long one app's capability map is kept.
//
// An hour, and deliberately NOT the catalogue's fifteen days. The catalogue is
// copy; this is what a write gate is resolved against, so a tool the provider
// re-annotates from read to write must not stay runnable-without-asking for a
// fortnight on every live machine.
const appCapsTTL = time.Hour

// appsRetry is how long a machine keeps an INCOMPLETE answer before it is pushed
// again.
//
// Far shorter than the TTL, because an incomplete answer is one an outage made:
// what is missing from it asks a person, and healing that should not wait an
// hour. Far longer than appsRetryAfter, because it is not free.
const appsRetry = 5 * time.Minute

// appsActionBytes is how much resolved answer one machine may be pushed.
//
// The guest refuses a body over its own 256 KiB cap and answers 400 BEFORE
// writing the file, so an oversized push does not merely lose the set -- it
// takes that machine's whole app session down, deterministically, on every
// retry, presenting as "connected apps unavailable" with nothing naming the
// cause. This sits under that with room for the session URL and id beside it.
//
// Reached only by somebody who connected a great many large apps: measured
// 2026-09-05, the five biggest apps the provider manages OAuth for come to 2538
// actions between them, around 140 KB. What happens past this is that whole apps
// are dropped, and absent means ask -- noisier, never more permissive.
const appsActionBytes = 192 << 10

// appCaps is what kind of thing each action is, as the PROVIDER annotates it.
// No catalogue of our own -- thousands of tools we would otherwise keep in step
// with somebody else's release.
//
// Kept PER APP rather than as one fleet-wide sweep. Two people who both
// connected Gmail still share an entry, which is the saving the old whole-list
// cache bought; what changed is that the work is bounded by what somebody
// connected rather than by what this build offers. Fetching all 122 would be 122
// round trips for an answer no machine could be pushed.
type appCaps struct {
	// fetch is a field so a test can answer without a provider.
	fetch func(context.Context, string) (map[string]string, error)

	mu sync.Mutex
	// held is bounded by the catalogue, since only an app that exists can be
	// connected, so there is nothing here to evict.
	held map[string]capEntry
}

// capEntry is one app's actions and when they stop counting. A nil map is an app
// that did not answer, which is why the deadline is carried beside it rather
// than inferred from whether there is anything here.
type capEntry struct {
	kinds   map[string]string
	expires time.Time
}

// newAppCaps prepares the cache. It fetches nothing until asked.
func newAppCaps(c *composio.Client) *appCaps {
	return &appCaps{fetch: c.Capabilities, held: map[string]capEntry{}}
}

// resolved is what each action needs from this person: auto to run, ask to raise
// a card, never to refuse. Flattened by slug, so the guest looks up one string
// and holds no vocabulary of its own.
//
// slugs is what this person CONNECTED, not what the build offers. An action
// outside that set is absent from the answer, and absent asks.
//
// The deadline is the caller's, not this cache's: a machine is pushed a COPY and
// keeps it until pushed again, so what it governs is when that machine is due
// another push.
func (a *appCaps) resolved(ctx context.Context, slugs []string,
	policy map[string]map[string]string) (map[string]string, time.Time) {
	held, until := a.capabilities(ctx, slugs)
	budgeted(held)
	return flatten(held, policy), until
}

// flatten resolves every action against this person's policy, by slug.
func flatten(held, policy map[string]map[string]string) map[string]string {
	out := make(map[string]string)
	for app, kinds := range held {
		for slug, capability := range kinds {
			out[slug] = actionFor(capability, policy[app][capability])
		}
	}
	return out
}

// budgeted drops whole apps, largest first, until the push will fit. In place,
// and it returns nothing so no caller reads it as a copy.
//
// Whole apps rather than a truncation, because half an app's actions is a person
// asked about some of its reads and not others for no reason they could see. The
// outer map is built per call, so nothing cached is disturbed.
func budgeted(held map[string]map[string]string) {
	for pushBytes(held) > appsActionBytes {
		app := largest(held)
		// Said out loud: the cost lands on a machine, hours later, as an agent
		// asking about reads. Silence here reads as a chatty gate with no cause.
		log.Printf("chat: capability map is too big to push; dropping %s, whose actions will ask", app)
		delete(held, app)
	}
}

// pushBytes is roughly what a resolved answer costs encoded: per entry a quoted
// slug, a colon, a quoted word and a comma.
func pushBytes(held map[string]map[string]string) int {
	n := 0
	for _, kinds := range held {
		for slug, capability := range kinds {
			n += len(slug) + len(capability) + 6
		}
	}
	return n
}

// largest is the app contributing most to the push, named alphabetically on a
// tie so two hosts do not drop different apps for the same person.
func largest(held map[string]map[string]string) string {
	name := ""
	for app, kinds := range held {
		if name == "" || len(kinds) > len(held[name]) ||
			(len(kinds) == len(held[name]) && app < name) {
			name = app
		}
	}
	return name
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

// capabilities is every named app's actions, and when the answer runs out.
// Parallel because this sits in front of a machine being handed its session.
func (a *appCaps) capabilities(ctx context.Context,
	slugs []string) (map[string]map[string]string, time.Time) {
	got := make([]capEntry, len(slugs))
	var wg sync.WaitGroup
	for i, slug := range slugs {
		wg.Go(func() { got[i] = a.one(ctx, slug) })
	}
	wg.Wait()
	return foldCaps(slugs, got)
}

// foldCaps collects what each app answered and the earliest deadline among them.
// An app that failed contributes nothing and a short clock, so its actions ask
// and the machine comes back for them in minutes rather than an hour.
func foldCaps(slugs []string, got []capEntry) (map[string]map[string]string, time.Time) {
	held := make(map[string]map[string]string, len(slugs))
	until := time.Now().Add(appCapsTTL)
	for i, entry := range got {
		if entry.kinds != nil {
			held[slugs[i]] = entry.kinds
		}
		if entry.expires.Before(until) {
			until = entry.expires
		}
	}
	return held, until
}

// one is a single app's actions, from the cache or from the provider.
func (a *appCaps) one(ctx context.Context, slug string) capEntry {
	if held, ok := a.fresh(slug); ok {
		return held
	}
	kinds, err := a.fetch(ctx, slug)
	if err != nil {
		// Named, with its reason. A count of missing apps cannot tell a provider
		// outage from one app that broke on its own, and the second never heals.
		log.Printf("chat: %s did not answer; its actions will ask: %v", slug, err)
		return capEntry{expires: time.Now().Add(appsRetry)}
	}
	ourView(kinds)
	return a.keep(slug, kinds)
}

// fresh returns one app's cached answer while it is still good. The bool is what
// says so: an app that legitimately exposes nothing is not nothing cached.
func (a *appCaps) fresh(slug string) (capEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	held, ok := a.held[slug]
	return held, ok && time.Now().Before(held.expires)
}

// keep stores one app's answer and starts its clock.
func (a *appCaps) keep(slug string, kinds map[string]string) capEntry {
	entry := capEntry{kinds: kinds, expires: time.Now().Add(appCapsTTL)}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held[slug] = entry
	return entry
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
