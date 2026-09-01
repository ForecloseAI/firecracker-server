package composio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// No key means no client, which is what turns the whole feature off without
// anybody having to remember a flag.
func TestNoKeyMeansNoClient(t *testing.T) {
	if New("", "") != nil {
		t.Fatal("a client was built with no key")
	}
}

// The session body is a set of security decisions, not defaults to inherit, and
// every one of these was verified against the live API. A prompt injection that
// could disconnect someone's apps; a remote shell the guest has no use for and
// which is ON unless the block is present; and the execute switch that has to be
// on or the whole surface is inert.
func TestSessionRequestPinsTheDangerousOptionsShut(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{
			"session_id": "sess_1", "mcp": map[string]string{"type": "http", "url": "https://x/mcp"}})
	}))
	defer srv.Close()

	if _, err := New("k", srv.URL).NewSession(context.Background(), "u", "https://back"); err != nil {
		t.Fatal(err)
	}
	manage, _ := got["manage_connections"].(map[string]any)
	if manage["enable_connection_removal"] != false {
		t.Error("a model could disconnect someone's apps")
	}
	if manage["callback_url"] != "https://back" {
		t.Errorf("callback_url is %v", manage["callback_url"])
	}
	// ON, and this is the one that reads backwards until you have seen the live
	// API: enable_multi_execute is not batched-versus-single, it is the ONLY
	// execute path. False makes the session unable to act at all.
	if ex, _ := got["execute"].(map[string]any); ex["enable_multi_execute"] != true {
		t.Error("the session was given no way to execute anything")
	}
	// Explicitly false, because OMITTING the block turns the workbench on and
	// brings a remote shell with it.
	if wb, ok := got["workbench"].(map[string]any); !ok || wb["enable"] != false {
		t.Error("a remote shell was left on")
	}
}

// The person's id travels as given. The machine id is the same UUID with its
// hyphens stripped, so sending the wrong one would isolate someone from their
// own connections without erroring.
func TestUserIDTravelsUnchanged(t *testing.T) {
	const sub = "3f8a1c92-5e4b-4d1a-9c77-0b2e5a6f8d31"
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if key := r.Header.Get("x-api-key"); key != "k" {
			t.Errorf("x-api-key is %q", key)
		}
		json.NewDecoder(r.Body).Decode(&got)
		json.NewEncoder(w).Encode(map[string]any{
			"session_id": "s", "mcp": map[string]string{"url": "https://x/mcp"}})
	}))
	defer srv.Close()

	sess, err := New("k", srv.URL).NewSession(context.Background(), sub, "")
	if err != nil {
		t.Fatal(err)
	}
	if got["user_id"] != sub {
		t.Errorf("user_id is %v", got["user_id"])
	}
	if sess.ID != "s" || sess.URL != "https://x/mcp" {
		t.Errorf("session is %+v", sess)
	}
}

// A session with no endpoint is an error, not a session. Storing one would hand
// every machine a URL it can never dial and report nothing.
func TestASessionWithNoEndpointIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"session_id": "sess_1"})
	}))
	defer srv.Close()
	if _, err := New("k", srv.URL).NewSession(context.Background(), "u", ""); err == nil {
		t.Fatal("a session with no mcp url was accepted")
	}
}

// A refusal names the status, and never the key: this error travels into logs
// the operator console renders.
func TestAnErrorCarriesTheStatusAndNotTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := New("sk-secret-value", srv.URL).NewSession(context.Background(), "u", "")
	if err == nil {
		t.Fatal("a 403 was accepted")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the error does not name the status: %v", err)
	}
	if strings.Contains(err.Error(), "sk-secret-value") {
		t.Errorf("the error leaked the key: %v", err)
	}
}

// THE test for the connections API. `user_id` singular is not rejected by the
// provider, it is IGNORED -- it answers with other people's accounts in the
// list. A caller that revoked what that returned would hand back strangers'
// grants, so the plural is asserted on the QUERY STRING, not on the result.
func TestConnectionsFiltersByUserIdsPlural(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode(map[string]any{"items": []any{
			map[string]any{"id": "ca_1", "status": "ACTIVE",
				"toolkit": map[string]string{"slug": "gmail"}}}})
	}))
	defer srv.Close()

	held, err := New("k", srv.URL).Connections(context.Background(), "user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "user_ids=user-1") {
		t.Errorf("query was %q, which must carry user_ids= and not user_id=", gotQuery)
	}
	if strings.Contains(strings.ReplaceAll(gotQuery, "user_ids", ""), "user_id") {
		t.Errorf("query %q uses the singular form, which the provider ignores", gotQuery)
	}
	want := []Connection{{ID: "ca_1", Toolkit: "gmail", Status: "ACTIVE"}}
	if len(held) != 1 || held[0] != want[0] {
		t.Errorf("got %+v", held)
	}
}

