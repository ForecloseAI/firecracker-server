// Package agentapi is the wire format the guest daemon and the host share.
//
// It exists because these types were previously declared twice -- once in
// internal/agentd and once in internal/agent -- and hand-kept in sync. They
// drifted, silently and for a long time: the daemon stamped `model` and `agent`
// on every event and the host's copy had neither field, so both were discarded
// on decode and no cost could be attributed to anything. Nothing errored,
// because a JSON field with nowhere to go is simply dropped.
//
// One declaration makes that class of bug a compile error instead of a review
// problem. Both sides import this; neither redeclares it.
package agentapi

import (
	"encoding/json"
	"time"
)

// BossID is the one agent id a client may assume exists. Every machine gets a
// boss at first boot and it cannot be deleted, so it is the address for anything
// that must reach "whoever is in charge here" without listing the roster first.
const BossID = "boss"

// Usage mirrors the SDK's usage block. There is deliberately no cost field: the
// Messages API returns token counts only, so events carry tokens plus the model
// id and pricing is applied host-side, where the table can change without
// rebuilding a guest image.
type Usage struct {
	// ClearedInputTokens and ClearedToolUses report what context editing
	// actually removed. They are the only evidence it is doing anything: a
	// clear_tool_uses edit that is configured but never fires looks exactly like
	// one that works, right up until the bill arrives.
	ClearedInputTokens int64 `json:"cleared_input_tokens,omitempty"`
	ClearedToolUses    int64 `json:"cleared_tool_uses,omitempty"`

	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// TotalTokens is every token billed across all four categories.
func (u Usage) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// UI tells a client how to render a pending interaction.
type UI struct {
	Kind    string   `json:"kind"` // text | confirm | choice | handoff
	Options []string `json:"options,omitempty"`
}

// Decision is a human's answer to a pending interaction. Scope, MaxUses and
// TTLSeconds turn a single approval into a batch consent.
type Decision struct {
	Decision   string `json:"decision"`
	Reason     string `json:"reason,omitempty"`
	Answer     string `json:"answer,omitempty"`
	Scope      string `json:"scope,omitempty"`
	MaxUses    int    `json:"max_uses,omitempty"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
}

// Raised is one agent waiting on a person: a raised hand.
//
// Agent is the point of it. Any agent may need the person -- every worker
// profile carries Bash and ask_human -- and the person must be able to see who
// is asking and answer that agent, rather than the boss answering on its behalf.
type Raised struct {
	ID       string          `json:"id"`
	Agent    string          `json:"agent"`
	Kind     string          `json:"kind"` // approval_required | question
	Tool     string          `json:"tool,omitempty"`
	Preview  string          `json:"preview,omitempty"`
	Input    json.RawMessage `json:"input,omitempty"`
	Question string          `json:"question,omitempty"`
	UI       *UI             `json:"ui,omitempty"`
	Since    time.Time       `json:"since"`
}

// PendingChange is one hub event: a hand raised, or one lowered.
type PendingChange struct {
	Raised    *Raised `json:"raised,omitempty"`
	ClearedID string  `json:"cleared_id,omitempty"`
}

// Event is one entry in an agent's log. The field set is the union of what
// every event type needs, only the relevant ones being set.
type Event struct {
	ID    int       `json:"id"`
	Agent string    `json:"agent"`
	Type  string    `json:"type"`
	TS    time.Time `json:"ts"`

	Text  string          `json:"text,omitempty"`
	From  string          `json:"from,omitempty"`
	To    string          `json:"to,omitempty"`
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	Model        string `json:"model,omitempty"`
	Usage        *Usage `json:"usage,omitempty"`
	DurationMS   int64  `json:"duration_ms,omitempty"`
	SessionState string `json:"session_state,omitempty"`
	Message      string `json:"message,omitempty"`
	IsError      bool   `json:"is_error,omitempty"`

	// A pending interaction waiting on a human, and its resolution.
	ApprovalID string `json:"approval_id,omitempty"`
	Preview    string `json:"preview,omitempty"`
	Question   string `json:"question,omitempty"`
	Kind       string `json:"kind,omitempty"`
	UI         *UI    `json:"ui,omitempty"`
	Decision   string `json:"decision,omitempty"`

	// A task folder opened by start_task, or carried by a delegation.
	TaskSlug  string `json:"task_slug,omitempty"`
	TaskTitle string `json:"task_title,omitempty"`
	TaskDir   string `json:"task_dir,omitempty"`
}

// EventsPage is the body of GET /agents/{id}/events?poll=1.
type EventsPage struct {
	Events      []Event `json:"events"`
	LastEventID int     `json:"last_event_id"`
}

// ModelUsage is what one model has cost, in tokens. Split by model because the
// price differs by a factor of ten across the range, so a single token total
// cannot be turned back into money.
type ModelUsage struct {
	Model string `json:"model"`
	Usage
	Turns int64 `json:"turns"`
}

// UsageReport is GET /usage: what this daemon has spent across every agent it
// runs, in tokens. No dollar figure -- the host owns the price table, so rates
// can change without rebuilding a guest image.
type UsageReport struct {
	ByModel        []ModelUsage `json:"by_model"`
	Turns          int64        `json:"turns"`
	LastDurationMS int64        `json:"last_duration_ms,omitempty"`
	LastActivity   time.Time    `json:"last_activity,omitzero"`
}

// Task is a piece of work an agent has open.
type Task struct {
	Slug      string    `json:"slug"`
	Title     string    `json:"title"`
	Dir       string    `json:"dir"`
	StartedAt time.Time `json:"started_at"`
}

// Record is one agent's durable identity: who it is, not whether it is running.
type Record struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Type      string    `json:"type"`
	CreatedAt time.Time `json:"created_at"`
	Task      *Task     `json:"task,omitempty"`
}

// Status is one agent as GET /agents reports it: identity plus what it is doing.
type Status struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	State        string `json:"state"`
	Live         bool   `json:"live"`
	Task         *Task  `json:"task,omitempty"`
	LastEventID  int    `json:"last_event_id"`
	Conversation int    `json:"conversation_bytes"`
}

// Health is GET /health. Ready is constant true rather than a real signal, and
// that is honest: the supervisor is fully built before the listener opens, so
// there is no window in which the daemon answers this un-ready.
type Health struct {
	OK           bool   `json:"ok"`
	Ready        bool   `json:"ready"`
	Agents       int    `json:"agents"`
	Live         int    `json:"live"`
	Working      int    `json:"working"`
	SessionState string `json:"session_state"`
}
