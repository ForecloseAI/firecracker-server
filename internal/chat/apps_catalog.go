package chat

import (
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"cracked/internal/composio"
)

// featured is the apps this build offers, in the order they are shown.
//
// The ONLY thing about an app written down here. Names, logos and blurbs come
// from the provider, because that is copy which goes stale while nobody notices
// -- what this project should be choosing is which apps to offer, not how to
// describe them.
var featured = []string{
	"gmail", "googlecalendar", "slack", "outlook", "microsoft_teams", "asana",
}

// appsCatalogTTL is how long the featured apps' copy is kept.
//
// Long, because it is the same for every person on the fleet and changes about
// as often as a company rebrands. The cost of it being stale is a slightly old
// blurb; the cost of not caching is six round trips on every Apps screen.
const appsCatalogTTL = time.Hour

// appCatalog keeps the featured apps' copy so the screen does not re-fetch it
// per person. One entry with one deadline, refreshed on read when stale --
// the idiom Caps uses in vncgw.go, with no background goroutine.
type appCatalog struct {
	// fetch is a field so a test can answer without a provider.
	fetch func(context.Context, string) (composio.Toolkit, error)

	mu      sync.Mutex
	held    []composio.Toolkit
	expires time.Time
}

// newAppCatalog prepares the cache. It fetches nothing until asked.
func newAppCatalog(c *composio.Client) *appCatalog {
	return &appCatalog{fetch: c.Toolkit}
}

// toolkits returns the featured apps, refreshing the copy when it has gone
// stale. It never fails: an app whose metadata could not be read still appears,
// named after its own slug, because a person who cannot see Slack cannot connect
// it either. Only a complete fetch is cached, so a bad minute is not kept for an
// hour.
func (a *appCatalog) toolkits(ctx context.Context) []composio.Toolkit {
	if held := a.fresh(); held != nil {
		return held
	}
	got, whole := a.fetchAll(ctx)
	if whole {
		a.keep(got)
	}
	return got
}

// fresh returns the cached copy while it is still good.
func (a *appCatalog) fresh() []composio.Toolkit {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Now().Before(a.expires) {
		return a.held
	}
	return nil
}

// keep stores a complete copy and starts its clock.
func (a *appCatalog) keep(held []composio.Toolkit) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held, a.expires = held, time.Now().Add(appsCatalogTTL)
}

// fetchAll reads every featured app, naming one that could not be read after its
// own slug so it still appears on the list.
func (a *appCatalog) fetchAll(ctx context.Context) ([]composio.Toolkit, bool) {
	return fanOut(ctx, a.fetch, func(slug string) composio.Toolkit {
		return composio.Toolkit{Slug: slug, Name: labelFor(slug)}
	})
}

// fanOut reads every featured app at once, reporting whether all of them
// answered. Parallel because this sits in front of a person opening a screen and
// six round trips in series is a second they watch.
//
// miss says what an app that did not answer contributes -- a placeholder the
// screen can still draw, or nothing at all. That is the ONLY thing the two
// callers disagree about, so it is an argument here rather than the reason for a
// second copy of the interesting part: index-addressed writes that need no lock,
// missed set before the Wait, and a loop variable captured per iteration.
func fanOut[E any](ctx context.Context, fetch func(context.Context, string) (E, error),
	miss func(string) E) ([]E, bool) {
	out := make([]E, len(featured))
	var missed atomic.Bool
	var wg sync.WaitGroup
	for i, slug := range featured {
		wg.Go(func() {
			got, err := fetch(ctx, slug)
			if err != nil {
				// Named, with its reason. The callers only say how MANY apps are
				// missing, which cannot tell a provider outage from one toolkit
				// that outgrew a decode limit -- and the second is the one that
				// never heals on its own.
				log.Printf("chat: %s did not answer: %v", slug, err)
				missed.Store(true)
				got = miss(slug)
			}
			out[i] = got
		})
	}
	wg.Wait()
	return out, !missed.Load()
}

// labelFor is the best name a slug alone can give, for an app whose metadata
// could not be read: "microsoft_teams" becomes "Microsoft Teams".
func labelFor(slug string) string {
	words := strings.Split(slug, "_")
	for i, w := range words {
		if w != "" {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

// App is one row of the Apps screen.
type App struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description"`
	LogoURL     string `json:"logoUrl"`
	// Initial and Hue are the avatar recipe the roster already uses. They are
	// sent alongside the logo, not instead of it, so an app whose logo does not
	// load degrades into a mark the client can draw rather than a grey box --
	// iOS refuses a non-HTTPS image with no error anyone can see.
	Initial string `json:"initial"`
	Hue     int    `json:"hue"`
	// Connected is reported rather than filtered out, so the client can grey a
	// card or hide it; dropping the row would take that choice away.
	Connected bool `json:"connected"`
	// ConnectionID is what disconnecting this app needs, carried on the row so
	// the screen does not have to fetch a second list to offer the button.
	ConnectionID string `json:"connectionId,omitempty"`
	// Status is the provider's own word -- ACTIVE, EXPIRED, INITIATED -- and is
	// what lets an expired connection offer Reconnect instead of reading as
	// though the app was never connected at all.
	Status string `json:"status,omitempty"`
}

// projectApps turns the catalogue and one person's connections into the rows the
// screen renders. Pure, and the order is the catalogue's.
func projectApps(toolkits []composio.Toolkit, held []composio.Connection) []App {
	out := make([]App, 0, len(toolkits))
	for _, kit := range toolkits {
		conn := connectionFor(held, kit.Slug)
		out = append(out, App{
			Slug: kit.Slug, Name: kit.Name, Description: kit.Description,
			LogoURL: kit.Logo, Initial: initialOf(kit.Name), Hue: hueOf(kit.Slug),
			Connected:    conn.Status == composio.StatusActive,
			ConnectionID: conn.ID, Status: conn.Status,
		})
	}
	return out
}

// connectionFor picks the connection that describes an app's state, or the zero
// one when this person holds none -- which projects to exactly the unconnected
// row, so there is no second return value to check.
//
// A person may hold several for one app -- an abandoned attempt beside a working
// account -- so an ACTIVE one wins over anything else. Taking the first would let
// a stale INITIATED row report a connected app as unconnected.
func connectionFor(held []composio.Connection, slug string) composio.Connection {
	var found composio.Connection
	for _, conn := range held {
		if !strings.EqualFold(conn.Toolkit, slug) {
			continue
		}
		if conn.Status == composio.StatusActive {
			return conn
		}
		found = conn
	}
	return found
}
