package agentd

import (
	"net/http"
	"net/netip"
	"net/url"

	"cracked/internal/agentapi"
)

// appsBodyCap bounds the push.
//
// It used to carry two short strings and was capped at 8 KiB. It now also
// carries the read-only set, measured at 13,073 bytes for the featured six on
// 2026-09-02 -- so the old cap refused every push, and because handlePutApps
// answers 400 before WriteApps, it took the SESSION down with it rather than
// just the set. Deterministic, self-repeating on the retry cooldown, and it
// presents as "connected apps unavailable" with nothing naming the cause.
//
// Sized for a list that grows on the PROVIDER's schedule, not ours: that is the
// point of fetching it rather than compiling it in, and it is why the headroom
// is generous rather than snug.
const appsBodyCap = 256 << 10

// handleGetApps reports the app-integration session this machine holds.
//
// A diagnostic, not part of the push: the host keeps its own record of which
// machines it has handed a session to. This is how you ask a live machine what
// it actually got, which is the question worth answering when an agent has no
// app tools and nobody can see why.
func (s *Server) handleGetApps(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, ReadApps(s.sup.stateDir))
}

// handlePutApps records the session the host minted for this machine's person.
func (s *Server) handlePutApps(w http.ResponseWriter, r *http.Request) {
	var a agentapi.Apps
	if !decode(w, r, appsBodyCap, &a) {
		return
	}
	// A URL we cannot dial is refused rather than stored. This call is the only
	// way a session reaches the machine, so accepting a bad one with a 204 would
	// leave the host believing the agents could reach the person's apps.
	if err := validSessionURL(a.SessionURL); err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err.Error(), "apps")
		return
	}
	// Read before the write: applyApps needs to know what this machine held, and
	// once the file is replaced there is nothing left to compare against.
	had := ReadApps(s.sup.stateDir)
	if err := WriteApps(s.sup.stateDir, a); err != nil {
		fail(w, http.StatusInternalServerError, "write_failed", err.Error(), "apps")
		return
	}
	s.applyApps(had, a)
	w.WriteHeader(http.StatusNoContent)
}

// applyApps points the machine at the new session and, only if it has to, makes
// the agents notice.
//
// Re-pointing is always safe and always cheap: a wrapped tool is bound to the
// server, not to a session, and resolves one per call -- so SetURL alone makes
// the next call dial the new address.
//
// Eviction is the expensive half and is reserved for a change agents cannot see
// any other way. Every running agent composed its tool surface when it started,
// so a surface that APPEARS or DISAPPEARS needs them rebuilt; a session that
// merely moved does not.
func (s *Server) applyApps(had, now agentapi.Apps) {
	s.sup.Apps().SetURL(now.SessionURL)
	if surfaceChanged(had, now) {
		s.sup.EvictIdle()
	}
}

// surfaceChanged reports whether the agents on this machine were built against
// something different from what it holds now.
//
// The URL is deliberately NOT the test. The host mints a fresh ticket on every
// push, and it pushes on every restart, so comparing URLs meant one deploy tore
// down every machine's session and rebuilt every idle agent on it -- each one's
// next turn paying a prompt-cache write -- for a session that had not moved at
// all. The session id is what actually identifies it.
func surfaceChanged(had, now agentapi.Apps) bool {
	if (had.SessionURL == "") != (now.SessionURL == "") {
		return true // the surface appeared or was taken away
	}
	return had.SessionID != now.SessionID
}

// validSessionURL accepts an endpoint this machine could actually dial.
//
// Empty is allowed and meaningful: it is how a machine has the surface taken
// away again.
//
// https anywhere, and http only to a private address. The URL is a bearer
// ticket, so plaintext to the open internet is refused -- but the broker the
// host runs sits on the other end of this machine's own tap, a point-to-point
// link that never touches a wire, and requiring TLS there would mean shipping a
// certificate into every guest image to protect a cable that does not exist.
func validSessionURL(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host == "" {
		return errNotDialable
	}
	if u.Scheme == "https" {
		return nil
	}
	if u.Scheme == "http" && isPrivateHost(u.Hostname()) {
		return nil
	}
	return errNotDialable
}

// isPrivateHost reports whether a host is an address on this machine's own
// side of the world -- the broker, or a test server.
func isPrivateHost(host string) bool {
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return addr.IsPrivate() || addr.IsLoopback()
}
