package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeApps stands a real MCP server up in process, so these tests exercise the
// same handshake, tools/list and tools/call the guest will.
func fakeApps(t *testing.T, tools ...*mcpsdk.Tool) *appsServer {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fake-apps", Version: "1"}, nil)
	for _, tool := range tools {
		srv.AddTool(tool, cannedResult(tool))
	}
	a := newAppsServer("https://backend.composio.dev/mcp/test")
	a.dial = func(ctx context.Context, _ string) (*mcpsdk.ClientSession, error) {
		return connectInMemory(ctx, srv)
	}
	return a
}

// A machine with no session offers no tools, and that is not an error: it is
// every VM until the host pushes one, and it is what keeps this feature off
// without a flag of its own.
func TestNoSessionMeansNoTools(t *testing.T) {
	tools, err := newAppsServer("").Tools(context.Background(), toolDeps{})
	if err != nil {
		t.Fatalf("an unconfigured machine errored: %v", err)
	}
	if len(tools) != 0 {
		t.Errorf("%d tools were offered", len(tools))
	}
}

// The session's surface reaches the model unfiltered: it was configured
// host-side to advertise exactly what we want, so a second allow list here would
// only be a place for the two to disagree.
func TestSessionToolsReachTheModel(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_SEARCH_TOOLS", "found"),
		namedTool("COMPOSIO_MULTI_EXECUTE_TOOL", "done"))
	tools, err := a.Tools(context.Background(), toolDeps{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("got %d tools", len(tools))
	}
}

