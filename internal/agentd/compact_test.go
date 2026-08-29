package agentd

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// userText builds a plain user message.
func userText(s string) anthropic.BetaMessageParam {
	return anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(s))
}

// assistantUse builds an assistant message that calls one tool.
func assistantUse(id string) anthropic.BetaMessageParam {
	return anthropic.BetaMessageParam{
		Role: anthropic.BetaMessageParamRoleAssistant,
		Content: []anthropic.BetaContentBlockParamUnion{
			anthropic.NewBetaToolUseBlock(id, map[string]any{}, "Bash")},
	}
}

// userResult builds the user message that answers a tool call.
func userResult(id string) anthropic.BetaMessageParam {
	return anthropic.NewBetaUserMessage(anthropic.NewBetaToolResultBlock(id, "output", false))
}

// A tool call and its result are one unit. Cutting between them leaves a
// tool_result answering a tool_use that is no longer in the history, which the
// API rejects -- and repairTail cannot heal it, because it only ever answers a
// dangling use at the TAIL. So the cut must always land on a user message that
// carries no tool_result.
func TestCutPointNeverSplitsAToolCall(t *testing.T) {
	msgs := []anthropic.BetaMessageParam{
		userText("first"),
		assistantUse("tu_01"),
		userResult("tu_01"),
		assistantUse("tu_02"),
		userResult("tu_02"),
		userText("second"),
		assistantUse("tu_03"),
		userResult("tu_03"),
		userText("third"),
	}
	for target := 0; target < len(msgs); target++ {
		at := cutPoint(msgs, target)
		if at == -1 {
			continue
		}
		if at < target {
			t.Fatalf("target %d: cut at %d, before the target", target, at)
		}
		if msgs[at].Role != anthropic.BetaMessageParamRoleUser {
			t.Fatalf("target %d: cut at %d, which is not a user message", target, at)
		}
		if hasToolResult(msgs[at]) {
			t.Fatalf("target %d: cut at %d, which answers a tool call", target, at)
		}
	}
}

// With no clean boundary left, there is nothing safe to do. Compaction must
// decline rather than cut anyway.
func TestCutPointReportsWhenThereIsNoSafeCut(t *testing.T) {
	msgs := []anthropic.BetaMessageParam{
		userText("first"),
		assistantUse("tu_01"),
		userResult("tu_01"),
	}
	if at := cutPoint(msgs, 1); at != -1 {
		t.Errorf("cutPoint = %d, want -1: nothing past index 1 can start a history", at)
	}
	if at := cutPoint(nil, 0); at != -1 {
		t.Errorf("cutPoint on an empty history = %d, want -1", at)
	}
}

// The API requires the first message to be a user message and the roles to
// alternate, so the summary cannot simply replace the prefix on its own.
func TestCompactionLeavesAWellFormedHistory(t *testing.T) {
	a := newTestAgent(t)
	stubSummary(t, "the person asked about deploys", nil)

	msgs := longHistory(20)
	a.messages = msgs
	a.lastInput = compactAtTokens + 1
	a.compactIfNeeded(context.Background())

	got := a.Messages()
	if len(got) >= len(msgs) {
		t.Fatalf("conversation did not shrink: %d -> %d", len(msgs), len(got))
	}
	if got[0].Role != anthropic.BetaMessageParamRoleUser {
		t.Errorf("history starts with role %q, want user", got[0].Role)
	}
	if ids := danglingUses(got); len(ids) != 0 {
		t.Errorf("compaction left %d unanswered tool calls: %v", len(ids), ids)
	}
	if !strings.Contains(textOf(got[0]), "the person asked about deploys") {
		t.Errorf("the summary is not in the history: %q", textOf(got[0]))
	}
}

// Everything after the cut has to survive untouched. The recent turns are the
// thread the agent is in the middle of, and a summary of those is a regression.
func TestCompactionKeepsTheRecentTailVerbatim(t *testing.T) {
	a := newTestAgent(t)
	stubSummary(t, "earlier work", nil)

	msgs := longHistory(20)
	a.messages = msgs
	a.lastInput = compactAtTokens + 1
	a.compactIfNeeded(context.Background())

	got := a.Messages()
	tail := got[2:] // past the synthetic pair
	kept := msgs[len(msgs)-len(tail):]
	for i := range tail {
		if textOf(tail[i]) != textOf(kept[i]) {
			t.Fatalf("kept message %d changed: %q != %q", i, textOf(tail[i]), textOf(kept[i]))
		}
	}
}

