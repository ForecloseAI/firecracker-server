package chat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"cracked/internal/composio"
)

// appsServer wires a gateway whose provider is a stub, and whose catalogue
// answers without reaching anything.
//
// appsClaims MUST be initialised. serverOver and accountServer both leave it
// nil, which is safe today only because ensureApps returns early on a nil
// provider -- the moment a test wires one in, claimApps writes to a nil map and
// panics.
func appsServer(t *testing.T, connections string, connFail bool) (*Server, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if connFail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(connections))
	}))
	t.Cleanup(srv.Close)

	v, mint := testAuth(t)
	s := &Server{
		auth: v, bridges: map[string]*Bridge{}, appsClaims: map[string]appsClaim{},
		cfg:      Config{Origin: "https://chat.example.com", Token: "fleet-token"},
		composio: composio.New("k", srv.URL),
	}
	s.catalog = &appCatalog{fetch: func(_ context.Context, slug string) (composio.Toolkit, error) {
		return composio.Toolkit{Slug: slug, Name: labelFor(slug), Logo: "https://l/" + slug}, nil
	}}
	return s, mint(testUserID, "tester@example.com")
}

// The screen gets every featured app, with the connected ones marked and
// carrying the id that disconnecting them will need.
func TestListAppsMarksWhatIsConnected(t *testing.T) {
	s, tok := appsServer(t, `{"items":[
		{"id":"ca_gmail","status":"ACTIVE","toolkit":{"slug":"gmail"}}]}`, false)
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
	s, tok := appsServer(t, "", true)
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
	s, tok := appsServer(t, `{"items":[
		{"id":"ca_1","status":"ACTIVE","toolkit":{"slug":"gmail"}},
		{"id":"ca_2","status":"ACTIVE","toolkit":{"slug":"linear"}}]}`, false)
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
	s, _ := appsServer(t, `{"items":[]}`, false)
	req := httptest.NewRequest(http.MethodGet, connectedPath, nil)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("the landing page answered %d without a token", rec.Code)
	}
}
