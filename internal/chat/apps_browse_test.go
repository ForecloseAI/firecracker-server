package chat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"testing"

	"cracked/internal/composio"
)

// browsing stands a server up over a catalogue of this build's own choosing,
// which is what the browse screen is about: the featured six are a rounding
// error in it.
func browsing(t *testing.T, held []composio.Toolkit) (*Server, string) {
	t.Helper()
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	s.catalog, _ = catalogOf(held)
	return s, tok
}

// catalog reads the browse screen, failing the test if it did not answer.
func catalog(t *testing.T, s *Server, tok, query string) Catalog {
	t.Helper()
	w := call(t, s, tok, "GET", "/v1/apps/catalog"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got Catalog
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %s: %v", w.Body, err)
	}
	return got
}

// THE test for opening the gate. Every app the provider carries and this build
// can put somebody through is reachable, not the six on the front screen.
func TestBrowsingReachesEveryAppTheProviderCarries(t *testing.T) {
	var held []composio.Toolkit
	for i := range browsePage * 3 {
		held = append(held, kits("app"+strconv.Itoa(i))...)
	}
	s, tok := browsing(t, held)

	var seen int
	cursor := ""
	for page := range 5 {
		got := catalog(t, s, tok, "?cursor="+cursor)
		seen += len(got.Apps)
		if got.NextCursor == "" {
			if seen != len(held) {
				t.Fatalf("the walk ended after %d of %d apps", seen, len(held))
			}
			return
		}
		if got.NextCursor == cursor {
			t.Fatalf("page %d handed back the cursor it was given", page)
		}
		cursor = got.NextCursor
	}
	t.Fatalf("the catalogue did not end: %d apps in five pages", seen)
}

// An app the provider carries but nobody can be put through never appears, and
// cannot be connected either. Three separate reasons, all ending as a Connect
// button that cannot work -- so each is pinned rather than left to whichever the
// filter happens to catch first.
func TestAnAppWeCannotConnectIsNeitherListedNorConnectable(t *testing.T) {
	held := kits("gmail")
	held = append(held,
		composio.Toolkit{Slug: "webscraper", Name: "Web Scraper", NoAuth: true},
		composio.Toolkit{Slug: "byo", Name: "Bring Your Own"},
		composio.Toolkit{Slug: "oldapp", Name: "Old App", ManagedAuth: true, Deprecated: true})
	s, tok := browsing(t, held)

	got := catalog(t, s, tok, "")
	if len(got.Apps) != 1 || got.Apps[0].Slug != "gmail" {
		t.Fatalf("listed %+v, want only the app we can actually offer", got.Apps)
	}
	for _, slug := range []string{"webscraper", "byo", "oldapp"} {
		if w := call(t, s, tok, "POST", "/v1/apps/"+slug+"/connect", ""); w.Code == http.StatusCreated {
			t.Errorf("%s was connected, and its auth config is now permanent", slug)
		}
	}
}

// Searching and picking a heading both narrow the same list, and both keep the
// provider's usage order -- somebody typing "cal" wants the calendar people
// actually use above a plugin for a tool they have never heard of.
func TestSearchAndCategoryNarrowTheSameList(t *testing.T) {
	s, tok := browsing(t, []composio.Toolkit{
		grouped("googlecalendar", "productivity"),
		grouped("calendly", "scheduling"),
		grouped("salesforce", "crm"),
	})

	got := catalog(t, s, tok, "?q=cal")
	if len(got.Apps) != 2 || got.Apps[0].Slug != "googlecalendar" {
		t.Errorf("searching gave %+v", got.Apps)
	}
	got = catalog(t, s, tok, "?category=crm")
	if len(got.Apps) != 1 || got.Apps[0].Slug != "salesforce" {
		t.Errorf("the heading gave %+v", got.Apps)
	}
	// Case is the screen's business, not the person's: a heading id arrives from
	// our own JSON, but a query is typed.
	if got := catalog(t, s, tok, "?q=CAL"); len(got.Apps) != 2 {
		t.Errorf("a capitalised search gave %d apps", len(got.Apps))
	}
}

// The screen opens on a taste of a few headings and then gets out of the way.
// Once somebody has searched or chosen, previews of other headings underneath
// the answer are furniture in front of it.
func TestThePreviewsAreOnlyOnTheScreenAsItOpens(t *testing.T) {
	s, tok := browsing(t, []composio.Toolkit{
		grouped("gmail", "productivity"), grouped("salesforce", "crm"),
	})

	got := catalog(t, s, tok, "")
	if len(got.Sections) != 2 {
		t.Fatalf("opened with %d previews, want one per heading", len(got.Sections))
	}
	if len(got.Groups) != 2 || got.Groups[0].Count != 1 {
		t.Errorf("headings are %+v, want each counted", got.Groups)
	}
	if len(got.Sections[0].Apps) == 0 {
		t.Error("a preview with nothing under it is a heading nobody can use")
	}
	if got := catalog(t, s, tok, "?q=gmail"); got.Sections != nil {
		t.Errorf("a search still carried %d previews", len(got.Sections))
	}
}

