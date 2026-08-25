package agentd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeServer stands a REAL MCP server up in-process and points a browserServer
// at it over the SDK's in-memory transport.
//
// Not a hand-written stub of the client: this exercises the same tools/list,
// tools/call and content decoding the guest will, so a mistake in how we drive
// the SDK shows up here rather than on a VM.
func fakeServer(t *testing.T, pageSize int, tools ...*mcpsdk.Tool) *browserServer {
	t.Helper()
	srv := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "fake-chrome", Version: "1"},
		&mcpsdk.ServerOptions{PageSize: pageSize})
	for _, tool := range tools {
		srv.AddTool(tool, cannedResult(tool))
	}
	b := newBrowserServer("http://127.0.0.1:9222")
	b.dial = func(ctx context.Context) (*mcpsdk.ClientSession, error) {
		client, server := mcpsdk.NewInMemoryTransports()
		if _, err := srv.Connect(ctx, server, nil); err != nil {
			return nil, err
		}
		return mcpsdk.NewClient(&mcpsdk.Implementation{Name: "test", Version: "1"}, nil).
			Connect(ctx, client, nil)
	}
	return b
}

// cannedResult answers a tool call with whatever the fixture stored in the
// tool's Title, so each test can say what the browser "returned".
func cannedResult(tool *mcpsdk.Tool) mcpsdk.ToolHandler {
	return func(context.Context, *mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		if tool.Title == "ERROR" {
			return &mcpsdk.CallToolResult{IsError: true,
				Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "the page said no"}}}, nil
		}
		if tool.Title == "IMAGE" {
			return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
				&mcpsdk.ImageContent{Data: []byte{0xff, 0xd8, 0xff}, MIMEType: "image/jpeg"}}}, nil
		}
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: tool.Title}}}, nil
	}
}

// namedTool builds a fixture tool whose canned answer is its Title.
func namedTool(name, answer string) *mcpsdk.Tool {
	return &mcpsdk.Tool{Name: name, Description: name + " does a thing", Title: answer,
		InputSchema: map[string]any{"type": "object"}}
}

// depsWithStore gives the browser tools a real snapshot store on a temp dir.
func depsWithStore(t *testing.T, srv *browserServer) toolDeps {
	t.Helper()
	store, err := newSnapshotStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return toolDeps{browser: true, chrome: srv, snaps: store, log: mustLog(t)}
}

// runTool drives one tool the way the runner would.
func runTool(t *testing.T, tools []anthropic.BetaTool, name string,
	args string) []anthropic.BetaToolResultBlockParamContentUnion {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() != name {
			continue
		}
		out, err := tool.Execute(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("%s returned a Go error, which the model reads as a broken tool: %v", name, err)
		}
		return out
	}
	t.Fatalf("no tool named %s in %d tools", name, len(tools))
	return nil
}

