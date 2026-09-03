package chat

import (
	"encoding/json"
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
		// Answers for whatever toolkit was asked for, so the featured check is
		// the ONLY thing standing between a slug and a minted link. A stub that
		// only knew gmail would refuse the others by itself and the test would
		// pass with the check deleted.
		fmt.Fprintf(w, `{"items":[{"id":"ac_1","toolkit":{"slug":%q},
			"is_composio_managed":true,"status":"ENABLED"}]}`,
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

// The screen gets every featured app, with the connected ones marked and
// carrying the id that disconnecting them will need.
func TestListAppsMarksWhatIsConnected(t *testing.T) {
	p := &provider{held: `{"items":[
		{"id":"ca_gmail","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`}
	s, tok := p.serve(t)
	w := call(t, s, tok, "GET", "/v1/apps", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got []App
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != len(featured) {
		t.Fatalf("got %d rows, want every featured app", len(got))
	}
	var gmail App
	for _, app := range got {
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
