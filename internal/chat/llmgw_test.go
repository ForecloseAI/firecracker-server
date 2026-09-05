package chat

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// modelBroker is the guest listener with only the model broker on it.
func modelBroker(upstream string) http.Handler {
	return guestRoutes(nil, NewLLMGateway("sk-ant-host", mustURL(upstream)))
}

// The happy path. The SDK's own path and query -- ?beta=true included -- must
// reach the model unchanged, the host key must be added, and the two headers
// the SDK depends on must survive: drop anthropic-version or anthropic-beta and
// every turn fails.
func TestTheBrokerAddsTheKeyAndKeepsThePath(t *testing.T) {
	up := newUpstream(t)
	h := modelBroker(up.srv.URL)
	rec := askGuest(h, http.MethodPost, "/v1/messages?beta=true", "172.16.0.2", map[string]string{
		"Content-Type": "application/json", "Anthropic-Version": "2023-06-01",
		"Anthropic-Beta": "context-management-2025-06-27", "X-Api-Key": "brokered"})
	if rec.Code != http.StatusOK || rec.Body.String() != "upstream ok" {
		t.Fatalf("status %d body %q", rec.Code, rec.Body)
	}
	if up.path != "/v1/messages" || up.query != "beta=true" {
		t.Errorf("upstream saw %s?%s", up.path, up.query)
	}
	if up.hdr.Get("x-api-key") != "sk-ant-host" {
		t.Errorf("upstream saw x-api-key %q", up.hdr.Get("x-api-key"))
	}
	if up.hdr.Get("anthropic-version") != "2023-06-01" ||
		up.hdr.Get("anthropic-beta") != "context-management-2025-06-27" {
		t.Errorf("anthropic headers arrived as %q / %q",
			up.hdr.Get("anthropic-version"), up.hdr.Get("anthropic-beta"))
	}
	if up.body != `{"jsonrpc":"2.0"}` {
		t.Errorf("body arrived as %q", up.body)
	}
	askGuest(h, http.MethodPost, "/v1/messages/count_tokens?beta=true", "172.16.0.2", nil)
	if up.path != "/v1/messages/count_tokens" {
		t.Errorf("count_tokens arrived as %s", up.path)
	}
}

// A guest is untrusted, so what it sends is replaced rather than edited: its own
// key, any bearer token, cookies and forwarding headers must not reach the model.
func TestAGuestCannotSmuggleCredentialsToTheModel(t *testing.T) {
	up := newUpstream(t)
	askGuest(modelBroker(up.srv.URL), http.MethodPost, "/v1/messages", "172.16.0.2", map[string]string{
		"X-Api-Key": "sk-ant-guest", "Authorization": "Bearer stolen", "Proxy-Authorization": "x",
		"Cookie": "session=1", "X-Forwarded-For": "1.2.3.4"})
	if up.hdr.Get("x-api-key") != "sk-ant-host" {
		t.Errorf("the guest's key reached the model: %q", up.hdr.Get("x-api-key"))
	}
	for _, h := range []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Forwarded-For"} {
		if v := up.hdr.Get(h); v != "" {
			t.Errorf("%s reached the model as %q", h, v)
		}
	}
}

// The gate is the source address, and it is the guest grid exactly: loopback,
// other private ranges, the host's own tap address and an off-grid address are
// all refused with a 404 before the model is dialled -- the same 404 as an
// unknown path, so nothing about the listener can be probed.
func TestOnlyAGuestAddressMayUseTheModelBroker(t *testing.T) {
	up := newUpstream(t)
	h := modelBroker(up.srv.URL)
	for _, from := range []string{"127.0.0.1", "10.0.0.5", "172.17.0.2", "192.168.1.2", "172.16.0.1", "172.16.0.3"} {
		if rec := askGuest(h, http.MethodPost, "/v1/messages", from, nil); rec.Code != http.StatusNotFound {
			t.Errorf("from %s: status %d", from, rec.Code)
		}
	}
	if up.hits != 0 {
		t.Fatalf("the model was dialled %d times for refused requests", up.hits)
	}
	if rec := askGuest(h, http.MethodPost, "/v1/messages", "172.16.0.6", nil); rec.Code != http.StatusOK {
		t.Errorf("slot 1's guest was refused: %d", rec.Code)
	}
}

