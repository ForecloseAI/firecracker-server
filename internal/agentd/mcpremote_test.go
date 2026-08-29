package agentd

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// remoteFixture stands a REAL MCP server up behind a REAL HTTP handler and
// records what it was sent.
//
// Not a hand-written stub: this exercises the same handshake, paging and
// tools/list a registered server will, so a mistake in how the SDK is driven
// shows up here rather than on a VM the first time somebody registers anything.
type remoteFixture struct {
	URL string

	mu      sync.Mutex
	auth    []string // the Authorization header of every request seen
	methods []string // the method of every request seen
}

// newRemoteFixture serves tools over the named transport. pageSize 0 leaves the
// server's default; 1 forces a cursor on every page.
func newRemoteFixture(t *testing.T, transport string, pageSize int, tools ...*mcpsdk.Tool) *remoteFixture {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fake-remote", Version: "1"},
		&mcpsdk.ServerOptions{PageSize: pageSize})
	for _, tool := range tools {
		srv.AddTool(tool, cannedResult(tool))
	}
	f := &remoteFixture{}
	hs := httptest.NewServer(f.record(mcpHandler(srv, transport)))
	t.Cleanup(hs.Close)
	f.URL = hs.URL
	return f
}

// mcpHandler is the server side of whichever transport is under test.
func mcpHandler(srv *mcpsdk.Server, transport string) http.Handler {
	get := func(*http.Request) *mcpsdk.Server { return srv }
	if transport == transportSSE {
		return mcpsdk.NewSSEHandler(get, nil)
	}
	return mcpsdk.NewStreamableHTTPHandler(get, nil)
}

