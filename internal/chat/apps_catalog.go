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
type catalogue struct {
	// kits is the connectable apps in the provider's usage order, which is the
	// order anything unfiltered is shown in.
	kits []composio.Toolkit
	// bySlug answers "is this an app we can connect, and what is it called" for
	// the routes that are handed a slug by a client.
	bySlug map[string]composio.Toolkit
	// groups is the headings the browse screen offers, each with how many apps
	// are under it. Only groupings with something in them: a heading that opens
	// onto nothing is worse than no heading.
	groups []Group
}

// Group is one heading on the browse screen.
type Group struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Count is how many connectable apps are under this heading. Sent so the
	// screen can order or hide headings without fetching each one.
	Count int `json:"count"`
}

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
}

// newAppCatalog prepares the cache. It fetches nothing until asked.
func newAppCatalog(c *composio.Client) *appCatalog {
	return &appCatalog{fetch: c.Toolkits, groupings: c.Categories}
}

// current is the catalogue, refetched when it has gone stale.
//
// It can be nil, and every caller has to say what that means for it: a screen
// falls back to the featured apps named after their own slugs, and a route
// handed a slug refuses rather than guessing. Only a complete reading is cached,
// so a bad minute is not kept for an hour.
func (a *appCatalog) current(ctx context.Context) *catalogue {
	if held := a.fresh(); held != nil {
		return held
	}
	kits, err := a.fetch(ctx)
	if err != nil {
		// Named with its reason: this is the difference between "the provider is
		// down" and "the catalogue outgrew the walk", and only the second one
		// never heals on its own.
		log.Printf("chat: the app catalogue could not be read: %v", err)
		return nil
	}
	held := buildCatalogue(kits, a.groupingsOf(ctx, kits))
	a.keep(held)
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
// build can offer, and counts what falls under each heading.
//
// The filter is the interesting half. An app with nothing to authorise, an app
// whose credentials a project has to bring itself, and an app being withdrawn
// all end the same way: a Connect button that cannot work. Dropping them here
// rather than at the screen is what stops a client, a route and a push each
// having their own opinion about which apps exist.
func buildCatalogue(kits []composio.Toolkit, groupings []composio.Category) *catalogue {
	held := &catalogue{bySlug: make(map[string]composio.Toolkit, len(kits))}
	counts := map[string]int{}
	for _, kit := range kits {
		if !kit.Connectable() {
			continue
		}
		held.kits = append(held.kits, kit)
		held.bySlug[kit.Slug] = kit
		for _, group := range kit.Categories {
			counts[group.ID]++
		}
	}
	for _, group := range groupings {
		if counts[group.ID] > 0 {
			held.groups = append(held.groups, Group{
				ID: group.ID, Name: group.Name, Count: counts[group.ID]})
		}
	}
	return held
}

// fresh returns the cached catalogue while it is still good.
func (a *appCatalog) fresh() *catalogue {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Now().Before(a.expires) {
		return a.held
	}
	return nil
}

// keep stores a complete reading and starts its clock.
func (a *appCatalog) keep(held *catalogue) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held, a.expires = held, time.Now().Add(appsCatalogTTL)
}

// toolkits is the featured apps' copy, in the order featured names them.
//
// It never fails: an app the catalogue could not be read for still appears,
// named after its own slug, because a person who cannot see Slack cannot connect
// it either -- and the Apps screen going blank during a provider outage is a
// worse answer than six rows with plain names on them.
func (a *appCatalog) toolkits(ctx context.Context) []composio.Toolkit {
	held := a.current(ctx)
	out := make([]composio.Toolkit, 0, len(featured))
	for _, slug := range featured {
		kit, ok := held.get(slug)
		if !ok {
			kit = composio.Toolkit{Slug: slug, Name: labelFor(slug)}
		}
		out = append(out, kit)
	}
	return out
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

// browse is one page of the catalogue, filtered as the screen asked.
//
// Ranked by the provider's usage order throughout, including inside a search:
// somebody typing "cal" wants Google Calendar above a calendar plugin for a tool
// they have never heard of, and the provider knows which is which.
func (c *catalogue) browse(query, group string, from, limit int) ([]composio.Toolkit, int) {
	if c == nil {
		return nil, 0
	}
	var out []composio.Toolkit
	for i, kit := range c.kits {
		if i < from || !matches(kit, query, group) {
			continue
		}
		if len(out) == limit {
			return out, i // more to come, and where to pick it up
		}
		out = append(out, kit)
	}
	return out, 0
}

// matches reports whether one app belongs on a filtered page. An empty query and
// an empty group both mean "everything", so the unfiltered screen is the same
// code path rather than a second one.
func matches(kit composio.Toolkit, query, group string) bool {
	if group != "" && !slices.ContainsFunc(kit.Categories,
		func(c composio.Category) bool { return strings.EqualFold(c.ID, group) }) {
		return false
	}
	if query == "" {
		return true
	}
	// Name and slug only, deliberately. Searching the blurbs turns "mail" into
	// two hundred apps that mention email somewhere, which is a worse answer than
	// the dozen actually called something like it.
	return strings.Contains(strings.ToLower(kit.Name), query) ||
		strings.Contains(strings.ToLower(kit.Slug), query)
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
		kits, _ := c.browse("", group.ID, 0, sectionApps)
		out = append(out, Section{Group: group, Apps: projectApps(kits, held)})
	}
	return out
}

// Section is one category preview on the browse screen.
type Section struct {
	Group
	// Apps is a taste of what is under the heading, never all of it.
	Apps []App `json:"apps"`
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
