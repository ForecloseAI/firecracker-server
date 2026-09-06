package chat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"cracked/internal/composio"
)

// provider stands the whole provider up: the connections a person holds, the
// auth config a link needs, and a record of what was actually asked for.
type provider struct {
	mu      sync.Mutex
	held    string // the connections body
	fail    bool   // answer everything 502, for the outage path
	deleted []string
	linked  []string
}

// serve starts it and returns a server pointed at it.
//
// appsClaims MUST be initialised. serverOver and accountServer both leave it
// nil, which is safe today only because ensureApps returns early on a nil
// provider -- the moment a test wires one in, claimApps writes to a nil map and
// panics.
func (p *provider) serve(t *testing.T) (*Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(p.route))
	t.Cleanup(srv.Close)

	v, mint := testAuth(t)
	s := &Server{
		auth: v, bridges: map[string]*Bridge{}, appsClaims: map[string]appsClaim{},
		cfg: Config{Origin: "https://chat.example.com", Token: "fleet-token",
			ComposioCallback: "https://chat.example.com/v1/apps/connected"},
		composio: composio.New("k", srv.URL),
	}
	s.catalog, _ = stubCatalog()
	return s, mint(testUserID, "tester@example.com")
}

// route answers the handful of provider endpoints these tests reach.
func (p *provider) route(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.fail:
		w.WriteHeader(http.StatusBadGateway)
	case r.Method == http.MethodDelete:
		p.deleted = append(p.deleted, strings.TrimPrefix(r.URL.Path, "/connected_accounts/"))
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/connected_accounts/link":
		p.link(w, r)
	case r.URL.Path == "/tool_router/session":
		w.Write([]byte(`{"session_id":"trs_1","mcp":{"type":"streamable_http",
			"url":"https://backend.composio.dev/mcp/trs_1"}}`))
	case r.URL.Path == "/auth_configs":
		// Answers for whatever toolkit was asked for, so the catalogue check is
		// the ONLY thing standing between a slug and a minted link. A stub that
		// only knew gmail would refuse the others by itself and the test would
		// pass with the check deleted.
		fmt.Fprintf(w, `{"items":[{"id":"ac_1","toolkit":{"slug":%q}}]}`,
			r.URL.Query().Get("toolkit_slug"))
	default:
		w.Write([]byte(p.held))
	}
}

