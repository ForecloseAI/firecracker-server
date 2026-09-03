package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
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
		// The set moves when the provider re-annotates a tool. If that counted as
		// a new surface it would evict every idle agent on the machine whenever
		// the host restarted with a fresher set -- the re-ticketing storm with a
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
	if got := ReadApps(dir); !slices.Equal(got.ReadOnly, want.ReadOnly) {
		t.Errorf("read back %+v", got)
	}
}

// THE test for the push on this side. appsBodyCap was 8 KiB "because it carries
// two short strings"; the real set is ~400 slugs and 13 KB, so adding it made
// every push 400 -- and handlePutApps answers before WriteApps, so it took the
// SESSION down with it rather than just the set. A machine ended up with no
// connected apps at all, on the healthy path, self-repeating on the cooldown.
func TestARealSizedReadOnlySetFitsThePush(t *testing.T) {
	s, _ := newTestServer(t)
	// Built from the longest real slug in the catalogue so the fixture cannot
	// flatter itself, and floor-checked below so it cannot shrink under the cap
	// it exists to test.
	set := make([]string, 400)
	for i := range set {
		set[i] = fmt.Sprintf("MICROSOFT_TEAMS_LIST_COMMUNICATIONS_CALLS_OPERATIONS_%03d", i)
	}
	body, err := json.Marshal(agentapi.Apps{SessionID: "sess_abc", ReadOnly: set,
		SessionURL: "http://172.16.0.1:8092/apps/0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 8<<10 {
		t.Fatalf("fixture is %d bytes, too small to have caught the old cap", len(body))
	}
	if rec := do(t, s, http.MethodPut, "/apps", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("a real-sized push answered %d: %s", rec.Code, rec.Body)
	}
	var got agentapi.Apps
	if err := json.Unmarshal(do(t, s, http.MethodGet, "/apps", "").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.ReadOnly, set) || got.SessionID != "sess_abc" {
		t.Errorf("read back %d slugs, session %q", len(got.ReadOnly), got.SessionID)
	}
}

// A push installs the set into the RUNNING server, not merely onto the disk.
// Landing it on disk alone is the shape this PR exists to fix: the file was
// right and the process asked about every read regardless.
func TestAPushInstallsTheReadOnlySet(t *testing.T) {
	s, _ := newTestServer(t)
	if rec := do(t, s, http.MethodPut, "/apps",
		`{"session_url":"https://backend.composio.dev/mcp/s","session_id":"s1",
		  "read_only":["GMAIL_FETCH_EMAILS"]}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put status %d: %s", rec.Code, rec.Body)
	}
	if !s.sup.Apps().reading("GMAIL_FETCH_EMAILS") {
		t.Error("the set reached the disk and not the server")
	}
	if s.sup.Apps().reading("GMAIL_SEND_EMAIL") {
		t.Error("an action nobody pushed reads as safe")
	}
}

// THE trap. SetURL returns early when the session has not moved, and a re-push
// carrying a fresher set on the SAME session is the ordinary case -- it is what
// surfaceChanged is pinned to allow. Installed from SetURL, this update would be
// the one that got dropped.
func TestASetMovesEvenWhenTheSessionDoesNot(t *testing.T) {
	s, _ := newTestServer(t)
	const same = `"session_url":"https://backend.composio.dev/mcp/s","session_id":"s1"`
	do(t, s, http.MethodPut, "/apps", `{`+same+`,"read_only":["GMAIL_FETCH_EMAILS"]}`)
	do(t, s, http.MethodPut, "/apps", `{`+same+`,"read_only":["SLACK_FIND_CHANNELS"]}`)
	if !s.sup.Apps().reading("SLACK_FIND_CHANNELS") {
		t.Error("a fresher set on an unchanged session was dropped")
	}
	if s.sup.Apps().reading("GMAIL_FETCH_EMAILS") {
		t.Error("the replaced set is still in force")
	}
}

// A machine that restarts must come back knowing what only reads, or it asks
// about every read until its next push.
func TestARestartComesBackWithItsSet(t *testing.T) {
	dir := t.TempDir()
	if err := WriteApps(dir, agentapi.Apps{SessionURL: "http://172.16.0.1:8092/apps/a",
		SessionID: "s1", ReadOnly: []string{"GMAIL_FETCH_EMAILS"}}); err != nil {
		t.Fatal(err)
	}
	// Through NewSupervisor, not newAppsServer: the bug this guards is the boot
	// path taking .SessionURL and dropping the rest, which a constructor test
	// cannot see because the constructor was never the thing that was wrong.
	sup, err := NewSupervisor(context.Background(), dir, t.TempDir(),
		testCatalog(t), "claude-haiku-4-5", 8)
	if err != nil {
		t.Fatal(err)
	}
	if !sup.Apps().reading("GMAIL_FETCH_EMAILS") {
		t.Error("a restarted machine forgot what only reads")
	}
	if sup.Apps().Current() != "http://172.16.0.1:8092/apps/a" {
		t.Errorf("it also forgot its session: %q", sup.Apps().Current())
	}
}
