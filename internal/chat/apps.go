package chat

import (
	"context"
	"fmt"
	"log"
	"net/url"
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
	if err := s.pushApps(ctx, user, view, cl); err != nil {
		log.Printf("chat: connected apps unavailable for %s: %v", view.ID, err)
		s.failApps(view.ID)
	}
}

// pushApps hands this person's machine a ticket to their session.
func (s *Server) pushApps(ctx context.Context, user string, view vmView, cl *agent.Client) error {
	held, err := s.sessionFor(ctx, user)
	if err != nil {
		return err
	}
	if err := validateComposioSessionURL(held.SessionURL); err != nil {
		return err
	}
	// The guest is handed a ticket to the broker, never the session itself. The
	// provider's endpoint needs the PROJECT api key, which is authority over
	// every user's connected accounts, so it stays on this side of the tap.
	hostIP, _, _ := hostnet.SlotAddrs(view.Slot)
	guestURL, err := s.gw.Register(view.ID, view.GuestIP, hostIP, held.SessionURL)
	if err != nil {
		return err
	}
	return cl.SetApps(agentapi.Apps{SessionURL: guestURL, SessionID: held.SessionID,
		ReadOnly: s.reads.slugs(ctx)})
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
	held = appsOf(sess)
	return held, s.apps.Put(ctx, user, held)
}

// appsOf is a minted session in the shape the guest is handed.
func appsOf(sess composio.Session) agentapi.Apps {
	return agentapi.Apps{SessionURL: sess.URL, SessionID: sess.ID}
}

// appsClaim is what this process has done about one machine's session.
type appsClaim struct {
	pushed bool
	failed time.Time
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
func (s *Server) claimApps(machine string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if held := s.appsClaims[machine]; held.pushed || time.Since(held.failed) < appsRetryAfter {
		return false
	}
	s.evictClaimsLocked()
	s.appsClaims[machine] = appsClaim{pushed: true}
	return true
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

// forgetApps drops that record, so the next request pushes again. Called when a
// machine is created or erased, both of which leave it holding nothing -- and so
// it clears the cooldown as well, which is the difference from failApps.
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
