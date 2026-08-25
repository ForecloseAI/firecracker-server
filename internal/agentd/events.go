package agentd

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Usage mirrors the SDK's usage block. There is deliberately no cost field:
// the Messages API returns token counts only, and the dollar figure the old
// TypeScript agent logged came from the Claude Code CLI's own pricing table.
// Events carry tokens plus the model id, and pricing is applied host-side.
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

// Event is one entry in an agent's log. The field set is the union of what
// every event type needs, only the relevant ones being set -- the same shape
// internal/agent/client.go already decodes, so the host side can consume these
// unchanged when it is wired up.
type Event struct {
	ID    int       `json:"id"`
	Agent string    `json:"agent"`
	Type  string    `json:"type"`
	TS    time.Time `json:"ts"`

	Text      string          `json:"text,omitempty"`
	From      string          `json:"from,omitempty"`
	To        string          `json:"to,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	MessageID string          `json:"message_id,omitempty"`

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

// Log is one agent's append-only event log, durable on disk so a transcript
// survives a restart. One per agent, not a package global: from Phase 6 there
// are many agents in this process and each owns its own id space.
type Log struct {
	mu     sync.Mutex
	path   string
	agent  string
	nextID int
	subs   map[chan Event]struct{}
}

// OpenLog opens an agent's log, resuming ids after the last entry so a restart
// continues the sequence rather than replaying ids a client has already seen.
func OpenLog(dir, agent string) (*Log, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	l := &Log{
		path:   filepath.Join(dir, "events.jsonl"),
		agent:  agent,
		nextID: 1,
		subs:   map[chan Event]struct{}{},
	}
	events, err := l.ReadAll()
	if err != nil {
		return nil, err
	}
	if n := len(events); n > 0 {
		l.nextID = events[n-1].ID + 1
	}
	return l, nil
}

// Append stamps an event, persists it, and fans it out to live subscribers.
//
// A failed write is not fatal: losing a transcript line must never take down
// an agent mid-turn, so the error is folded into the returned event instead.
func (l *Log) Append(e Event) Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	e.ID, e.Agent, e.TS = l.nextID, l.agent, time.Now().UTC()
	l.nextID++
	l.write(e)
	for ch := range l.subs {
		select {
		case ch <- e:
		default: // a slow reader drops frames rather than stalling the agent
		}
	}
	return e
}

// write appends one JSON line. Caller holds l.mu.
//
// The failures here are reported and then swallowed: a log that cannot be
// written must not take the agent down. But they must not be SILENT either --
// this file is the only durable diagnostic an agent has, so a full or read-only
// overlay would otherwise lose every event while /health still answers ok.
// stderr is the one channel left, and under systemd it reaches the journal.
func (l *Log) write(e Event) {
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		log.Printf("agentd: cannot open event log %s: %v", l.path, err)
		return
	}
	defer f.Close()
	buf, err := json.Marshal(e)
	if err != nil {
		log.Printf("agentd: cannot encode %s event: %v", e.Type, err)
		return
	}
	if _, err := f.Write(append(buf, '\n')); err != nil {
		log.Printf("agentd: cannot append to %s: %v", l.path, err)
	}
}

// ReadAll returns every persisted event, skipping any line that will not
// parse -- a truncated final line after a hard kill must not blind the whole
// log.
//
// This re-reads the file per call, which is what the TypeScript agent did too.
// Fine at Phase 2 sizes; revisit if Phase 3's SSE reconnects make it show up.
func (l *Log) ReadAll() ([]Event, error) {
	f, err := os.Open(l.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return scanEvents(f)
}

// scanEvents parses a JSONL stream into events.
func scanEvents(f *os.File) ([]Event, error) {
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.ID > 0 {
			out = append(out, e)
		}
	}
	return out, sc.Err()
}

// Since returns every event after id, for Last-Event-ID replay in Phase 3.
func (l *Log) Since(id int) ([]Event, error) {
	all, err := l.ReadAll()
	if err != nil {
		return nil, err
	}
	for i, e := range all {
		if e.ID > id {
			return all[i:], nil
		}
	}
	return nil, nil
}

// LastID is the id of the most recent event, or 0 when the log is empty.
func (l *Log) LastID() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.nextID - 1
}

// Subscribe registers a live listener and returns it with its unsubscribe.
func (l *Log) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	l.mu.Lock()
	l.subs[ch] = struct{}{}
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.subs, ch)
	}
}