// A person with more accounts than fit in one page still has all of them
// revoked; stopping at page one would leave live grants behind.
func TestConnectionsFollowsTheCursor(t *testing.T) {
	pages := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		body := map[string]any{"items": []any{
			map[string]any{"id": "ca_" + r.URL.Query().Get("cursor"), "status": "ACTIVE",
				"toolkit": map[string]string{"slug": "slack"}}}}
		if pages < 3 {
			body["next_cursor"] = "p" + string(rune('0'+pages))
		}
		json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	held, err := New("k", srv.URL).Connections(context.Background(), "u")
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 3 {
		t.Errorf("walked %d pages and collected %d accounts", pages, len(held))
	}
}

// A DELETE answers with no body at all. Decoding one is an EOF, which would
// report every successful revoke as a failure -- and account deletion refuses to
// proceed on a failed revoke, so that would make erasing an account impossible.
func TestDisconnectToleratesAnEmptyBody(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := New("k", srv.URL).Disconnect(context.Background(), "ca_1"); err != nil {
		t.Fatalf("a 204 was read as a failure: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/connected_accounts/ca_1" {
		t.Errorf("sent %s %s", gotMethod, gotPath)
	}
}

// A toolkit's copy comes from the provider, so nothing about an app is written
// down here to go stale.
func TestToolkitReadsTheProvidersOwnCopy(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Write([]byte(`{"slug":"gmail","name":"Gmail","meta":{
			"logo":"https://logos.composio.dev/api/gmail","description":"Google's email service"}}`))
	}))
	defer srv.Close()

	got, err := New("k", srv.URL).Toolkit(context.Background(), "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/toolkits/gmail" {
		t.Errorf("asked for %q", gotPath)
	}
	want := Toolkit{Slug: "gmail", Name: "Gmail",
		Logo: "https://logos.composio.dev/api/gmail", Description: "Google's email service"}
	if got != want {
		t.Errorf("got %+v", got)
	}
}

// THE test for this pair. toolkit_slug is SINGULAR here, and the plural form is
// not rejected -- it is ignored, answering with every config in the project. A
// caller that trusted the first row back would mint a link against whichever app
// happened to sort first, and connect the wrong one to somebody's account.
//
// It is the reverse of connected_accounts, where the plural user_ids is live and
// the singular is ignored. Neither is guessable, so both are pinned here.
func TestAuthConfigLooksUpByTheSingularSlug(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth_configs" && r.Method == http.MethodGet:
			gotQuery = r.URL.RawQuery
			w.Write([]byte(`{"items":[{"id":"ac_1","toolkit":{"slug":"gmail"}}]}`))
		default:
			json.NewEncoder(w).Encode(map[string]any{
				"redirect_url": "https://connect.composio.dev/link/lk_1",
				"expires_at":   "2099-01-01T00:00:00Z"})
		}
	}))
	defer srv.Close()

	if _, err := New("k", srv.URL).Link(context.Background(), "u", "gmail", "https://back"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "toolkit_slug=gmail") {
		t.Errorf("query was %q, which must carry the singular toolkit_slug", gotQuery)
	}
	if strings.Contains(gotQuery, "toolkit_slugs") {
		t.Errorf("query %q uses the plural, which the provider ignores", gotQuery)
	}
}

// A config the lookup did not find is created, because the provider will not
// infer one from the toolkit and the link call refuses to run without an id.
func TestAuthConfigIsCreatedWhenTheAppHasNoneYet(t *testing.T) {
	var created string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/auth_configs" && r.Method == http.MethodGet:
			w.Write([]byte(`{"items":[]}`)) // nobody has connected this app yet
		case r.URL.Path == "/auth_configs" && r.Method == http.MethodPost:
			var body struct {
				Toolkit struct {
					Slug string `json:"slug"`
				} `json:"toolkit"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			created = body.Toolkit.Slug
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"auth_config":{"id":"ac_new"}}`))
		default:
			var body linkReq
			json.NewDecoder(r.Body).Decode(&body)
			if body.AuthConfigID != "ac_new" {
				t.Errorf("the link used auth_config_id %q", body.AuthConfigID)
			}
			json.NewEncoder(w).Encode(map[string]any{
				"redirect_url": "https://connect.composio.dev/link/lk_2",
				"expires_at":   "2099-01-01T00:00:00Z"})
		}
	}))
	defer srv.Close()

	got, err := New("k", srv.URL).Link(context.Background(), "u", "asana", "https://back")
	if err != nil {
		t.Fatal(err)
	}
	if created != "asana" {
		t.Errorf("created a config for %q", created)
	}
	if got.URL != "https://connect.composio.dev/link/lk_2" {
		t.Errorf("link url is %q", got.URL)
	}
	if got.ExpiresAt.IsZero() {
		t.Error("the deadline was dropped, so nobody can tell a stale link from a fresh one")
	}
}

// A link with no url is an error, not a link. Handing one to a screen would
// render a button that goes nowhere.
func TestALinkWithNoURLIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth_configs" && r.Method == http.MethodGet {
			w.Write([]byte(`{"items":[{"id":"ac_1","toolkit":{"slug":"gmail"}}]}`))
			return
		}
		w.Write([]byte(`{"expires_at":"2099-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()
	if _, err := New("k", srv.URL).Link(context.Background(), "u", "gmail", ""); err == nil {
		t.Fatal("a link with no url was accepted")
	}
}
