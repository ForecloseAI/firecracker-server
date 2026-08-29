package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cracked/internal/agentapi"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpServerOver builds a route server whose registrations reach a real MCP
// server over HTTP, so the whole registration path runs end to end.
func mcpServerOver(t *testing.T, tools ...*mcpsdk.Tool) (*Server, string) {
	t.Helper()
	f := newRemoteFixture(t, transportHTTP, 0, tools...)
	return NewServer(newTestSupervisor(t)), f.URL
}

// register posts one registration and returns the recorder.
func register(t *testing.T, s *Server, name, rawURL string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"name":"` + name + `","url":"` + rawURL + `","headers":{"Authorization":"Bearer sk-live-SECRET"}}`
	return do(t, s, "POST", "/mcp", body)
}

// listed decodes GET /mcp.
func listed(t *testing.T, s *Server) []agentapi.MCPServer {
	t.Helper()
	w := do(t, s, "GET", "/mcp", "")
	var out []agentapi.MCPServer
	if json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("could not read the list: %s", w.Body.String())
	}
	return out
}

// TestListingMCPServersNeverDialsAnything pins the poll rule. An app on a
// two-second refresh must not open a connection to somebody else's server from
// the person's own address on every tick.
func TestListingMCPServersNeverDialsAnything(t *testing.T) {
	s, url := mcpServerOver(t, namedTool("search_pages", "ok"))
	if w := register(t, s, "Notion", url); w.Code != http.StatusCreated {
		t.Fatalf("register = %d %s", w.Code, w.Body)
	}
	var dials atomic.Int64
	s.sup.MCP().dial = func(context.Context, mcpRecord) (*mcpsdk.ClientSession, error) {
		dials.Add(1)
		return nil, nil
	}
	for range 10 {
		if got := listed(t, s); len(got) != 1 {
			t.Fatalf("listed %d servers", len(got))
		}
	}
	if n := dials.Load(); n != 0 {
		t.Errorf("ten polls opened %d connections", n)
	}
}

// TestRegisteringAnUnreachableServerStoresNothing keeps a half-registered
// server out of the list, where it would look successful and never work.
//
// The address is one that ACCEPTS NOTHING AND SAYS NOTHING, which is the shape a
// firewalled or mistyped host takes and the case the transport does not bound on
// its own: without the watchdog in connect() this returned after 75 seconds, far
// past the host's 30s write client, so the host would report a failure on a
// registration that had actually stored.
func TestRegisteringAnUnreachableServerStoresNothing(t *testing.T) {
	shrinkProbeTimeout(t, 2*time.Second)
	s, _ := mcpServerOver(t)
	start := time.Now()
	w := register(t, s, "Dead", "https://127.0.0.2:9/mcp")
	if w.Code == http.StatusCreated {
		t.Fatalf("an unreachable server registered: %s", w.Body)
	}
	if took := time.Since(start); took > 3*mcpProbeTimeout {
		t.Errorf("the refusal took %s, so the deadline did not bound the dial", took)
	}
	if got := listed(t, s); len(got) != 0 {
		t.Errorf("a failed registration left %d servers behind", len(got))
	}
}

// shrinkProbeTimeout keeps a test that waits out a deadline from waiting out the
// production one.
func shrinkProbeTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	was := mcpProbeTimeout
	mcpProbeTimeout = d
	t.Cleanup(func() { mcpProbeTimeout = was })
}

// TestRegisteringAPrivateAddressSaysWhyRatherThanTimingOut turns a fifteen
// second mystery into an instant, honest refusal.
func TestRegisteringAPrivateAddressSaysWhyRatherThanTimingOut(t *testing.T) {
	s, _ := mcpServerOver(t)
	start := time.Now()
	w := register(t, s, "Laptop", "http://192.168.1.5/mcp")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a private address gave %d, want 400: %s", w.Code, w.Body)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the refusal took %s, so it went to the network first", took)
	}
	if !strings.Contains(w.Body.String(), "private network") {
		t.Errorf("the refusal does not say why: %s", w.Body)
	}
}

// TestTheSameServerCannotBeRegisteredTwice keeps two registrations from
// contributing tools whose namespaced names shadow each other.
func TestTheSameServerCannotBeRegisteredTwice(t *testing.T) {
	s, url := mcpServerOver(t, namedTool("search_pages", "ok"))
	register(t, s, "Notion", url)
	if w := register(t, s, "Notion Again", url); w.Code != http.StatusConflict {
		t.Errorf("the second registration gave %d, want 409: %s", w.Code, w.Body)
	}
}

// TestEveryMCPMutationEvictsIdleAgents is what makes a change reach the agents.
// Tools are assembled once when an agent is built, so without this a
// registration reaches nobody until one happens to be recycled.
func TestEveryMCPMutationEvictsIdleAgents(t *testing.T) {
	s, url := mcpServerOver(t, namedTool("search_pages", "ok"))
	cases := []struct{ method, path, body string }{
		{"POST", "/mcp", `{"name":"Notion","url":"` + url + `"}`},
		{"PATCH", "/mcp/notion", `{"enabled":false}`},
		{"DELETE", "/mcp/notion", ""},
	}
	for _, c := range cases {
		if _, err := s.sup.Get(BossID); err != nil {
			t.Fatal(err)
		}
		if got := do(t, s, c.method, c.path, c.body); got.Code >= 300 {
			t.Fatalf("%s %s = %d %s", c.method, c.path, got.Code, got.Body)
		}
		if n := s.sup.LiveCount(); n != 0 {
			t.Errorf("%s %s left %d agents holding the old tool surface", c.method, c.path, n)
		}
	}
}

// TestAnEmptyPatchBodyDoesNotDisableAServer pins the pointer on MCPUpdate. A
// bool would make {} silently take the person's tools away.
func TestAnEmptyPatchBodyDoesNotDisableAServer(t *testing.T) {
	s, url := mcpServerOver(t, namedTool("search_pages", "ok"))
	register(t, s, "Notion", url)
	if w := do(t, s, "PATCH", "/mcp/notion", `{}`); w.Code != http.StatusBadRequest {
		t.Fatalf("an empty patch gave %d, want 400", w.Code)
	}
	if got := listed(t, s); len(got) != 1 || !got[0].Enabled {
		t.Error("an empty patch turned the server off")
	}
}

// TestPatchingAnUnknownServerIs404 keeps a typo from reading as success.
func TestPatchingAnUnknownServerIs404(t *testing.T) {
	s, _ := mcpServerOver(t)
	if w := do(t, s, "PATCH", "/mcp/nope", `{"enabled":false}`); w.Code != http.StatusNotFound {
		t.Errorf("patching an unknown server gave %d, want 404", w.Code)
	}
	if w := do(t, s, "DELETE", "/mcp/nope", ""); w.Code != http.StatusNotFound {
		t.Errorf("deleting an unknown server gave %d, want 404", w.Code)
	}
}

// TestNoMCPResponseCarriesTheSecret checks EVERY route rather than the one that
// was remembered. A redaction that is applied per handler is a redaction that
// will eventually be forgotten in one of them.
func TestNoMCPResponseCarriesTheSecret(t *testing.T) {
	s, url := mcpServerOver(t, namedTool("search_pages", "ok"))
	bodies := []string{
		register(t, s, "Notion", url).Body.String(),
		do(t, s, "GET", "/mcp", "").Body.String(),
		do(t, s, "PATCH", "/mcp/notion", `{"enabled":false}`).Body.String(),
	}
	for i, body := range bodies {
		if strings.Contains(body, "sk-live-SECRET") {
			t.Errorf("response %d handed the token back: %s", i, body)
		}
	}
	if got := listed(t, s); len(got) != 1 || len(got[0].HeaderKeys) != 1 {
		t.Errorf("the header key is not reported, so a client cannot see what is set: %+v", got)
	}
}

// TestARegisteredServerReportsTheToolsTheModelWillSee keeps the list a person
// reads identical to the surface their agents are given.
func TestARegisteredServerReportsTheToolsTheModelWillSee(t *testing.T) {
	s, url := mcpServerOver(t, namedTool("search_pages", "ok"))
	w := register(t, s, "Notion", url)
	var got agentapi.MCPServer
	if json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("could not read the registration: %s", w.Body)
	}
	if len(got.Tools) != 1 || got.Tools[0] != "mcp__notion__search_pages" {
		t.Errorf("reported %v, want the namespaced name the model is offered", got.Tools)
	}
	if !got.Reachable {
		t.Error("a successful registration reports itself unreachable")
	}
}
