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
	done, err := s.pushApps(ctx, user, view, cl)
	if err != nil {
		log.Printf("chat: connected apps unavailable for %s: %v", view.ID, err)
		s.failApps(view.ID)
		return
	}
	s.doneApps(view.ID, done)
}

// pushed is what a push that landed leaves behind: when the answer it handed
// over stops counting, and which apps it was an answer ABOUT.
//
// The second is what lets a connection made after the push reach the machine
// before its hour is up. Without it, connecting an app and then asking an agent
// to use it means every read in that app raising a card until the machine next
// comes due -- which is the whole feature working and looking broken.
type pushed struct {
	until time.Time
	apps  string
	// known says whether apps is a reading of this person's connections at all,
	// as opposed to the floor a failed read falls back to.
	//
	// A separate field and not the empty string, which is what this was and was
	// wrong: somebody with NOTHING connected marks as "" too, so their claim read
	// as a guess and the first app they ever connected never brought the machine
	// back early. That is precisely the person opening the catalogue is for.
	known bool
}

// pushApps hands this person's machine a ticket to their session, reporting how
// long the answer it pushed is good for.
func (s *Server) pushApps(ctx context.Context, user string, view vmView, cl *agent.Client) (pushed, error) {
	held, err := s.sessionFor(ctx, user)
	if err != nil {
		return pushed{}, err
	}
	if err := validateComposioSessionURL(held.SessionURL); err != nil {
		return pushed{}, err
	}
	// Resolved BEFORE the ticket exists, though nothing here needs it yet.
	//
	// On a cold cache this is several round trips to the provider, and everything
	// between Register and SetApps widens a window that already had teeth: this
	// runs detached, so a machine erased and recreated mid-push leaves the old
	// goroutine holding a ticket forgetApps has already dropped. It then pushes
	// that dead ticket over the replacement's good one -- and the replacement's
	// claim is latched pushed, so nothing tries again and the machine has no
	// connected apps until the host restarts. Ordering does not close that
	// window, which is the claim's to close; it declines to widen it by a
	// provider round trip.
	apps, mark, whole := s.appsToResolve(ctx, user)
	actions, until := s.kinds.resolved(ctx, apps, held.Policy)
	if !whole {
		// Guessed at, so it does not get to stand for an hour. The apps below are
		// the featured six rather than this person's own, which is right for
		// somebody who has connected some of them and wrong for everybody else.
		until = earlier(until, time.Now().Add(appsRetry))
	}
	// The guest is handed a ticket to the broker, never the session itself. The
	// provider's endpoint needs the PROJECT api key, which is authority over
	// every user's connected accounts, so it stays on this side of the tap.
	hostIP, _, _ := hostnet.SlotAddrs(view.Slot)
	guestURL, err := s.gw.Register(view.ID, view.GuestIP, hostIP, held.SessionURL)
	if err != nil {
		return pushed{}, err
	}
	if err := cl.SetApps(agentapi.Apps{SessionURL: guestURL, SessionID: held.SessionID,
		Actions: actions}); err != nil {
		return pushed{}, err
	}
	return pushed{until: until, apps: mark, known: whole}, nil
}

// appsResolveCap bounds how many of a person's OWN apps one push asks the
// provider about. The floor goes on top, so the fan-out is this plus the
// featured few -- worth saying, because the number that matters is the one that
// leaves at once.
//
// Separate from appsActionCap, which bounds the BYTES a machine is handed: this
// one bounds the fan-out that produces them. An app's actions are a round trip
// each, they go out together, and they all have to land inside appsMintTimeout
// alongside minting the session -- so somebody who has connected two hundred
// apps must not aim two hundred simultaneous requests at the provider every time
// one of their machines boots.
//
// Far above anybody real, like the other two. What it costs when it bites is the
// tail of a very long list asking about its reads.
const appsResolveCap = 32

