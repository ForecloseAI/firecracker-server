package agentd

import (
	"context"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// managerWith registers one enabled server whose sessions come from a fake
// source, so the whole assembly path runs without a socket.
func managerWith(t *testing.T, src *fakeSource, tools ...string) (*MCPManager, mcpRecord) {
	t.Helper()
	store, err := LoadMCPStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	specs := make([]mcpToolSpec, 0, len(tools))
	for _, name := range tools {
		specs = append(specs, specFor(name))
	}
	rec, err := store.Add(mcpRecord{Name: "Notion", URL: "https://mcp.notion.com/mcp",
		Transport: transportHTTP, Tools: specs})
	if err != nil {
		t.Fatal(err)
	}
	m := NewMCPManager(store)
	m.dial = func(ctx context.Context, _ mcpRecord) (*mcpsdk.ClientSession, error) {
		return src.session(ctx)
	}
	return m, rec
}

// TestARegisteredToolReachesAProfileThatNamesOnlyItsOwn is THE test of this
// feature.
//
// keepAllowed drops any tool a profile does not name, silently, and every
// shipped profile but an empty-list one names its tools. A profile written
// months ago cannot name a server registered this morning, so without the merge
// in permitted() registration would report success, the tools would be built,
// and every one of them would be thrown away with nothing reporting it.
func TestARegisteredToolReachesAProfileThatNamesOnlyItsOwn(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "ok"))
	m, _ := managerWith(t, src, "search_pages")
	deps := toolDeps{mcp: m, log: mustLog(t)}

	tools, err := Tools(roots{workspace: t.TempDir(), own: t.TempDir()}, deps,
		[]string{"Read", "Grep"})
	if err != nil {
		t.Fatal(err)
	}
	if !hasTool(tools, "mcp__notion__search_pages") {
		t.Fatalf("a registered tool was dropped by the profile filter: %v", namesOf(tools))
	}
	if !hasTool(tools, "Read") {
		t.Error("the profile lost the tools it did name")
	}
	if hasTool(tools, "Bash") {
		t.Error("the merge widened the profile beyond what it allows")
	}
}

// TestBuildingAnAgentDialsNothing is what lets an agent start while a
// registered server is down, and what keeps a slow third party out of every
// cold start. The wrappers come off schemas captured at registration.
func TestBuildingAnAgentDialsNothing(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "ok"))
	m, _ := managerWith(t, src, "search_pages")
	m.dial = func(context.Context, mcpRecord) (*mcpsdk.ClientSession, error) {
		t.Fatal("building the tool surface opened a connection")
		return nil, nil
	}
	if got := len(m.Tools()); got != 1 {
		t.Errorf("built %d tools without dialling, want 1", got)
	}
}

// TestADisabledServerOffersNoTools is what disable has to mean for an agent
// built after the change.
func TestADisabledServerOffersNoTools(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "ok"))
	m, rec := managerWith(t, src, "search_pages")
	if _, err := m.Store().SetEnabled(rec.ID, false); err != nil {
		t.Fatal(err)
	}
	m.Refresh(rec.ID)
	if got := len(m.Tools()); got != 0 {
		t.Errorf("a disabled server still offers %d tools", got)
	}
}

// TestADisabledServerRefusesToReconnectMidTurn is the one that is easy to get
// wrong.
//
// A registered server's tools no longer retry, but the holder still redials on
// the NEXT call, so closing the session is not enough on its own: without the
// flag a running agent would quietly reconnect and disable would be decorative
// until that agent happened to be recycled.
//
// The store is written first and then Refresh is called, which is the order
// handlePatchMCP uses -- Refresh reads what the store now says rather than
// assuming every change is a retirement.
func TestADisabledServerRefusesToReconnectMidTurn(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "found"))
	m, rec := managerWith(t, src, "search_pages")
	tools := m.Tools() // an agent now holds these

	if got := textOf(runTool(t, tools, "mcp__notion__search_pages", `{}`)); got != "found" {
		t.Fatalf("the tool did not work before being disabled: %q", got)
	}
	if _, err := m.Store().SetEnabled(rec.ID, false); err != nil {
		t.Fatal(err)
	}
	m.Refresh(rec.ID)
	got := textOf(runTool(t, tools, "mcp__notion__search_pages", `{}`))
	if !strings.Contains(got, "turned off") {
		t.Errorf("a disabled server answered a live agent with %q", got)
	}
}

