package agentd

import (
	"encoding/json"
	"net/http"
	"testing"

	"cracked/internal/agentapi"
)

// A machine nobody has pushed to reports no session, and that is not an error --
// it is the ordinary state of every VM until the host gets to it.
func TestAppsStartsEmpty(t *testing.T) {
	s, _ := newTestServer(t)
	rec := do(t, s, http.MethodGet, "/apps", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	var got agentapi.Apps
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionURL != "" {
		t.Errorf("a fresh machine already had a session: %q", got.SessionURL)
	}
}

// What the host pushes is what the machine reads back, and what its agents dial.
func TestAppsRoundTripsAndPointsTheServer(t *testing.T) {
	s, _ := newTestServer(t)
	const url = "https://backend.composio.dev/mcp/sess_abc"
	if rec := do(t, s, http.MethodPut, "/apps",
		`{"session_url":"`+url+`","session_id":"sess_abc"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put status %d: %s", rec.Code, rec.Body)
	}
	rec := do(t, s, http.MethodGet, "/apps", "")
	var got agentapi.Apps
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.SessionURL != url || got.SessionID != "sess_abc" {
		t.Errorf("read back %+v", got)
	}
	if s.sup.Apps().Current() != url {
		t.Errorf("the server is pointed at %q", s.sup.Apps().Current())
	}
}

// A url we could not dial safely is refused rather than stored. This call is the
// only way a session reaches a machine, so accepting a bad one with a 204 would
// leave the host believing the agents could reach the person's apps.
func TestAppsRefusesAUrlItCannotDial(t *testing.T) {
	s, _ := newTestServer(t)
	for _, bad := range []string{"http://composio.dev/mcp", "http://93.184.216.34/mcp",
		"ftp://x/y", "not a url at all", "/mcp"} {
		rec := do(t, s, http.MethodPut, "/apps", `{"session_url":"`+bad+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%q was accepted with %d", bad, rec.Code)
		}
	}
	if s.sup.Apps().Current() != "" {
		t.Errorf("a refused url still landed: %q", s.sup.Apps().Current())
	}
}

// The broker the host runs is reached over plain http at a tap-local address.
// Refusing that would mean shipping a certificate into every guest image to
// protect a link that never leaves the box.
func TestAppsAcceptsThePrivateBroker(t *testing.T) {
	s, _ := newTestServer(t)
	const broker = "http://172.16.0.1:8092/apps/2f6c9c0f1d8e4a7b"
	if rec := do(t, s, http.MethodPut, "/apps", `{"session_url":"`+broker+`"}`); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if s.sup.Apps().Current() != broker {
		t.Errorf("the broker url did not land: %q", s.sup.Apps().Current())
	}
}

// Empty is allowed and meaningful: it is how a machine has the surface taken
// away again when the host stops configuring a provider.
func TestAppsAcceptsAnEmptySessionAsRemoval(t *testing.T) {
	s, _ := newTestServer(t)
	do(t, s, http.MethodPut, "/apps", `{"session_url":"https://backend.composio.dev/mcp/s"}`)
	if rec := do(t, s, http.MethodPut, "/apps", `{"session_url":""}`); rec.Code != http.StatusNoContent {
		t.Fatalf("status %d", rec.Code)
	}
	if s.sup.Apps().Current() != "" {
		t.Errorf("the session survived removal: %q", s.sup.Apps().Current())
	}
}

// THE test for this file now. The host mints a fresh ticket on every push and
// pushes on every restart, so comparing URLs meant one deploy tore down every
// machine's session and rebuilt every idle agent on it -- each one's next turn
// paying a prompt-cache write -- for a session that had not moved at all.
func TestARemintedTicketForTheSameSessionIsNotASurfaceChange(t *testing.T) {
	const a = "http://172.16.0.1:8092/apps/1111111111111111"
	const b = "http://172.16.0.1:8092/apps/2222222222222222"
	cases := []struct {
		name     string
		had, now agentapi.Apps
		want     bool
	}{
		{"same session, fresh ticket",
			agentapi.Apps{SessionURL: a, SessionID: "s1"},
			agentapi.Apps{SessionURL: b, SessionID: "s1"}, false},
		{"identical push",
			agentapi.Apps{SessionURL: a, SessionID: "s1"},
			agentapi.Apps{SessionURL: a, SessionID: "s1"}, false},
		{"a different session",
			agentapi.Apps{SessionURL: a, SessionID: "s1"},
			agentapi.Apps{SessionURL: b, SessionID: "s2"}, true},
		{"the surface appears",
			agentapi.Apps{},
			agentapi.Apps{SessionURL: a, SessionID: "s1"}, true},
		{"the surface is taken away",
			agentapi.Apps{SessionURL: a, SessionID: "s1"},
			agentapi.Apps{}, true},
		// The read-only set is refreshed hourly and pushed on every restart. If
		// that counted as a new surface it would evict every idle agent on the
		// fleet whenever the provider shipped a tool -- the same storm, with a
		// new cause. Nothing an agent composed at startup depends on it.
		{"only the read-only set moved",
			agentapi.Apps{SessionURL: a, SessionID: "s1", ReadOnly: []string{"GMAIL_FETCH_EMAILS"}},
			agentapi.Apps{SessionURL: a, SessionID: "s1", ReadOnly: []string{"GMAIL_LIST_LABELS"}}, false},
	}
	for _, c := range cases {
		if got := surfaceChanged(c.had, c.now); got != c.want {
			t.Errorf("%s: surfaceChanged = %v, want %v", c.name, got, c.want)
		}
	}
}

// Re-pointing still happens on every push, whether or not agents are rebuilt --
// otherwise the machine would keep dialling a ticket the host has forgotten.
func TestARemintedTicketStillRepointsTheServer(t *testing.T) {
	s, _ := newTestServer(t)
	const first = "http://172.16.0.1:8092/apps/1111111111111111"
	const second = "http://172.16.0.1:8092/apps/2222222222222222"
	do(t, s, http.MethodPut, "/apps", `{"session_url":"`+first+`","session_id":"s1"}`)
	do(t, s, http.MethodPut, "/apps", `{"session_url":"`+second+`","session_id":"s1"}`)
	if got := s.sup.Apps().Current(); got != second {
		t.Errorf("the machine is still dialling %q", got)
	}
}

// The read-only set has to survive the disk, because it is what the gate reads
// and a machine that restarts must not come back asking about every read.
func TestTheReadOnlySetSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	want := agentapi.Apps{SessionURL: "http://172.16.0.1:8092/apps/aaaa", SessionID: "s1",
		ReadOnly: []string{"GMAIL_FETCH_EMAILS", "SLACK_FIND_CHANNELS"}}
	if err := WriteApps(dir, want); err != nil {
		t.Fatal(err)
	}
	got := ReadApps(dir)
	if len(got.ReadOnly) != 2 || got.ReadOnly[0] != "GMAIL_FETCH_EMAILS" {
		t.Errorf("read back %+v", got)
	}
}

// An empty set must round-trip as empty rather than as absent-and-therefore-
// anything. omitempty drops it from the JSON, so this pins that reading a file
// without the field gives nothing rather than a surprise.
func TestNoReadOnlySetReadsBackAsNone(t *testing.T) {
	dir := t.TempDir()
	if err := WriteApps(dir, agentapi.Apps{SessionURL: "http://x/apps/a", SessionID: "s1"}); err != nil {
		t.Fatal(err)
	}
	if got := ReadApps(dir); len(got.ReadOnly) != 0 {
		t.Errorf("invented %v", got.ReadOnly)
	}
}
