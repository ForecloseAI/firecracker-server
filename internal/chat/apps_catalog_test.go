package chat

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cracked/internal/composio"
)

// kits is a catalogue answer: apps this build could actually offer, which means
// the provider holds credentials for each of them.
func kits(slugs ...string) []composio.Toolkit {
	out := make([]composio.Toolkit, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, composio.Toolkit{Slug: s, Name: labelFor(s),
			Logo: "https://l/" + s, ManagedAuth: true})
	}
	return out
}

// grouped is one app filed under one heading.
func grouped(slug, group string) composio.Toolkit {
	kit := kits(slug)[0]
	kit.Categories = []composio.Category{{ID: group, Name: labelFor(group)}}
	return kit
}

// bySlug is one app's row, or a zero row.
//
// Looked up rather than filtered in place. "If this row is slack, check its
// name" asserts nothing at all when the row has lost its slug -- the condition
// simply stops matching, and the test goes green on a catalogue missing the app
// it was written to defend.
func bySlug(kits []composio.Toolkit, slug string) composio.Toolkit {
	for _, kit := range kits {
		if kit.Slug == slug {
			return kit
		}
	}
	return composio.Toolkit{}
}

// stubCatalog answers with the featured apps and nothing else, counting the
// walks of the catalogue.
func stubCatalog() (*appCatalog, *atomic.Int64) {
	return catalogOf(kits(featured...))
}

// catalogOf answers with one fixed catalogue, counting the walks.
func catalogOf(held []composio.Toolkit) (*appCatalog, *atomic.Int64) {
	var calls atomic.Int64
	c := &appCatalog{
		fetch: func(context.Context) ([]composio.Toolkit, error) {
			calls.Add(1)
			return held, nil
		},
		groupings: func(context.Context) ([]composio.Category, error) { return nil, nil },
	}
	return c, &calls
}

// THE test for the projection. A person may hold several connections for one
// app -- an abandoned attempt beside a working account -- and taking the first
// would report a connected Gmail as unconnected.
func TestAnActiveConnectionWinsOverAnAbandonedOne(t *testing.T) {
	// One on either side of the working one, so neither "take the first" nor
	// "take the last" passes this by accident.
	held := []composio.Connection{
		{ID: "ca_1", Toolkit: "gmail", Status: "INITIATED"},
		{ID: "ca_2", Toolkit: "gmail", Status: composio.StatusActive},
		{ID: "ca_3", Toolkit: "gmail", Status: "EXPIRED"},
	}
	got := projectApps(kits("gmail"), held)
	if len(got) != 1 {
		t.Fatalf("got %d rows", len(got))
	}
	if !got[0].Connected || got[0].Status != composio.StatusActive {
		t.Errorf("row is %+v, want connected and ACTIVE", got[0])
	}
}

// An expired connection is reported as itself rather than as absent, so the
// screen can offer Reconnect instead of pretending it was never connected.
func TestAnExpiredConnectionKeepsItsStatus(t *testing.T) {
	got := projectApps(kits("slack"),
		[]composio.Connection{{ID: "ca_1", Toolkit: "slack", Status: "EXPIRED"}})
	if got[0].Connected {
		t.Error("an expired connection was reported as working")
	}
	if got[0].Status != "EXPIRED" {
		t.Errorf("status is %q, so the screen cannot tell it apart from never connected", got[0].Status)
	}
}

// Every featured app is a row whether or not it is connected. Filtering the
// connected ones out would take that choice away from the client.
func TestUnconnectedAppsAreStillRows(t *testing.T) {
	got := projectApps(kits("gmail", "asana"), nil)
	if len(got) != 2 {
		t.Fatalf("got %d rows, want both", len(got))
	}
	for _, app := range got {
		if app.Connected || app.Status != "" {
			t.Errorf("%s reads as connected: %+v", app.Slug, app)
		}
		if app.Initial == "" {
			t.Errorf("%s has no avatar fallback, so a blocked logo renders as nothing", app.Slug)
		}
	}
}

// The list is never null, because a screen mapping over it would crash.
func TestTheProjectionIsNeverNull(t *testing.T) {
	if got := projectApps(nil, nil); got == nil {
		t.Fatal("an empty catalogue projected to null rather than []")
	}
}

