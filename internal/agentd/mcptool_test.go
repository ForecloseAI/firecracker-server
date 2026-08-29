package agentd

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSource is an mcpSession over a real in-process MCP server, so a wrapper
// under test drives the same tools/call path a registered server will.
type fakeSource struct {
	name string
	srv  *mcpsdk.Server

	mu     sync.Mutex
	sess   *mcpsdk.ClientSession
	called []string // the tool name every call asked the SERVER for
}

// newFakeSource serves the given tools in-process.
func newFakeSource(t *testing.T, tools ...*mcpsdk.Tool) *fakeSource {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fake", Version: "1"}, nil)
	f := &fakeSource{name: "the test server", srv: srv}
	for _, tool := range tools {
		f.add(tool, cannedResult(tool))
	}
	return f
}

// add registers a tool and records the name each call arrives under.
func (f *fakeSource) add(tool *mcpsdk.Tool, h mcpsdk.ToolHandler) {
	f.srv.AddTool(tool, func(ctx context.Context, r *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		f.mu.Lock()
		f.called = append(f.called, r.Params.Name)
		f.mu.Unlock()
		return h(ctx, r)
	})
}

func (f *fakeSource) label() string { return f.name }

// session connects on first use, over the SDK's in-memory transport.
func (f *fakeSource) session(ctx context.Context) (*mcpsdk.ClientSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sess != nil {
		return f.sess, nil
	}
	client, server := mcpsdk.NewInMemoryTransports()
	if _, err := f.srv.Connect(ctx, server, nil); err != nil {
		return nil, err
	}
	sess, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, client, nil)
	if err != nil {
		return nil, err
	}
	f.sess = sess
	return sess, nil
}

func (f *fakeSource) drop(dead *mcpsdk.ClientSession) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sess == dead {
		f.sess = nil
	}
}

// reconnect forgets the cached session so the next one is new, which is what a
// real dial does after a holder closed the connection it was given.
func (f *fakeSource) reconnect() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sess = nil
}

// deadFirstSource hands out one session whose server side is already gone, and
// healthy ones after that, so a test can see what a wrapper does when a call
// fails at the TRANSPORT rather than at the tool -- the shape a restarted
// server, or a connection dropped mid-call, actually takes.
type deadFirstSource struct {
	*fakeSource

	mu    sync.Mutex
	dials int
}

// dialCount is how many times a wrapper asked for a session. Two means it
// retried.
func (d *deadFirstSource) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.dials
}

func (d *deadFirstSource) session(ctx context.Context) (*mcpsdk.ClientSession, error) {
	d.mu.Lock()
	d.dials++
	first := d.dials == 1
	d.mu.Unlock()
	if !first {
		d.fakeSource.reconnect()
		return d.fakeSource.session(ctx)
	}
	client, server := mcpsdk.NewInMemoryTransports()
	serving, err := d.fakeSource.srv.Connect(ctx, server, nil)
	if err != nil {
		return nil, err
	}
	sess, err := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil).
		Connect(ctx, client, nil)
	if err != nil {
		return nil, err
	}
	serving.Close() // the far end goes away; the next call fails on the wire
	return sess, nil
}

// TestARegisteredToolIsNotRunTwiceAfterATransportFailure is the duplicate side
// effect.
//
// A transport that dies mid-call looks identical whether or not the server ran
// the tool, so replaying an arbitrary MCP operation files the issue -- or sends
// the message -- twice, with nothing anywhere reporting it. One attempt, and an
// answer that tells the model the outcome is unknown so it can decide.
func TestARegisteredToolIsNotRunTwiceAfterATransportFailure(t *testing.T) {
	f := newFakeSource(t, namedTool("create_issue", "filed"))
	src := &deadFirstSource{fakeSource: f}
	tools := wrapSpecs([]mcpToolSpec{specFor("create_issue")}, src, "linear", 5*time.Second)

	got := textOf(runTool(t, tools, "mcp__linear__create_issue", `{}`))
	if n := src.dialCount(); n != 1 {
		t.Errorf("the tool asked for %d sessions, want 1: a second one is a replayed call", n)
	}
	if asked := f.asked(); len(asked) != 0 {
		t.Errorf("the server was asked for %v after a dead transport, want no replay", asked)
	}
	if !strings.Contains(got, "may or may not have run") {
		t.Errorf("the model was told %q, which does not say the outcome is unknown", got)
	}
}

// TestTheBrowserStillRetriesAfterATransportFailure is the behaviour the opt-out
// above must not have taken away. Chrome restarts on Restart=always and takes
// the connection with it; without the retry the model keeps calling a corpse.
func TestTheBrowserStillRetriesAfterATransportFailure(t *testing.T) {
	f := newFakeSource(t, namedTool("click", "clicked"))
	src := &deadFirstSource{fakeSource: f}
	tools := wrapAll([]*mcpsdk.Tool{namedTool("click", "clicked")}, src, toolDeps{})

	if got := textOf(runTool(t, tools, "click", `{}`)); got != "clicked" {
		t.Errorf("the browser did not recover from a restart: %q", got)
	}
	if n := src.dialCount(); n != 2 {
		t.Errorf("the browser asked for %d sessions, want 2: it did not retry", n)
	}
}

// asked reports the tool names the server was actually sent.
func (f *fakeSource) asked() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.called...)
}

// specFor builds a stored spec for a tool the fixture serves.
func specFor(name string) mcpToolSpec {
	return mcpToolSpec{Name: name, Desc: name + " does a thing",
		Schema: json.RawMessage(`{"type":"object"}`)}
}