// A heading with nothing under it is worse than no heading: it opens onto an
// empty screen and reads as an outage.
func TestAHeadingWithNothingUnderItIsNotOffered(t *testing.T) {
	c, _ := catalogOf([]composio.Toolkit{grouped("gmail", "productivity")})
	c.groupings = func(context.Context) ([]composio.Category, error) {
		return []composio.Category{
			{ID: "productivity", Name: "Productivity"}, {ID: "empty", Name: "Empty"}}, nil
	}
	held := c.current(context.Background())
	if len(held.groups) != 1 || held.groups[0].ID != "productivity" {
		t.Errorf("headings are %+v", held.groups)
	}
}

// The headings have two sources on purpose. The endpoint is authoritative and
// ordered; the apps carry their own, so the list can always be rebuilt. Falling
// back rather than failing leaves a browse screen that lists and searches every
// app and has merely lost its furniture.
func TestHeadingsAreRebuiltFromTheAppsWhenTheProviderWillNotList(t *testing.T) {
	c, _ := catalogOf([]composio.Toolkit{grouped("gmail", "productivity")})
	c.groupings = func(context.Context) ([]composio.Category, error) {
		return nil, errors.New("provider had a bad minute")
	}
	held := c.current(context.Background())
	if len(held.groups) != 1 || held.groups[0].Name != "Productivity" {
		t.Errorf("headings are %+v, want them rebuilt from the apps", held.groups)
	}
	if len(held.kits) != 1 {
		t.Error("the apps went missing with the headings")
	}
}

// A catalogue that could not be read is a 502 rather than an empty one: empty
// reads as "there are no other apps", which is a wrong answer somebody would
// believe and stop looking at.
func TestABrowseScreenWithNoCatalogueSaysSoRatherThanShowingNothing(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	s.catalog = &appCatalog{
		fetch: func(context.Context) ([]composio.Toolkit, error) {
			return nil, errors.New("provider had a bad minute")
		},
		groupings: func(context.Context) ([]composio.Category, error) { return nil, nil },
	}
	if w := call(t, s, tok, "GET", "/v1/apps/catalog", ""); w.Code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502: %s", w.Code, w.Body)
	}
}

// During a catalogue outage the gate falls back to exactly what it was before
// the catalogue existed: six slugs written down here, every one already driven
// end to end and already holding a config anywhere this has been used. So the
// fallback cannot create anything new, and somebody who cannot reach the
// provider's catalogue can still connect the apps most people came for.
func TestAnOutageFallsBackToTheGateThisRouteUsedToHave(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	s.catalog = &appCatalog{
		fetch: func(context.Context) ([]composio.Toolkit, error) {
			return nil, errors.New("provider had a bad minute")
		},
		groupings: func(context.Context) ([]composio.Category, error) { return nil, nil },
	}
	if w := call(t, s, tok, "POST", "/v1/apps/gmail/connect", ""); w.Code != http.StatusCreated {
		t.Errorf("a featured app was refused during an outage: %d %s", w.Code, w.Body)
	}
	if w := call(t, s, tok, "POST", "/v1/apps/salesforce/connect", ""); w.Code == http.StatusCreated {
		t.Error("an app nobody has vouched for was connected on a blind fallback")
	}
}

// Minting is bounded per person, because each mint may leave an auth config
// behind that is project-wide, permanent and counted against this project's
// plan. The old allowlist bounded that to six slugs by accident; nothing else
// does now.
func TestMintingConnectLinksIsBoundedPerPerson(t *testing.T) {
	s, tok := browsing(t, kits(featured...))
	for i := range connectBurst {
		if w := call(t, s, tok, "POST", "/v1/apps/gmail/connect", ""); w.Code != http.StatusCreated {
			t.Fatalf("mint %d was refused: %d %s", i, w.Code, w.Body)
		}
	}
	w := call(t, s, tok, "POST", "/v1/apps/gmail/connect", "")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d, want 429 once the burst is spent", w.Code)
	}
	// 429 and not a refusal about the app: nothing is wrong with what they asked
	// for, and a client told "we do not have that app" would stop offering it.
	if w.Code == http.StatusBadRequest {
		t.Error("a rate limit was reported as an app we do not carry")
	}
}
