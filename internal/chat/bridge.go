package chat

import (
	"context"
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
	"cracked/internal/vm"
)

// chatAgent is who the chat page is talking to. The daemon runs a whole roster,
// but the person addresses the boss: it is the agent that delegates, and its log
// records both the handing out of work and the answers coming back. Workers'
// own transcripts stay on their own logs, which is where they belong.
//
// A worker that needs the PERSON is the case this does not cover, and it is why
// pending interactions are about to stop being reconstructed from one agent's
// event log at all.
const chatAgent = agentapi.BossID

// guestPort is where agentd listens inside every VM. A var, not a const, so a
// test can point it at a stub, matching how idleGrace is shrunk below.
var guestPort = 8080

const (
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
	// Agent names who is asking, on pending cards only. Any agent can raise its
	// hand, and the person answers THAT agent -- so the card has to say which.
	Agent    string `json:"agent,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	Detail   string `json:"detail,omitempty"`
	UI       *UI    `json:"ui,omitempty"`
	Decision string `json:"decision,omitempty"`
	OK       bool   `json:"ok,omitempty"`
	DurMS    int64  `json:"duration_ms,omitempty"`
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
	// closed is terminal, unlike a cancelled ctx. Subscribe treats a cancelled
	// context as an idle stop and revives from it, which for a deleted machine
	// would bring back the very consumer dropping it was meant to remove.
	closed bool
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
	// A second consumer on the SAME context: the transcript comes from the
	// boss's log, but a raised hand can come from any agent on the machine, and
	// one log cannot carry both. Sharing the context is what keeps the lifecycle
	// single -- stopIfEmpty cancels both, Subscribe revives both.
	go b.runPending(ctx)
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

// runPending reconnects to the machine's raised hands forever, on the same
// backoff as the transcript.
func (b *Bridge) runPending(ctx context.Context) {
	delay := backoffBase
	for ctx.Err() == nil {
		if err := b.consumePending(ctx); err != nil && ctx.Err() == nil {
			log.Printf("chat bridge %s: pending: %v", b.id, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, backoffMax)
	}
}

// consumePending resolves the guest and streams its raised hands.
func (b *Bridge) consumePending(ctx context.Context) error {
	view, err := b.control.VM(b.id)
	if err != nil {
		return err
	}
	cl := agent.New(view.GuestIP, guestPort)
	now, err := cl.Pending()
	if err != nil {
		return err
	}
	b.resync(now)
	return cl.StreamPending(ctx, b.onPending)
}

// onPending turns one hub change into a card appearing or being taken down.
//
// Both frames carry ID 0 deliberately: they are live state, not points in the
// guest's log, so they must not move the resume watermark.
func (b *Bridge) onPending(c agentapi.PendingChange) {
	if c.ClearedID != "" {
		b.forget(c.ClearedID)
		b.emit(Frame{Kind: "resolved", PendingID: c.ClearedID})
		return
	}
	// The hub replays its whole current set on every connect, so a reconnect
	// re-raises cards the page is already showing. Without this the page stacks
	// a duplicate node per card per blip, and only the newest ever greys out.
	if b.known(c.Raised.ID) {
		return
	}
	b.emit(b.cardFor(*c.Raised))
}

// known reports whether a card is already being shown.
func (b *Bridge) known(id string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.pending[id]
	return ok
}

// resync reconciles against the hub's current set when a stream opens.
//
// The stream only ever replays `raised`, never `cleared`, so a card answered
// while this bridge was idle-stopped is invisible to it. Without this diff that
// card returns on the next page load with buttons that 404 -- the stale-orphan
// failure this phase set out to end, coming back through the one door the
// stream cannot cover.
func (b *Bridge) resync(now []agentapi.Raised) {
	keep := make(map[string]bool, len(now))
	for _, r := range now {
		keep[r.ID] = true
	}
	for _, id := range b.dropMissing(keep) {
		b.emit(Frame{Kind: "resolved", PendingID: id})
	}
	for _, r := range now {
		if !b.known(r.ID) {
			b.emit(b.cardFor(r))
		}
	}
}

// dropMissing forgets every card the hub no longer holds, returning their ids.
func (b *Bridge) dropMissing(keep map[string]bool) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var gone []string
	for id := range b.pending {
		if !keep[id] {
			gone = append(gone, id)
			delete(b.pending, id)
		}
	}
	return gone
}

// cardFor builds a card, stores it so an answer can be looked up, and returns
// the frame that renders it.
func (b *Bridge) cardFor(r agentapi.Raised) Frame {
	p := buildPending(r)
	if p.UI.Kind == "handoff" && b.caps != nil {
		p.UI.URL = b.caps.Mint(b.id)
	}
	b.mu.Lock()
	b.pending[p.ID] = p
	b.mu.Unlock()
	return Frame{Kind: "pending", PendingID: p.ID, Agent: p.Agent,
		Prompt: p.Prompt, Detail: p.Detail, UI: &p.UI}
}

// consume resolves the guest and streams its events until the stream ends.
func (b *Bridge) consume(ctx context.Context) error {
	view, err := b.control.VM(b.id)
	if err != nil {
		return err
	}
	cl := agent.New(view.GuestIP, guestPort)
	return cl.Stream(ctx, chatAgent, b.since(), b.onEvent)
}

// since reports the last event id seen, so a reconnect resumes without gaps.
func (b *Bridge) since() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.last
}

// onEvent maps one guest event onto a frame and publishes it.
func (b *Bridge) onEvent(ev agentapi.Event) {
	b.mu.Lock()
	if ev.ID > b.last {
		b.last = ev.ID
	}
	b.mu.Unlock()
	if f, ok := b.frameFor(ev); ok {
		b.emit(f)
	}
}

// toolBeat narrates one tool call. Only the path reaches the browser, never the
// input body: a Write input carries the whole file, which for a memory file is
// kilobytes of JSON per beat.
func toolBeat(ev agentapi.Event) Frame {
	label, detail := beatLabel(ev.Tool, ev.Input)
	return Frame{ID: ev.ID, Kind: "beat", Tool: ev.Tool, Label: label, Detail: detail}
}

// teamBeat narrates working as a team, which is most of what a boss does and
// none of which was visible before.
//
// The title goes in the LABEL, not in Detail: the page reads only the label on
// a beat and drops one that is empty, so a detail here would be invisible and a
// missing label renders as silence.
func teamBeat(ev agentapi.Event) string {
	switch {
	case ev.Type == "delegation":
		return "Handed " + ev.TaskTitle + " to " + ev.To
	case ev.Type == "task_start":
		return "Started: " + ev.TaskTitle
	case ev.From != "":
		return "Heard back from " + ev.From
	case ev.To != "":
		return "Messaged " + ev.To
	}
	return "Talking to the team"
}

// frameFor translates a guest event. Types not listed are deliberately
// dropped: thinking especially must never reach the transcript.
func (b *Bridge) frameFor(ev agentapi.Event) (Frame, bool) {
	switch ev.Type {
	case "user":
		return Frame{ID: ev.ID, Kind: "say", Role: "user", Text: ev.Text}, true
	case "text":
		return Frame{ID: ev.ID, Kind: "say", Role: "agent", Text: ev.Text}, true
	case "tool_use":
		return toolBeat(ev), true
	case "state":
		return Frame{ID: ev.ID, Kind: "state", State: ev.SessionState}, true
	case "agent_message", "delegation", "task_start":
		return Frame{ID: ev.ID, Kind: "beat", Tool: ev.Type, Label: teamBeat(ev)}, true
	case "turn_complete":
		return Frame{ID: ev.ID, Kind: "turn", OK: !ev.IsError, DurMS: ev.DurationMS}, true
	case "error":
		return Frame{ID: ev.ID, Kind: "error", Text: ev.Message}, true
	}
	return Frame{}, false
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
	if b.closed {
		// The machine is gone. Hand back a channel that never carries anything
		// rather than reconnecting: this consumer holds the deleted machine's
		// event watermark, and the replacement boots under the same id.
		log.Printf("chat bridge %s: refusing to revive a deleted machine", b.id)
		delete(b.subs, ch)
		close(ch)
		return ch
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
	evs, _, err := agent.New(view.GuestIP, guestPort).EventsSince(chatAgent, since)
	if err != nil {
		log.Printf("chat bridge %s: history fetch: %v", b.id, err)
		return nil
	}
	return b.replay(evs)
}

// replay converts a batch of events, keeping only the newest ringSize frames.
func (b *Bridge) replay(evs []agentapi.Event) []Frame {
	out := make([]Frame, 0, len(evs))
	for _, ev := range evs {
		if f, ok := b.frameFor(ev); ok {
			out = append(out, f)
		}
	}
	if len(out) > ringSize {
		out = out[len(out)-ringSize:]
	}
	return b.withPending(out)
}

// withPending appends a card for every hand currently up.
//
// The transcript no longer carries cards at all, so there is nothing to
// reconcile against: the hub is the only source and it is authoritative. What
// this replaces walked the replayed window looking for cards it had missed --
// which silently dropped any card older than the window while the agent stayed
// blocked, and appended a duplicate node for every one it re-emitted.
//
// ID 0 keeps emitFrame's resume watermark untouched: a card is live state, not
// a point in the guest's log.
func (b *Bridge) withPending(out []Frame) []Frame {
	for _, p := range b.cards() {
		ui := p.UI
		// The card's original capability is 15 minutes old at best.
		if ui.Kind == "handoff" && b.caps != nil {
			ui.URL = b.caps.Mint(b.id)
		}
		out = append(out, Frame{Kind: "pending", PendingID: p.ID, Agent: p.Agent,
			Prompt: p.Prompt, Detail: p.Detail, UI: &ui})
	}
	return out
}

// cards is every raised hand, in a stable order rather than map order.
func (b *Bridge) cards() []*Pending {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]*Pending, 0, len(b.pending))
	for _, p := range b.pending {
		out = append(out, p)
	}
	// Ordered by when the person was asked. Sorting by id would group by agent
	// instead, because ids are namespaced -- so a worker's newer question would
	// sort above the boss's older one purely on the letter it starts with.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Since.Equal(out[j].Since) {
			return out[i].Since.Before(out[j].Since)
		}
		return out[i].ID < out[j].ID
	})
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

// Close ends this bridge for good, for when its machine is deleted.
//
// stopIfEmpty is the idle path and leaves the bridge revivable; this is the
// terminal one. The consumer loops re-resolve the VM on every reconnect by
// design, so without cancelling them they would retry a machine that no longer
// exists every fifteen seconds, forever.
func (b *Bridge) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	// Before cancelling, because cancelling is what Subscribe reads as an idle
	// stop -- a subscriber racing the delete would otherwise revive it.
	b.closed = true
	if b.idle != nil {
		b.idle.Stop()
		b.idle = nil
	}
	b.stop()
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
