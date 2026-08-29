package agentd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

const (
	// compactAtTokens is the billed input size above which the oldest part of
	// the conversation is summarized away.
	//
	// Well above clearTrigger so that clearing tool results does its job first
	// and this only ever handles the accumulated TEXT, which nothing else can
	// remove. What a conversation costs is the average history across the turns
	// between compactions, not the compaction itself: at 60k that average is
	// ~40k tokens a turn, at 150k it is ~95k. Since the summary call is a few
	// cents every few days either way, a higher trigger buys nothing and
	// doubles the standing bill.
	compactAtTokens = 60_000

	// compactAtBytes bounds conversation.json itself, which the token trigger
	// does not: a screenshot the API has stopped charging for is still on disk,
	// still re-sent on every turn, and still read whole into memory by load()
	// in a 4 GiB guest.
	compactAtBytes = 8 << 20

	// keepDivisor leaves the last 1/keepDivisor of the history verbatim, so a
	// compaction never costs the agent the thread it is in the middle of.
	keepDivisor = 4

	// summaryModel is cheap on purpose: this reads the whole prefix, and the
	// job is condensing text that is already there rather than reasoning.
	summaryModel = "claude-haiku-4-5"

	// summaryMaxTokens bounds the summary. It lands in every later request, so
	// a summary that rambles is paid for on every turn until the next
	// compaction.
	summaryMaxTokens = 2048

	// renderCap bounds one block in the summarizer's input, so a single fat
	// tool result cannot crowd out the shape of the conversation around it.
	renderCap = 1_000
)

// summaryAsk is what the summarizer is told to produce. It asks for the things
// that cannot be recovered by re-running a tool: what the person wanted, what
// was decided, and what is still open.
const summaryAsk = `The transcript above is the earlier part of your own conversation, which is about to be replaced by your summary of it. Write that summary as notes to yourself.

Cover: what the person asked for and why, decisions made and the reasoning behind them, standing preferences they expressed, work still open, file paths and identifiers you will need again, and how any scheduled runs turned out. Leave out tool output you could get again by re-running the tool. Be specific -- names, paths and numbers, not "various files". Write prose, no preamble.`

// ackSummary is the assistant half of the pair that replaces the prefix. The
// API needs the roles to alternate, so the summary cannot stand on its own.
const ackSummary = "Understood -- I have the earlier context above and will continue from there."

// summarize condenses the messages being dropped. A package var so a test can
// stub it without an API key, the way ChromeURL is pointed at a stub.
var summarize = callSummary

// compactIfNeeded summarizes the oldest part of the conversation once it has
// grown past either limit, so a long-lived agent stops re-paying for its whole
// history on every turn.
//
// Called between turns, never inside one: the history is at a complete boundary
// there, and a failure here must not be able to fail the person's turn.
func (a *Agent) compactIfNeeded(ctx context.Context) {
	if !a.needsCompaction() {
		return
	}
	msgs := a.Messages()
	cut := cutPoint(msgs, len(msgs)-len(msgs)/keepDivisor)
	if cut <= 0 {
		return // nowhere safe to cut; try again after the next turn
	}
	a.setState("working")
	defer a.setState("idle")
	summary, err := summarize(ctx, a, msgs[:cut])
	if err != nil {
		a.log.Append(Event{Type: "error", Message: "could not compact: " + err.Error()})
		return
	}
	a.applyCompaction(msgs, cut, summary)
}

// needsCompaction reports whether either limit has been passed.
func (a *Agent) needsCompaction() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastInput > compactAtTokens || a.convBytes > compactAtBytes
}

// applyCompaction swaps the summarized prefix in and persists the result,
// keeping one rollback copy. Any failure leaves the conversation as it was.
func (a *Agent) applyCompaction(msgs []anthropic.BetaMessageParam, cut int, summary string) {
	before := a.ConversationBytes()
	if err := a.backupConversation(); err != nil {
		a.log.Append(Event{Type: "error", Message: "could not keep a pre-compaction copy: " + err.Error()})
		return
	}
	a.mu.Lock()
	a.messages = append(synthPair(summary, cut), msgs[cut:]...)
	// Cleared so the next check waits for a real measurement. Left as it was,
	// a turn that fails before producing a usage event would leave the old,
	// pre-compaction figure standing and compact again -- summarizing the
	// summary, and losing a little more of the conversation each time.
	a.lastInput = 0
	a.mu.Unlock()
	if err := a.save(); err != nil {
		a.log.Append(Event{Type: "error", Message: "could not save the compacted conversation: " + err.Error()})
		return
	}
	a.log.Append(Event{Type: "compaction", Message: fmt.Sprintf(
		"summarized %d of %d messages; conversation %d -> %d bytes",
		cut, len(msgs), before, a.ConversationBytes())})
}

