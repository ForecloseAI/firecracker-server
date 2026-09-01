package chat

import (
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// upstreamSpy stands in for the provider's MCP endpoint and records what the
// broker actually sent it.
type upstreamSpy struct {
	srv    *httptest.Server
	apikey string
	auth   string
	path   string
}

// newUpstream starts a spy that answers everything with "upstream ok".
func newUpstream(t *testing.T) *upstreamSpy {
	t.Helper()
	spy := &upstreamSpy{}
	spy.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy.apikey, spy.auth, spy.path = r.Header.Get("x-api-key"), r.Header.Get("Authorization"), r.URL.Path
		w.Write([]byte("upstream ok"))
	}))
	t.Cleanup(spy.srv.Close)
	return spy
}

// ask sends one request through the broker from a chosen guest address.
func ask(gw *AppsGateway, url, from string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, url, strings.NewReader(`{"jsonrpc":"2.0"}`))
	req.RemoteAddr = from + ":51234"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	return rec
}

// The happy path: the guest reaches its session and the credential is added on
// this side of the tap, so the guest never holds it.
func TestTheBrokerAddsTheCredentialTheGuestNeverHas(t *testing.T) {
	up := newUpstream(t)
	gw := NewAppsGateway("the-project-key", "0.0.0.0:8092")
	url, err := gw.Register("m1", "172.16.0.2", "172.16.0.1", up.srv.URL+"/mcp/sess_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(url, "http://172.16.0.1:8092/apps/") {
		t.Fatalf("the guest was told to dial %q", url)
	}
	if strings.Contains(url, "the-project-key") || strings.Contains(url, "sess_1") {
		t.Fatalf("the guest url carries something it should not: %q", url)
	}
	rec := ask(gw, url, "172.16.0.2", nil)
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream ok" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
	if up.apikey != "the-project-key" {
		t.Errorf("upstream saw x-api-key %q", up.apikey)
	}
	if up.path != "/mcp/sess_1" {
		t.Errorf("upstream saw path %q", up.path)
	}
}

// THE test. A ticket is pinned to one guest's address, so a ticket that leaked
// -- or was left behind when a slot was recycled -- is useless to anyone else.
func TestATicketOnlyWorksFromTheGuestItWasMintedFor(t *testing.T) {
	up := newUpstream(t)
	gw := NewAppsGateway("k", "0.0.0.0:8092")
	url, _ := gw.Register("m1", "172.16.0.2", "172.16.0.1", up.srv.URL+"/mcp/sess_1")

	for _, from := range []string{"172.16.0.6", "172.16.4.2", "127.0.0.1", "10.0.0.5"} {
		if rec := ask(gw, url, from, nil); rec.Code != http.StatusNotFound {
			t.Errorf("a request from %s got %d", from, rec.Code)
		}
	}
	if up.apikey != "" {
		t.Error("upstream was reached by the wrong guest")
	}
}

// An invented ticket gets the same answer as a wrong address: a broker that told
// them apart would be an oracle for guessing tickets.
func TestAnUnknownTicketIsIndistinguishableFromAWrongAddress(t *testing.T) {
	gw := NewAppsGateway("k", "0.0.0.0:8092")
	gw.Register("m1", "172.16.0.2", "172.16.0.1", "https://example.com/mcp")
	for _, path := range []string{"/apps/deadbeef", "/apps/", "/apps/x/y", "/", "/mcp"} {
		if rec := ask(gw, "http://172.16.0.1:8092"+path, "172.16.0.2", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s got %d", path, rec.Code)
		}
	}
}

// The guest composes these headers and it is untrusted, so anything that looks
// like authentication is dropped before ours goes on. Otherwise it could smuggle
// a key of its own upstream, or override the one the broker adds.
func TestAGuestCannotSmuggleItsOwnCredentials(t *testing.T) {
	up := newUpstream(t)
	gw := NewAppsGateway("the-real-key", "0.0.0.0:8092")
	url, _ := gw.Register("m1", "172.16.0.2", "172.16.0.1", up.srv.URL+"/mcp")
	ask(gw, url, "172.16.0.2", map[string]string{
		"x-api-key":     "a-key-the-guest-made-up",
		"Authorization": "Bearer stolen",
	})
	if up.apikey != "the-real-key" {
		t.Errorf("the guest overrode the credential: %q", up.apikey)
	}
	if up.auth != "" {
		t.Errorf("the guest's Authorization was forwarded: %q", up.auth)
	}
}