// TestReEnablingAServerRevivesTheAgentsAlreadyHoldingIt is the other half of
// the disable path, and the one that bricks an agent rather than merely
// inconveniencing it.
//
// A running agent's wrappers point at ONE holder object, because the surface is
// frozen when the agent is built. Retiring that object on disable and building a
// fresh one on re-enable leaves the agent bound to the retired one for the rest
// of its life -- told that a server which is switched on has been turned off --
// and nothing recycles an agent that never goes idle. The boss is exactly that
// agent.
func TestReEnablingAServerRevivesTheAgentsAlreadyHoldingIt(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "found"))
	m, rec := managerWith(t, src, "search_pages")
	// A real dial opens a NEW connection; the fixture caches one, and the holder
	// closes whatever it was given on every change.
	m.dial = func(ctx context.Context, _ mcpRecord) (*mcpsdk.ClientSession, error) {
		src.reconnect()
		return src.session(ctx)
	}
	tools := m.Tools() // an agent now holds these, and keeps them

	if _, err := m.Store().SetEnabled(rec.ID, false); err != nil {
		t.Fatal(err)
	}
	m.Refresh(rec.ID)
	if got := textOf(runTool(t, tools, "mcp__notion__search_pages", `{}`)); !strings.Contains(got, "turned off") {
		t.Fatalf("a disabled server answered a live agent with %q", got)
	}

	if _, err := m.Store().SetEnabled(rec.ID, true); err != nil {
		t.Fatal(err)
	}
	m.Refresh(rec.ID)
	if got := textOf(runTool(t, tools, "mcp__notion__search_pages", `{}`)); got != "found" {
		t.Errorf("after re-enabling, an agent that held the tools still gets %q", got)
	}
}

// TestARemovedServerStaysGoneForALiveAgent is the case that must NOT be revived:
// removal has no way back, so the holder is dropped and stays refusing.
func TestARemovedServerStaysGoneForALiveAgent(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "found"))
	m, rec := managerWith(t, src, "search_pages")
	tools := m.Tools()

	if err := m.Store().Remove(rec.ID); err != nil {
		t.Fatal(err)
	}
	m.Refresh(rec.ID)
	if got := textOf(runTool(t, tools, "mcp__notion__search_pages", `{}`)); !strings.Contains(got, "turned off") {
		t.Errorf("a removed server answered a live agent with %q", got)
	}
	if got := len(m.Tools()); got != 0 {
		t.Errorf("a removed server still offers %d tools to a new agent", got)
	}
}

// TestARegisteredToolCarriesNoAgentState keeps this off browser.go's rake,
// where the cached wrappers hold the FIRST caller's snapshot store and log, so
// a second agent's snapshots would land in the first agent's directory.
func TestARegisteredToolCarriesNoAgentState(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "ok"))
	m, _ := managerWith(t, src, "search_pages")
	tool, ok := m.Tools()[0].(*mcpTool)
	if !ok {
		t.Fatalf("registered tool is %T, want *mcpTool", m.Tools()[0])
	}
	if tool.deps.log != nil || tool.deps.snaps != nil || tool.deps.gate != nil {
		t.Error("a registered tool captured one agent's state and would share it with the rest")
	}
}

// TestARegisteredServerGetsItsOwnPerCallTimeout keeps a third party from
// holding a turn open the way only the local browser is allowed to.
func TestARegisteredServerGetsItsOwnPerCallTimeout(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "ok"))
	m, _ := managerWith(t, src, "search_pages")
	tool := m.Tools()[0].(*mcpTool)
	if tool.callTimeout <= 0 || tool.callTimeout > 5*time.Minute {
		t.Errorf("a registered tool has a %s per-call bound", tool.callTimeout)
	}
}

// TestTheWholeSchemaSurvivesTheStoreRoundTrip stops the schema-loss bug being
// reintroduced through disk. A schema that loses additionalProperties or anyOf
// leaves the model guessing arguments, and nothing reports it.
func TestTheWholeSchemaSurvivesTheStoreRoundTrip(t *testing.T) {
	src := newFakeSource(t, namedTool("search_pages", "ok"))
	store, err := LoadMCPStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	spec := specFor("search_pages")
	spec.Schema = []byte(`{"type":"object","additionalProperties":false,` +
		`"$defs":{"q":{"type":"string"}},"properties":{"q":{"$ref":"#/$defs/q"}}}`)
	if _, err := store.Add(mcpRecord{Name: "Notion", URL: "https://x/mcp",
		Tools: []mcpToolSpec{spec}}); err != nil {
		t.Fatal(err)
	}
	m := NewMCPManager(store)
	m.dial = func(ctx context.Context, _ mcpRecord) (*mcpsdk.ClientSession, error) {
		return src.session(ctx)
	}
	schema := m.Tools()[0].InputSchema()
	for _, key := range []string{"additionalProperties", "$defs", "properties"} {
		if _, ok := schema.ExtraFields[key]; !ok {
			t.Errorf("the model never sees %s: %v", key, schema.ExtraFields)
		}
	}
}
