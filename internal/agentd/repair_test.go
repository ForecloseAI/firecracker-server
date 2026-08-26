package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// useTail is a history ending in an assistant message that called two tools and
// never heard back -- what the iteration ceiling and a max_tokens cut both leave.
func useTail() []anthropic.BetaMessageParam {
	return []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("open the page")),
		{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{
			anthropic.NewBetaToolUseBlock("toolu_1", map[string]any{}, "click"),
			anthropic.NewBetaToolUseBlock("toolu_2", map[string]any{}, "fill"),
		}},
	}
}

// resultIDs lists the tool_result ids in one message, in order.
func resultIDs(m anthropic.BetaMessageParam) []string {
	var ids []string
	for _, b := range m.Content {
		if b.OfToolResult != nil {
			ids = append(ids, b.OfToolResult.ToolUseID)
		}
	}
	return ids
}

// Every unanswered call gets a result, or the next request is rejected outright.
func TestRepairTailAnswersEveryDanglingUse(t *testing.T) {
	got := repairTail(useTail())
	if len(got) != 3 {
		t.Fatalf("history went from 2 to %d messages; wanted one appended", len(got))
	}
	last := got[2]
	if last.Role != anthropic.BetaMessageParamRoleUser {
		t.Fatalf("results landed in a %q message; tool_result must be user role", last.Role)
	}
	ids := resultIDs(last)
	if len(ids) != 2 || ids[0] != "toolu_1" || ids[1] != "toolu_2" {
		t.Fatalf("answered %v; wanted both calls, in order", ids)
	}
}

// The results say the call did not happen, so the model does not read silence as
// success and carry on as though the page had been clicked.
func TestRepairTailMarksResultsAsErrors(t *testing.T) {
	last := repairTail(useTail())[2]
	for _, b := range last.Content {
		if b.OfToolResult == nil {
			continue
		}
		if !b.OfToolResult.IsError.Or(false) {
			t.Fatal("synthesised result is not an error; the model would take it as a success")
		}
	}
}

// A finished turn must not grow a message. This is the common path -- getting it
// wrong would append a spurious result after every single turn.
func TestRepairTailLeavesAFinishedTurnAlone(t *testing.T) {
	msgs := []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock("hello")),
		{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{
			anthropic.NewBetaTextBlock("hi back"),
		}},
	}
	if got := repairTail(msgs); len(got) != 2 {
		t.Fatalf("clean history grew to %d messages", len(got))
	}
	if got := repairTail(nil); got != nil {
		t.Fatalf("empty history became %v", got)
	}
}

// A tool_use already answered is not answered twice: the results message is the
// tail, so there is nothing dangling.
func TestRepairTailIgnoresAnAnsweredCall(t *testing.T) {
	msgs := append(useTail(), anthropic.NewBetaUserMessage(
		anthropic.NewBetaToolResultBlock("toolu_1", "ok", false),
		anthropic.NewBetaToolResultBlock("toolu_2", "ok", false),
	))
	if got := repairTail(msgs); len(got) != 3 {
		t.Fatalf("answered history grew to %d messages", len(got))
	}
}

// The whole point of repairing on load: an agent wedged by a history saved before
// this existed comes back able to take a message.
func TestLoadHealsAPoisonedConversation(t *testing.T) {
	dir := t.TempDir()
	buf, err := json.Marshal(useTail())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conversation.json"), buf, 0o640); err != nil {
		t.Fatal(err)
	}
	a := &Agent{dir: dir}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	if len(a.messages) != 3 {
		t.Fatalf("loaded %d messages; the orphan tool_use was not answered", len(a.messages))
	}
	if ids := resultIDs(a.messages[2]); len(ids) != 2 {
		t.Fatalf("healed history answers %v; wanted both calls", ids)
	}
}

// Results already on disk must not be logged again. Starting the watermark at
// zero re-read the whole restored conversation on every turn, which put a long
// session's entire tool output into the log once per message.
func TestRecordResultsOnlyLogsWhatIsNew(t *testing.T) {
	log, err := OpenLog(t.TempDir(), "boss")
	if err != nil {
		t.Fatal(err)
	}
	a := &Agent{log: log}
	history := append(useTail(), anthropic.NewBetaUserMessage(
		anthropic.NewBetaToolResultBlock("toolu_1", "old", false),
		anthropic.NewBetaToolResultBlock("toolu_2", "old", false),
	))

	if got := a.recordResults(history, len(history)); got != len(history) {
		t.Fatalf("watermark moved to %d, want %d", got, len(history))
	}
	if n := countType(t, log, "tool_result"); n != 0 {
		t.Fatalf("logged %d results from history that was already answered", n)
	}
	fresh := append(history, anthropic.NewBetaUserMessage(
		anthropic.NewBetaToolResultBlock("toolu_3", "new", true)))
	a.recordResults(fresh, len(history))
	if n := countType(t, log, "tool_result"); n != 1 {
		t.Fatalf("logged %d results, want just the new one", n)
	}
}

// countType totals the events of one type in a log.
func countType(t *testing.T, log *Log, kind string) int {
	t.Helper()
	events, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == kind {
			n++
		}
	}
	return n
}