// Slots are recycled. A route left behind after a machine is recreated -- or
// after its address is handed to a different machine -- is exactly how one
// person's agent would end up acting as another.
func TestRecyclingInvalidatesTheOldRoute(t *testing.T) {
	up := newUpstream(t)
	gw := NewAppsGateway("k", "0.0.0.0:8092")

	old, _ := gw.Register("m1", "172.16.0.2", "172.16.0.1", up.srv.URL+"/mcp/old")
	fresh, _ := gw.Register("m1", "172.16.0.2", "172.16.0.1", up.srv.URL+"/mcp/new")
	if rec := ask(gw, old, "172.16.0.2", nil); rec.Code != http.StatusNotFound {
		t.Errorf("a re-registered machine's old ticket still works: %d", rec.Code)
	}
	if rec := ask(gw, fresh, "172.16.0.2", nil); rec.Code != http.StatusOK {
		t.Errorf("the fresh ticket does not: %d", rec.Code)
	}

	// A different machine lands on the recycled address.
	other, _ := gw.Register("m2", "172.16.0.2", "172.16.0.1", up.srv.URL+"/mcp/other")
	if rec := ask(gw, fresh, "172.16.0.2", nil); rec.Code != http.StatusNotFound {
		t.Error("the previous occupant's ticket survived the address being reused")
	}
	if rec := ask(gw, other, "172.16.0.2", nil); rec.Code != http.StatusOK {
		t.Errorf("the new occupant cannot reach its own session: %d", rec.Code)
	}

	gw.Forget("m2")
	if rec := ask(gw, other, "172.16.0.2", nil); rec.Code != http.StatusNotFound {
		t.Error("Forget left the route behind")
	}
}

// The guest must not learn where its session actually lives, and an error string
// is the usual way that escapes.
func TestAnUpstreamFailureSaysNothingAboutUpstream(t *testing.T) {
	gw := NewAppsGateway("k", "0.0.0.0:8092")
	// A port nothing is listening on, so the hop fails.
	url, _ := gw.Register("m1", "172.16.0.2", "172.16.0.1", "http://127.0.0.1:1/mcp/sess_SECRET")
	rec := ask(gw, url, "172.16.0.2", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "sess_SECRET") || strings.Contains(rec.Body.String(), "127.0.0.1") {
		t.Errorf("the failure named upstream: %q", rec.Body)
	}
}

// No provider, no broker, and therefore no guest-facing port at all.
func TestNoKeyMeansNoBroker(t *testing.T) {
	if NewAppsGateway("", "0.0.0.0:8092") != nil {
		t.Error("a broker was built with no credential to add")
	}
	if NewAppsGateway("k", "") != nil {
		t.Error("a broker was built with nowhere to listen")
	}
}

// An allow list, not a strip list: the guest composes these and it is untrusted,
// so anything not on the list must not reach the provider. Proxy-Authorization
// is the one a strip list written today would have missed.
func TestOnlyTheHeadersTheTransportNeedsAreForwarded(t *testing.T) {
	var seen http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Clone()
	}))
	defer up.Close()

	gw := NewAppsGateway("the-real-key", "0.0.0.0:8092")
	url, _ := gw.Register("m1", "172.16.0.2", "172.16.0.1", up.URL+"/mcp")
	ask(gw, url, "172.16.0.2", map[string]string{
		"Content-Type":        "application/json",
		"Mcp-Session-Id":      "sess-from-the-transport",
		"Proxy-Authorization": "Basic c21vdWdnbGVk",
		"X-Forwarded-For":     "1.2.3.4",
		"Cookie":              "a=b",
		"X-Composio-Admin":    "please",
	})
	if got := seen.Get("Mcp-Session-Id"); got != "sess-from-the-transport" {
		t.Errorf("the transport's own header was dropped: %q", got)
	}
	if got := seen.Get("x-api-key"); got != "the-real-key" {
		t.Errorf("upstream saw x-api-key %q", got)
	}
	for _, h := range []string{"Proxy-Authorization", "Cookie", "X-Composio-Admin", "X-Forwarded-For"} {
		if got := seen.Get(h); got != "" {
			t.Errorf("%s reached upstream: %q", h, got)
		}
	}
}

// The journal is read aloud in debugging sessions, so a session url in it is a
// credential in a paste buffer.
func TestAFailedHopKeepsTheSessionOutOfTheLog(t *testing.T) {
	var buf strings.Builder
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)

	gw := NewAppsGateway("k", "0.0.0.0:8092")
	url, _ := gw.Register("m1", "172.16.0.2", "172.16.0.1", "http://127.0.0.1:1/mcp/sess_SECRET")
	ask(gw, url, "172.16.0.2", nil)

	if strings.Contains(buf.String(), "sess_SECRET") {
		t.Errorf("the session url reached the journal: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "m1") {
		t.Errorf("the line does not say which machine it was about: %q", buf.String())
	}
}

// The table is bounded: a service that runs for weeks must not accumulate a
// route for every machine it has ever pushed to.
func TestTheRouteTableIsBounded(t *testing.T) {
	gw := NewAppsGateway("k", "0.0.0.0:8092")
	for i := range appsRouteCap + 40 {
		if _, err := gw.Register(fmt.Sprintf("m%d", i), fmt.Sprintf("172.16.%d.%d", i/64, i%64),
			"172.16.0.1", "https://example.com/mcp"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(gw.routes); n > appsRouteCap {
		t.Errorf("the table grew to %d routes", n)
	}
}