// record notes every request before passing it on.
func (f *remoteFixture) record(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.methods = append(f.methods, r.Method)
		f.mu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// sawAuth reports whether any request carried the given Authorization header.
func (f *remoteFixture) sawAuth(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, got := range f.auth {
		if got == want {
			return true
		}
	}
	return false
}

// sawMethod reports whether any request used the given method.
func (f *remoteFixture) sawMethod(want string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, got := range f.methods {
		if got == want {
			return true
		}
	}
	return false
}

// recordFor builds a registration pointing at a fixture.
func recordFor(url, transport string, headers map[string]string) mcpRecord {
	return mcpRecord{ID: "test", Name: "Test", URL: url, Transport: transport, Headers: headers}
}

// names pulls the advertised names out of a probe result.
func names(specs []mcpToolSpec) []string {
	out := make([]string, 0, len(specs))
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// TestBothTransportsListTheSameTools proves a registration works over either
// wire. A transport that connects and then advertises nothing is the failure
// this catches: the person is told the server registered and gets no tools.
func TestBothTransportsListTheSameTools(t *testing.T) {
	for _, transport := range []string{transportHTTP, transportSSE} {
		f := newRemoteFixture(t, transport, 0,
			namedTool("search_pages", "ok"), namedTool("create_page", "ok"))
		ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
		specs, err := probeRemote(ctx, recordFor(f.URL, transport, nil))
		cancel()
		if err != nil {
			t.Fatalf("%s: %v", transport, err)
		}
		if got := len(specs); got != 2 {
			t.Errorf("%s advertised %d tools, want 2: %v", transport, got, names(specs))
		}
	}
}

// TestEveryPageOfARemoteToolListIsRead pins the paging. A tools/list result
// carries a NextCursor, so a single call can return a partial list -- and a tool
// that never arrives simply never reaches the model, with nothing reporting it.
func TestEveryPageOfARemoteToolListIsRead(t *testing.T) {
	f := newRemoteFixture(t, transportHTTP, 1, namedTool("a", "ok"), namedTool("b", "ok"),
		namedTool("c", "ok"), namedTool("d", "ok"))
	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
	defer cancel()
	specs, err := probeRemote(ctx, recordFor(f.URL, transportHTTP, nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 4 {
		t.Errorf("read %d tools across pages, want 4: %v", len(specs), names(specs))
	}
}

// TestTheWholeAdvertisedSchemaIsKept proves the probe stores the schema as the
// server sent it. Round-tripping through a Go type drops $defs, anyOf and
// additionalProperties, and the model is then left guessing arguments.
func TestTheWholeAdvertisedSchemaIsKept(t *testing.T) {
	tool := namedTool("search", "ok")
	tool.InputSchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"q": map[string]any{"type": "string"}},
	}
	f := newRemoteFixture(t, transportHTTP, 0, tool)
	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
	defer cancel()
	specs, err := probeRemote(ctx, recordFor(f.URL, transportHTTP, nil))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(specs[0].Schema); !strings.Contains(got, "additionalProperties") {
		t.Errorf("the stored schema lost additionalProperties: %s", got)
	}
}

// TestTheRegisteredAuthHeaderReachesTheServer proves the round tripper actually
// injects. Without it a server authenticates fine by hand and 401s from the
// guest, which reads to the person as their token being wrong.
func TestTheRegisteredAuthHeaderReachesTheServer(t *testing.T) {
	f := newRemoteFixture(t, transportHTTP, 0, namedTool("search", "ok"))
	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
	defer cancel()
	rec := recordFor(f.URL, transportHTTP, map[string]string{"Authorization": "Bearer sk-live-1"})
	if _, err := probeRemote(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if !f.sawAuth("Bearer sk-live-1") {
		t.Error("the server never saw the registered Authorization header")
	}
}

// TestAProbeDoesNotLeaveASessionOpen proves registration closes what it opened.
// A streamable session is terminated with a DELETE, so its absence is the proof
// that a person adding four servers would hold four idle sockets forever.
func TestAProbeDoesNotLeaveASessionOpen(t *testing.T) {
	f := newRemoteFixture(t, transportHTTP, 0, namedTool("search", "ok"))
	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
	defer cancel()
	if _, err := probeRemote(ctx, recordFor(f.URL, transportHTTP, nil)); err != nil {
		t.Fatal(err)
	}
	if !f.sawMethod(http.MethodDelete) {
		t.Error("the probe never terminated its session")
	}
}

// TestAnAuthHeaderIsNotFollowedToAnotherHost is the credential-exfiltration
// guard. The round tripper adds the person's token to every request it sees, so
// without CheckRedirect a server that 302s elsewhere has that token delivered to
// a host they never registered, and nothing else here would catch it.
func TestAnAuthHeaderIsNotFollowedToAnotherHost(t *testing.T) {
	elsewhere := newRemoteFixture(t, transportHTTP, 0, namedTool("search", "ok"))
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL, http.StatusFound)
	}))
	defer redirector.Close()

	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
	defer cancel()
	rec := recordFor(redirector.URL, transportHTTP, map[string]string{"Authorization": "Bearer sk-live-1"})
	if _, err := probeRemote(ctx, rec); err == nil {
		t.Fatal("a cross-host redirect was followed")
	}
	if elsewhere.sawAuth("Bearer sk-live-1") {
		t.Error("the token was handed to a host it was never registered on")
	}
}

// TestAnUnreachableServerFailsRatherThanHanging proves a registration that
// cannot connect comes back as an error. A probe that outlasts the host's 30s
// write client would have the host report failure on a registration that
// succeeded, and the person's retry would then collide with it.
func TestAnUnreachableServerFailsRatherThanHanging(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "http://" + ln.Addr().String()
	ln.Close()

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
	defer cancel()
	if _, err := probeRemote(ctx, recordFor(dead, transportHTTP, nil)); err == nil {
		t.Fatal("a closed port registered successfully")
	}
	if took := time.Since(start); took >= mcpProbeTimeout {
		t.Errorf("the probe took %s, which is the whole timeout rather than a refusal", took)
	}
}

// TestAnUnknownTransportIsRefused keeps a typo out of the store, where it would
// become a server that can never be dialled.
func TestAnUnknownTransportIsRefused(t *testing.T) {
	if _, err := transportFor(recordFor("https://example.com/mcp", "grpc", nil), 0); err == nil {
		t.Error("an unknown transport was accepted")
	}
}