// A summariser that fails must cost nothing. The conversation keeps growing
// until a later turn manages it, which is the right trade against dropping
// history no one has read.
func TestFailedSummaryLeavesTheConversationUntouched(t *testing.T) {
	a := newTestAgent(t)
	stubSummary(t, "", errors.New("the model is unreachable"))

	msgs := longHistory(20)
	a.messages = msgs
	a.lastInput = compactAtTokens + 1
	a.compactIfNeeded(context.Background())

	if got := len(a.Messages()); got != len(msgs) {
		t.Errorf("conversation changed on a failed summary: %d -> %d", len(msgs), got)
	}
	if !loggedType(a, "error") {
		t.Error("a failed compaction was not reported in the transcript")
	}
	if loggedType(a, "compaction") {
		t.Error("a failed compaction reported success")
	}
}

// Under both limits there is nothing to do, and a summariser call would be paid
// for on every single turn.
func TestNoCompactionBelowTheLimits(t *testing.T) {
	a := newTestAgent(t)
	stubSummary(t, "should never run", nil)

	msgs := longHistory(20)
	a.messages = msgs
	a.lastInput = compactAtTokens - 1
	a.convBytes = compactAtBytes - 1
	a.compactIfNeeded(context.Background())

	if got := len(a.Messages()); got != len(msgs) {
		t.Errorf("compacted below the limits: %d -> %d", len(msgs), got)
	}
}

// A conversation full of base64 the API has stopped charging for still has to
// be compacted: it is re-sent on every turn and read whole into memory at start.
func TestBytesAloneTriggerCompaction(t *testing.T) {
	a := newTestAgent(t)
	stubSummary(t, "a long browsing session", nil)

	msgs := longHistory(20)
	a.messages = msgs
	a.lastInput = 0 // nothing billed, so only the byte limit can fire
	a.convBytes = compactAtBytes + 1
	a.compactIfNeeded(context.Background())

	if got := len(a.Messages()); got >= len(msgs) {
		t.Errorf("the byte limit did not compact: %d -> %d", len(msgs), got)
	}
}

// The copy is what makes a bad summary recoverable.
func TestCompactionKeepsARollbackCopy(t *testing.T) {
	a := newTestAgent(t)
	stubSummary(t, "earlier work", nil)

	a.messages = longHistory(20)
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	a.lastInput = compactAtTokens + 1
	a.compactIfNeeded(context.Background())

	if _, err := os.Stat(a.prevConvPath()); err != nil {
		t.Errorf("no pre-compaction copy was kept: %v", err)
	}
	if !loggedType(a, "compaction") {
		t.Error("a successful compaction was not reported in the transcript")
	}
}

// Rendering must not put base64 in the summariser's request: it is most of what
// the prefix weighs, and the summary cannot use it.
func TestRenderingLeavesImagesOut(t *testing.T) {
	msgs := []anthropic.BetaMessageParam{
		userText("look at this"),
		{Role: anthropic.BetaMessageParamRoleUser,
			Content: []anthropic.BetaContentBlockParamUnion{
				anthropic.NewBetaImageBlock(anthropic.BetaBase64ImageSourceParam{
					MediaType: "image/jpeg", Data: "AAAABBBBCCCC"})}},
	}
	got := renderForSummary(msgs)
	if strings.Contains(got, "AAAABBBBCCCC") {
		t.Error("the rendered prefix carries the image payload")
	}
	if !strings.Contains(got, "screenshot") {
		t.Errorf("the rendered prefix does not say a screenshot was there: %q", got)
	}
}

// stubSummary swaps the summariser for the length of one test.
func stubSummary(t *testing.T, out string, err error) {
	t.Helper()
	prev := summarize
	summarize = func(context.Context, *Agent, []anthropic.BetaMessageParam) (string, error) {
		return out, err
	}
	t.Cleanup(func() { summarize = prev })
}

