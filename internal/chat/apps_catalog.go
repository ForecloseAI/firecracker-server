package chat

import (
	"context"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"cracked/internal/composio"
)

// featured is the apps put in front of somebody first, in the order they are
// shown. It is no longer the apps this build OFFERS -- every app the provider
// can connect is offered now, and the Apps screen browses the rest by category.
//
// So this list stopped being a gate and became a recommendation, and the two
// want different things written down. A gate wants to be short and defensible;
// a recommendation wants to be the handful of apps most people came here for.
// These six are also the ones this project has actually driven end to end, which
// is why nothing else has been promoted into it on the strength of being popular.
//
// Still the ONLY thing about an app written down here. Names, logos, blurbs and
// groupings come from the provider, because that is copy which goes stale while
// nobody notices.
var featured = []string{
	"gmail", "googlecalendar", "slack", "outlook", "microsoft_teams", "asana",
}

// appsCatalogTTL is how long the catalogue is kept.
//
// Long, because it is the same for every person on the fleet and changes about
// as often as somebody launches an integration. The cost of it being stale is a
// blurb a month old, or an app shipped this morning that a person cannot find
// until lunchtime; the cost of not caching it is a walk of the provider's whole
// catalogue on every Apps screen.
const appsCatalogTTL = time.Hour

// catalogue is one reading of the provider's catalogue: every app this build can
// put somebody through, in the provider's usage order, and the groupings they
// fall into.
//
// Held whole rather than queried per screen. Search and category filtering run
// against this copy in memory, which is what makes browsing a thousand apps cost
// nothing per keystroke -- and it means one answer decides what is connectable,
// rather than each route re-deciding it against a differently-filtered page.
//
// Immutable once built. Every field is derived in buildCatalogue and only read
// afterwards, which is what lets a reader take the pointer under the lock and
// then use it without one.
type catalogue struct {
	// rows is the connectable apps in the provider's usage order, which is the
	// order anything unfiltered is shown in.
	rows []row
	// bySlug answers "is this an app we can connect, and what is it called" for
	// the routes that are handed a slug by a client.
	bySlug map[string]composio.Toolkit
	// groups is the headings the browse screen offers, each with how many apps
	// are under it, and the few apps its preview shows. Only groupings with
	// something in them: a heading that opens onto nothing is worse than none.
	groups []heading
}

// row is one app plus the text a search is matched against.
//
// Folded once when the catalogue is built rather than per comparison. A browse
// request scans every row seven times over -- once for the page and once per
// heading preview -- and does it again on each keystroke, so lowercasing a few
// thousand names and slugs inside the loop is thousands of throwaway strings for
// a value that is fixed for the hour the catalogue is cached.
type row struct {
	kit    composio.Toolkit
	search string
}

// heading is one browse-screen grouping with the apps it leads with.
type heading struct {
	Group
	// leads are the first few apps under it, chosen once when the catalogue is
	// built. Toolkits and not rendered rows, because what a person has connected
	// is theirs and this copy is the fleet's.
	leads []composio.Toolkit
}

// Group is one heading on the browse screen.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Count is how many connectable apps are under this heading. Sent so the
	// screen can order or hide headings without fetching each one.
	Count int `json:"count"`
}

// appsCatalogRetry is how long a failed reading is remembered.
//
// The reason appsRetryAfter exists, at the other end of the same feature: with
// no cooldown, a provider having a bad ten minutes means every Apps screen,
// every browse and every connect attempt runs its own walk of up to twenty pages
// -- and connectApp consults this BEFORE the per-person mint limiter, so one
// caller looping that route turns each inbound request into twenty outbound ones.
const appsCatalogRetry = 30 * time.Second

// appCatalog keeps that reading so the screen does not re-walk it per person.
// One entry with one deadline, refreshed on read when stale -- the idiom Caps
// uses in vncgw.go, with no background goroutine.
type appCatalog struct {
	// fetch and groupings are fields so a test can answer without a provider.
	fetch     func(context.Context) ([]composio.Toolkit, error)
	groupings func(context.Context) ([]composio.Category, error)

	mu      sync.Mutex
	held    *catalogue
	expires time.Time
	// cooldown is when a failed reading stops being remembered.
	cooldown time.Time
	// walking is closed when the reading in flight finishes, and is what makes
	// the others wait for it rather than start their own.
	walking chan struct{}
}

// newAppCatalog prepares the cache. It fetches nothing until asked.
func newAppCatalog(c *composio.Client) *appCatalog {
	return &appCatalog{fetch: c.Toolkits, groupings: c.Categories}
}