// appsToResolve is the apps whose actions this person's machine needs resolved,
// and whether the list is actually theirs.
//
// Their CONNECTED apps, in any status, and then the featured ones on top.
//
// The connected apps are the substance. The featured list was the right set
// only while those were the only apps anybody could connect: an action in an app
// somebody has not connected cannot run whatever we say about it, so resolving a
// thousand of them would spend a thousand round trips on nothing.
//
// Any status, including INITIATED, and that is the useful half. A row exists at
// the provider from the moment a link is minted, so an app somebody is signing
// into right now is already in the next push rather than asking about its reads
// until the machine next comes due.
//
// The featured apps stay on the end anyway, and that is a floor rather than a
// leftover. A person can connect an app WITHOUT this service ever hearing about
// it: an agent mints its own link through the provider, and the page they land
// on afterwards is deliberately anonymous. Nothing then re-pushes until the Apps
// screen is next opened or the claim runs out -- up to an hour of a freshly
// connected app raising a card on every read, which is the feature working and
// looking broken. Covering the handful most people connect costs about four
// hundred entries of a bounded push and no round trips once the fleet-wide cache
// is warm, and it is exactly the set that used to be resolved for everybody.
//
// Never an error. A push that cannot read this person's connections falls back
// to the floor alone and says the answer is a guess: a machine with no resolved
// actions asks about everything, and a provider having a bad minute should not
// turn every agent chatty.
func (s *Server) appsToResolve(ctx context.Context, user string) ([]string, string, bool) {
	held, err := s.composio.Connections(ctx, user)
	if err != nil {
		log.Printf("chat: could not read connections for %s, resolving the "+
			"featured apps instead: %v", user, err)
		// No mark: a guess must not be remembered as the set this machine was
		// answered about, or the first route to see the real one would take it
		// for a change and drop a ticket over nothing.
		return featured, "", false
	}
	return withFeatured(appsIn(held)), appsMark(held), true
}

// withFeatured adds the floor to a person's own apps, keeping theirs first.
//
// Order matters twice over: theirs are what the caps are bounded in favour of,
// and theirs are what survives if the push fills up. The floor is small enough
// that it never gets there.
func withFeatured(apps []string) []string {
	for _, app := range featured {
		if !slices.Contains(apps, app) {
			apps = append(apps, app)
		}
	}
	return apps
}

// appsMark is a person's connected apps reduced to one comparable string.
//
// Sorted and deduplicated, so it says only WHICH apps -- the provider's ordering
// is not stable enough to compare, and a second account for an app already
// connected changes nothing about what may run unasked. Status is left out for
// the same reason: a connection expiring does not reclassify a single action.
//
// A string rather than a hash: it is a few hundred bytes at worst, and a log
// line naming the apps is worth more than one naming a number.
func appsMark(held []composio.Connection) string {
	apps := make([]string, 0, len(held))
	for _, conn := range held {
		if conn.Toolkit != "" && !slices.Contains(apps, conn.Toolkit) {
			apps = append(apps, conn.Toolkit)
		}
	}
	slices.Sort(apps)
	return strings.Join(apps, ",")
}

// appsIn is the apps a person holds an account for, in the order the provider
// listed them, without repeats -- somebody can hold several accounts for one app
// and resolving it twice would cost a second round trip for the same answer.
func appsIn(held []composio.Connection) []string {
	out := make([]string, 0, len(held))
	seen := make(map[string]bool, len(held))
	for _, conn := range held {
		if conn.Toolkit == "" || seen[conn.Toolkit] {
			continue
		}
		seen[conn.Toolkit] = true
		if len(out) == appsResolveCap {
			log.Printf("chat: %d connected apps is the most one push resolves; "+
				"%s and anything after it will ask", appsResolveCap, conn.Toolkit)
			return out
		}
		out = append(out, conn.Toolkit)
	}
	return out
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
	policy := held.Policy
	held = appsOf(sess)
	held.Policy = policy
	return held, s.apps.Put(ctx, user, held)
}

// appsOf is a minted session in the shape the guest is handed.
func appsOf(sess composio.Session) agentapi.Apps {
	return agentapi.Apps{SessionURL: sess.URL, SessionID: sess.ID}
}

// appsClaim is what this process has done about one machine's session.
type appsClaim struct {
	pushed bool
	// apps is the mark of the connected set the pushed answer was about, so a
	// route that sees a different one knows this machine is holding an answer to
	// a question that has changed. Only meaningful when known is set.
	apps string
	// known distinguishes a claim whose mark is a real reading from one taken
	// in flight or answered with the floor after a failed read. Nothing compares
	// against the second kind.
	known bool
	// seen is the latest connected set any route has observed since this claim
	// was taken, and sawSeen says whether one has been observed at all -- the
	// empty string is a real answer here, for somebody who holds nothing.
	//
	// It exists for the window where a push is IN FLIGHT. A route that reads
	// somebody's connections then has nothing to compare against yet, and
	// throwing the reading away loses it for good: the push lands afterwards
	// and latches the set IT read, which is already out of date.
	seen    string
	sawSeen bool
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
	s.evictClaimsLocked()
	s.appsClaims[machine] = appsClaim{pushed: true, expires: time.Now().Add(appsMintTimeout)}
	return true
}

