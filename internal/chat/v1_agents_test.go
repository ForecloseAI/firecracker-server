package chat

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"cracked/internal/agentapi"
)

// fakeGuest is a daemon with a roster that actually changes, so a test can
// activate an agent and then see it in the next call rather than asserting
// against a frozen fixture.
type fakeGuest struct {
	mu         sync.Mutex
	roster     []agentapi.Status
	sendStatus int // non-zero to make the next message fail with this code
	sent       []string

	events        []agentapi.Event
	resolved      []resolution
	resolveStatus int // non-zero to make the next resolve fail with this code

	sched         []agentapi.Schedule
	created       agentapi.CreateAgentReq // the last create the gateway forwarded
	createStatus  int                     // non-zero to refuse the next create with this code
	createMessage string                  // and this message
	patched       agentapi.AgentPatch     // the last patch the gateway forwarded
	person        agentapi.Person         // the last profile the gateway forwarded
	body          string                  // the raw JSON of the last message, as it came off the wire
}

// resolution is one decision the gateway forwarded, kept so a test can assert
// on the body the SERVER authored rather than on what a client asked for.
type resolution struct {
	id   string
	body map[string]any
}

// fakeProfiles is the catalog the fake serves: a boss plus two specialists,
// which is enough to cover exclusion, activation and duplicates.
var fakeProfiles = []agentapi.Profile{
	{Key: "boss", Title: "Boss", Description: "Runs the team"},
	{Key: "coder", Title: "Coder", Description: "Writes code"},
	{Key: "researcher", Title: "Researcher", Description: "Reads the web", Browser: true},
	{Key: "custom", Title: "Custom", Description: "Built by you", Browser: true},
}

// routes wires the guest endpoints these tests depend on.
func (g *fakeGuest) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /agents", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		json.NewEncoder(w).Encode(g.roster)
	})
	mux.HandleFunc("GET /agent-types", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(fakeProfiles)
	})
	mux.HandleFunc("POST /agents", g.create)
	mux.HandleFunc("PATCH /agents/{id}", g.update)
	mux.HandleFunc("DELETE /agents/{id}", g.remove)
	mux.HandleFunc("POST /agents/{id}/messages", g.message)
	mux.HandleFunc("GET /agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		json.NewEncoder(w).Encode(agentapi.EventsPage{Events: g.events, LastEventID: len(g.events)})
	})
	// Answers the way a guest whose owner has patched it would: the person has
	// root in their own VM, so nothing it says about a file can be believed.
	mux.HandleFunc("GET /agents/{id}/outbox/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Content-Disposition", "inline")
		w.Header().Set("Cache-Control", "public, max-age=31536000")
		w.Write([]byte("the report"))
	})
	mux.HandleFunc("POST /approvals/{apid}", g.resolve)
	mux.HandleFunc("GET /schedules", g.schedules)
	mux.HandleFunc("DELETE /schedules/{id}", g.dropSchedule)
	mux.HandleFunc("PUT /person", g.putPerson)
	return mux
}

// putPerson records the profile the gateway forwarded, so a test can assert on
// what actually reached the guest.
func (g *fakeGuest) putPerson(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	json.NewDecoder(r.Body).Decode(&g.person)
	w.WriteHeader(http.StatusNoContent)
}

// resolve records what the gateway sent, or refuses with the configured status.
func (g *fakeGuest) resolve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resolveStatus != 0 {
		w.WriteHeader(g.resolveStatus)
		return
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	g.resolved = append(g.resolved, resolution{id: r.PathValue("apid"), body: body})
	json.NewEncoder(w).Encode(map[string]any{"approval_id": r.PathValue("apid")})
}

