package chat

import (
	"time"
)

// slugCap bounds an app slug. The longest in the provider's catalogue is a good
// deal shorter; this is the length past which a string is not a slug at all.
const slugCap = 64

// validSlug reports whether a string is shaped like an app slug.
//
// Lowercase letters, digits and separators: "gmail", "microsoft_teams",
// "google-drive". A shape check and nothing more -- what decides whether an app
// EXISTS is the catalogue, and this only decides whether a string is worth
// asking the catalogue about, or worth writing into somebody's stored settings.
//
// It matters because a slug now reaches three provider endpoints and one stored
// row where six known strings used to. Path-escaped at every one of those, so
// this is not the thing standing between a client and a traversal -- it is what
// keeps a megabyte of arbitrary bytes out of a URL and a settings row.
func validSlug(s string) bool {
	if s == "" || len(s) > slugCap {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// policyAppCap bounds how many apps one person keeps permissions for.
//
// The row is read on every push, and dropping the featured allowlist from
// setAppPolicy means nothing else limits what can go into it. Far above any real
// use: each entry costs a deliberate tap on a settings screen, and the catalogue
// itself is browsed rather than swept.
const policyAppCap = 128

// connectBurst is how many connect links one person may mint in connectWindow.
//
// Generous for a person -- connecting five apps in a sitting is an ordinary
// afternoon -- and small against what this route can leave behind. Each mint may
// create an auth config that is project-wide, permanent and counted against this
// project's plan, so the ceiling is on the CREATE path's blast radius rather
// than on anybody's patience.
const connectBurst = 10

// connectWindow is the span connectBurst is counted over.
const connectWindow = time.Minute

// connectRateCap bounds the table, for the reason appsClaimCap does: a service
// running for weeks must not keep a row for every person who ever connected
// something.
const connectRateCap = 512

// mayConnect reports whether this person may mint another connect link now,
// recording the attempt when they may.
//
// Per person rather than per app: minting ten links for one app is fine and
// ordinary -- links expire in ten minutes and a person who wandered off needs a
// fresh one -- while ten links for ten apps they have never heard of is the
// shape worth stopping.
func (s *Server) mayConnect(user string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connects == nil {
		// Made here rather than required of the caller. This is a guard on a
		// route, and a guard that turns into a panic when somebody builds a
		// Server without it is worse than no guard at all -- appsClaims already
		// has that shape and it is a trap every test has to know about.
		s.connects = map[string][]time.Time{}
	}
	now := time.Now()
	held := s.connects[user][:0:0]
	for _, at := range s.connects[user] {
		if now.Sub(at) < connectWindow {
			held = append(held, at)
		}
	}
	if len(held) >= connectBurst {
		// Kept rather than dropped: the window has to keep sliding while somebody
		// is over it, or a caller hammering the route resets their own clock.
		s.connects[user] = held
		return false
	}
	s.evictConnectsLocked()
	s.connects[user] = append(held, now)
	return true
}

// evictConnectsLocked keeps the table bounded. Caller holds s.mu. Which row goes
// is not worth choosing: an eviction costs somebody a forgotten attempt or two,
// and the cap is far above a live fleet.
func (s *Server) evictConnectsLocked() {
	now := time.Now()
	for user, held := range s.connects {
		if len(s.connects) < connectRateCap {
			return
		}
		if len(held) == 0 || now.Sub(held[len(held)-1]) >= connectWindow {
			delete(s.connects, user)
		}
	}
	for user := range s.connects {
		if len(s.connects) < connectRateCap {
			return
		}
		delete(s.connects, user)
	}
}