// current is the catalogue, re-read when it has gone stale.
//
// It can be nil, and every caller has to say what that means for it: a screen
// falls back to the featured apps named after their own slugs, and a route handed
// a slug refuses rather than guessing.
//
// One reader at a time does the reading. Without that, the moment the hour lapses
// every request in flight starts its own walk of the whole catalogue -- twenty
// provider requests of five hundred rows each, all producing the same answer, and
// with a user-facing connect POST sitting behind one of them.
func (a *appCatalog) current(ctx context.Context) *catalogue {
	for {
		a.mu.Lock()
		if time.Now().Before(a.expires) {
			held := a.held
			a.mu.Unlock()
			return held
		}
		if time.Now().Before(a.cooldown) {
			a.mu.Unlock()
			return nil
		}
		if wait := a.walking; wait != nil {
			a.mu.Unlock()
			select {
			case <-wait:
				// Round again rather than returning what the winner got: it may
				// have failed, and the cooldown set above is what stops this
				// becoming the next attempt in a queue of them.
				continue
			case <-ctx.Done():
				return nil
			}
		}
		done := make(chan struct{})
		a.walking = done
		a.mu.Unlock()

		held := a.read(ctx)
		a.mu.Lock()
		a.walking = nil
		if held != nil {
			a.held, a.expires = held, time.Now().Add(appsCatalogTTL)
		} else {
			a.cooldown = time.Now().Add(appsCatalogRetry)
		}
		a.mu.Unlock()
		close(done)
		return held
	}
}

// read fetches and builds one reading, or nil when there is nothing usable.
//
// A reading with no connectable apps in it counts as nothing usable, and that is
// the interesting case. Two of the three reasons an app is filtered out fail
// open -- a missing no_auth or deprecated decodes as false and the app stays --
// but composio_managed_auth_schemes fails CLOSED: if the list endpoint ever stops
// carrying it, every app decodes as unconnectable and the filter empties the
// catalogue. Cached, that answer is a dead feature for an hour, fleet-wide: a
// blank Apps screen, a browse screen with nothing on it, and connectApp refusing
// every slug -- including the featured six, because the outage fallback is only
// reached when there is no catalogue at all. A catalogue that offers nobody
// anything is indistinguishable from not having one, so it is treated as not
// having one.
func (a *appCatalog) read(ctx context.Context) *catalogue {
	kits, err := a.fetch(ctx)
	if err != nil {
		// Named with its reason: this is the difference between "the provider is
		// down" and "the catalogue outgrew the walk", and only the second one
		// never heals on its own.
		log.Printf("chat: the app catalogue could not be read: %v", err)
		return nil
	}
	held := buildCatalogue(kits, a.groupingsOf(ctx, kits))
	if len(held.rows) == 0 {
		log.Printf("chat: the catalogue answered with %d apps and none can be "+
			"connected; treating it as unread", len(kits))
		return nil
	}
	return held
}

// groupingsOf is the provider's own list of headings, or the ones the catalogue
// itself implies.
//
// Two sources for one list, deliberately. The endpoint is authoritative and
// ordered, which is what a screen wants; the apps carry their own groupings, so
// a heading list can always be rebuilt from them. Falling back rather than
// failing means a browse screen that has lost its headings still lists every app
// and still searches -- which is the whole feature, minus its furniture.
func (a *appCatalog) groupingsOf(ctx context.Context, kits []composio.Toolkit) []composio.Category {
	got, err := a.groupings(ctx)
	if err == nil && len(got) > 0 {
		return got
	}
	if err != nil {
		log.Printf("chat: the app groupings could not be read: %v", err)
	}
	var out []composio.Category
	seen := map[string]bool{}
	for _, kit := range kits {
		for _, group := range kit.Categories {
			if !seen[group.ID] {
				seen[group.ID] = true
				out = append(out, group)
			}
		}
	}
	return out
}

// buildCatalogue reduces a reading of the provider's catalogue to what this
// build can offer, counts what falls under each heading, and picks the few apps
// each heading leads with.
//
// The filter is the interesting half. An app with nothing to authorise, an app
// whose credentials a project has to bring itself, and an app being withdrawn
// all end the same way: a Connect button that cannot work. Dropping them here
// rather than at the screen is what stops a client, a route and a push each
// having their own opinion about which apps exist.
//
// The headings are counted and their leads collected in the SAME pass that
// filters, because the alternative is one full scan of the catalogue per heading
// every time the browse screen opens.
func buildCatalogue(kits []composio.Toolkit, groupings []composio.Category) *catalogue {
	held := &catalogue{bySlug: make(map[string]composio.Toolkit, len(kits))}
	counts := map[string]int{}
	leads := map[string][]composio.Toolkit{}
	for _, kit := range kits {
		if !kit.Connectable() {
			continue
		}
		held.rows = append(held.rows, row{
			kit: kit, search: strings.ToLower(kit.Name + " " + kit.Slug)})
		held.bySlug[kit.Slug] = kit
		for _, group := range kit.Categories {
			counts[group.ID]++
			if len(leads[group.ID]) < sectionApps {
				leads[group.ID] = append(leads[group.ID], kit)
			}
		}
	}
	for _, group := range groupings {
		if counts[group.ID] == 0 {
			continue
		}
		held.groups = append(held.groups, heading{
			Group: Group{ID: group.ID, Name: group.Name, Count: counts[group.ID]},
			leads: leads[group.ID],
		})
	}
	return held
}

// get is one app, and whether the catalogue has it. A nil catalogue has nothing,
// which is the answer a route wants during an outage: refuse rather than guess.
func (c *catalogue) get(slug string) (composio.Toolkit, bool) {
	if c == nil {
		return composio.Toolkit{}, false
	}
	kit, ok := c.bySlug[slug]
	return kit, ok
}

