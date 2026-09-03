package chat

import (
	"context"
	"log"
	"sync"
	"time"

	"cracked/internal/agentapi"
	"cracked/internal/composio"
)

// appCapsTTL is how long one app's capability map is kept. An hour, matching the
// catalogue: it is the provider's answer, the same for everyone on the fleet, and
// it moves only when they ship or re-annotate a tool.
const appCapsTTL = time.Hour

// appsRetry is how long a machine keeps an INCOMPLETE answer before it is pushed
// again.
//
// Far shorter than the TTL, because an incomplete answer is one an outage made:
// what is missing from it asks a person, and healing that should not wait an
// hour. Far longer than appsRetryAfter, because it is not free -- what is missing
// is never cached, so every machine coming due re-fetches it, and a 30-second
// cadence would aim that at a provider already having a bad day.
const appsRetry = 5 * time.Minute

// appsActionCap bounds how many actions one machine is pushed.
//
// The guest refuses a push over appsBodyCap, 256 KiB, and does it in a way that
// takes the SESSION down with the set rather than just the set -- see
// internal/agentd/routes_apps.go, which says so. That used to be unreachable
// because the answer covered six apps and nothing else; now it covers whatever a
// person has connected, so something has to be the thing that gives.
//
// The arithmetic: ~55 bytes per entry, measured at ~50 KB for the 910 tools of
// the featured six on 2026-09-02. Dropping the ask entries below roughly halves
// what a real answer carries, so 2000 is about 110 KB -- comfortably under the
// guest's ceiling, and well past what anyone reaches. The featured six, all
// connected, resolve to about four hundred.
//
// Sized to be unreachable rather than snug, deliberately: what it costs when it
// bites is a person's last few apps asking about reads, and that is a cost they
// cannot see the reason for. Refusals are emitted ahead of reads so that if it
// does bite, what it gives up is a card nobody needed rather than a switch
// somebody deliberately turned off.
const appsActionCap = 2000

// appCapsCap bounds the per-app cache, for the reason appsClaimCap does: the
// catalogue is a thousand apps deep now, and a service running for weeks must
// not keep an entry for every app anybody on the fleet has ever connected.
const appCapsCap = 256

// appCaps is what kind of thing each app's actions are, as the PROVIDER
// annotates them. No catalogue of our own -- hundreds of tools per app we would
// otherwise keep in step with somebody else's release.
//
// Fleet-wide and cached, unlike the policy it is resolved against, which is one
// person's and stored. Keeping the expensive half shared is the whole reason the
// two are separate: a person changing a setting must not cost a round trip per
// app they have connected.
//
// Keyed per app rather than held as one set, which is what opening the catalogue
// changed. A set covering six named apps could be fetched whole and expire
// whole; there is no whole to fetch when the apps in question are whichever ones
// this person happens to have connected.
type appCaps struct {
	// fetch is a field so a test can answer without a provider.
	fetch func(context.Context, string) (map[string]string, error)

	mu   sync.Mutex
	held map[string]capsEntry
}

// capsEntry is one app's actions and when they go stale.
type capsEntry struct {
	caps    map[string]string
	expires time.Time
}

// newAppCaps prepares the cache. It fetches nothing until asked.
func newAppCaps(c *composio.Client) *appCaps {
	return &appCaps{fetch: c.Capabilities, held: map[string]capsEntry{}}
}

// resolved is what each action needs from this person: auto to run, never to
// refuse, and ABSENT to ask. Flattened by slug, so the guest looks up one string
// and holds no vocabulary of its own.
//
// apps is what to resolve over -- the apps this person has connected, since an
// action in an app they have not connected cannot run whatever we say about it.
// Their order decides what survives the cap.
//
// Nothing that resolves to asking is sent. The guest already treats an unknown
// slug as ask (needs() in internal/agentd/apps.go), so this is the same policy
// expressed in half the bytes -- and the bytes are the thing that has a ceiling.
//
// The deadline is the caller's, not this cache's: a machine is pushed a COPY and
// keeps it until pushed again, so what it governs is when that machine is due
// another push.
func (a *appCaps) resolved(ctx context.Context, apps []string,
	policy map[string]map[string]string) (map[string]string, time.Time) {
	held, until := a.capabilities(ctx, apps)
	out := make(map[string]string)
	// Refusals first, and that ordering is the whole point of two passes.
	//
	// Dropping an entry costs it its meaning, because the guest reads absence as
	// ask -- so what the cap gives up matters. Giving up a read costs a card
	// nobody needed to see. Giving up a REFUSAL costs a person the answer they
	// went into a settings screen to give: an action they switched off would ask
	// instead, and a card is something somebody can say yes to. One pass over a
	// map would decide which by iteration order, differently on every push.
	for _, want := range []string{agentapi.ActionNever, agentapi.ActionAuto} {
		if a.fill(out, apps, held, policy, want) {
			return out, until
		}
	}
	return out, until
}