// cutPoint is the first index at or after target that can safely begin a
// history: a user message carrying no tool_result blocks. -1 when there is none.
//
// Cutting anywhere else orphans a tool_result at the HEAD -- it would answer a
// tool_use that was just dropped -- and the API rejects that. repairTail cannot
// heal it either: it looks only at the last message, and it can synthesise a
// missing result, not a missing use. See danglingUses, which relies on the
// matching invariant that only the tail is ever orphaned.
func cutPoint(msgs []anthropic.BetaMessageParam, target int) int {
	for i := max(target, 0); i < len(msgs); i++ {
		if msgs[i].Role == anthropic.BetaMessageParamRoleUser && !hasToolResult(msgs[i]) {
			return i
		}
	}
	return -1
}

// hasToolResult reports whether a message carries any tool_result block.
func hasToolResult(m anthropic.BetaMessageParam) bool {
	for _, b := range m.Content {
		if b.OfToolResult != nil {
			return true
		}
	}
	return false
}

// synthPair renders a summary as the exchange that replaces the messages it
// covers, keeping the history starting on a user message with roles alternating.
func synthPair(summary string, covered int) []anthropic.BetaMessageParam {
	head := fmt.Sprintf("[Earlier conversation -- %d messages -- summarized]\n\n%s", covered, summary)
	return []anthropic.BetaMessageParam{
		anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(head)),
		{Role: anthropic.BetaMessageParamRoleAssistant,
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(ackSummary)}},
	}
}

// callSummary asks the cheap model to condense the prefix.
//
// The tokens are booked against the transcript and the meter like any other
// response. They are real spend on a real model, and a compaction the person
// never sees must not be a hole in what /usage reports.
func callSummary(ctx context.Context, a *Agent, msgs []anthropic.BetaMessageParam) (string, error) {
	msg, err := a.client.Beta.Messages.New(ctx, anthropic.BetaMessageNewParams{
		Model:     summaryModel,
		MaxTokens: summaryMaxTokens,
		Messages: []anthropic.BetaMessageParam{anthropic.NewBetaUserMessage(
			anthropic.NewBetaTextBlock(renderForSummary(msgs) + "\n\n" + summaryAsk))},
	})
	if err != nil {
		return "", err
	}
	// Booked before the text is checked: the call was billed whether or not it
	// came back usable.
	a.bookUsage(msg.Model, Usage{
		InputTokens:              msg.Usage.InputTokens,
		OutputTokens:             msg.Usage.OutputTokens,
		CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
	})
	var out strings.Builder
	for _, block := range msg.Content {
		if b, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			out.WriteString(b.Text)
		}
	}
	if strings.TrimSpace(out.String()) == "" {
		return "", errors.New("the summarizer returned no text")
	}
	return out.String(), nil
}

// renderForSummary flattens messages into plain text for the summarizer.
//
// The prefix is read as a document rather than replayed as a conversation. That
// keeps base64 screenshots out of the request -- which is most of what makes
// this call cheap -- and it means a prefix ending mid-tool-call cannot produce a
// malformed request.
func renderForSummary(msgs []anthropic.BetaMessageParam) string {
	var out strings.Builder
	for _, m := range msgs {
		fmt.Fprintf(&out, "\n[%s]\n", m.Role)
		for _, b := range m.Content {
			switch {
			case b.OfText != nil:
				out.WriteString(capTextAt(b.OfText.Text, renderCap) + "\n")
			case b.OfToolUse != nil:
				fmt.Fprintf(&out, "(called %s)\n", b.OfToolUse.Name)
			case b.OfToolResult != nil:
				out.WriteString(capTextAt(resultBlockText(*b.OfToolResult), renderCap) + "\n")
			case b.OfImage != nil:
				out.WriteString("(screenshot)\n")
			}
		}
	}
	return out.String()
}

// prevConvPath is where the pre-compaction conversation is kept.
func (a *Agent) prevConvPath() string { return filepath.Join(a.dir, "conversation.prev.json") }

// backupConversation keeps one copy of the conversation as it was, so a bad
// summary is recoverable. One copy, overwritten: disk is tight in the guest.
func (a *Agent) backupConversation() error {
	buf, err := os.ReadFile(a.convPath())
	if os.IsNotExist(err) {
		return nil // nothing saved yet, so nothing to lose
	}
	if err != nil {
		return err
	}
	return os.WriteFile(a.prevConvPath(), buf, 0o640)
}