// The catalogue is walked once and then kept, so opening the screen does not
// cost a walk of a thousand apps per person.
func TestTheCatalogIsFetchedOncePerTTL(t *testing.T) {
	c, calls := stubCatalog()
	for range 3 {
		if got := c.toolkits(context.Background()); len(got) != len(featured) {
			t.Fatalf("got %d apps, want %d", len(got), len(featured))
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("walked it %d times, want once -- the cache is not holding", n)
	}
}

// An app whose copy could not be read still appears, named after its own slug:
// somebody who cannot see Slack on the list cannot connect it either, and a
// blank Apps screen during a provider outage is the worse of the two answers.
// Nothing is cached, so a bad minute is not kept for an hour.
func TestAFailedFetchStillLeavesTheAppOnTheList(t *testing.T) {
	var calls atomic.Int64
	fail := true
	c := &appCatalog{
		fetch: func(context.Context) ([]composio.Toolkit, error) {
			calls.Add(1)
			if fail {
				return nil, errors.New("provider had a bad minute")
			}
			return kits(featured...), nil
		},
		groupings: func(context.Context) ([]composio.Category, error) { return nil, nil },
	}
	got := c.toolkits(context.Background())
	if len(got) != len(featured) {
		t.Fatalf("got %d apps, want all %d", len(got), len(featured))
	}
	if slack := bySlug(got, "slack"); slack.Name != "Slack" {
		t.Errorf("the unreadable app is %+v, want it named after its own slug", slack)
	}

	// A failed walk is held on a short cooldown rather than retried per request.
	// Without it a provider having a bad ten minutes means every Apps screen,
	// every browse and every connect attempt runs its own walk of up to twenty
	// pages -- and connectApp reaches this BEFORE the per-person mint limiter.
	fail = false
	calls.Store(0)
	c.toolkits(context.Background())
	if calls.Load() != 0 {
		t.Error("a failed walk was retried on the very next request")
	}

	// Nothing was CACHED though, so once the cooldown is up the next reader gets
	// the real copy rather than an hour of slug-named placeholders.
	c.mu.Lock()
	c.cooldown = time.Now().Add(-time.Second)
	c.mu.Unlock()
	got = c.toolkits(context.Background())
	if calls.Load() == 0 {
		t.Fatal("a failed walk was cached for an hour")
	}
	if slack := bySlug(got, "slack"); slack.Logo == "" {
		t.Errorf("the retry did not pick up the real copy: %+v", slack)
	}
}

// A catalogue that answers with apps and offers none of them is treated as no
// catalogue at all.
//
// Two of the three filters fail open -- a missing no_auth or deprecated decodes
// as false and the app stays -- but managed auth fails CLOSED. If the list
// endpoint stops carrying composio_managed_auth_schemes, every app reads as
// unconnectable. Cached, that is a dead feature for an hour fleet-wide: a blank
// Apps screen, an empty browse, and connectApp refusing every slug including the
// featured six, because their fallback only fires when there is NO catalogue.
func TestACatalogueThatOffersNothingIsTreatedAsUnread(t *testing.T) {
	var held []composio.Toolkit
	for _, kit := range kits(featured...) {
		kit.ManagedAuth = false // the field the provider stopped sending
		held = append(held, kit)
	}
	c, _ := catalogOf(held)

	if got := c.current(context.Background()); got != nil {
		t.Fatalf("a catalogue offering nothing was kept: %+v", got)
	}
	// Which is what keeps the featured six reachable: the connect route's outage
	// fallback is reached only when current() answers with nothing.
	got := c.toolkits(context.Background())
	if len(got) != len(featured) {
		t.Errorf("the Apps screen went blank: %d rows", len(got))
	}
}

// A stale copy is refreshed rather than served forever.
func TestAStaleCatalogIsRefetched(t *testing.T) {
	c, calls := stubCatalog()
	c.toolkits(context.Background())
	c.mu.Lock()
	c.expires = time.Now().Add(-time.Second)
	c.mu.Unlock()
	calls.Store(0)
	c.toolkits(context.Background())
	if calls.Load() == 0 {
		t.Error("a stale catalogue was served without refreshing")
	}
}

// "microsoft_teams" has to read as an app name, not a database key.
func TestSlugsFallBackToAReadableName(t *testing.T) {
	for slug, want := range map[string]string{
		"microsoft_teams": "Microsoft Teams", "gmail": "Gmail", "": "",
	} {
		if got := labelFor(slug); got != want {
			t.Errorf("labelFor(%q) = %q, want %q", slug, got, want)
		}
	}
}

// One reader walks the catalogue; the rest wait for it.
//
// Without this, the moment the hour lapses every request in flight starts its
// own walk of the whole thing -- up to twenty provider requests of five hundred
// rows each, all producing the same answer -- and a user-facing connect POST
// sits behind one of them.
func TestOnlyOneReaderWalksTheCatalogue(t *testing.T) {
	var walks atomic.Int64
	release := make(chan struct{})
	c := &appCatalog{
		fetch: func(context.Context) ([]composio.Toolkit, error) {
			walks.Add(1)
			<-release // hold the walk open while the others pile up
			return kits(featured...), nil
		},
		groupings: func(context.Context) ([]composio.Category, error) { return nil, nil },
	}

	var wg sync.WaitGroup
	got := make([]*catalogue, 16)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = c.current(context.Background())
		}()
	}
	// Let them all reach the cache before the walk finishes.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := walks.Load(); n != 1 {
		t.Errorf("%d readers each walked the whole catalogue", n)
	}
	for i, held := range got {
		if held == nil || len(held.rows) != len(featured) {
			t.Fatalf("reader %d got %+v, want the answer the winner produced", i, held)
		}
	}
}