// longHistory builds n alternating messages ending on a complete turn, with a
// tool call in the middle so a naive cut would split one.
func longHistory(n int) []anthropic.BetaMessageParam {
	var out []anthropic.BetaMessageParam
	for i := 0; len(out) < n; i++ {
		out = append(out, userText("question "+string(rune('a'+i%26))))
		if i%3 == 0 {
			out = append(out, assistantUse("tu_"+string(rune('a'+i%26))), userResult("tu_"+string(rune('a'+i%26))))
		}
		out = append(out, anthropic.BetaMessageParam{
			Role:    anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock("answer")},
		})
	}
	return out
}

// textOf joins a message's text blocks, for comparing messages in tests.
func textOf(m anthropic.BetaMessageParam) string {
	var out strings.Builder
	for _, b := range m.Content {
		if b.OfText != nil {
			out.WriteString(b.OfText.Text)
		}
	}
	return out.String()
}

// loggedType reports whether the agent's transcript holds an event of a type.
func loggedType(a *Agent, kind string) bool {
	events, err := a.log.ReadAll()
	if err != nil {
		return false
	}
	for _, e := range events {
		if e.Type == kind {
			return true
		}
	}
	return false
}

// The compacted history has to survive a restart, because that is the only way
// it is ever read again -- and the person's transcript has to be untouched by
// it, since events.jsonl is what the app draws and conversation.json is only
// ever the model's input.
func TestCompactionSurvivesRestartAndLeavesTheTranscriptAlone(t *testing.T) {
	a := newTestAgent(t)
	stubSummary(t, "earlier work on the deploy", nil)

	a.messages = longHistory(20)
	if err := a.save(); err != nil {
		t.Fatal(err)
	}
	before, _ := a.log.ReadAll()
	a.lastInput = compactAtTokens + 1
	a.compactIfNeeded(context.Background())
	compacted := a.Messages()

	after, _ := a.log.ReadAll()
	if len(after) < len(before) {
		t.Errorf("the transcript lost events: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if after[i].ID != before[i].ID || after[i].Type != before[i].Type {
			t.Fatalf("event %d was rewritten by compaction", i)
		}
	}

	reopened, err := New("boss", a.dir, t.TempDir(), testProfile(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Messages()
	if len(got) != len(compacted) {
		t.Fatalf("restart read back %d messages, want %d", len(got), len(compacted))
	}
	if ids := danglingUses(got); len(ids) != 0 {
		t.Errorf("the restored history has unanswered tool calls: %v", ids)
	}
}

// A turn that fails before the model answers produces no usage event, so the
// pre-compaction figure would still be standing when the next check runs. If
// that were allowed to fire, it would summarize the summary and lose a little
// more of the conversation on every failed turn.
func TestCompactionDoesNotRepeatWithoutAFreshMeasurement(t *testing.T) {
	a := newTestAgent(t)
	stubSummary(t, "earlier work", nil)

	a.messages = longHistory(20)
	a.lastInput = compactAtTokens + 1
	a.compactIfNeeded(context.Background())
	once := a.Messages()

	a.compactIfNeeded(context.Background()) // no turn ran in between
	if got := len(a.Messages()); got != len(once) {
		t.Errorf("compacted twice with no new measurement: %d -> %d", len(once), got)
	}
}

// The summary is a real request to a real model. It is spend the person never
// sees any output from, so if it does not reach the meter and the transcript,
// GET /usage under-reports every compaction and the VM's cost quietly drifts
// from what it actually was.
func TestSummarySpendReachesTheMeterAndTheTranscript(t *testing.T) {
	a := newTestAgent(t)
	a.team = &Supervisor{meter: OpenMeter(t.TempDir())}

	a.bookUsage(summaryModel, Usage{InputTokens: 4000, OutputTokens: 300})

	rows := a.meter().Report().ByModel
	found := false
	for _, row := range rows {
		if row.Model != summaryModel {
			continue
		}
		found = true
		if row.InputTokens != 4000 || row.OutputTokens != 300 {
			t.Errorf("meter booked %d in / %d out, want 4000 / 300",
				row.InputTokens, row.OutputTokens)
		}
	}
	if !found {
		t.Fatalf("the summary model is absent from the meter: %+v", rows)
	}
	if !loggedType(a, "usage") {
		t.Error("the summary call left no usage event in the transcript")
	}
}