// doneApps records a push that landed, due again when its set goes stale.
//
// The re-push goes through pushApps like the first one, which mints a fresh
// ticket and drops the old. That rotation is why this is not on a timer: it
// happens on the next request to reach the machine, so a machine nobody is using
// is not re-ticketed on a schedule for a set nobody is reading.
func (s *Server) doneApps(machine string, done pushed) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// An answer to a question that has already changed is not recorded as the
	// current one. Somebody can finish connecting an app while this push is
	// crossing the internet: a route then reads their connections, finds a claim
	// with nothing to compare against yet, and leaves what it saw here. Latching
	// over it would bury the newer reading under the older answer for the rest of
	// the hour -- and nothing afterwards is guaranteed to look again.
	//
	// Only against a real reading of our own. A push that fell back to the floor
	// already comes due on the short clock, and re-pushing it at once would aim a
	// retry at a provider that has just failed.
	if held, ok := s.appsClaims[machine]; ok && done.known && held.sawSeen && held.seen != done.apps {
		log.Printf("chat: %s was answered about %q while %q was already read; pushing again",
			machine, done.apps, held.seen)
		delete(s.appsClaims, machine)
		return
	}
	// Usually an overwrite of this machine's own in-flight claim, but not always:
	// another machine's claim may have evicted it while this push was crossing
	// the internet, and re-adding it unchecked is how the table creeps past its
	// cap one long push at a time.
	s.evictClaimsLocked()
	s.appsClaims[machine] = appsClaim{pushed: true, expires: done.until,
		apps: done.apps, known: done.known}
}

// noteApps drops a machine's claim when the apps behind it have changed, so the
// next request pushes an answer about the apps this person actually holds.
//
// Called from wherever this service reads somebody's connections for its own
// reasons, which is the only place it learns of a connection made anywhere else
// -- an agent's own connect card never touches this service, and the page
// somebody lands on afterwards is deliberately anonymous (see connected.go).
// The Apps screen opening is what usually catches it, which is where a person
// who just connected something is standing.
//
// The hour-long claim remains the backstop. This only shortens the wait.
//
// The reading is recorded even when there is nothing to compare it against yet,
// which is the in-flight case: dropping it there would let the push that is
// already crossing the internet land afterwards and latch the older set.
func (s *Server) noteApps(machine, mark string) {
	s.mu.Lock()
	held, ok := s.appsClaims[machine]
	if ok {
		// Left on the claim whatever state it is in, so a reading taken while a
		// push is in flight survives it. doneApps is what acts on that one; the
		// check below can only speak for a push that has already landed.
		held.seen, held.sawSeen = mark, true
		s.appsClaims[machine] = held
	}
	stale := ok && held.pushed && held.known && held.apps != mark
	s.mu.Unlock()
	if !stale {
		return
	}
	log.Printf("chat: %s was answered about %q and now holds %q; pushing again",
		machine, held.apps, mark)
	// dueApps and NOT forgetApps: the ticket stays. Nothing about who this
	// machine is or which session is theirs has changed -- only which apps the
	// answer covers -- and dropping the route would leave the guest dialling a
	// ticket the broker now refuses, in the middle of the very retry the person
	// just connected an app for. Register rotates it on the next push.
	s.dueApps(machine)
}

// evictClaimsLocked keeps the table bounded. Caller holds s.mu. Which entry goes
// is not worth choosing: the cap is far above any live fleet, so an eviction
// costs one redundant push and nothing else.
func (s *Server) evictClaimsLocked() {
	for machine := range s.appsClaims {
		if len(s.appsClaims) < appsClaimCap {
			return
		}
		delete(s.appsClaims, machine)
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

// dueApps brings a machine up for another push, leaving what it is holding
// alone.
//
// The claim and the ticket are separate things and only some callers want both
// gone. A machine whose ANSWER has gone stale still has the right session and
// the right owner, so its route is still correct -- and dropping it there is not
// a smaller version of forgetApps but a worse one: the guest goes on dialling a
// ticket the broker has started refusing until something re-pushes, which is a
// live agent losing its app tools mid-call.
func (s *Server) dueApps(machine string) {
	s.mu.Lock()
	delete(s.appsClaims, machine)
	s.mu.Unlock()
}

// forgetApps drops that record AND the ticket, so the next request pushes again
// and nothing can reach the old route in the meantime. Called when a machine is
// created or erased, both of which leave it holding nothing -- and so it clears
// the cooldown as well, which is the difference from failApps.
func (s *Server) forgetApps(machine string) {
	s.dueApps(machine)
	// The ticket goes with it. A route left behind after a machine is recreated
	// -- or after its slot is handed to somebody else's machine -- is exactly how
	// one person's agent would end up acting as another.
	if s.gw != nil {
		s.gw.Forget(machine)
	}
}
