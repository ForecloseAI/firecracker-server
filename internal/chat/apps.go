package chat

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"slices"
	"strings"
	"time"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
	"cracked/internal/composio"
	"cracked/internal/hostnet"
)

// appsMintTimeout bounds minting a session and pushing it. Generous, because it
// crosses the internet, but bounded: a stuck provider must not leave a goroutine
// and a claim standing for the life of the process.
const appsMintTimeout = 20 * time.Second

// appsRetryAfter is how long a failed mint or push is remembered.
//
// Without it, a provider having a bad ten minutes means one full mint attempt
// per machine per REQUEST, and the app opens by fetching every agent's thread in
// parallel. The guest half of this keeps the same guard for the same reason;
// see appsRetryAfter in internal/agentd.
const appsRetryAfter = 30 * time.Second

// ensureApps makes sure this person's machine can reach their connected apps.
//
// Best effort throughout. Neither the provider nor the database having a bad
// minute may stop someone reaching their agents -- the worst outcome here is a
// machine whose agents have no app tools this boot, which the next request
// retries.
func (s *Server) ensureApps(ctx context.Context, user string, view vmView, cl *agent.Client) {
	if s.composio == nil || s.apps == nil || s.gw == nil || !s.claimApps(view.ID) {
		return
	}
	// Detached, and the context carried for its VALUES rather than its
	// cancellation. This sits in front of the SSE stream and the app's first call
	// after sign-in, and the guest push does not honour a context at all -- so
	// synchronously it is the provider deciding how long the app looks hung. The
	// losers of the claim above already return with no session and nothing breaks,
	// which is what makes waiting for the winner buy nothing.
	go s.mintApps(context.WithoutCancel(ctx), user, view, cl)
}

// mintApps hands the machine its session, releasing the claim if it cannot.
func (s *Server) mintApps(ctx context.Context, user string, view vmView, cl *agent.Client) {
	ctx, cancel := context.WithTimeout(ctx, appsMintTimeout)
	defer cancel()
	until, slugs, err := s.pushApps(ctx, user, view, cl)
	if err != nil {
		log.Printf("chat: connected apps unavailable for %s: %v", view.ID, err)
		s.failApps(view.ID)
		return
	}
	s.doneApps(view.ID, until, slugs)
}

// pushApps hands this person's machine a ticket to their session, reporting how
// long the answer it pushed is good for and which apps it was resolved against.
func (s *Server) pushApps(ctx context.Context, user string, view vmView,
	cl *agent.Client) (time.Time, []string, error) {
	row, err := s.sessionFor(ctx, user)
	if err != nil {
		return time.Time{}, nil, err
	}
	if err := validateComposioSessionURL(row.SessionURL); err != nil {
		return time.Time{}, nil, err
	}
	conns, err := s.composio.Connections(ctx, user)
	if err != nil {
		return time.Time{}, nil, err
	}
	slugs := activeSlugs(conns)
	// Resolved BEFORE the ticket exists. See handOver for the window that keeps
	// every provider round trip on this side of Register.
	actions, until := s.kinds.resolved(ctx, slugs, row.Policy)
	if err := s.handOver(view, cl, row, actions); err != nil {
		return time.Time{}, nil, err
	}
	return until, slugs, nil
}

// handOver gives the machine a ticket to the broker and the answer to obey.
//
// The guest is handed a ticket, never the session itself: the provider's
// endpoint needs the PROJECT api key, which is authority over every user's
// connected accounts, so it stays on this side of the tap.
//
// Everything between Register and SetApps widens a window that already had
// teeth. This runs detached, so a machine erased and recreated mid-push leaves
// the old goroutine holding a ticket forgetApps has already dropped; it then
// pushes that dead ticket over the replacement's good one, and the
// replacement's claim is latched pushed, so nothing tries again and the machine
// has no connected apps until the host restarts. Ordering does not close that
// window -- that is the claim's to close -- but nothing slow belongs in here.
func (s *Server) handOver(view vmView, cl *agent.Client,
	held agentapi.Apps, actions map[string]string) error {
	hostIP, _, _ := hostnet.SlotAddrs(view.Slot)
	guestURL, err := s.gw.Register(view.ID, view.GuestIP, hostIP, held.SessionURL)
	if err != nil {
		return err
	}
	return cl.SetApps(agentapi.Apps{SessionURL: guestURL, SessionID: held.SessionID,
		Actions: actions})
}

