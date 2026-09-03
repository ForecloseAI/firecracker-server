package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
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
		// The answer moves when the provider re-annotates a tool, and now also
		// when the person changes a setting. If that counted as a new surface it
		// would evict every idle agent on the machine whenever the host restarted
		// with a fresher one -- the re-ticketing storm with a new cause. Nothing
		// an agent composed at startup depends on it.
		{"only the resolved answer moved",
			agentapi.Apps{SessionURL: a, SessionID: "s1", Actions: map[string]string{"GMAIL_FETCH_EMAILS": agentapi.ActionAuto}},
			agentapi.Apps{SessionURL: a, SessionID: "s1", Actions: map[string]string{"GMAIL_LIST_LABELS": agentapi.ActionAuto}}, false},
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
func TestTheResolvedActionsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	want := agentapi.Apps{SessionURL: "http://172.16.0.1:8092/apps/aaaa", SessionID: "s1",
		Actions: map[string]string{"GMAIL_FETCH_EMAILS": agentapi.ActionAuto,
			"SLACK_FIND_CHANNELS": agentapi.ActionAuto}}
	if err := WriteApps(dir, want); err != nil {
		t.Fatal(err)
	}
	if got := ReadApps(dir); !maps.Equal(got.Actions, want.Actions) {
		t.Errorf("read back %+v", got)
	}
}

// THE test for the push on this side. appsBodyCap was 8 KiB "because it carries
// two short strings"; the real set is ~400 slugs and 13 KB, so adding it made
// every push 400 -- and handlePutApps answers before WriteApps, so it took the
// SESSION down with it rather than just the set. A machine ended up with no
// connected apps at all, on the healthy path, self-repeating on the cooldown.
func TestARealSizedAnswerFitsThePush(t *testing.T) {
	s, _ := newTestServer(t)
	// Built from the longest real slug in the catalogue so the fixture cannot
	// flatter itself, and floor-checked below so it cannot shrink under the cap
	// it exists to test.
	//
	// Sized to the RESOLVED answer, which is every action the six apps expose --
	// 910 of them, not the ~400 the read-only set carried. Growing the payload is
	// what this whole PR does, so the fixture grows with it or it stops guarding
	// the thing it was written for.
	set := make(map[string]string, 910)
	for i := range 910 {
		set[fmt.Sprintf("MICROSOFT_TEAMS_LIST_COMMUNICATIONS_CALLS_OPERATIONS_%03d", i)] =
			agentapi.ActionAsk
	}
	body, err := json.Marshal(agentapi.Apps{SessionID: "sess_abc", Actions: set,
		SessionURL: "http://172.16.0.1:8092/apps/0123456789abcdef0123456789abcdef"})
	if err != nil {
		t.Fatal(err)
	}
	if len(body) < 32<<10 {
		t.Fatalf("fixture is %d bytes, too small to stand in for a real answer", len(body))
	}
	if rec := do(t, s, http.MethodPut, "/apps", string(body)); rec.Code != http.StatusNoContent {
		t.Fatalf("a real-sized push answered %d: %s", rec.Code, rec.Body)
	}
	var got agentapi.Apps
	if err := json.Unmarshal(do(t, s, http.MethodGet, "/apps", "").Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(got.Actions, set) || got.SessionID != "sess_abc" {
		t.Errorf("read back %d actions, session %q", len(got.Actions), got.SessionID)
	}
}

// A push installs the set into the RUNNING server, not merely onto the disk.
// Landing it on disk alone is the shape this PR exists to fix: the file was
// right and the process asked about every read regardless.
func TestAPushInstallsTheResolvedActions(t *testing.T) {
	s, _ := newTestServer(t)
	if rec := do(t, s, http.MethodPut, "/apps",
		`{"session_url":"https://backend.composio.dev/mcp/s","session_id":"s1",
		  "actions":{"GMAIL_FETCH_EMAILS":"auto"}}`); rec.Code != http.StatusNoContent {
		t.Fatalf("put status %d: %s", rec.Code, rec.Body)
	}
	if s.sup.Apps().needs("GMAIL_FETCH_EMAILS") != agentapi.ActionAuto {
		t.Error("the answer reached the disk and not the server")
	}
	if got := s.sup.Apps().needs("GMAIL_SEND_EMAIL"); got != agentapi.ActionAsk {
		t.Errorf("an action nobody pushed needs %q, not asking", got)
	}
}

// THE trap: a fresher answer on an unchanged session is the ordinary push -- and
// now also how a person's setting takes effect -- so anything that gated it on
// the URL having moved would drop exactly the update this exists to deliver.
func TestASetMovesEvenWhenTheSessionDoesNot(t *testing.T) {
	s, _ := newTestServer(t)
	const same = `"session_url":"https://backend.composio.dev/mcp/s","session_id":"s1"`
	do(t, s, http.MethodPut, "/apps", `{`+same+`,"actions":{"GMAIL_FETCH_EMAILS":"auto"}}`)
	do(t, s, http.MethodPut, "/apps", `{`+same+`,"actions":{"SLACK_FIND_CHANNELS":"auto"}}`)
	if s.sup.Apps().needs("SLACK_FIND_CHANNELS") != agentapi.ActionAuto {
		t.Error("a fresher answer on an unchanged session was dropped")
	}
	if s.sup.Apps().needs("GMAIL_FETCH_EMAILS") == agentapi.ActionAuto {
		t.Error("the replaced set is still in force")
	}
}

// A machine that restarts must come back knowing what it may do without asking,
// or it asks about every read until its next push.
func TestARestartComesBackWithItsAnswer(t *testing.T) {
	dir := t.TempDir()
	if err := WriteApps(dir, agentapi.Apps{SessionURL: "http://172.16.0.1:8092/apps/a",
		SessionID: "s1", Actions: map[string]string{"GMAIL_FETCH_EMAILS": agentapi.ActionAuto}}); err != nil {
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
	if sup.Apps().needs("GMAIL_FETCH_EMAILS") != agentapi.ActionAuto {
		t.Error("a restarted machine forgot what it may do without asking")
	}
	if sup.Apps().Current() != "http://172.16.0.1:8092/apps/a" {
		t.Errorf("it also forgot its session: %q", sup.Apps().Current())
	}
}
