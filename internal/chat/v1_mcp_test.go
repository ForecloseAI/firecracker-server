package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"cracked/internal/agentapi"
)

// fakeMCPGuest mirrors the guest's /mcp routes, recording the body it was sent
// so a test can assert on what the gateway actually forwarded.
type fakeMCPGuest struct {
	mu       sync.Mutex
	received []agentapi.MCPRegistration
	status   int    // non-zero to refuse the next registration with this code
	reason   string // the guest's own explanation for that refusal
}

// routes wires the four endpoints the /v1 surface talks to.
func (g *fakeMCPGuest) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /mcp", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]agentapi.MCPServer{{
			ID: "notion", Name: "Notion", URL: "https://mcp.notion.com/mcp",
			Enabled: true, HeaderKeys: []string{"Authorization"},
			Tools: []string{"mcp__notion__search_pages"}, Reachable: true,
		}})
	})
	mux.HandleFunc("POST /mcp", g.add)
	mux.HandleFunc("PATCH /mcp/{id}", g.patch)
	mux.HandleFunc("DELETE /mcp/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return mux
}

// add records the registration and answers however the test asked it to.
func (g *fakeMCPGuest) add(w http.ResponseWriter, r *http.Request) {
	var reg agentapi.MCPRegistration
	json.NewDecoder(r.Body).Decode(&reg)
	g.mu.Lock()
	g.received = append(g.received, reg)
	code, reason := g.status, g.reason
	g.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]string{"error": "refused", "message": reason})
		return
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(agentapi.MCPServer{ID: "notion", Name: reg.Name,
		URL: reg.URL, Enabled: true, HeaderKeys: []string{"Authorization"},
		Tools: []string{"mcp__notion__search_pages"}, Reachable: true})
}

// patch answers an enable or disable.
func (g *fakeMCPGuest) patch(w http.ResponseWriter, r *http.Request) {
	var in agentapi.MCPUpdate
	json.NewDecoder(r.Body).Decode(&in)
	json.NewEncoder(w).Encode(agentapi.MCPServer{ID: r.PathValue("id"), Name: "Notion",
		Enabled: in.Enabled != nil && *in.Enabled})
}

// newMCPFake stands the gateway up over a guest that speaks /mcp.
func newMCPFake(t *testing.T) (*Server, *fakeMCPGuest, string) {
	t.Helper()
	g := &fakeMCPGuest{}
	srv := httptest.NewServer(g.routes())
	t.Cleanup(srv.Close)
	v, mint := testAuth(t)
	s := &Server{control: stubControl(t, srv.URL, "running"), auth: v,
		cfg: Config{Origin: "https://chat.example.com", Token: "fleet-token"}}
	return s, g, mint(testUserID, "tester@example.com")
}

// registration is the body the app sends, token and all.
const registration = `{"name":"Notion","url":"https://mcp.notion.com/mcp",` +
	`"headers":{"Authorization":"Bearer sk-live-SECRET"}}`

// TestARegistrationReachesTheGuestVerbatim proves the gateway forwards the
// credential rather than redacting on the way IN. Redaction belongs on the way
// back; applied here it would register every server with no token at all, and
// the person would see a server that authenticates against nothing.
func TestARegistrationReachesTheGuestVerbatim(t *testing.T) {
	s, g, tok := newMCPFake(t)
	if w := call(t, s, tok, "POST", "/v1/mcp", registration); w.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.received) != 1 {
		t.Fatalf("the guest saw %d registrations", len(g.received))
	}
	if got := g.received[0].Headers["Authorization"]; got != "Bearer sk-live-SECRET" {
		t.Errorf("the guest was sent %q instead of the person's token", got)
	}
}

// TestAnAlreadyRegisteredServerComesBackAsAConflict keeps a client from
// retrying forever. Flattened to 502 the app cannot tell "try again" from "you
// already have this one".
func TestAnAlreadyRegisteredServerComesBackAsAConflict(t *testing.T) {
	s, g, tok := newMCPFake(t)
	g.status, g.reason = http.StatusConflict, "this server is already registered"
	if w := call(t, s, tok, "POST", "/v1/mcp", registration); w.Code != http.StatusConflict {
		t.Errorf("a duplicate gave %d, want 409: %s", w.Code, w.Body)
	}
}

// TestTheReasonAURLWasRejectedReachesTheApp is what StatusError.Message bought.
// Without it the person is shown a bare 400 and told nothing they can act on.
func TestTheReasonAURLWasRejectedReachesTheApp(t *testing.T) {
	s, g, tok := newMCPFake(t)
	g.status = http.StatusBadRequest
	g.reason = "this address is on a private network, which this machine's agents cannot reach"
	w := call(t, s, tok, "POST", "/v1/mcp", registration)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "private network") {
		t.Errorf("the guest's reason did not reach the app: %s", w.Body)
	}
}

// TestAnUnreachableServerStaysA502 keeps "try again" distinguishable from "fix
// your input".
func TestAnUnreachableServerStaysA502(t *testing.T) {
	s, g, tok := newMCPFake(t)
	g.status, g.reason = http.StatusBadGateway, "this server did not answer in time"
	if w := call(t, s, tok, "POST", "/v1/mcp", registration); w.Code != http.StatusBadGateway {
		t.Errorf("an unreachable server gave %d, want 502", w.Code)
	}
}

// TestNoSecretComesBackFromV1 checks the read path the app actually uses.
func TestNoSecretComesBackFromV1(t *testing.T) {
	s, _, tok := newMCPFake(t)
	bodies := []string{
		call(t, s, tok, "POST", "/v1/mcp", registration).Body.String(),
		call(t, s, tok, "GET", "/v1/mcp", "").Body.String(),
	}
	for i, body := range bodies {
		if strings.Contains(body, "sk-live-SECRET") {
			t.Errorf("response %d handed the token back: %s", i, body)
		}
		if !strings.Contains(body, "Authorization") {
			t.Errorf("response %d does not say which header is set: %s", i, body)
		}
	}
}

// TestAnEmptyUpdateIsRefusedAtTheGateway keeps a client bug from reaching the
// guest as a silent disable.
func TestAnEmptyUpdateIsRefusedAtTheGateway(t *testing.T) {
	s, _, tok := newMCPFake(t)
	if w := call(t, s, tok, "PATCH", "/v1/mcp/notion", `{}`); w.Code != http.StatusBadRequest {
		t.Errorf("an empty update gave %d, want 400", w.Code)
	}
}

// TestDisablingAServerAnswersWithItsNewState so a client need not re-list to
// know what happened.
func TestDisablingAServerAnswersWithItsNewState(t *testing.T) {
	s, _, tok := newMCPFake(t)
	w := call(t, s, tok, "PATCH", "/v1/mcp/notion", `{"enabled":false}`)
	var got agentapi.MCPServer
	if json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("could not read the reply: %s", w.Body)
	}
	if got.Enabled {
		t.Error("a disable came back enabled")
	}
}
