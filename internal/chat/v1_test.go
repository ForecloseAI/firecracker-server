package chat

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"cracked/internal/agentapi"
)

// testUser swaps the hardcoded user list for one known login and restores it.
func testUser(t *testing.T) user {
	t.Helper()
	u := user{"tester@example.com", "pw", "tok_test_1234", "alice-1"}
	old := users
	users = []user{u}
	t.Cleanup(func() { users = old })
	return u
}

// The app replays a bearer token, the stream can only use a query param, and the
// built-in page only has a cookie. All three have to reach the same user.
func TestEveryDoorResolvesTheSameUser(t *testing.T) {
	u := testUser(t)
	bearer := httptest.NewRequest("GET", "/v1/agents", nil)
	bearer.Header.Set("Authorization", "Bearer "+u.Token)
	query := httptest.NewRequest("GET", "/v1/stream?token="+u.Token, nil)
	cookie := httptest.NewRequest("GET", "/chat", nil)
	cookie.AddCookie(&http.Cookie{Name: sessionCookie, Value: u.Token})
	for name, r := range map[string]*http.Request{
		"bearer": bearer, "query": query, "cookie": cookie} {
		if got, ok := userFor(r); !ok || got != u.Email {
			t.Errorf("%s resolved to (%q, %v)", name, got, ok)
		}
	}
}

// A request with no credentials must not match. This is the one worth pinning:
// the lookup compares against every user, so an empty token must never be
// treated as equal to anything.
func TestBadTokensAreRefused(t *testing.T) {
	testUser(t)
	for _, tok := range []string{"", "tok_wrong", "tok_test_123"} {
		r := httptest.NewRequest("GET", "/v1/agents", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		if _, ok := userFor(r); ok {
			t.Errorf("token %q was accepted", tok)
		}
	}
}

// login matches on both fields, not either.
func TestLoginNeedsBothFields(t *testing.T) {
	u := testUser(t)
	if _, ok := login(u.Email, "wrong"); ok {
		t.Error("a wrong password was accepted")
	}
	if _, ok := login("someone@else.com", u.Password); ok {
		t.Error("an unknown email was accepted")
	}
	if tok, ok := login(u.Email, u.Password); !ok || tok != u.Token {
		t.Errorf("login = (%q, %v)", tok, ok)
	}
}

// The client treats any non-2xx as failure and does not read the body, so a
// missing token must produce 401 -- never the web page's 302 to an HTML login,
// which would surface as an unreadable error.
func TestAPIGuardAnswers401NotARedirect(t *testing.T) {
	testUser(t)
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

// Sign-in returns the token the app stores; every later call replays it.
func TestSignInReturnsAToken(t *testing.T) {
	u := testUser(t)
	s := &Server{}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"email":"tester@example.com","password":"pw"}`)
	s.signIn(w, httptest.NewRequest("POST", "/v1/auth/sign-in", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var got sessionResp
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Token != u.Token || got.Email != u.Email {
		t.Fatalf("session = %+v", got)
	}
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+got.Token)
	if _, ok := userFor(r); !ok {
		t.Error("the returned token does not authenticate")
	}
}

// A wrong password must not mint a token.
func TestSignInRejectsABadPassword(t *testing.T) {
	testUser(t)
	s := &Server{}
	w := httptest.NewRecorder()
	body := strings.NewReader(`{"email":"tester@example.com","password":"wrong"}`)
	s.signIn(w, httptest.NewRequest("POST", "/v1/auth/sign-in", body))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// 204 is load-bearing: the client calls res.json() on any other 2xx and would
// throw on an empty body.
func TestSignOutReturns204WithNoBody(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	s.signOut(w, httptest.NewRequest("POST", "/v1/auth/sign-out", nil), "tester@example.com")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
}

// stubGuest answers the two calls a roster fetch makes, and counts them so a
// test can prove listing never reaches the agent-starting event route.
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
	u := testUser(t)
	s := &Server{control: stubControl(t, guest.URL, "running")}
	r := httptest.NewRequest("GET", "/v1/agents", nil)
	r.Header.Set("Authorization", "Bearer "+u.Token)
	_ = u.Machine
	w := httptest.NewRecorder()
	s.apiGuard(s.listAgents)(w, r)

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
	testUser(t)
	s := &Server{control: stubControl(t, guest.URL, "paused")}
	w := httptest.NewRecorder()
	s.listAgents(w, httptest.NewRequest("GET", "/v1/agents", nil), "tester@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

// A user with no machine assigned gets an empty roster, not an error and
// certainly not an attempt to boot a VM named "".
func TestNoMachineBootsNothing(t *testing.T) {
	old := users
	users = []user{{"nomachine@example.com", "pw", "tok_nm", ""}}
	t.Cleanup(func() { users = old })
	s := &Server{}
	w := httptest.NewRecorder()
	s.listAgents(w, httptest.NewRequest("GET", "/v1/agents", nil), "nomachine@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "[]" {
		t.Errorf("body = %s, want an empty array", got)
	}
}

// Every tester must own a distinct machine, or two people share a roster and
// each sees the other's conversations.
func TestEveryUserHasTheirOwnMachine(t *testing.T) {
	seen := map[string]string{}
	for _, u := range users {
		if u.Machine == "" {
			t.Errorf("%s has no machine", u.Email)
			continue
		}
		if other, dup := seen[u.Machine]; dup {
			t.Errorf("%s and %s share machine %q", other, u.Email, u.Machine)
		}
		seen[u.Machine] = u.Email
	}
	if len(users) > 5 {
		t.Errorf("%d users but the host has 5 VM slots", len(users))
	}
}

// Machine ids go straight into a control-plane URL, so they must match the id
// shape the control plane itself enforces.
func TestMachineIDsAreValid(t *testing.T) {
	ok := regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	for _, u := range users {
		if !ok.MatchString(u.Machine) {
			t.Errorf("%s has an unusable machine id %q", u.Email, u.Machine)
		}
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
