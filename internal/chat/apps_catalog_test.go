package chat

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cracked/internal/composio"
)

// kits is a catalogue answer, in the order the provider gives one.
func kits(slugs ...string) []composio.Toolkit {
	out := make([]composio.Toolkit, 0, len(slugs))
	for _, s := range slugs {
		out = append(out, composio.Toolkit{Slug: s, Name: labelFor(s), Logo: "https://l/" + s})
	}
	return out
}

// testCatalog is what the provider offers these tests. Deliberately short of
// "linear", which several tests use as an app an agent connected and the
// catalogue has never heard of.
var testCatalog = kits("gmail", "googlecalendar", "slack", "notion", "asana")

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

// stubCatalog answers with testCatalog without a provider, counting the fetches.
func stubCatalog() (*appCatalog, *atomic.Int64) {
	var calls atomic.Int64
	c := &appCatalog{fetch: func(context.Context) ([]composio.Toolkit, error) {
		calls.Add(1)
		return testCatalog, nil
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

// Every app on the page is a row whether or not it is connected. Filtering the
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

// The catalogue is fetched once and then kept, so opening the screen does not
// walk the provider's whole list per person.
func TestTheCatalogIsFetchedOncePerTTL(t *testing.T) {
	c, calls := stubCatalog()
	for range 3 {
		got, err := c.toolkits(context.Background())
		if err != nil || len(got) != len(testCatalog) {
			t.Fatalf("got %d apps, %v", len(got), err)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want once -- the cache is not holding", n)
	}
}

// The two caches keep different things on different clocks, and this is the one
// that must not follow the other.
//
// The catalogue is copy and is kept for a fortnight. A capability map is what a
// write gate is resolved against, so a tool the provider re-annotates from read
// to write has to stop being runnable-without-asking within the hour. Sharing a
// deadline in either direction is wrong: one way spends round trips on names
// that did not change, the other leaves a live machine acting on a stale
// annotation for two weeks.
func TestTheCatalogOutlivesTheCapabilityClockByFar(t *testing.T) {
	if appsCatalogTTL <= appCapsTTL {
		t.Fatalf("the catalogue is kept %v against the capability map's %v",
			appsCatalogTTL, appCapsTTL)
	}
	c, calls := stubCatalog()
	c.toolkits(context.Background())
	c.mu.Lock()
	// Wind the clock on by an hour without waiting one.
	c.expires = c.expires.Add(-appCapsTTL - time.Minute)
	c.mu.Unlock()
	calls.Store(0)
	c.toolkits(context.Background())
	if calls.Load() != 0 {
		t.Error("an hour-old catalogue was re-fetched, so it is on the capability clock")
	}
}

// A catalogue that could not be read is an ERROR, never an empty list.
//
// The six-slug version could not fail: it fetched each app on its own and named
// a missing one after its slug, so a bad minute cost a blurb. One list call has
// no such half state -- and an empty list is how "no provider is configured here"
// is spelled, so answering with one would tell somebody their apps had vanished.
func TestAFailedCatalogIsAnErrorAndIsNotCached(t *testing.T) {
	var calls atomic.Int64
	fail := true
	c := &appCatalog{fetch: func(context.Context) ([]composio.Toolkit, error) {
		calls.Add(1)
		if fail {
			return nil, errors.New("provider had a bad minute")
		}
		return testCatalog, nil
	}}
	if got, err := c.toolkits(context.Background()); err == nil {
		t.Fatalf("an outage answered with %d apps and no error", len(got))
	}

	fail = false
	calls.Store(0)
	got, err := c.toolkits(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() == 0 {
		t.Fatal("a failure was cached, so the catalogue stays empty for a fortnight")
	}
	if bySlug(got, "slack").Logo == "" {
		t.Error("the retry did not pick up the real catalogue")
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

// offers separates "we do not carry that" from "we could not find out", because
// telling somebody we have never heard of Gmail on the strength of one bad
// minute is a wrong answer rather than a slow one.
func TestOffersTellsARefusalApartFromAnOutage(t *testing.T) {
	c, _ := stubCatalog()
	if held, err := c.offers(context.Background(), "gmail"); !held || err != nil {
		t.Errorf("gmail: %v %v", held, err)
	}
	if held, err := c.offers(context.Background(), "linear"); held || err != nil {
		t.Errorf("an app the catalogue does not carry: %v %v", held, err)
	}

	down := &appCatalog{fetch: func(context.Context) ([]composio.Toolkit, error) {
		return nil, errors.New("provider had a bad minute")
	}}
	if _, err := down.offers(context.Background(), "gmail"); err == nil {
		t.Error("an outage reported as an app we do not carry")
	}
}

// A cursor goes out and comes back as the same place, and the last page carries
// none -- which is how a client knows to stop without being told a count.
func TestPagingCoversTheCatalogueWithNoOverlapOrGap(t *testing.T) {
	all := kits("a", "b", "c", "d", "e")
	var walked []string
	offset, pages := 0, 0
	for {
		page, next := pageOf(all, offset, 2)
		for _, kit := range page {
			walked = append(walked, kit.Slug)
		}
		pages++
		if next == "" {
			break
		}
		got, ok := offsetOf(next)
		if !ok {
			t.Fatalf("this route would not accept the cursor it wrote: %q", next)
		}
		offset = got
	}
	if strings.Join(walked, "") != "abcde" {
		t.Errorf("walked %q", walked)
	}
	if pages != 3 {
		t.Errorf("took %d pages over 5 rows of 2", pages)
	}
}

// Past the end is an empty page rather than a wrapped one: a client that kept a
// cursor across a shrinking catalogue must not be handed the first rows again.
func TestAPageBeyondTheEndIsEmpty(t *testing.T) {
	page, next := pageOf(kits("a", "b"), 9, 2)
	if len(page) != 0 || next != "" {
		t.Errorf("got %d rows and cursor %q", len(page), next)
	}
}

// A cursor this route did not write is refused rather than silently restarting
// from the top, which would scroll somebody back to the beginning with nothing
// to explain it.
func TestAnUnreadableCursorIsRefused(t *testing.T) {
	for _, raw := range []string{"not base64!!", "bm90LWEtbnVtYmVy", "LTE"} {
		if _, ok := offsetOf(raw); ok {
			t.Errorf("accepted %q", raw)
		}
	}
	if got, ok := offsetOf(""); !ok || got != 0 {
		t.Errorf("no cursor should be the first page, got %d %v", got, ok)
	}
}

// THE test for searching here rather than in the client. A client holding one
// page would filter only what it had loaded, so an app far down the list would
// be unfindable by typing its name -- which is exactly how somebody looks for
// one app among a hundred.
func TestSearchReachesPastTheFirstPage(t *testing.T) {
	all := append(kits("gmail", "slack"), composio.Toolkit{Slug: "wrike",
		Name: "Wrike", Description: "Work management", Categories: []string{"project management"}})
	got := matching(all, "wrike", "")
	if len(got) != 1 || got[0].Slug != "wrike" {
		t.Fatalf("got %+v", got)
	}
	// A page of one over the unfiltered list would not have reached it.
	if page, _ := pageOf(all, 0, 1); bySlug(page, "wrike").Slug != "" {
		t.Fatal("the fixture does not put the match past the first page")
	}
	if got := matching(all, "", "project management"); len(got) != 1 {
		t.Errorf("a category filter kept %d rows", len(got))
	}
	if got := matching(all, "work MANAGEMENT", ""); len(got) != 1 {
		t.Errorf("searching a description, case-insensitively, kept %d rows", len(got))
	}
	if got := matching(all, "", ""); len(got) != len(all) {
		t.Errorf("an empty search kept %d of %d rows", len(got), len(all))
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