// The key is lent for turns and nothing wider: only the two message endpoints,
// and only POST. Everything else the key could reach is 404.
func TestOnlyTheTwoMessagePathsAreProxied(t *testing.T) {
	up := newUpstream(t)
	h := modelBroker(up.srv.URL)
	refused := []struct{ method, path string }{
		{http.MethodGet, "/v1/messages"}, {http.MethodPost, "/v1/models"},
		{http.MethodPost, "/v1/complete"}, {http.MethodPost, "/v1/messages/batches"},
		{http.MethodPost, "/v1/messages/"}, {http.MethodPost, "/v1/files"},
	}
	for _, r := range refused {
		if rec := askGuest(h, r.method, r.path, "172.16.0.2", nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status %d", r.method, r.path, rec.Code)
		}
	}
	if up.hits != 0 {
		t.Fatalf("the model was dialled %d times for refused paths", up.hits)
	}
}

// A streamed answer must reach the guest as it is produced. The recorder cannot
// show that, so this runs a real server whose handler stamps a guest address on
// the request: the first event has to arrive while the model is still holding
// the response open.
func TestAStreamedResponseIsFlushedAsItArrives(t *testing.T) {
	release := make(chan struct{})
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "event: message_start\n\n")
		http.NewResponseController(w).Flush()
		<-release
	}))
	t.Cleanup(model.Close)
	broker := modelBroker(model.URL)
	listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = "172.16.0.2:40000"
		broker.ServeHTTP(w, r)
	}))
	t.Cleanup(listener.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	res, err := client.Post(listener.URL+"/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	first, err := bufio.NewReader(res.Body).ReadString('\n')
	close(release)
	if err != nil || first != "event: message_start\n" {
		t.Fatalf("first line %q, %v: the stream was buffered", first, err)
	}
}

// When the model cannot be reached the guest gets a plain 502 that names
// neither the key nor the upstream. The journal may name the upstream -- it is
// no secret -- but never the key.
func TestAnUpstreamFailureNeverNamesTheKey(t *testing.T) {
	buf := captureLog(t)
	rec := askGuest(modelBroker("http://127.0.0.1:1"), http.MethodPost, "/v1/messages", "172.16.0.2", nil)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "sk-ant-host") || strings.Contains(body, "127.0.0.1") {
		t.Errorf("the failure told the guest too much: %q", body)
	}
	if strings.Contains(buf.String(), "sk-ant-host") {
		t.Errorf("the key reached the journal: %q", buf)
	}
}

// One line per guest request, saying who, what and how it went -- and never
// the body, which is the person's own prompt.
func TestEveryGuestRequestIsLoggedWithoutItsBody(t *testing.T) {
	buf := captureLog(t)
	up := newUpstream(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader("the person's private prompt"))
	req.RemoteAddr = "172.16.0.2:51234"
	modelBroker(up.srv.URL).ServeHTTP(httptest.NewRecorder(), req)
	line := buf.String()
	for _, want := range []string{"172.16.0.2", "POST /v1/messages", "200"} {
		if !strings.Contains(line, want) {
			t.Errorf("log %q lacks %q", line, want)
		}
	}
	if strings.Contains(line, "private prompt") {
		t.Errorf("the body reached the log: %q", line)
	}
}

// An apps ticket is half of what lets a request through, so the shared log line
// must not carry it even though it is in the path.
func TestTheGuestLogNeverCarriesATicket(t *testing.T) {
	buf := captureLog(t)
	askGuest(guestRoutes(NewAppsGateway("k", "0.0.0.0:8092"), nil), http.MethodPost,
		"/apps/deadbeefcafe/mcp", "172.16.0.2", nil)
	if line := buf.String(); strings.Contains(line, "deadbeefcafe") || !strings.Contains(line, "/apps/") {
		t.Errorf("log %q", line)
	}
}

// Each broker is mounted only when it exists, so a listener with just one of
// them 404s the other's prefix rather than routing it somewhere surprising.
func TestEachBrokerIsMountedOnlyWhenPresent(t *testing.T) {
	up := newUpstream(t)
	if rec := askGuest(modelBroker(up.srv.URL), http.MethodPost, "/apps/anything", "172.16.0.2", nil); rec.Code != http.StatusNotFound {
		t.Errorf("/apps/ answered %d with no apps broker", rec.Code)
	}
	appsOnly := guestRoutes(NewAppsGateway("k", "0.0.0.0:8092"), nil)
	if rec := askGuest(appsOnly, http.MethodPost, "/v1/messages", "172.16.0.2", nil); rec.Code != http.StatusNotFound {
		t.Errorf("/v1/ answered %d with no model broker", rec.Code)
	}
	if up.hits != 0 {
		t.Errorf("the model was dialled %d times", up.hits)
	}
}
