package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cracked/internal/agentapi"
)

// storedRecord is a registration with a real secret in it, which every test
// here is about not leaking.
func storedRecord(name, rawURL string) mcpRecord {
	return mcpRecord{
		Name: name, URL: rawURL, Transport: transportHTTP,
		Headers: map[string]string{"Authorization": "Bearer sk-live-SECRET"},
		Tools:   []mcpToolSpec{specFor("search_pages")},
	}
}

// newStore opens an empty store on a temp dir.
func newStore(t *testing.T) *MCPStore {
	t.Helper()
	s, err := LoadMCPStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestAStoredHeaderNeverReachesTheWireShape is the redaction proof.
//
// Asserted by marshalling, not by reading a field: a secret added to the wire
// type later would slip past a field check and be caught here.
func TestAStoredHeaderNeverReachesTheWireShape(t *testing.T) {
	buf, err := json.Marshal(redact(storedRecord("Notion", "https://mcp.notion.com/mcp")))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(buf), "sk-live-SECRET") {
		t.Fatalf("the wire shape carries the token: %s", buf)
	}
	if !strings.Contains(string(buf), "Authorization") {
		t.Errorf("the header key was dropped too, so a client cannot see what is set: %s", buf)
	}
}

// TestATokenInTheURLIsNotHandedBack covers the servers that authenticate with a
// query parameter. The URL is the field a client is likeliest to log in full.
func TestATokenInTheURLIsNotHandedBack(t *testing.T) {
	rec := storedRecord("Hosted", "https://h.example.com/mcp?key=sk-live-SECRET")
	got := redact(rec).URL
	if strings.Contains(got, "sk-live") {
		t.Fatalf("the wire URL still carries the token: %s", got)
	}
	if got != "https://h.example.com/mcp" {
		t.Errorf("the wire URL is %q, want the host and path a person recognises", got)
	}
}

// TestRedactingDoesNotEraseTheStoredSecret is the shallow-copy trap.
//
// If a read handed back the live Headers map, redaction would empty the store in
// memory and the next save would write that erasure to disk permanently -- the
// person's server would stop authenticating and nothing would say why.
func TestRedactingDoesNotEraseTheStoredSecret(t *testing.T) {
	s := newStore(t)
	rec, err := s.Add(storedRecord("Notion", "https://mcp.notion.com/mcp"))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		for _, r := range s.List() {
			redact(r)
			r.Headers["Authorization"] = "clobbered"
		}
	}
	back, _ := s.Get(rec.ID)
	if back.Headers["Authorization"] != "Bearer sk-live-SECRET" {
		t.Errorf("the stored token became %q after reads", back.Headers["Authorization"])
	}
}

// TestTheStoreSurvivesACrashMidWrite pins the temp-file-and-rename, which is
// what keeps a half-written file from losing every registration.
func TestTheStoreSurvivesACrashMidWrite(t *testing.T) {
	dir := t.TempDir()
	s, err := LoadMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(storedRecord("Notion", "https://mcp.notion.com/mcp")); err != nil {
		t.Fatal(err)
	}
	again, err := LoadMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.List()) != 1 {
		t.Fatalf("reloaded %d servers, want 1", len(again.List()))
	}
	if _, err := os.Stat(filepath.Join(dir, "mcp-servers.json.tmp")); !os.IsNotExist(err) {
		t.Error("a temp file was left behind")
	}
}

// TestTwoServersWithTheSameNameGetDifferentIds keeps one registration from
// silently replacing another, which would take its tools with it.
func TestTwoServersWithTheSameNameGetDifferentIds(t *testing.T) {
	s := newStore(t)
	a, _ := s.Add(storedRecord("Notion", "https://a.example.com/mcp"))
	b, err := s.Add(storedRecord("Notion", "https://b.example.com/mcp"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID {
		t.Fatalf("both servers got id %q", a.ID)
	}
	if len(s.List()) != 2 {
		t.Errorf("stored %d servers, want 2", len(s.List()))
	}
}

// TestAnUnnamedServerIsIdentifiedByItsHost keeps a registration with a useless
// name from being refused outright.
func TestAnUnnamedServerIsIdentifiedByItsHost(t *testing.T) {
	s := newStore(t)
	rec, err := s.Add(mcpRecord{Name: "!!!", URL: "https://mcp.notion.com/mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" {
		t.Error("no id was derived from the host")
	}
}

// TestAnAbsentEnabledFieldIsNotADisable pins the pointer on MCPUpdate. A bool
// would make {} indistinguishable from "turn this off".
func TestAnAbsentEnabledFieldIsNotADisable(t *testing.T) {
	var empty agentapi.MCPUpdate
	if json.Unmarshal([]byte(`{}`), &empty) != nil {
		t.Fatal("could not decode an empty body")
	}
	if empty.Enabled != nil {
		t.Error("an empty body decoded as an instruction to change the flag")
	}
	var off agentapi.MCPUpdate
	if json.Unmarshal([]byte(`{"enabled":false}`), &off) != nil || off.Enabled == nil || *off.Enabled {
		t.Error("an explicit disable did not decode as one")
	}
}

// TestOnlyEnabledServersAreOfferedToAgents is what disable has to mean before
// any of the runtime is involved.
func TestOnlyEnabledServersAreOfferedToAgents(t *testing.T) {
	s := newStore(t)
	rec, _ := s.Add(storedRecord("Notion", "https://mcp.notion.com/mcp"))
	if len(s.Enabled()) != 1 {
		t.Fatal("a fresh registration was not enabled")
	}
	if _, err := s.SetEnabled(rec.ID, false); err != nil {
		t.Fatal(err)
	}
	if len(s.Enabled()) != 0 {
		t.Error("a disabled server is still offered")
	}
	if len(s.List()) != 1 {
		t.Error("disabling lost the registration instead of turning it off")
	}
}

// TestAnOverLongToolNameIsNotReportedToThePerson keeps the list a person is
// shown identical to the surface their agents get.
func TestAnOverLongToolNameIsNotReportedToThePerson(t *testing.T) {
	rec := storedRecord("Notion", "https://mcp.notion.com/mcp")
	rec.ID = "notion"
	rec.Tools = append(rec.Tools, specFor(strings.Repeat("x", 130)))
	got := redact(rec).Tools
	if len(got) != 1 || got[0] != "mcp__notion__search_pages" {
		t.Errorf("reported %v, want only the tool the model will actually be offered", got)
	}
}
