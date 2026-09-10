package chat

import (
	"context"
	"encoding/base64"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cracked/internal/composio"
)

// appsCatalogTTL is how long the provider's catalogue is kept.
//
// Fifteen days, and the length is the point: this is the provider's whole list
// of apps, identical for every person on the fleet, and it moves when somebody
// rebrands or a new integration ships. The cost of it being a fortnight stale is
// an app that appears late; the cost of a short clock is two round trips in
// front of a screen for an answer that did not change.
//
// Worth being plain about what it buys, because it is less than it looks: the
// cache is process memory with nothing behind it, and cracked-chat restarts on
// every deploy, so in practice this is TWO REQUESTS PER PROCESS and the deadline
// only matters to a host left running for a fortnight.
const appsCatalogTTL = 15 * 24 * time.Hour

// appCatalog keeps the provider's catalogue so the screen does not re-fetch it
// per person. One entry with one deadline, refreshed on read when stale -- the
// idiom Caps uses in vncgw.go, with no background goroutine.
type appCatalog struct {
	// fetch is a field so a test can answer without a provider.
	fetch func(context.Context) ([]composio.Toolkit, error)

	mu      sync.Mutex
	held    []composio.Toolkit
	expires time.Time
}

// newAppCatalog prepares the cache. It fetches nothing until asked.
func newAppCatalog(c *composio.Client) *appCatalog {
	return &appCatalog{fetch: c.Toolkits}
}

// toolkits returns every app this build can connect, refreshing the copy when it
// has gone stale.
//
// It CAN fail now, where the six-slug version could not: that one fetched each
// app on its own and named a missing one after its slug, so a bad minute cost a
// blurb. One list call has no such half state, and a caller that turned the
// failure into an empty catalogue would tell somebody their apps had vanished.
func (a *appCatalog) toolkits(ctx context.Context) ([]composio.Toolkit, error) {
	if held := a.fresh(); held != nil {
		return held, nil
	}
	got, err := a.fetch(ctx)
	if err != nil {
		return nil, err
	}
	a.keep(got)
	return got, nil
}

// offers reports whether the catalogue holds this app, or why it could not say.
// The two answers are kept apart so a provider outage is not reported to
// somebody as an app we do not carry.
func (a *appCatalog) offers(ctx context.Context, slug string) (bool, error) {
	held, err := a.toolkits(ctx)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(held,
		func(kit composio.Toolkit) bool { return kit.Slug == slug }), nil
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

// keep stores the catalogue and starts its clock.
func (a *appCatalog) keep(held []composio.Toolkit) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.held, a.expires = held, time.Now().Add(appsCatalogTTL)
}

// Page sizes for the directory. A client asking for everything gets a page.
const (
	appsPageDefault = 50
	appsPageMax     = 200
)

// matching narrows the catalogue to what the directory asked for.
//
// Searched HERE rather than in the client, and that is the other half of paging
// this route rather than scope beside it: a client holding one page of fifty
// would otherwise filter only what it happened to have loaded, and an app at
// rank ninety would be unfindable by typing its name. Costs no provider call --
// this is the cached slice.
func matching(kits []composio.Toolkit, query, category string) []composio.Toolkit {
	query = strings.ToLower(strings.TrimSpace(query))
	out := make([]composio.Toolkit, 0, len(kits))
	for _, kit := range kits {
		if category != "" && !slices.Contains(kit.Categories, category) {
			continue
		}
		if query != "" && !strings.Contains(searchable(kit), query) {
			continue
		}
		out = append(out, kit)
	}
	return out
}

// searchable is everything about an app a person might type. The slug is in it
// because that is what an agent's connect card names, and somebody reading one
// will type what they saw.
func searchable(kit composio.Toolkit) string {
	return strings.ToLower(strings.Join(
		append([]string{kit.Slug, kit.Name, kit.Description}, kit.Categories...), " "))
}

// pageOf cuts one page out of the catalogue and says where the next begins.
//
// The cursor is an offset, and that is sound here in a way it would not be over
// a live query: this is one cached snapshot with a fifteen-day deadline, so a
// page boundary cannot slide under somebody mid-scroll.
func pageOf(kits []composio.Toolkit, offset, limit int) ([]composio.Toolkit, string) {
	if offset >= len(kits) {
		return nil, ""
	}
	end := min(offset+limit, len(kits))
	if end == len(kits) {
		return kits[offset:end], ""
	}
	return kits[offset:end], cursorOf(end)
}

// cursorOf hides an offset behind a string, so a client passes back what it was
// given rather than doing arithmetic this route would then have to keep true.
func cursorOf(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(offset)))
}

// offsetOf reads a cursor, refusing one this route did not write. A cursor that
// cannot be read is a 400 rather than a silent restart from the top, which would
// scroll somebody back to the beginning with no error to explain it.
func offsetOf(cursor string) (int, bool) {
	if cursor == "" {
		return 0, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	offset, err := strconv.Atoi(string(raw))
	return offset, err == nil && offset >= 0
}

// labelFor is the best name a slug alone can give, for an app the catalogue does
// not describe: "microsoft_teams" becomes "Microsoft Teams".
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
	// Categories is the provider's own grouping, so the directory's filters need
	// no table of our own. Free-form and lowercase, and there are around forty of
	// them, so a screen showing all as chips would show a wall of them.
	Categories []string `json:"categories,omitempty"`
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

// AppPage is one page of the directory. An object rather than a bare list
// because the catalogue grows on the PROVIDER's release schedule, and a route
// whose answer does that is one that has to be changed under pressure later.
type AppPage struct {
	Items []App `json:"items"`
	// NextCursor is absent on the last page, which is how a client knows to stop
	// rather than by comparing a count it would have to be told separately.
	NextCursor string `json:"nextCursor,omitempty"`
}

// projectApps turns the catalogue and one person's connections into the rows the
// screen renders. Pure, and the order is the catalogue's -- which is the
// provider's own, by popularity.
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