// create adds a roster row, giving it the id agentd would derive from the type
// -- or, for a custom agent, from the name. The last request is kept so a test
// can see exactly what the gateway forwarded, key included.
func (g *fakeGuest) create(w http.ResponseWriter, r *http.Request) {
	var req agentapi.CreateAgentReq
	json.NewDecoder(r.Body).Decode(&req)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.created = req
	if g.createStatus != 0 {
		w.WriteHeader(g.createStatus)
		json.NewEncoder(w).Encode(map[string]string{"error": "bad_request", "message": g.createMessage})
		return
	}
	// Mirrors agentd, which is the one authority: a custom agent needs a name
	// and a role.
	if req.Type == agentapi.CustomType && (strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Instructions) == "") {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "bad_request", "message": "name: must be 1 to 40 characters"})
		return
	}
	// Mirrors agentd: an empty name falls back to the lowercase type key.
	name, id := req.Name, req.Type
	if name == "" {
		name = req.Type
	}
	if req.Type == agentapi.CustomType {
		id = strings.ToLower(strings.ReplaceAll(name, " ", "-"))
	}
	st := agentapi.Status{ID: id, Name: name, Type: req.Type,
		Instructions: req.Instructions, Model: req.Model.View()}
	for _, p := range fakeProfiles {
		if p.Key == req.Type {
			st.Description, st.Browser = p.Description, p.Browser
		}
	}
	g.roster = append(g.roster, st)
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(st)
}

// update applies a patch the way agentd does, and keeps it for assertions.
func (g *fakeGuest) update(w http.ResponseWriter, r *http.Request) {
	var patch agentapi.AgentPatch
	json.NewDecoder(r.Body).Decode(&patch)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.patched = patch
	for i, st := range g.roster {
		if st.ID != r.PathValue("id") {
			continue
		}
		// Mirrors agentd: only a custom agent's role or model may change.
		if st.Type != agentapi.CustomType && (patch.Instructions != nil || patch.Model != nil) {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"message": "only a custom agent's instructions or model can change"})
			return
		}
		if patch.Name != nil {
			st.Name = *patch.Name
		}
		if patch.Instructions != nil {
			st.Instructions = *patch.Instructions
		}
		if patch.Model != nil {
			st.Model = patch.Model.ModelConfig.View()
			if patch.Model.Clear {
				st.Model = nil
			}
		}
		g.roster[i] = st
		json.NewEncoder(w).Encode(st)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

// remove drops a roster row.
func (g *fakeGuest) remove(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	g.mu.Lock()
	defer g.mu.Unlock()
	for i, st := range g.roster {
		if st.ID == id {
			g.roster = append(g.roster[:i], g.roster[i+1:]...)
			json.NewEncoder(w).Encode(map[string]any{"id": id, "deleted": true})
			return
		}
	}
	w.WriteHeader(http.StatusConflict)
}

// message records a queued turn, or fails with the configured status.
func (g *fakeGuest) message(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sendStatus != 0 {
		w.WriteHeader(g.sendStatus)
		return
	}
	// Kept as bytes, not just decoded: a test that asserts on a decoded field can
	// only see fields the struct still declares, so it could not notice a zone
	// coming back onto the wire.
	raw, _ := io.ReadAll(r.Body)
	g.body = string(raw)
	var req struct{ Text string }
	json.Unmarshal(raw, &req)
	g.sent = append(g.sent, req.Text)
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{
		"message_id": "m_001", "session_state": "working", "last_event_id": 41 + len(g.sent)})
}

// newFake stands a whole gateway up over a fake guest, with a signed-in user.
// The returned string is that user's access token.
func newFake(t *testing.T) (*Server, *fakeGuest, string) {
	t.Helper()
	g := &fakeGuest{roster: []agentapi.Status{{ID: "boss", Name: "Boss", Type: "boss", Description: "Runs the team"}}}
	s, tok := serverOver(t, g)
	return s, g, tok
}

// serverOver wires a gateway to an already-configured fake guest.
func serverOver(t *testing.T, g *fakeGuest) (*Server, string) {
	t.Helper()
	srv := httptest.NewServer(g.routes())
	t.Cleanup(srv.Close)
	v, mint := testAuth(t)
	s := &Server{control: stubControl(t, srv.URL, "running"), auth: v,
		cfg: Config{Origin: "https://chat.example.com", Token: "fleet-token"}}
	return s, mint(testUserID, "tester@example.com")
}

// call runs one request through the guard, as the app would reach it.
func call(t *testing.T, s *Server, tok, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, r)
	return w
}