// THE test for this file. A wrapped tool closes over ONE agent's deps -- its
// gate, its log -- so a surface built for the first agent and handed to the
// second would raise that agent's approvals in someone else's transcript. The
// listing is cached; the wrapping must not be.
func TestToolsAreWrappedPerAgentNotCached(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_MULTI_EXECUTE_TOOL", "done"))
	first, err := a.Tools(context.Background(), toolDeps{self: "boss"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Tools(context.Background(), toolDeps{self: "cody"})
	if err != nil {
		t.Fatal(err)
	}
	if first[0] == second[0] {
		t.Fatal("two agents were handed the same wrapped tool")
	}
	if got := second[0].(*mcpTool).deps.self; got != "cody" {
		t.Errorf("the second agent's tool belongs to %q", got)
	}
}

// Pointing the machine at a new session drops what was held for the old one, so
// a re-push after a host restart cannot leave agents dialling a dead endpoint.
func TestSetURLDropsTheOldSession(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_SEARCH_TOOLS", "found"))
	if _, err := a.Tools(context.Background(), toolDeps{}); err != nil {
		t.Fatal(err)
	}
	a.SetURL("https://backend.composio.dev/mcp/other")
	if a.listed != nil || a.sess != nil {
		t.Error("the old session survived a repoint")
	}
	if a.Current() != "https://backend.composio.dev/mcp/other" {
		t.Errorf("Current is %q", a.Current())
	}
}

// Re-pushing the same session must not throw away a live connection and the
// tools/list it already paid for. The host pushes on every boot.
func TestSetURLToTheSameSessionKeepsIt(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_SEARCH_TOOLS", "found"))
	if _, err := a.Tools(context.Background(), toolDeps{}); err != nil {
		t.Fatal(err)
	}
	a.SetURL(a.Current())
	if a.listed == nil || a.sess == nil {
		t.Error("an unchanged push dropped the session")
	}
}

// Connected-app calls are NOT gated today, deliberately: the permission layer
// is still to be written and a name-shaped guess at it let writes through while
// reading as enforcement. This test pins that state so the day somebody wires a
// hook in, it fails and reminds them to say so here.
func TestConnectedAppCallsAreNotGatedYet(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_MULTI_EXECUTE_TOOL", "sent"))
	gate := NewGate(mustLog(t), NewInteractions(), t.TempDir())
	tools, err := a.Tools(context.Background(), toolDeps{gate: gate})
	if err != nil {
		t.Fatal(err)
	}
	if got := tools[0].(*mcpTool).before; got != nil {
		t.Fatal("something is gating connected-app calls; update this test and the docs")
	}
	// A send reaches the provider with nobody asked. That is the current contract.
	args, _ := json.Marshal(map[string]any{"tools": []any{
		map[string]any{"tool_slug": "GMAIL_SEND_EMAIL", "arguments": map[string]any{}}}})
	out, err := tools[0].Execute(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if got := blockText(out); !strings.Contains(got, "sent") {
		t.Errorf("the call did not reach the provider: %q", got)
	}
}

// A dial that failed is remembered briefly. Building an agent is in the path of
// a person's message, so without this a provider having a bad ten minutes would
// add the full dial timeout to every message sent to an evicted agent.
func TestAFailedDialIsNotRetriedImmediately(t *testing.T) {
	a := newAppsServer("https://backend.composio.dev/mcp/test")
	dials := 0
	a.dial = func(context.Context, string) (*mcpsdk.ClientSession, error) {
		dials++
		return nil, errNotDialable // any failure will do
	}
	for range 3 {
		if _, err := a.Tools(context.Background(), toolDeps{}); err == nil {
			t.Fatal("a broken session reported tools")
		}
	}
	if dials != 1 {
		t.Errorf("dialled %d times during the cooldown", dials)
	}
}

// The cooldown is not permanent: a provider that comes back is picked up.
func TestTheCooldownExpires(t *testing.T) {
	a := fakeApps(t, namedTool("COMPOSIO_SEARCH_TOOLS", "found"))
	a.failedAt = time.Now().Add(-appsRetryAfter - time.Second)
	tools, err := a.Tools(context.Background(), toolDeps{})
	if err != nil {
		t.Fatalf("a recovered session stayed shut: %v", err)
	}
	if len(tools) != 1 {
		t.Errorf("got %d tools", len(tools))
	}
	if !a.failedAt.IsZero() {
		t.Error("a success left the failure marker set")
	}
}

// THE test for this file's security property. The session URL is a bearer
// credential for everything the person has connected, and transport errors wrap
// url.Error, which embeds the whole request URL. That string becomes a tool
// result the model reads and an entry in the event log -- so a prompt injection
// could read the capability straight out of its own context.
func TestTheSessionURLNeverTravelsInAnError(t *testing.T) {
	const url = "https://backend.composio.dev/mcp/sess_SECRET_CAPABILITY"
	a := newAppsServer(url)
	a.dial = func(_ context.Context, endpoint string) (*mcpsdk.ClientSession, error) {
		// Shaped like what the transport really returns.
		return nil, fmt.Errorf(`Post %q: dial tcp: lookup failed`, endpoint)
	}

	_, err := a.Tools(context.Background(), toolDeps{})
	if err == nil {
		t.Fatal("a broken dial reported tools")
	}
	if strings.Contains(err.Error(), "sess_SECRET_CAPABILITY") {
		t.Errorf("the session url leaked into a Tools error: %v", err)
	}
	if !strings.Contains(err.Error(), sessionPlaceholder) {
		t.Errorf("the error lost the fact that it was about the session: %v", err)
	}

	// The same on the path that reaches the model: session() feeds mcpTool.call,
	// whose error becomes the tool result.
	a.failedAt = time.Time{}
	if _, err := a.session(context.Background()); strings.Contains(err.Error(), "sess_SECRET_CAPABILITY") {
		t.Errorf("the session url leaked into a call error: %v", err)
	}
	if got := a.redact(fmt.Errorf("wrapped: %s", url)); strings.Contains(got.Error(), "sess_SECRET") {
		t.Errorf("redact let it through: %v", got)
	}

	// The case a plain substring match misses: the client normalised what we
	// dialled -- here a trailing slash -- so the error carries a URL that is not
	// the string we stored. Walking the chain for the *url.Error's own URL is
	// what catches it.
	normalised := &neturl.Error{Op: "Post", URL: url + "/", Err: errors.New("dial tcp")}
	if got := a.redact(normalised); strings.Contains(got.Error(), "sess_SECRET_CAPABILITY") {
		t.Errorf("a normalised url leaked: %v", got)
	}
}

// blockText joins the text a tool result carried.
func blockText(blocks []anthropic.BetaToolResultBlockParamContentUnion) string {
	var out strings.Builder
	for _, b := range blocks {
		if b.OfText != nil {
			out.WriteString(b.OfText.Text)
		}
	}
	return out.String()
}

// THE test for the locking. A cold dial crosses the internet and can hang for
// the full timeout; while it does, every other caller must still be served.
// Holding the state lock across it -- which is what browserServer does, safely,
// because its dial is a local exec -- blocked a live agent's tool call, the
// error path reporting the failure, and the host's own PUT /apps.
func TestADialInFlightBlocksNobody(t *testing.T) {
	a := newAppsServer("https://backend.composio.dev/mcp/test")
	dialing, release := make(chan struct{}), make(chan struct{})
	a.dial = func(ctx context.Context, _ string) (*mcpsdk.ClientSession, error) {
		close(dialing)
		<-release
		return nil, errNoSession
	}
	go a.Tools(context.Background(), toolDeps{})
	<-dialing // a dial is now in flight and will not finish until we say so

	done := make(chan string, 3)
	go func() { a.SetURL("https://backend.composio.dev/mcp/other"); done <- "SetURL" }()
	go func() { a.redact(errNoSession); done <- "redact" }()
	go func() { a.Current(); done <- "Current" }()
	for range 3 {
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			close(release)
			t.Fatal("a caller was blocked behind the dial")
		}
	}
	close(release)
}
