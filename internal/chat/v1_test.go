package chat

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"cracked/internal/agentapi"
)

// The client treats any non-2xx as failure and does not read the body, so a
// missing token must produce 401 -- never a redirect to an HTML page, which
// would surface as an unreadable error.
func TestAPIGuardAnswers401NotARedirect(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.apiGuard(func(http.ResponseWriter, *http.Request, string) {
		t.Fatal("handler ran without a session")
	})(w, httptest.NewRequest("GET", "/v1/agents", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Errorf("guard redirected to %q", loc)
	}
}

// roster is one machine's agents plus their profiles.
func roster() ([]agentapi.Status, []agentapi.Profile) {
	return []agentapi.Status{
			{ID: "boss", Name: "Boss", Type: "boss"},
			{ID: "cody", Name: "Cody", Type: "coder",
				Task: &agentapi.Task{Title: "Reconciling invoices"}},
		}, []agentapi.Profile{
			{Key: "boss", Description: "Runs the team", Browser: true},
			{Key: "coder", Description: "Writes code", Browser: false},
		}
}

// The projection is what the whole roster screen renders from.
func TestProjectRoster(t *testing.T) {
	st, profiles := roster()
	got := projectRoster(st, profiles, "alice-1", true)
	if len(got) != 2 {
		t.Fatalf("got %d agents", len(got))
	}
	boss, cody := got[0], got[1]
	if boss.Role != "Runs the team" || !boss.Capabilities.Browse {
		t.Errorf("boss = %+v", boss)
	}
	if cody.Capabilities.Browse || cody.Capabilities.Call {
		t.Error("capabilities must come from the profile, and call is never true")
	}
	if cody.Task != "Reconciling invoices" {
		t.Errorf("task = %q", cody.Task)
	}
	if boss.Task != "" {
		t.Errorf("an agent between jobs must report no task, got %q", boss.Task)
	}
	if boss.Machine != "alice-1 · Linux" {
		t.Errorf("machine = %q", boss.Machine)
	}
	if boss.Initial != "B" || boss.Shape != "diamond" {
		t.Errorf("avatar = %q/%q", boss.Initial, boss.Shape)
	}
}

// Status.Live is deliberately not the source of `online`: an idle agent is
// evicted to save memory, and reporting that as offline would show a healthy
// roster as dead.
func TestOnlineIsNotTakenFromLive(t *testing.T) {
	st, profiles := roster()
	for i := range st {
		st[i].Live = false
	}
	for _, a := range projectRoster(st, profiles, "alice-1", true) {
		if !a.Online {
			t.Errorf("%s went offline because it held no goroutine", a.ID)
		}
	}
}

// A colour that changed between two screens would read as a different agent.
func TestHueIsStableAndInRange(t *testing.T) {
	for _, id := range []string{"boss", "cody", "analyst-2"} {
		first := hueOf(id)
		if first != hueOf(id) {
			t.Fatalf("hue for %s is not stable", id)
		}
		if first < 0 || first > 359 {
			t.Errorf("hue for %s = %d, out of range", id, first)
		}
	}
}

// A non-ASCII name must not render as half a character.
func TestInitialHandlesMultibyteNames(t *testing.T) {
	for name, want := range map[string]string{"Émile": "É", "寿司": "寿", "": "?"} {
		if got := initialOf(name); got != want {
			t.Errorf("initialOf(%q) = %q, want %q", name, got, want)
		}
	}
}

// An unknown profile still needs a shape, or the client renders nothing.
func TestUnknownProfileStillGetsAShape(t *testing.T) {
	if got := shapeOf("something-new"); got != "circle" {
		t.Errorf("shape = %q, want a fallback", got)
	}
}

// stubGuest answers the two calls a roster fetch makes, and counts anything
// else so a test can prove listing never reaches the agent-starting event route.
func stubGuest(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		st, _ := roster()
		json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("GET /agent-types", func(w http.ResponseWriter, r *http.Request) {
		_, p := roster()
		json.NewEncoder(w).Encode(p)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		*hits++ // anything else would mean we took a route that starts agents
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// stubControl stands in for the control plane, reporting one VM in the given
// state and pointing at the stub guest. It repoints guestPort for the test, so
// agent.New reaches the stub rather than a real VM's 8080.
func stubControl(t *testing.T, guestURL, state string) *Control {
	t.Helper()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(guestURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	old := guestPort
	guestPort, _ = strconv.Atoi(port)
	t.Cleanup(func() { guestPort = old })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"id": "alice-1", "state": state, "guest_ip": host})
	}))
	t.Cleanup(srv.Close)
	return NewControl(srv.URL, "test-token")
}

// The whole chain, end to end: a bearer token in, the app's roster JSON out.
func TestListAgentsEndToEnd(t *testing.T) {
	hits := 0
	guest := stubGuest(t, &hits)
	v, mint := testAuth(t)
	s := &Server{control: stubControl(t, guest.URL, "running"), auth: v,
		cfg: Config{Origin: "https://chat.example.com", Token: "fleet-token"}}
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+mint(testUserID, "tester@example.com"))
	w := httptest.NewRecorder()
	// Through the real handler chain, so the token is verified the way a live
	// request would verify it rather than by a guard called in isolation.
	s.Routes().ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var got []Agent
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "boss" || got[1].Task != "Reconciling invoices" {
		t.Fatalf("roster = %+v", got)
	}
	if hits != 0 {
		t.Errorf("the roster fetch touched %d agent-starting routes", hits)
	}
}

// A machine that is not running reports an empty roster rather than an error:
// the VM is paused, not broken, and an error would render as a failed load.
func TestPausedMachineIsNotAnError(t *testing.T) {
	hits := 0
	guest := stubGuest(t, &hits)
	s := &Server{control: stubControl(t, guest.URL, "paused")}
	w := httptest.NewRecorder()
	s.listAgents(w, httptest.NewRequest("GET", "/v1/agents", nil), testUserID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// A user with no machine assigned gets an empty roster, not an error and
// certainly not an attempt to boot a VM named "".
func TestNoMachineBootsNothing(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	// A subject that is not a UUID derives no machine id at all.
	s.listAgents(w, httptest.NewRequest("GET", "/v1/agents", nil), "not-a-uuid")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("body = %s, want an empty array", got)
	}
}

// The screenshot path is joined onto a base URL that already ends in /v1, so it
// must not carry that prefix itself. It did, the client asked for /v1/v1/... and
// the handoff card rendered an empty frame with nothing reporting a problem.
func TestHandoffShotPathIsRelativeToTheApiRoot(t *testing.T) {
	m, ok := projectMessage(agentapi.Event{
		ID: 235, Agent: "boss", Type: "question", Kind: "handoff",
		Question: "take over", Shot: "handoff-235-thumb.png",
	})
	if !ok {
		t.Fatal("a handoff question did not project to a message")
	}
	if strings.HasPrefix(m.Shot, "/v1/") {
		t.Errorf("shot path %q repeats the /v1 the client already has", m.Shot)
	}
	if m.Shot != "/threads/boss/shots/handoff-235-thumb.png" {
		t.Errorf("shot path is %q", m.Shot)
	}
}