// THE one that matters. A content-rich page snapshots at 6-11k tokens and every
// tool result is re-sent on every later turn, so a handful of them dominate a
// conversation. The TypeScript digest was inert for an entire commit while
// files appeared correctly on disk and ~23k tokens a turn kept accruing, so
// asserting a file exists proves nothing -- this asserts what the model gets.
func TestSnapshotResultsAreStillDigested(t *testing.T) {
	huge := strings.Repeat("uid=1_1 link \"a line of a page that goes on\"\n", 4000) + "ZZMARKERZZ"
	srv := fakeServer(t, 0, namedTool("take_snapshot", huge))
	d := depsWithStore(t, srv)
	tools, err := srv.Tools(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	got := runTool(t, tools, "take_snapshot", `{}`)
	if len(got) != 1 || got[0].OfText == nil {
		t.Fatalf("result = %+v, want one text block", got)
	}
	text := got[0].OfText.Text
	if strings.Contains(text, "ZZMARKERZZ") {
		t.Error("the model still received the whole snapshot")
	}
	if !strings.Contains(text, "[full snapshot:") {
		t.Errorf("no trailer naming the spilled file: %q", text[max(0, len(text)-200):])
	}
}

// ListToolsResult carries a NextCursor, so a single tools/list can return a
// partial page. A tool that never arrives simply never reaches the model, and
// nothing anywhere reports it -- the agent is just quietly less capable.
func TestEveryPageOfToolsIsListed(t *testing.T) {
	srv := fakeServer(t, 1,
		namedTool("navigate_page", "ok"), namedTool("take_snapshot", "ok"),
		namedTool("click", "ok"), namedTool("fill", "ok"))
	tools, err := srv.Tools(context.Background(), toolDeps{browser: true, chrome: srv})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 4 {
		t.Errorf("got %d tools with a page size of 1, want all 4", len(tools))
	}
}

// The lazy-browser bug's exact shape, one layer along. Tools are built once per
// agent at construction, so a server that dies and respawns would leave every
// existing agent holding tools pointed at a corpse -- browser-less for the life
// of the process, with nothing but one line in a log to say so. Chrome restarts
// on Restart=always and takes the server's connection with it, so this is the
// ordinary case rather than an exotic one.
func TestToolsKeepWorkingAfterTheServerRestarts(t *testing.T) {
	srv := fakeServer(t, 0, namedTool("navigate_page", "opened"))
	tools, err := srv.Tools(context.Background(), toolDeps{browser: true, chrome: srv})
	if err != nil {
		t.Fatal(err)
	}
	if got := runTool(t, tools, "navigate_page", `{}`); got[0].OfText.Text != "opened" {
		t.Fatalf("first call = %q", got[0].OfText.Text)
	}
	srv.Close() // as a dying node process would
	got := runTool(t, tools, "navigate_page", `{}`)
	if got[0].OfText == nil || got[0].OfText.Text != "opened" {
		t.Errorf("after a restart the tool returned %+v, want it to reconnect", got[0])
	}
}

// The server advertises about 53 tools; the ones not named in browserAllowed
// must never reach the model, because every schema is charged on every turn.
func TestOnlyTheAllowlistedToolsReachTheModel(t *testing.T) {
	srv := fakeServer(t, 0,
		namedTool("take_snapshot", "ok"), namedTool("take_heapsnapshot", "ok"),
		namedTool("lighthouse_audit", "ok"), namedTool("evaluate_script", "ok"))
	tools, err := srv.Tools(context.Background(), toolDeps{browser: true, chrome: srv})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name() != "take_snapshot" {
		var names []string
		for _, tool := range tools {
			names = append(names, tool.Name())
		}
		t.Errorf("offered %v, want only take_snapshot", names)
	}
}

// A tool that says no is something the model can fix; a Go error becomes an
// is_error result it reads as a broken tool instead.
func TestAToolErrorComesBackAsTextNotAGoError(t *testing.T) {
	srv := fakeServer(t, 0, namedTool("click", "ERROR"))
	tools, err := srv.Tools(context.Background(), toolDeps{browser: true, chrome: srv})
	if err != nil {
		t.Fatal(err)
	}
	got := runTool(t, tools, "click", `{"uid":"1_1"}`)
	if got[0].OfText == nil || !strings.Contains(got[0].OfText.Text, "the page said no") {
		t.Errorf("result = %+v, want the server's own wording", got[0])
	}
}

// A screenshot has to arrive as an image block. Stringifying it would hand the
// model base64 it cannot see and cannot say it cannot see.
func TestAnImageResultStaysAnImage(t *testing.T) {
	srv := fakeServer(t, 0, namedTool("take_screenshot", "IMAGE"))
	tools, err := srv.Tools(context.Background(), toolDeps{browser: true, chrome: srv})
	if err != nil {
		t.Fatal(err)
	}
	got := runTool(t, tools, "take_screenshot", `{}`)
	if got[0].OfImage == nil {
		t.Fatalf("result = %+v, want an image block", got[0])
	}
	if got[0].OfImage.Source.OfBase64 == nil ||
		string(got[0].OfImage.Source.OfBase64.MediaType) != "image/jpeg" {
		t.Errorf("image source = %+v, want a base64 jpeg", got[0].OfImage.Source)
	}
}

// mcp.NewBetaTool round-trips the schema into a struct with fields for only
// properties, required and type, and its ExtraFields map lacks the tag the
// decoder needs -- so $defs, additionalProperties and anyOf are dropped with
// nothing reporting it and the model starts guessing arguments. We use
// BetaToolInputSchema instead, and this is what would catch a regression to the
// convenient path.
func TestTheWholeSchemaReachesTheModel(t *testing.T) {
	tool := namedTool("fill", "ok")
	tool.InputSchema = map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           map[string]any{"uid": map[string]any{"type": "string"}},
		"required":             []any{"uid"},
	}
	srv := fakeServer(t, 0, tool)
	tools, err := srv.Tools(context.Background(), toolDeps{browser: true, chrome: srv})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tools[0].InputSchema())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "additionalProperties") {
		t.Errorf("schema lost additionalProperties: %s", raw)
	}
	if !strings.Contains(string(raw), `"uid"`) {
		t.Errorf("schema lost its properties: %s", raw)
	}
}

// The live bug this preserves: attachBrowser used to connect at agent
// construction, so an agent built before Chrome was listening got NO browser
// tools for its whole life. agentd is deliberately not ordered after
// chrome.service, so that window is real -- the tools must exist and the
// failure must be text the model can retry.
func TestBrowserToolsSurviveTheServerBeingDown(t *testing.T) {
	srv := newBrowserServer("http://127.0.0.1:9222")
	srv.dial = func(context.Context) (*mcpsdk.ClientSession, error) {
		return nil, context.DeadlineExceeded
	}
	if _, err := srv.Tools(context.Background(), toolDeps{browser: true, chrome: srv}); err == nil {
		t.Fatal("a dead server listed tools")
	}
	// Not cached: the next agent built retries rather than inheriting the failure.
	if srv.tools != nil {
		t.Error("a failed listing was cached, so no later agent could ever get a browser")
	}
}

// Five of the six shipped profiles never open a page. Building their tools must
// not fork node, and thirteen supervisor tests would otherwise each start one.
func TestABrowserlessProfileStartsNothing(t *testing.T) {
	srv := fakeServer(t, 0, namedTool("take_snapshot", "ok"))
	tools, err := browserTools(toolDeps{browser: false, chrome: srv})
	if err != nil || tools != nil {
		t.Errorf("browserTools = %v, %v; want nothing for a browserless profile", tools, err)
	}
	if srv.sess != nil {
		t.Error("a browserless profile started the browser server")
	}
}
