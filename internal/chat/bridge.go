package chat

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"cracked/internal/agent"
	"cracked/internal/vm"
)

const (
	guestPort   = 8080
	ringSize    = 200
	backoffMax  = 15 * time.Second
	backoffBase = time.Second
)

// idleGrace is how long a bridge keeps its guest stream open after the last
// browser leaves. A var, not a const, so tests can shrink it.
var idleGrace = 60 * time.Second

// Frame is what the browser receives. One shape for the whole protocol.
type Frame struct {
	ID        int    `json:"id"`
	Kind      string `json:"kind"`
	Role      string `json:"role,omitempty"`
	Text      string `json:"text,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Label     string `json:"label,omitempty"`
	State     string `json:"state,omitempty"`
	PendingID string `json:"pending_id,omitempty"`
	Prompt    string `json:"prompt,omitempty"`
	Detail    string `json:"detail,omitempty"`
	UI        *UI    `json:"ui,omitempty"`
	Decision  string `json:"decision,omitempty"`
	OK        bool   `json:"ok,omitempty"`
	DurMS     int64  `json:"duration_ms,omitempty"`
}

// Bridge consumes one VM's event log and fans frames out to browsers.
type Bridge struct {
	id      string
	control *Control
	caps    *Caps

	mu      sync.Mutex
	subs    map[chan Frame]struct{}
	pending map[string]*Pending
	last    int
	ctx     context.Context
	stop    context.CancelFunc
	idle    *time.Timer
}

// newBridge starts a consumer for one VM id.
func newBridge(id string, control *Control, caps *Caps) *Bridge {
	b := &Bridge{id: id, control: control, caps: caps,
		subs: map[chan Frame]struct{}{}, pending: map[string]*Pending{}}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.startLocked()
	return b
}

// startLocked launches the guest consumer. The caller holds b.mu.
func (b *Bridge) startLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	b.ctx, b.stop = ctx, cancel
	go b.run(ctx)
}

// run reconnects to the guest forever, re-resolving its IP each time because a
// delete/recreate moves the VM to a different slot and a different address.
func (b *Bridge) run(ctx context.Context) {
	delay := backoffBase
	for ctx.Err() == nil {
		if err := b.consume(ctx); err != nil && ctx.Err() == nil {
			b.emit(Frame{Kind: "state", State: "reconnecting"})
			log.Printf("chat bridge %s: %v", b.id, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, backoffMax)
	}
}

// consume resolves the guest and streams its events until the stream ends.
func (b *Bridge) consume(ctx context.Context) error {
	view, err := b.control.VM(b.id)
	if err != nil {
		return err
	}
	cl := agent.New(view.GuestIP, guestPort)
	return cl.Stream(ctx, b.since(), b.onEvent)
}

// since reports the last event id seen, so a reconnect resumes without gaps.
func (b *Bridge) since() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

// onEvent maps one guest event onto a frame and publishes it.
func (b *Bridge) onEvent(ev agent.Event) {
	b.mu.Lock()
	if ev.ID > b.last {
		b.last = ev.ID
	}
	b.mu.Unlock()
	if f, ok := b.frameFor(ev); ok {
		b.emit(f)
	}
}

// frameFor translates a guest event. Types not listed are deliberately
// dropped: thinking especially must never reach the transcript.
func (b *Bridge) frameFor(ev agent.Event) (Frame, bool) {
	switch ev.Type {
	case "user":
		return Frame{ID: ev.ID, Kind: "say", Role: "user", Text: ev.Text}, true
	case "text":
		return Frame{ID: ev.ID, Kind: "say", Role: "agent", Text: ev.Text}, true
	case "tool_use":
		// Only the path reaches the browser, never the input body: a Write
		// input carries the whole file, which for a memory file is kilobytes
		// of JSON per beat.
		label, detail := beatLabel(ev.Tool, ev.Input)
		return Frame{ID: ev.ID, Kind: "beat", Tool: ev.Tool, Label: label, Detail: detail}, true
	case "state":
		return Frame{ID: ev.ID, Kind: "state", State: ev.SessionState}, true
	case "approval_required", "question":
		return b.pendingFrame(ev), true
	case "decision":
		b.forget(ev.ApprovalID)
		return Frame{ID: ev.ID, Kind: "resolved", PendingID: ev.ApprovalID, Decision: ev.Decision}, true
	case "turn_complete":
		return Frame{ID: ev.ID, Kind: "turn", OK: !ev.IsError, DurMS: ev.DurationMS}, true
	case "error":
		return Frame{ID: ev.ID, Kind: "error", Text: ev.Message}, true
	}
	return Frame{}, false
}

// pendingFrame registers a pending interaction and renders its card.
func (b *Bridge) pendingFrame(ev agent.Event) Frame {
	p := buildPending(ev)
	if p.UI.Kind == "handoff" {
		p.UI.URL = b.caps.Mint(b.id)
	}
	b.mu.Lock()
	b.pending[p.ID] = p
	b.mu.Unlock()
	return Frame{ID: ev.ID, Kind: "pending", PendingID: p.ID,
		Prompt: p.Prompt, Detail: p.Detail, UI: &p.UI}
}

// forget drops a resolved pending interaction.
func (b *Bridge) forget(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.pending, id)
}

// Pending looks up a live pending interaction by id.
func (b *Bridge) Pending(id string) (*Pending, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.pending[id]
	return p, ok
}

// emit publishes a frame to every subscriber, dropping for a slow one rather
// than blocking the whole bridge.
func (b *Bridge) emit(f Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- f:
		default:
		}
	}
}

// Subscribe registers a browser and returns its frame channel. It restarts the
// consumer when an idle timeout stopped it: the bridge stays in the server's
// map after stopping, so without this every later visit gets a live-looking SSE
// connection that no frame is ever written to.
func (b *Bridge) Subscribe() chan Frame {
	ch := make(chan Frame, 64)
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[ch] = struct{}{}
	if b.idle != nil {
		b.idle.Stop()
		b.idle = nil
	}
	if b.ctx.Err() != nil {
		log.Printf("chat bridge %s: reviving after idle stop", b.id)
		b.startLocked()
	}
	return ch
}

// history replays recent events straight from the guest so a page load shows
// the conversation, then live frames continue from there.
func (b *Bridge) history(since int) []Frame {
	view, err := b.control.VM(b.id)
	if err != nil {
		log.Printf("chat bridge %s: history resolve: %v", b.id, err)
		return nil
	}
	evs, _, err := agent.New(view.GuestIP, guestPort).EventsSince(since)
	if err != nil {
		log.Printf("chat bridge %s: history fetch: %v", b.id, err)
		return nil
	}
	return b.replay(evs)
}

// replay converts a batch of events, keeping only the newest ringSize frames.
func (b *Bridge) replay(evs []agent.Event) []Frame {
	out := make([]Frame, 0, len(evs))
	for _, ev := range evs {
		if f, ok := b.frameFor(ev); ok {
			out = append(out, f)
		}
	}
	if len(out) > ringSize {
		out = out[len(out)-ringSize:]
	}
	return out
}

// marshal renders a frame for the wire.
func (f Frame) marshal() []byte {
	buf, err := json.Marshal(f)
	if err != nil {
		return []byte(`{"kind":"error","text":"frame encode failed"}`)
	}
	return buf
}

// vmView is re-exported for handlers that need the VM's state.
type vmView = vm.View

// Unsubscribe drops a browser and stops the bridge once nobody is left.
func (b *Bridge) Unsubscribe(ch chan Frame) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subs, ch)
	if len(b.subs) == 0 && b.idle == nil {
		b.idle = time.AfterFunc(idleGrace, b.stopIfEmpty)
	}
}

// stopIfEmpty ends the guest stream when the last browser has really gone.
func (b *Bridge) stopIfEmpty() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.subs) == 0 {
		log.Printf("chat bridge %s: idle, stopping guest stream", b.id)
		b.stop()
	}
}