// activeSlugs is the distinct apps a person has a WORKING account with, sorted
// so two readings of an unchanged set compare equal. Lowercased because
// connectionFor already treats the provider's spelling as case-insensitive.
//
// ACTIVE only, and that carries two jobs. Resolving an app whose grant has
// lapsed would spend a provider call on actions that cannot run; and because
// this same set is what noticeApps compares, counting a half-finished
// connection would make INITIATED-becomes-ACTIVE look like no change at all --
// so the moment somebody finishes signing in would be the one moment nothing
// noticed.
func activeSlugs(held []composio.Connection) []string {
	out := make([]string, 0, len(held))
	for _, conn := range held {
		if conn.Status == composio.StatusActive {
			out = append(out, strings.ToLower(conn.Toolkit))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// validateComposioSessionURL is the boundary between caller-writable storage
// and the broker that adds the project-wide API key. A user can edit their own
// PostgREST row, so only the provider's exact HTTPS origin may receive that key.
func validateComposioSessionURL(raw string) error {
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "https" || target.Host != "backend.composio.dev" || target.User != nil {
		return fmt.Errorf("connected apps: refusing non-Composio session URL")
	}
	return nil
}

// sessionFor is this person's session, minted and recorded on first use.
//
// The store is asked first, so a session outlives the machine that used it: a VM
// recreated from nothing gets the same session back and sees every app already
// connected, with no second trip through anyone's sign-in page.
func (s *Server) sessionFor(ctx context.Context, user string) (agentapi.Apps, error) {
	held, err := s.apps.Get(ctx, user)
	if err != nil || held.SessionURL != "" {
		return held, err
	}
	// The Supabase id WITH its hyphens. The machine id is the same UUID with
	// them stripped, and the two are both hex strings of similar length -- so a
	// mix-up here would isolate someone from their own connections and report
	// nothing.
	sess, err := s.composio.NewSession(ctx, user, s.cfg.ComposioCallback)
	if err != nil {
		return agentapi.Apps{}, err
	}
	// Carried across the re-mint. A person can set a policy before a machine has
	// ever been pushed to -- the permissions screen does not wait for a session
	// -- so the row can hold one with no URL beside it.
	held = agentapi.Apps{SessionURL: sess.URL, SessionID: sess.ID, Policy: held.Policy}
	return held, s.apps.Put(ctx, user, held)
}

// appsClaim is what this process has done about one machine's session.
type appsClaim struct {
	pushed bool
	// slugs is the connected apps the pushed answer was resolved against,
	// sorted. A set that differs from what this person holds now is a push
	// already out of date, which is how a newly connected app reaches a machine
	// in a screen refresh rather than in an hour.
	//
	// Nil while a push is IN FLIGHT, which is what settled() reads: a claim is
	// taken before the work, so between claimApps and doneApps there is nothing
	// to compare against, and expiring it there would start a second push while
	// the first was still crossing the internet.
	slugs []string
	// expires is when a pushed claim stops counting, which is the deadline of
	// the answer that push handed over.
	//
	// A pushed claim used to latch forever, so the TTL governed only what the
	// NEXT machine to boot was told: an answer fetched during an outage stayed
	// partial, and a tool the provider stopped annotating readOnlyHint stayed
	// runnable-without-asking on every live machine until the host restarted. A
	// deadline is what makes expiry reach machines that already have a copy.
	expires time.Time
	failed  time.Time
}

// settled reports whether this claim holds an answer that actually landed.
//
// pushed alone is not that question: claimApps takes the claim BEFORE the work
// with pushed already true, so the two states are told apart by whether there
// are slugs beside it. One spelling, because three call sites asking it
// differently is how one of them ends up expiring a push still in flight.
func (c appsClaim) settled() bool { return c.pushed && c.slugs != nil }

// appsClaimCap bounds the table, for the reason appsRouteCap does: a service
// running for weeks must not keep an entry per machine it has ever served.
const appsClaimCap = 256

// claimApps takes responsibility for pushing to a machine, reporting whether
// this caller is the one that got it.
//
// Claimed BEFORE the work rather than marked after, which makes it an in-flight
// guard as well as a memo. The app opens by fetching every agent's thread in
// parallel and connecting the stream, so first sign-in is exactly when a dozen
// requests arrive at once -- and a check-then-mark would let all of them pass
// and mint a dozen sessions for one person.
//
// A claim is taken with the IN-FLIGHT deadline, not the set's: the push has not
// happened yet and has nothing to report. doneApps replaces it with the real one
// on success, failApps with a cooldown on failure -- so a goroutine that dies
// without doing either frees the machine after appsMintTimeout rather than
// stranding it, which is the same bound the push itself runs under.
func (s *Server) claimApps(machine string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.appsClaims[machine]
	if held.pushed && time.Now().Before(held.expires) {
		return false
	}
	if !held.pushed && time.Since(held.failed) < appsRetryAfter {
		return false
	}
	evictTo(s.appsClaims, appsClaimCap)
	s.appsClaims[machine] = appsClaim{pushed: true, expires: time.Now().Add(appsMintTimeout)}
	return true
}

// doneApps records a push that landed, due again when its set goes stale or when
// the apps it was resolved against change.
//
// The re-push goes through pushApps like the first one, which mints a fresh
// ticket and drops the old. That rotation is why this is not on a timer: it
// happens on the next request to reach the machine, so a machine nobody is using
// is not re-ticketed on a schedule for a set nobody is reading.
func (s *Server) doneApps(machine string, until time.Time, slugs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Usually an overwrite of this machine's own in-flight claim, but not always:
	// another machine's claim may have evicted it while this push was crossing
	// the internet, and re-adding it unchecked is how the table creeps past its
	// cap one long push at a time.
	evictTo(s.appsClaims, appsClaimCap)
	s.appsClaims[machine] = appsClaim{pushed: true, expires: until, slugs: slugs}
}

// noticeApps drops a machine's claim when this person's connected apps are no
// longer the set its last push was resolved against.
//
// Cheap enough to sit on a list route: the caller already has these connections
// in hand, so this costs a comparison rather than a request. It is one of two
// triggers -- see noticeConnect in approval.go, because the flow that matters
// most does not involve the Apps screen at all.
func (s *Server) noticeApps(user string, held []composio.Connection) {
	now := activeSlugs(held)
	machine := machineFor(user)
	s.mu.Lock()
	defer s.mu.Unlock()
	if claim := s.appsClaims[machine]; claim.settled() && !slices.Equal(claim.slugs, now) {
		s.staleLocked(machine, claim)
	}
}

// staleApps marks a machine's answer out of date WITHOUT taking its ticket away.
//
// The route is the whole difference from forgetApps, and it decides whether this
// helps or hurts: both callers fire while an agent is mid-call. Somebody
// answering a Connect card is answering an agent that is about to retry the very
// call it was blocked on, and dropping the ticket first makes that retry 404 at
// the broker and the guest sit out its own cooldown -- a failure at exactly the
// moment this exists to produce a success.
//
// Zeroing the deadline is enough. claimApps treats a pushed claim past its
// expiry as available, and the re-push registers a fresh ticket over the old one
// anyway. forgetApps stays for the machine-lifetime cases -- created and erased
// -- where there is no live ticket worth keeping.
func (s *Server) staleApps(machine string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if claim := s.appsClaims[machine]; claim.settled() {
		s.staleLocked(machine, claim)
	}
}

// staleLocked expires one claim. The caller holds s.mu, which is what lets
// noticeApps decide and act without dropping the lock in between.
func (s *Server) staleLocked(machine string, claim appsClaim) {
	claim.expires = time.Time{}
	s.appsClaims[machine] = claim
}

// evictTo keeps one of this package's bounded tables under its cap. The caller
// holds whatever lock guards the map.
//
// Which entry goes is not worth choosing: both caps are far above any live
// fleet, so nothing live is ever evicted in practice -- the point is only that
// a service running for weeks does not keep an entry per machine it has ever
// seen. An eviction costs one redundant push and nothing else.
func evictTo[K comparable, V any](m map[K]V, limit int) {
	for k := range m {
		if len(m) < limit {
			return
		}
		delete(m, k)
	}
}

// failApps releases the claim behind a cooldown, so an outage costs one attempt
// per machine rather than one per request. The route goes too: a half-registered
// ticket is one the guest was never told about.
func (s *Server) failApps(machine string) {
	s.mu.Lock()
	s.appsClaims[machine] = appsClaim{failed: time.Now()}
	s.mu.Unlock()
	if s.gw != nil {
		s.gw.Forget(machine)
	}
}

// forgetApps drops that record, so the next request pushes again. Called ONLY
// when a machine is created or erased, both of which leave it holding nothing --
// and so it clears the cooldown as well, which is the difference from failApps.
//
// Anything that merely changes the ANSWER a live machine holds wants staleApps
// instead: this takes the ticket with it, and a machine that still exists may be
// mid-call on it.
func (s *Server) forgetApps(machine string) {
	s.mu.Lock()
	delete(s.appsClaims, machine)
	s.mu.Unlock()
	// The ticket goes with it. A route left behind after a machine is recreated
	// -- or after its slot is handed to somebody else's machine -- is exactly how
	// one person's agent would end up acting as another.
	if s.gw != nil {
		s.gw.Forget(machine)
	}
}