// fill adds every action resolving to one answer, reporting whether the ceiling
// stopped it short.
func (a *appCaps) fill(out map[string]string, apps []string,
	held map[string]map[string]string, policy map[string]map[string]string, want string) bool {
	for _, app := range apps {
		for slug, capability := range held[app] {
			if actionFor(capability, policy[app][capability]) != want {
				continue
			}
			if len(out) == appsActionCap {
				// Said out loud, because nobody can see the reason from the
				// outside: what is left over asks, which reads as a gate having a
				// bad day rather than a ceiling being reached.
				log.Printf("chat: %d actions is the most one machine is pushed; "+
					"%s and anything after it will ask", appsActionCap, app)
				return true
			}
			out[slug] = want
		}
	}
	return false
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

// capabilities is each of these apps' actions and what kind each is, with the
// deadline of the soonest thing in it.
//
// Never fails, and what could not be read is never cached: what is missing from
// it asks a person, and caching that would spend an hour asking about reads that
// are perfectly safe.
func (a *appCaps) capabilities(ctx context.Context,
	apps []string) (map[string]map[string]string, time.Time) {
	held := make(map[string]map[string]string, len(apps))
	until := time.Now().Add(appCapsTTL)
	var mu sync.Mutex
	var wg sync.WaitGroup
	// Parallel because this sits in front of a machine somebody is waiting on,
	// and an app's tools are a round trip each: a person with a dozen connected
	// would otherwise wait a dozen of them in series, inside a mint timeout that
	// also has to cover minting the session itself.
	for _, app := range apps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			caps, expires, ok := a.appCapabilities(ctx, app)
			mu.Lock()
			defer mu.Unlock()
			if !ok {
				// Not cached, and the machine given it comes back on the short
				// clock rather than the full TTL -- an outage's answer must not
				// outlive it.
				until = earlier(until, time.Now().Add(appsRetry))
				return
			}
			held[app] = caps
			until = earlier(until, expires)
		}()
	}
	wg.Wait()
	if len(held) != len(apps) {
		// Said out loud because the cost lands somewhere else entirely: the
		// machine pushed this keeps it until pushed again, and every action
		// missing from it asks. Silence here reads as a chatty gate.
		log.Printf("chat: %d of %d apps answered; some reads will ask", len(held), len(apps))
	}
	return held, until
}

// appCapabilities is one app's actions, from the cache or the provider.
//
// The bool is what says whether it was read at all, and it has to be: an app
// with no actions is a perfectly good answer worth caching for the hour, while
// an app that did not answer is retried on the short clock. Reading a nil map as
// the second would put every app the provider legitimately has nothing for on a
// five-minute loop forever.
func (a *appCaps) appCapabilities(ctx context.Context, app string) (map[string]string, time.Time, bool) {
	if held, ok := a.fresh(app); ok {
		return held.caps, held.expires, true
	}
	got, err := a.fetch(ctx, app)
	if err != nil {
		// Named, with its reason. A count of missing apps cannot tell a provider
		// outage from one toolkit that outgrew a decode limit -- and the second
		// is the one that never heals on its own.
		log.Printf("chat: %s did not answer: %v", app, err)
		return nil, time.Time{}, false
	}
	ourView(got)
	return got, a.keep(app, got), true
}

// fresh returns one app's cached actions while they are still good. The bool is
// what says so: an app with no actions is not an app nothing is cached for.
func (a *appCaps) fresh(app string) (capsEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	held, ok := a.held[app]
	return held, ok && time.Now().Before(held.expires)
}

// keep stores one app's actions and starts its clock, reporting when it runs out.
func (a *appCaps) keep(app string, caps map[string]string) time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evictLocked()
	expires := time.Now().Add(appCapsTTL)
	a.held[app] = capsEntry{caps: caps, expires: expires}
	return expires
}

// evictLocked keeps the table bounded. Caller holds a.mu. Expired entries go
// first because they are free to lose; past that, which one goes is not worth
// choosing -- the cap is far above what a fleet reaches, so an eviction costs one
// re-fetch and nothing else.
func (a *appCaps) evictLocked() {
	now := time.Now()
	for app, held := range a.held {
		if len(a.held) < appCapsCap {
			return
		}
		if now.After(held.expires) {
			delete(a.held, app)
		}
	}
	for app := range a.held {
		if len(a.held) < appCapsCap {
			return
		}
		delete(a.held, app)
	}
}

// earlier is the sooner of two deadlines. time.Time is not ordered, so min does
// not take it, and a push is only good until the FIRST thing in it goes stale.
func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
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