// The gallery is what the person picks from, so the boss must not be in it:
// every machine already has one and it cannot be deleted.
func TestGalleryExcludesTheBoss(t *testing.T) {
	s, _, u := newFake(t)
	var got []Template
	w := call(t, s, u, "GET", "/v1/agent-types", "")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status %d: %v", w.Code, err)
	}
	if len(got) != 2 {
		t.Fatalf("gallery = %+v, want the two specialists", got)
	}
	for _, tpl := range got {
		if tpl.ID == "boss" {
			t.Error("the boss is offered as something to add")
		}
		if tpl.Active {
			t.Errorf("%s is active on a fresh machine", tpl.ID)
		}
	}
}

// A card carries the same avatar recipe as the agent it becomes, which works
// only because a type is activated once and keeps the template's id.
func TestGalleryCardMatchesTheAgentItBecomes(t *testing.T) {
	s, _, u := newFake(t)
	var gallery []Template
	json.Unmarshal(call(t, s, u, "GET", "/v1/agent-types", "").Body.Bytes(), &gallery)
	var made Agent
	json.Unmarshal(call(t, s, u, "POST", "/v1/agents", `{"templateId":"coder"}`).Body.Bytes(), &made)
	for _, tpl := range gallery {
		if tpl.ID != "coder" {
			continue
		}
		if tpl.Hue != made.Hue || tpl.Shape != made.Shape || tpl.Initial != made.Initial {
			t.Errorf("card %+v does not match agent %+v", tpl, made)
		}
	}
}

// Activating is the core of the add flow: it must return the finished row, and
// that row must then be on the roster.
func TestActivateReturnsTheAgentAndItSticks(t *testing.T) {
	s, _, u := newFake(t)
	w := call(t, s, u, "POST", "/v1/agents", `{"templateId":"researcher"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var made Agent
	json.Unmarshal(w.Body.Bytes(), &made)
	if made.ID != "researcher" || !made.Online {
		t.Fatalf("agent = %+v", made)
	}
	// The daemon falls back to the lowercase type key when no name is sent, so
	// the card would read "researcher" where the gallery said "Researcher".
	if made.Name != "Researcher" {
		t.Errorf("name = %q, want the gallery's title", made.Name)
	}
	if !made.Capabilities.Browse {
		t.Error("the researcher's browser capability did not come from its profile")
	}
	var roster []Agent
	json.Unmarshal(call(t, s, u, "GET", "/v1/agents", "").Body.Bytes(), &roster)
	if len(roster) != 2 {
		t.Fatalf("roster = %+v, want boss plus researcher", roster)
	}
}

// One of each kind. A second would be created as "coder-2", which the gallery
// has no card for and the person never asked for.
func TestActivatingTwiceIsRefused(t *testing.T) {
	s, g, u := newFake(t)
	call(t, s, u, "POST", "/v1/agents", `{"templateId":"coder"}`)
	w := call(t, s, u, "POST", "/v1/agents", `{"templateId":"coder"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", w.Code)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.roster) != 2 {
		t.Errorf("the refused call still created something: %+v", g.roster)
	}
}

// Once active, the card says so, so the client can grey it out.
func TestGalleryMarksActive(t *testing.T) {
	s, _, u := newFake(t)
	call(t, s, u, "POST", "/v1/agents", `{"templateId":"coder"}`)
	var gallery []Template
	json.Unmarshal(call(t, s, u, "GET", "/v1/agent-types", "").Body.Bytes(), &gallery)
	for _, tpl := range gallery {
		if tpl.ID == "coder" && !tpl.Active {
			t.Error("coder is on the roster but its card is not marked active")
		}
		if tpl.ID == "researcher" && tpl.Active {
			t.Error("researcher is marked active but was never added")
		}
	}
}

// An unknown template must be a client error, not a 502 that reads as an outage.
func TestUnknownTemplateIsRejected(t *testing.T) {
	s, _, u := newFake(t)
	for _, body := range []string{`{"templateId":"nope"}`, `{"templateId":""}`, `{"templateId":"boss"}`} {
		if w := call(t, s, u, "POST", "/v1/agents", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s gave %d, want 400", body, w.Code)
		}
	}
}

// Retiring returns 204 with an empty body: the client calls res.json() on any
// other 2xx and would throw on nothing.
func TestRetireReturns204AndRemoves(t *testing.T) {
	s, _, u := newFake(t)
	call(t, s, u, "POST", "/v1/agents", `{"templateId":"coder"}`)
	w := call(t, s, u, "DELETE", "/v1/agents/coder", "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", w.Body.String())
	}
	var gallery []Template
	json.Unmarshal(call(t, s, u, "GET", "/v1/agent-types", "").Body.Bytes(), &gallery)
	for _, tpl := range gallery {
		if tpl.ID == "coder" && tpl.Active {
			t.Error("a retired type is still marked active, so it cannot be re-added")
		}
	}
}

