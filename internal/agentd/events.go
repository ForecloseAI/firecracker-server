package agentd

import (
	"bufio"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// The wire format lives in internal/agentapi so the host and this daemon cannot
// declare it twice and drift apart, which is exactly what happened before: the
// host's copy of Event had no Model or Agent field, so both were dropped on
// decode and nothing anywhere reported a problem. These are aliases, not new
// types, so the two sides are the same type to the compiler.
type (
	Event      = agentapi.Event
	Usage      = agentapi.Usage
	UI         = agentapi.UI
	Decision   = agentapi.Decision
	Task       = agentapi.Task
	Record     = agentapi.Record
	Status     = agentapi.Status
	EventsPage = agentapi.EventsPage
	Health     = agentapi.Health
)

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

// ReadLogSince reads one agent's log straight off disk, without any running
// agent behind it.
//
// The events are a file; only the LIVE stream needs a subscription, and a
// snapshot that started an agent would mean a client drawing a conversation
// spawned the whole roster into memory to do it.
func ReadLogSince(dir string, id int) ([]Event, int, error) {
	f, err := os.Open(filepath.Join(dir, "events.jsonl"))
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	all, err := scanEvents(f)
	if err != nil {
		return nil, 0, err
	}
	return after(all, id), lastID(all), nil
}

// after is every event newer than id.
func after(all []Event, id int) []Event {
	for i, e := range all {
		if e.ID > id {
			return all[i:]
		}
	}
	return nil
}

// lastID is the id of the final entry, or 0 for an empty log.
func lastID(all []Event) int {
	if len(all) == 0 {
		return 0
	}
	return all[len(all)-1].ID
}

// Since returns every event after id, for Last-Event-ID replay.
func (l *Log) Since(id int) ([]Event, error) {
	all, err := l.ReadAll()
	if err != nil {
		return nil, err
	}
	return after(all, id), nil
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
