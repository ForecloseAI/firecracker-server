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
	a, err := New(Record{ID: "boss", Name: "Boss"}, t.TempDir(), t.TempDir(), testProfile(), nil)
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
	// No "user" here: Turn is the one-shot path and does not queue, so the
	// person's message is logged by Send -- which is what the HTTP surface
	// calls, and what makes a message sent mid-turn visible immediately.
	for _, want := range []string{"state", "error", "turn_complete"} {
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

	reopened, err := New(Record{ID: "boss", Name: "Boss"}, a.dir, t.TempDir(), testProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// Three, not two: this fixture ends on a tool_use nothing answered, which is
	// the shape that used to 400 every later turn, so load now heals it.
	if got := len(reopened.Messages()); got != 3 {
		t.Fatalf("restored %d messages, want 3", got)
	}
	block := reopened.Messages()[1].Content[1].OfToolUse
	if block == nil || block.Name != "Read" {
		t.Errorf("tool_use block did not survive the round trip: %+v", reopened.Messages()[1])
	}
	if ids := resultIDs(reopened.Messages()[2]); len(ids) != 1 || ids[0] != "tu_1" {
		t.Errorf("restored history answers %v, want the tu_1 call", ids)
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

// A message must appear in the transcript when it is SENT, not when the turn
// that runs it begins. Queued behind a long turn it would otherwise be invisible
// for minutes, which reads to the person as "my message did not send".
func TestSendLogsTheMessageImmediately(t *testing.T) {
	a := newTestAgent(t)
	if err := a.Send("are you there?"); err != nil {
		t.Fatal(err)
	}
	events, err := a.Log().ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "user" && e.Text == "are you there?" {
			return
		}
	}
	t.Error("a sent message was not in the log before its turn ran")
}

// And a message the agent REFUSED must not appear. Logging before the enqueue
// would show the person their message was accepted when it was dropped.
func TestRefusedMessageIsNotLogged(t *testing.T) {
	a := newTestAgent(t)
	for i := 0; i < inboxDepth; i++ {
		if err := a.Send("filler"); err != nil {
			t.Fatalf("filling the inbox failed early: %v", err)
		}
	}
	if err := a.Send("one too many"); err == nil {
		t.Fatal("a full inbox accepted another message")
	}
	events, _ := a.Log().ReadAll()
	for _, e := range events {
		if e.Text == "one too many" {
			t.Error("a refused message was logged as though it had been accepted")
		}
	}
}