// agentd answers 409 for both "no such agent" and "that is the boss". A client
// retrying on 409 would retry a missing agent forever, so the gateway has to
// tell them apart itself.
func TestRetireDistinguishesMissingFromBoss(t *testing.T) {
	s, _, u := newFake(t)
	if w := call(t, s, u, "DELETE", "/v1/agents/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("unknown agent gave %d, want 404", w.Code)
	}
	if w := call(t, s, u, "DELETE", "/v1/agents/boss", ""); w.Code != http.StatusConflict {
		t.Errorf("boss gave %d, want 409", w.Code)
	}
}

// The stored message is what the client appends to the thread, so it has to
// carry the guest's own event id -- a later history fetch reports the same line
// under that id.
func TestSendReturnsTheStoredMessage(t *testing.T) {
	s, g, u := newFake(t)
	w := call(t, s, u, "POST", "/v1/threads/boss/messages", `{"text":"ship it"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var msg Message
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Kind != "text" || msg.From != "me" || msg.Text != "ship it" {
		t.Fatalf("message = %+v", msg)
	}
	if msg.ID != "42" {
		t.Errorf("id = %q, want the guest's event id", msg.ID)
	}
	if msg.Time.IsZero() {
		t.Error("message has no timestamp")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent) != 1 || g.sent[0] != "ship it" {
		t.Errorf("guest received %v", g.sent)
	}
}

// The same text twice must reach the agent twice. The web path hashes text into
// a 5-second bucket and silently drops the repeat; a person saying "yes" twice
// would lose the second with no error at all.
func TestRepeatedTextIsNotSwallowed(t *testing.T) {
	s, g, u := newFake(t)
	for i := 0; i < 2; i++ {
		if w := call(t, s, u, "POST", "/v1/threads/boss/messages", `{"text":"yes"}`); w.Code != http.StatusOK {
			t.Fatalf("send %d gave %d", i, w.Code)
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent) != 2 {
		t.Errorf("guest received %d messages, want 2", len(g.sent))
	}
}

// A busy agent is worth retrying and an unreachable guest is not, so 503 must
// survive rather than being flattened into 502 the way /api/send does.
func TestBusyAgentSurfacesAs503(t *testing.T) {
	s, g, u := newFake(t)
	g.sendStatus = http.StatusServiceUnavailable
	w := call(t, s, u, "POST", "/v1/threads/boss/messages", `{"text":"hi"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 carries no Retry-After, so a client cannot know when to try again")
	}
}

// Empty text must not reach the agent: agentd rejects it anyway, and a blank
// turn would burn a model call saying nothing.
func TestEmptyTextIsRejected(t *testing.T) {
	s, g, u := newFake(t)
	if w := call(t, s, u, "POST", "/v1/threads/boss/messages", `{"text":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.sent) != 0 {
		t.Error("an empty message reached the guest")
	}
}

// Every new route must refuse an unauthenticated caller.
func TestNewRoutesNeedAToken(t *testing.T) {
	s, _, _ := newFake(t)
	for _, c := range []struct{ method, path string }{
		{"GET", "/v1/agent-types"}, {"POST", "/v1/agents"},
		{"DELETE", "/v1/agents/coder"}, {"POST", "/v1/threads/boss/messages"},
		{"DELETE", "/v1/account"},
		{"GET", "/v1/apps"},
		{"GET", "/v1/apps/connections"},
		{"POST", "/v1/apps/gmail/connect"},
		{"DELETE", "/v1/apps/connections/ca_1"},
		{"PUT", "/v1/apps/gmail/policy"},
	} {
		r := httptest.NewRequest(c.method, c.path, strings.NewReader("{}"))
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s gave %d, want 401", c.method, c.path, w.Code)
		}
	}
}
