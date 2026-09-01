package chat

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"cracked/internal/composio"
)

// kits is a catalogue answer for the featured apps.
func kits(slugs ...string) []composio.Toolkit {
	out := make([]composio.Toolkit, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, composio.Toolkit{Slug: s, Name: labelFor(s), Logo: "https://l/" + s})
	}
	return out
}

// stubCatalog answers for any slug without a provider, counting the fetches.
func stubCatalog() (*appCatalog, *atomic.Int64) {
	var calls atomic.Int64
	c := &appCatalog{fetch: func(_ context.Context, slug string) (composio.Toolkit, error) {
		calls.Add(1)
		return composio.Toolkit{Slug: slug, Name: labelFor(slug), Logo: "https://l/" + slug}, nil
	}}
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

// The copy is fetched once and then kept, so opening the screen does not cost
// six round trips per person.
func TestTheCatalogIsFetchedOncePerTTL(t *testing.T) {
	c, calls := stubCatalog()
	for range 3 {
		if got := c.toolkits(context.Background()); len(got) != len(featured) {
			t.Fatalf("got %d apps, want %d", len(got), len(featured))
		}
	}
	if n := calls.Load(); n != int64(len(featured)) {
		t.Errorf("fetched %d times, want %d -- the cache is not holding", n, len(featured))
	}
}

// An app whose metadata could not be read still appears, named after its own
// slug: somebody who cannot see Slack on the list cannot connect it either.
// And a partial answer is NOT cached, so a bad minute is not kept for an hour.
func TestAFailedFetchStillLeavesTheAppOnTheList(t *testing.T) {
	var calls atomic.Int64
	fail := true
	c := &appCatalog{fetch: func(_ context.Context, slug string) (composio.Toolkit, error) {
		calls.Add(1)
		if fail && slug == "slack" {
			return composio.Toolkit{}, errors.New("provider had a bad minute")
		}
		return composio.Toolkit{Slug: slug, Name: labelFor(slug), Logo: "https://l/" + slug}, nil
	}}
	got := c.toolkits(context.Background())
	if len(got) != len(featured) {
		t.Fatalf("got %d apps, want all %d", len(got), len(featured))
	}
	for _, kit := range got {
		if kit.Slug == "slack" && kit.Name != "Slack" {
			t.Errorf("the unreadable app is named %q", kit.Name)
		}
	}
	// Nothing was cached, so the next open tries again and gets the real copy.
	fail = false
	calls.Store(0)
	got = c.toolkits(context.Background())
	if calls.Load() == 0 {
		t.Fatal("a partial answer was cached for an hour")
	}
	for _, kit := range got {
		if kit.Slug == "slack" && kit.Logo == "" {
			t.Error("the retry did not pick up the real copy")
		}
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
