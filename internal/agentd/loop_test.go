package agentd

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// newTestAgent builds an agent with no tools, rooted at a temp dir.
func newTestAgent(t *testing.T) *Agent {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-not-a-real-key-for-offline-tests")
	a, err := New("boss", t.TempDir(), t.TempDir(), testProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// The rollback rule, and the reason this phase exists.
//
// NextMessage appends the assistant message immediately but appends the
// matching tool_result only at the START of the next call, so a turn abandoned
// partway leaves an orphan tool_use. Adopting that history would make the very
// next request malformed, and the agent would be permanently stuck. A failed
// turn must therefore leave the conversation exactly as it was.
func TestFailedTurnLeavesConversationUntouched(t *testing.T) {
	a := newTestAgent(t)
	before := len(a.Messages())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // fail before any network call
	if err := a.Turn(ctx, "this turn cannot succeed"); err == nil {
		t.Fatal("Turn on a cancelled context returned no error")
	}
	if got := len(a.Messages()); got != before {
		t.Errorf("conversation grew from %d to %d on a failed turn", before, got)
	}
	if _, err := os.Stat(filepath.Join(a.dir, "conversation.json")); !os.IsNotExist(err) {
		t.Error("a failed turn persisted a conversation file")
	}
}

// A failed turn still belongs in the transcript: the person needs to see that
// something was attempted and how it ended, even though the model never saw it.
func TestFailedTurnIsStillLogged(t *testing.T) {
	a := newTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.Turn(ctx, "doomed")

	events, err := a.Log().ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	for _, want := range []string{"user", "state", "error", "turn_complete"} {
		if !seen[want] {
			t.Errorf("a failed turn logged no %q event; got %v", want, seen)
		}
	}
	if a.State() != "idle" {
		t.Errorf("state after a failed turn = %q, want idle", a.State())
	}
}

// The conversation is the agent's whole memory of a session, so it has to
// survive a process restart byte for byte -- including tool_use blocks, whose
// param types marshal through the SDK's own machinery rather than plain structs.
func TestConversationSurvivesRestart(t *testing.T) {
	a := newTestAgent(t)
	a.messages = []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("remember 47")),
		{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{
			{OfText: &anthropic.BetaTextBlockParam{Text: "noted"}},
			{OfToolUse: &anthropic.BetaToolUseBlockParam{ID: "tu_1", Name: "Read",
				Input: map[string]any{"path": "go.mod"}}},
		}},
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}

	reopened, err := New("boss", a.dir, t.TempDir(), testProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(reopened.Messages()); got != 2 {
		t.Fatalf("restored %d messages, want 2", got)
	}
	block := reopened.Messages()[1].Content[1].OfToolUse
	if block == nil || block.Name != "Read" {
		t.Errorf("tool_use block did not survive the round trip: %+v", reopened.Messages()[1])
	}
}

// save writes via a temp file and renames, so an interrupted write cannot
// leave a truncated history that load would then reject at next boot.
func TestSaveLeavesNoTempFileBehind(t *testing.T) {
	a := newTestAgent(t)
	a.messages = []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hi")),
	}
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.convPath() + ".tmp"); !os.IsNotExist(err) {
		t.Error("save left its temp file behind")
	}
}

// testProfile is a minimal profile for tests that do not care about the role.
func testProfile() Profile {
	return Profile{Key: "test", Model: "claude-haiku-4-5", Prompt: "test"}
}