// link records which auth config a connect asked for, and answers with a page.
func (p *provider) link(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AuthConfigID string `json:"auth_config_id"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	p.linked = append(p.linked, body.AuthConfigID)
	json.NewEncoder(w).Encode(map[string]any{
		"redirect_url": "https://connect.composio.dev/link/lk_1",
		"expires_at":   "2099-01-01T00:00:00Z"})
}

// listPage reads one page of the directory, failing the test on anything else.
func listPage(t *testing.T, s *Server, tok, query string) AppPage {
	t.Helper()
	w := call(t, s, tok, "GET", "/v1/apps"+query, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got AppPage
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// The screen gets the provider's catalogue, with the connected ones marked and
// carrying the id that disconnecting them will need.
func TestListAppsMarksWhatIsConnected(t *testing.T) {
	p := &provider{held: `{"items":[
		{"id":"ca_gmail","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`}
	s, tok := p.serve(t)
	got := listPage(t, s, tok, "")
	if len(got.Items) != len(testCatalog) {
		t.Fatalf("got %d rows, want the whole catalogue", len(got.Items))
	}
	var gmail App
	for _, app := range got.Items {
		if app.Slug == "gmail" {
			gmail = app
		} else if app.Connected {
			t.Errorf("%s reads as connected", app.Slug)
		}
	}
	if !gmail.Connected || gmail.ConnectionID != "ca_gmail" {
		t.Errorf("gmail row is %+v", gmail)
	}
}

// A page is a page, and the cursor it hands back walks the rest without
// repeating a row or skipping one.
func TestTheDirectoryIsWalkedAPageAtATime(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	seen := map[string]bool{}
	query := "?limit=2"
	for pages := 0; ; pages++ {
		if pages > len(testCatalog) {
			t.Fatal("the walk did not terminate")
		}
		got := listPage(t, s, tok, query)
		for _, app := range got.Items {
			if seen[app.Slug] {
				t.Fatalf("%s appeared on two pages", app.Slug)
			}
			seen[app.Slug] = true
		}
		if got.NextCursor == "" {
			break
		}
		query = "?limit=2&cursor=" + got.NextCursor
	}
	if len(seen) != len(testCatalog) {
		t.Errorf("walked %d of %d apps", len(seen), len(testCatalog))
	}
}

// A cursor this route did not write is refused, rather than quietly restarting
// the list from the top under somebody who was halfway down it.
func TestAnUnreadableCursorIsA400(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	w := call(t, s, tok, "GET", "/v1/apps?cursor=not-a-page", "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", w.Code)
	}
}

// A client asking for the whole catalogue gets a page, because the point of
// paging a route is that its answer stays a fixed size as the provider ships.
func TestAnUnboundedLimitIsClamped(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/v1/apps?limit=100000", nil)
	if got := limitOf(req); got != appsPageMax {
		t.Errorf("limit is %d, want the cap of %d", got, appsPageMax)
	}
	for _, raw := range []string{"", "0", "-3", "many"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/apps?limit="+raw, nil)
		if got := limitOf(req); got != appsPageDefault {
			t.Errorf("limit=%q gave %d, want the default", raw, got)
		}
	}
}

// Searching happens here, so a match past the first page is still findable --
// which is the whole reason it could not stay in the client once this paged.
func TestSearchingReachesTheWholeCatalogue(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	got := listPage(t, s, tok, "?limit=1&q=asana")
	if len(got.Items) != 1 || got.Items[0].Slug != "asana" {
		t.Fatalf("got %+v", got.Items)
	}
	// The fixture is only honest if asana is genuinely not on the first
	// unfiltered page, or this passes without a search at all.
	if first := listPage(t, s, tok, "?limit=1"); first.Items[0].Slug == "asana" {
		t.Fatal("the catalogue puts the match on the first page anyway")
	}
}

// A provider having a bad minute must not end the person's session. The client
// signs out globally on any 401, so an app-level failure has to be a 502.
func TestAProviderFailureIsNeverA401(t *testing.T) {
	p := &provider{fail: true}
	s, tok := p.serve(t)
	for _, path := range []string{"/v1/apps", "/v1/apps/connections"} {
		w := call(t, s, tok, "GET", path, "")
		if w.Code == http.StatusUnauthorized {
			t.Fatalf("%s answered 401, which signs the person out of the product", path)
		}
		if w.Code != http.StatusBadGateway {
			t.Errorf("%s answered %d, want 502", path, w.Code)
		}
	}
}

// A catalogue that could not be read is a 502 and never an empty page. The
// client renders an empty list as "no apps are offered here", so answering with
// one would tell somebody their apps had disappeared.
func TestAnUnreadableCatalogIsA502NotAnEmptyPage(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	s.catalog = &appCatalog{fetch: func(context.Context) ([]composio.Toolkit, error) {
		return nil, errors.New("provider had a bad minute")
	}}
	w := call(t, s, tok, "GET", "/v1/apps", "")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status %d, want 502 -- an empty page reads as an empty product", w.Code)
	}
	// And the guards refuse rather than claiming we do not carry the app.
	w = call(t, s, tok, "POST", "/v1/apps/gmail/connect", "")
	if w.Code != http.StatusBadGateway {
		t.Errorf("connect answered %d during an outage, want 502", w.Code)
	}
}

// The connections list is strictly more than the Apps screen: an agent can
// connect any app the provider supports, and those still have to be visible.
func TestConnectionsIncludeAppsTheCatalogNeverOffers(t *testing.T) {
	p := &provider{held: `{"items":[
		{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}},
		{"id":"ca_2","status":"ACTIVE","toolkit":{"slug":"linear"}}]}`}
	s, tok := p.serve(t)
	w := call(t, s, tok, "GET", "/v1/apps/connections", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got []Connection
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got) != 2 {
		t.Fatalf("got %d, want both including the unlisted app: %+v", len(got), got)
	}
	for _, c := range got {
		if c.Slug == "linear" && c.Name != "Linear" {
			t.Errorf("an unlisted app is named %q", c.Name)
		}
	}
}

// An app the catalogue does not carry is refused before its slug reaches the
// provider, which would happily mint a link for any of the fourteen hundred it
// cannot complete a sign-in for.
//
// The auth-config stub answers for whatever it is asked, so this guard is the
// only thing that can refuse.
func TestAnAppTheCatalogueDoesNotCarryIsRefused(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	// The policy route refuses outright without a store to write to, so it needs
	// one here or it would answer 502 before reaching the guard under test.
	s.apps = &heldAppsStore{}
	for _, path := range []string{"/v1/apps/shopify/connect", "/v1/apps/shopify/policy"} {
		method, body := "POST", ""
		if strings.HasSuffix(path, "/policy") {
			method, body = "PUT", `{"capability":"write","policy":"auto"}`
		}
		if w := call(t, s, tok, method, path, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s answered %d, want 400", path, w.Code)
		}
	}
	if len(p.linked) != 0 {
		t.Errorf("a refused slug still reached the provider: %q", p.linked)
	}
}

// An app the catalogue DOES carry goes through, and this is the half that would
// pass with the guard inverted. gmail was one of the six; notion never was.
func TestAnyAppInTheCatalogueCanBeConnected(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, tok := p.serve(t)
	if w := call(t, s, tok, "POST", "/v1/apps/notion/connect", ""); w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if len(p.linked) != 1 {
		t.Fatalf("the provider was asked for %d links", len(p.linked))
	}
}

// With no provider configured the screen renders an empty state, not an error
// and not null -- a client mapping over null crashes.
func TestNoProviderIsAnEmptyShelfNotAFailure(t *testing.T) {
	v, mint := testAuth(t)
	s := &Server{auth: v, bridges: map[string]*Bridge{},
		cfg: Config{Origin: "https://chat.example.com", Token: "fleet-token"}}
	tok := mint(testUserID, "tester@example.com")
	for _, path := range []string{"/v1/apps", "/v1/apps/connections"} {
		w := call(t, s, tok, "GET", path, "")
		if w.Code != http.StatusOK {
			t.Errorf("%s answered %d", path, w.Code)
		}
		if body := w.Body.String(); body == "null\n" || body == "null" {
			t.Errorf("%s answered null rather than []", path)
		}
	}
}

// The OAuth landing page shares the /v1/apps namespace and must stay reachable
// without a token: the browser coming back from a provider carries none.
func TestTheLandingPageStaysTokenFree(t *testing.T) {
	p := &provider{held: `{"items":[]}`}
	s, _ := p.serve(t)
	req := httptest.NewRequest(http.MethodGet, connectedPath, nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the landing page answered %d without a token", rec.Code)
	}
}
