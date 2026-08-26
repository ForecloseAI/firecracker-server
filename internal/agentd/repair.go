package agentd

import "github.com/anthropics/anthropic-sdk-go"

// stoppedNote is what the model reads where a tool result should have been.
const stoppedNote = "This did not run. The turn stopped before it could."

// repairTail answers a tool call the turn stopped before running, so the next
// request is well formed.
//
// The SDK reports four endings as success while the last assistant message still
// holds tool_use blocks it never executed: the iteration ceiling, which it checks
// BEFORE running pending tools, and stop reasons refusal, max_tokens and
// model_context_window_exceeded. Adopting that history persists an orphan, and
// every later turn appends a user block after it, which the API rejects with
// "tool_use ids were found without tool_result blocks immediately after". The
// agent is then wedged for good: the rollback rule keeps the poisoned history and
// discards the turn that failed on it, so it never heals on its own.
//
// The results are synthesised rather than the message truncated. Dropping the
// assistant message would drop the text it produced along with the orphan, and
// the API only asks that a result exist, not that it succeeded.
func repairTail(msgs []anthropic.BetaMessageParam) []anthropic.BetaMessageParam {
	ids := danglingUses(msgs)
	if len(ids) == 0 {
		return msgs
	}
	blocks := make([]anthropic.BetaContentBlockParamUnion, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, anthropic.NewBetaToolResultBlock(id, stoppedNote, true))
	}
	return append(msgs, anthropic.NewBetaUserMessage(blocks...))
}

// danglingUses lists the tool_use ids in the final message that nothing answers.
//
// Only the tail can be orphaned. A turn is adopted whole or rolled back whole, so
// a half-answered message never reaches disk; anything earlier already has its
// results in the message that follows it.
func danglingUses(msgs []anthropic.BetaMessageParam) []string {
	if len(msgs) == 0 {
		return nil
	}
	last := msgs[len(msgs)-1]
	if last.Role != anthropic.BetaMessageParamRoleAssistant {
		return nil
	}
	var ids []string
	for _, block := range last.Content {
		if block.OfToolUse != nil {
			ids = append(ids, block.OfToolUse.ID)
		}
	}
	return ids
}
