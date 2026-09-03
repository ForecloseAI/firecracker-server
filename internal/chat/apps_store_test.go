package chat

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cracked/internal/agentapi"
)

// withTestToken is a context carrying a caller's access token, as the logging
// middleware would have put there.
func withTestToken(raw string) context.Context {
	return context.WithValue(context.Background(), tokenKey{}, raw)
}

// THE test for this file. The project key identifies the project and the
// PERSON'S token is what makes auth.uid() them -- so a key in the bearer slot
// would silently drop the request to anonymous, and the policy would hide every
// row rather than fail in a way anyone would notice.
func TestTheTwoCredentialsGoInTheirOwnSlots(t *testing.T) {
	var apikey, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apikey, auth = r.Header.Get("apikey"), r.Header.Get("Authorization")
		w.Write([]byte("[]"))
	}))
	defer srv.Close()

	if _, err := newPGApps(srv.URL, "sb_publishable_x").
		Get(withTestToken("the-users-jwt"), "u"); err != nil {
		t.Fatal(err)
	}
	if apikey != "sb_publishable_x" {
		t.Errorf("apikey header is %q", apikey)
	}
	if auth != "Bearer the-users-jwt" {
		t.Errorf("Authorization is %q", auth)
	}
}

// A request with no caller cannot be made as anybody, so it is refused rather
// than sent -- an unauthenticated PostgREST call would come back as an empty
// result, which reads exactly like "this person has no session".
func TestAStoreCallWithNoCallerIsRefused(t *testing.T) {
	store := newPGApps("https://project.supabase.co", "sb_publishable_x")
	if _, err := store.Get(context.Background(), "u"); err == nil {
		t.Fatal("a call with no token was attempted")
	}
}

// No row is not an error. It means "mint one", never "this person does not
// exist" -- which is what keeps someone signing in for the first time working.
func TestNoRowIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("[]"))
	}))
	defer srv.Close()
	got, err := newPGApps(srv.URL, "k").Get(withTestToken("t"), "u")
	if err != nil {
		t.Fatalf("an absent row errored: %v", err)
	}
	if got.SessionURL != "" {
		t.Errorf("something came back: %+v", got)
	}
}

// A stored session comes back in the shape the guest is handed.
func TestGetReadsTheRowBack(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		json.NewEncoder(w).Encode([]map[string]any{{
			"session_id": "sess_9", "mcp_url": "https://backend.composio.dev/mcp/9",
			"policy": map[string]map[string]string{"gmail": {"write": "never"}}}})
	}))
	defer srv.Close()
	got, err := newPGApps(srv.URL, "k").Get(withTestToken("t"), "u")
	if err != nil {
		t.Fatal(err)
	}
	// Field by field: Apps carries two pushed-only fields the store never
	// persists, so the struct is not comparable and == would not mean what it
	// looks like it means.
	if got.SessionURL != "https://backend.composio.dev/mcp/9" || got.SessionID != "sess_9" {
		t.Errorf("got %+v", got)
	}
	if got.ReadOnly != nil {
		t.Errorf("the store invented a read-only set: %v", got.ReadOnly)
	}
	if got.Actions != nil {
		t.Errorf("the store invented resolved actions: %v", got.Actions)
	}
	// Asserted on the QUERY, not only the answer: a fake that ignores select=
	// hands back a policy either way, so dropping the column from the list would
	// pass here and return everyone to defaults against a real PostgREST.
	if !strings.Contains(gotQuery, "policy") {
		t.Errorf("select does not ask for the policy: %q", gotQuery)
	}
	if got.Policy["gmail"]["write"] != "never" {
		t.Errorf("the policy did not come back: %v", got.Policy)
	}
}

// One row per person, so a second mint replaces the first rather than colliding.
func TestPutUpserts(t *testing.T) {
	var prefer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefer = r.Header.Get("Prefer")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	err := newPGApps(srv.URL, "k").Put(withTestToken("t"), "u", agentapi.Apps{SessionURL: "https://x"})
	if err != nil {
		t.Fatal(err)
	}
	if want := "resolution=merge-duplicates"; !strings.Contains(prefer, want) {
		t.Errorf("Prefer is %q, want it to carry %q", prefer, want)
	}
}

// The policy travels in the row Put writes. The upsert this rides on updates the
// columns the payload names, so a Put that omitted it would leave the column to
// behaviour worth not depending on -- and being wrong there returns somebody to
// defaults they had changed, on the next session re-mint, silently.
func TestPutCarriesThePolicy(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	err := newPGApps(srv.URL, "k").Put(withTestToken("t"), "u", agentapi.Apps{
		SessionURL: "https://backend.composio.dev/mcp/9", SessionID: "sess_9",
		Policy: map[string]map[string]string{"gmail": {"write": "never"}}})
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("body %q: %v", body, err)
	}
	got, _ := json.Marshal(rows[0]["policy"])
	if string(got) != `{"gmail":{"write":"never"}}` {
		t.Errorf("the row went without its policy: %s", body)
	}
}