// textOf joins the text a tool result carried, so a test can assert on what
// the model actually reads rather than on the block shape.
func textOf(blocks []anthropic.BetaToolResultBlockParamContentUnion) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.OfText != nil {
			sb.WriteString(b.OfText.Text)
		}
	}
	return sb.String()
}

// TestAPrefixedToolCallsTheServerByItsBareName is the load-bearing test of the
// whole namespacing scheme.
//
// The model sees mcp__notion__search_pages; the server only knows search_pages.
// Sending the model-facing name over the wire would fail every registered tool
// at CALL time -- after registration reported success and the schema reached the
// model -- and would be visible nowhere but a transcript.
func TestAPrefixedToolCallsTheServerByItsBareName(t *testing.T) {
	f := newFakeSource(t, namedTool("search_pages", "two pages"))
	tools := wrapSpecs([]mcpToolSpec{specFor("search_pages")}, f, "notion", time.Second)
	if len(tools) != 1 {
		t.Fatalf("wrapped %d tools, want 1", len(tools))
	}
	if got := tools[0].Name(); got != "mcp__notion__search_pages" {
		t.Errorf("the model is offered %q, want mcp__notion__search_pages", got)
	}
	if got := textOf(runTool(t, tools, "mcp__notion__search_pages", `{}`)); got != "two pages" {
		t.Errorf("the call returned %q, want the server's answer", got)
	}
	if asked := f.asked(); len(asked) != 1 || asked[0] != "search_pages" {
		t.Errorf("the server was asked for %v, want [search_pages]", asked)
	}
}

// TestAStructuredOnlyResultStillReachesTheModel covers a server that answers
// with structuredContent and no content. The spec allows it and
// chrome-devtools-mcp never does it, so without the fallback the model gets an
// empty result and is told nothing went wrong.
func TestAStructuredOnlyResultStillReachesTheModel(t *testing.T) {
	f := newFakeSource(t)
	tool := namedTool("lookup", "")
	f.add(tool, func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{StructuredContent: map[string]any{"found": 3}}, nil
	})
	tools := wrapSpecs([]mcpToolSpec{specFor("lookup")}, f, "erp", time.Second)
	if got := textOf(runTool(t, tools, "mcp__erp__lookup", `{}`)); !strings.Contains(got, "found") {
		t.Errorf("a structured-only result reached the model as %q", got)
	}
}

// TestASilentServerFailsTheCallRatherThanTheTurn covers a server that accepts a
// connection and then says nothing. Without a per-call bound it would hold the
// turn open until the person gave up.
func TestASilentServerFailsTheCallRatherThanTheTurn(t *testing.T) {
	f := newFakeSource(t)
	tool := namedTool("hang", "")
	f.add(tool, func(ctx context.Context, _ *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	tools := wrapSpecs([]mcpToolSpec{specFor("hang")}, f, "slow", 200*time.Millisecond)
	start := time.Now()
	got := textOf(runTool(t, tools, "mcp__slow__hang", `{}`))
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the call took %s, so the turn was held open", took)
	}
	if !strings.Contains(got, "the test server") {
		t.Errorf("the failure text %q does not name the server that failed", got)
	}
}

// TestTheBrowserKeepsNoPerCallTimeout pins that the browser still rides the
// turn's context. A deadline here would cut a legitimately slow page action.
func TestTheBrowserKeepsNoPerCallTimeout(t *testing.T) {
	srv := fakeServer(t, 0, namedTool("click", "clicked"))
	tools, err := srv.Tools(context.Background(), depsWithStore(t, srv))
	if err != nil {
		t.Fatal(err)
	}
	bare, ok := tools[0].(*mcpTool)
	if !ok {
		t.Fatalf("browser tool is %T, want *mcpTool", tools[0])
	}
	if bare.callTimeout != 0 {
		t.Errorf("the browser grew a %s per-call timeout", bare.callTimeout)
	}
	if bare.name != bare.wire || bare.name != "click" {
		t.Errorf("browser tool is name=%q wire=%q, want both bare", bare.name, bare.wire)
	}
}

// TestASnapshotFromARegisteredServerIsNotDigested keeps the browser's snapshot
// spill off somebody else's tool. A registered server advertising take_snapshot
// would otherwise have its result rewritten and spilled into the browser store.
func TestASnapshotFromARegisteredServerIsNotDigested(t *testing.T) {
	long := strings.Repeat("page text ", 800)
	f := newFakeSource(t, namedTool(snapshotToolName, long))
	tools := wrapSpecs([]mcpToolSpec{specFor(snapshotToolName)}, f, "other", time.Second)
	if got := textOf(runTool(t, tools, modelName("other", snapshotToolName), `{}`)); got != long {
		t.Errorf("a registered %s was rewritten: %d chars back, want %d",
			snapshotToolName, len(got), len(long))
	}
}

// TestAToolNameTooLongForTheAPIIsNotOffered keeps a name the Messages API would
// reject out of the surface, rather than letting it 400 the person's next turn.
func TestAToolNameTooLongForTheAPIIsNotOffered(t *testing.T) {
	f := newFakeSource(t)
	long := strings.Repeat("x", 130)
	tools := wrapSpecs([]mcpToolSpec{specFor(long), specFor("ok")}, f, "srv", time.Second)
	if len(tools) != 1 || tools[0].Name() != "mcp__srv__ok" {
		t.Errorf("offered %d tools, want only the usable one", len(tools))
	}
	if validToolName(modelName("srv", long)) {
		t.Error("an over-long name passed validation")
	}
}
