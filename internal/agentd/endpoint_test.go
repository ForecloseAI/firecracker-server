package agentd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// routeFixture is a guest's routing table: the tap's own /30, a default-looking
// route on lo with no gateway, and the real default route through the host.
const routeFixture = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n" +
	"eth0\t000010AC\t00000000\t0001\t0\t0\t0\tFCFFFFFF\t0\t0\t0\n" +
	"lo\t00000000\t00000000\t0001\t0\t0\t0\t00000000\t0\t0\t0\n" +
	"eth0\t00000000\t010010AC\t0003\t0\t0\t0\t00000000\t0\t0\t0\n"

// clearModelEnv empties every variable the SDK reads, so a developer's own
// credential cannot leak into a test that means to exercise the broker path.
func clearModelEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL"} {
		t.Setenv(k, "")
	}
}

// useRouteTable points the gateway lookup at a file for the rest of the test.
func useRouteTable(t *testing.T, path string) {
	t.Helper()
	was := routeTable
	routeTable = path
	t.Cleanup(func() { routeTable = was })
}

// The guest's broker is its default gateway, read from the kernel rather than
// configured: an address baked into the image would be every machine unable to
// take a turn the day the addressing changed. The table has lines that must not
// be mistaken for it -- the tap's own subnet, and a default-looking route with
// no gateway -- and the address is little-endian, which is easy to get backwards.
func TestGatewayIsReadFromTheDefaultRoute(t *testing.T) {
	gw, ok := parseRoute(routeFixture)
	if !ok || gw != "172.16.0.1" {
		t.Fatalf("parseRoute = %q, %v; want 172.16.0.1", gw, ok)
	}
	if gw, ok := parseRoute("Iface\tDestination\n"); ok {
		t.Fatalf("an empty table yielded %q", gw)
	}
}

// A brokered endpoint with no gateway cannot work, and the startup line has to
// say so rather than every turn failing with a connection error.
func TestNoDefaultRouteIsAnError(t *testing.T) {
	clearModelEnv(t)
	useRouteTable(t, filepath.Join(t.TempDir(), "missing"))
	ep := defaultEndpoint()
	if ep.err == nil || ep.baseURL != "" {
		t.Fatalf("endpoint without a route: %+v", ep)
	}
	if !strings.Contains(ep.String(), "no broker") {
		t.Errorf("startup line does not say so: %q", ep.String())
	}
}

// A developer's own key must keep working exactly as before: no broker, the
// SDK's own environment handling, ANTHROPIC_BASE_URL included.
func TestAnEnvCredentialWinsOverTheBroker(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-x")
	if ep := defaultEndpoint(); ep != (endpoint{}) {
		t.Fatalf("with a key in the environment the endpoint was %+v", ep)
	}
	if line := DescribeEndpoint(); !strings.Contains(line, "environment") {
		t.Errorf("startup line %q", line)
	}
}

// With no credential, an explicit base URL names the broker -- a guest that is
// told where to go instead of reading the route table, and every test here.
func TestBrokeredModeHonoursABaseURLOverride(t *testing.T) {
	clearModelEnv(t)
	t.Setenv("ANTHROPIC_BASE_URL", "http://x:1")
	ep := defaultEndpoint()
	if ep.baseURL != "http://x:1" || ep.key != brokerKey || ep.err != nil {
		t.Fatalf("endpoint = %+v", ep)
	}
}

// modelSpy stands in for the model service and remembers what it was asked.
type modelSpy struct {
	srv                      *httptest.Server
	path, key, version, beta string
}

// modelReply is the smallest assistant message the SDK accepts: one text block,
// a final stop reason and a usage block for the meter.
const modelReply = `{"id":"msg_test","type":"message","role":"assistant","model":"claude-haiku-4-5",` +
	`"content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","stop_sequence":null,` +
	`"usage":{"input_tokens":12,"output_tokens":3,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}}`

// fakeModel answers every request with modelReply.
func fakeModel(t *testing.T) *modelSpy {
	t.Helper()
	spy := &modelSpy{}
	spy.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		spy.path, spy.key = r.URL.Path, r.Header.Get("x-api-key")
		spy.version, spy.beta = r.Header.Get("anthropic-version"), r.Header.Get("anthropic-beta")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(modelReply))
	}))
	t.Cleanup(spy.srv.Close)
	return spy
}

// The first turn in this suite that reaches a model. Before the broker there
// was no seam: the client was built with no options and read only the
// environment. With one, a fake model service can stand in and prove a whole
// turn -- the request, the headers the broker must forward, the usage booked --
// without a key or a network.
func TestATurnReachesTheModelThroughTheBaseURLSeam(t *testing.T) {
	clearModelEnv(t)
	fake := fakeModel(t)
	t.Setenv("ANTHROPIC_BASE_URL", fake.srv.URL)
	a, err := New("boss", t.TempDir(), t.TempDir(), testProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Turn(context.Background(), "hi"); err != nil {
		t.Fatalf("turn: %v", err)
	}
	if fake.path != "/v1/messages" || fake.key != brokerKey {
		t.Errorf("the model saw %s with key %q", fake.path, fake.key)
	}
	if fake.version == "" || !strings.Contains(fake.beta, "context-management-2025-06-27") {
		t.Errorf("anthropic-version %q anthropic-beta %q", fake.version, fake.beta)
	}
	assertTurnLogged(t, a)
}

// assertTurnLogged checks the turn left the events a real one does: the text,
// the usage against the right model, a clean completion and no error.
func assertTurnLogged(t *testing.T, a *Agent) {
	t.Helper()
	events, err := a.Log().Since(0)
	if err != nil {
		t.Fatal(err)
	}
	var usage, text, done bool
	for _, e := range events {
		switch e.Type {
		case "usage":
			usage = e.Model == "claude-haiku-4-5" && e.Usage.InputTokens == 12
		case "text":
			text = e.Text == "hello"
		case "turn_complete":
			done = !e.IsError
		case "error":
			t.Errorf("error logged: %s", e.Message)
		}
	}
	if !usage || !text || !done {
		t.Errorf("usage %v text %v done %v", usage, text, done)
	}
	if n := len(a.Messages()); n != 2 {
		t.Errorf("history has %d messages, want the question and the answer", n)
	}
}