// browsePage is how many apps one page of the browse screen carries. Enough that
// scrolling a category rarely needs a second request, small enough that the
// first paint of a thousand-app catalogue is not a megabyte of JSON.
const browsePage = 60

// sectionApps is how many apps a category preview shows. A handful, because it
// is a taste of what is under a heading rather than the heading's contents --
// tapping through is what shows those.
const sectionApps = 8

// sections is how many category previews the unfiltered screen leads with.
const sections = 6

// browse is one page of the catalogue, filtered as the screen asked, with where
// the next page starts and whether there is one.
//
// Ranked by the provider's usage order throughout, including inside a search:
// somebody typing "cal" wants Google Calendar above a calendar plugin for a tool
// they have never heard of, and the provider knows which is which.
//
// The bool is separate from the index rather than folded into a zero, because a
// zero is a legitimate place for a page to start and "there is nothing more" is
// not a position at all. Folding them worked only while every caller happened to
// pass a positive limit.
func (c *catalogue) browse(query, group string, from, limit int) ([]composio.Toolkit, int, bool) {
	if c == nil {
		return nil, 0, false
	}
	var out []composio.Toolkit
	for i := from; i < len(c.rows); i++ {
		if !matches(c.rows[i], query, group) {
			continue
		}
		if len(out) == limit {
			return out, i, true
		}
		out = append(out, c.rows[i].kit)
	}
	return out, 0, false
}

// matches reports whether one app belongs on a filtered page. An empty query and
// an empty group both mean "everything", so the unfiltered screen is the same
// code path rather than a second one.
//
// Both arguments arrive already lowercased, as the row's search text is, so this
// is plain equality and containment rather than a fold per comparison.
func matches(r row, query, group string) bool {
	if group != "" && !slices.ContainsFunc(r.kit.Categories,
		func(c composio.Category) bool { return c.ID == group }) {
		return false
	}
	// Name and slug only, deliberately. Searching the blurbs turns "mail" into
	// two hundred apps that mention email somewhere, which is a worse answer than
	// the dozen actually called something like it.
	return query == "" || strings.Contains(r.search, query)
}

// preview is the first few apps under each of the leading headings, which is
// what the browse screen shows before anybody searches or picks one.
//
// The rows carry this person's connections like every other row does. A preview
// that showed a connected app as unconnected would be the one screen in the
// product that lies about it, and the reader has no way to tell which of the
// two lists in front of them is the honest one.
func (c *catalogue) preview(held []composio.Connection) []Section {
	if c == nil {
		return nil
	}
	out := make([]Section, 0, min(sections, len(c.groups)))
	for _, group := range c.groups {
		if len(out) == sections {
			break
		}
		out = append(out, Section{Group: group.Group, Apps: projectApps(group.leads, held)})
	}
	return out
}

// Section is one category preview on the browse screen.
type Section struct {
	Group
	// Apps is a taste of what is under the heading, never all of it.
	Apps []App `json:"apps"`
}

// headings is the groupings the browse screen offers, without the apps each one
// leads with -- those go out only in the previews.
func (c *catalogue) headings() []Group {
	if c == nil {
		return nil
	}
	out := make([]Group, 0, len(c.groups))
	for _, group := range c.groups {
		out = append(out, group.Group)
	}
	return out
}

// toolkits is the featured apps' copy, in the order featured names them.
//
// It never fails: when the catalogue could not be read AT ALL the featured apps
// still appear, named after their own slugs, because a person who cannot see
// Slack cannot connect it either -- and a blank Apps screen during a provider
// outage is the worse of the two answers. That fallback is safe only because the
// connect route falls back to the same list.
//
// A readable catalogue that simply does not CARRY a featured app is the opposite
// case, and it drops the row. The app is missing because it was filtered out --
// withdrawn, or no longer something the provider holds credentials for -- and
// connectApp consults this same catalogue and would refuse it. Naming it after
// its slug and showing it anyway is a Connect button that can never work, on the
// screen most people only ever see.
func (a *appCatalog) toolkits(ctx context.Context) []composio.Toolkit {
	held := a.current(ctx)
	out := make([]composio.Toolkit, 0, len(featured))
	for _, slug := range featured {
		switch kit, ok := held.get(slug); {
		case ok:
			out = append(out, kit)
		case held == nil:
			out = append(out, composio.Toolkit{Slug: slug, Name: labelFor(slug)})
		default:
			// Worth saying: a featured app the catalogue has stopped carrying is
			// something to go and look at, not something to quietly drop forever.
			log.Printf("chat: %s is featured and the catalogue does not offer it", slug)
		}
	}
	return out
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
	// Categories are the provider's own groupings for this app, carried on the
	// row so a screen can group what it was given without a second lookup per
	// app. Absent for a row the catalogue could not be read for.
	Categories []composio.Category `json:"categories,omitempty"`
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
			LogoURL: kit.Logo, Categories: kit.Categories,
			Initial: initialOf(kit.Name), Hue: hueOf(kit.Slug),
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
