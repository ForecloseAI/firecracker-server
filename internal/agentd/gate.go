package agentd

import (
	"context"
	"fmt"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// How long a pending interaction waits. Someone answering an approval may be
// away from the screen, but a plain question left hanging for half an hour just
// blocks the agent, so questions expire sooner.
//
// Vars, not consts, so tests can shrink them -- the same trick internal/chat
// uses for idleGrace.
var (
	approvalTimeout = 30 * time.Minute
	questionTimeout = 10 * time.Minute
)

// pending is one interaction blocking a tool handler.
type pending struct {
	answer chan Decision
	tool   string
}

// grant is a batch consent: the next N calls of a tool skip the prompt.
type grant struct {
	tool      string
	remaining int
	expiresAt time.Time
}

// Gate decides which tool calls need a human, and blocks the calling handler
// until one answers, times out, or the turn is interrupted.
//
// Thread-safe by necessity, not by caution: the SDK runner calls tool handlers
// CONCURRENTLY when the model invokes several tools in one turn.
type Gate struct {
	log *Log
	hub *Interactions
	// dir is the agent's own directory, where a handoff's screenshot is kept.
	dir string

	// onWait moves the agent's count of people it is blocked on, by +1 and -1.
	// Set by the agent once it exists; nil in a test, and in the moment between
	// the gate being built and the agent being assembled around it.
	onWait func(int)

	mu      sync.Mutex
	pending map[string]*pending
	grants  []grant
	seq     int
}

// NewGate builds a gate that records its decisions in log and raises its
// pending interactions on hub, which may be nil in a test.
// Seeded from the event log rather than from zero. seq only ever lived in
// memory, so every restart began at ap_001 again and handed a fresh ask the same
// id as one already answered before the restart -- which made history stamp the
// old verdict onto the new card. The person then saw a question that said it was
// already answered, could not answer it, and the agent stayed blocked on them
// until it timed out. Log ids are monotonic and survive a restart, so starting
// there cannot repeat.
func NewGate(log *Log, hub *Interactions, dir string) *Gate {
	return &Gate{log: log, hub: hub, dir: dir, seq: log.LastID(), pending: map[string]*pending{}}
}

// shotName is a unique file name for the next handoff capture. Uses the gate's
// own sequence rather than a timestamp so two handoffs in the same second cannot
// overwrite each other.
func (g *Gate) shotName() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return "handoff-" + pad(g.seq+1) + ".png"
}

// Check blocks until a human allows or denies a gated call.
//
// A denial is returned as an error carrying the reason, which the runner hands
// back to the model as the tool's result. The wording matters: without "do not
// retry" the model tends to look for another route to the same action.
func (g *Gate) Check(ctx context.Context, tool, preview string, input any) error {
	if g.consume(tool) {
		return nil
	}
	d, err := g.await(ctx, approvalTimeout, tool, Event{
		Type: "approval_required", Tool: tool, Preview: preview, Input: encode(input),
	})
	if err != nil {
		return err
	}
	if d.Decision == "allow" {
		return nil
	}
	return fmt.Errorf("the person declined this. Reason: %s. Do not retry it", orDefault(d.Reason, "none given"))
}

// Ask puts a question to the person and waits for their answer.
func (g *Gate) Ask(ctx context.Context, question string, ui UI) (string, error) {
	timeout := questionTimeout
	ev := Event{Type: "question", Question: question, Kind: ui.Kind, UI: &ui}
	if ui.Kind == "handoff" {
		timeout = approvalTimeout // they have to go and do something
		// A handoff asks the person to take over this machine's screen. Showing
		// them what is on it is the difference between "sign in to something" and
		// a page they recognise, so the card carries a picture of where they will
		// land -- taken now, because by the time they open it the agent may have
		// moved on.
		ev.Shot = captureScreen(g.dir, g.shotName())
	}
	d, err := g.await(ctx, timeout, "", ev)
	if err != nil {
		return "", err
	}
	return d.Answer, nil
}

// await registers a pending interaction, logs it, and blocks on the answer.
func (g *Gate) await(ctx context.Context, timeout time.Duration, tool string, ev Event) (Decision, error) {
	id, ch := g.register(ev.Type, tool)
	ev.ApprovalID = id
	// Both, and they are not the same thing: the log is this agent's transcript,
	// which must record that it asked; the hub is the live set of hands up, which
	// is what a person is shown and answers.
	g.log.Append(ev)
	g.hub.Raise(raisedFrom(id, g.log.agent, ev))
	// After the ask is on the log, so a client reads "here is a question" and then
	// "I am not typing", rather than the other way round.
	g.wait(1)
	defer g.wait(-1)
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case d := <-ch:
		return d, nil
	case <-timer.C:
		g.forget(id)
		return Decision{}, fmt.Errorf("timed out waiting for a human")
	case <-ctx.Done():
		g.forget(id)
		return Decision{}, ctx.Err()
	}
}

// wait reports that one more, or one fewer, person is blocking this agent.
func (g *Gate) wait(delta int) {
	if g.onWait != nil {
		g.onWait(delta)
	}
}

// register mints an id and the channel its answer will arrive on.
//
// The id carries the agent that raised it. seq is per-gate and gates are
// per-agent, so without the prefix the boss's first approval and a worker's
// first are both "ap_001" -- one card would overwrite the other in any client
// that keys on the id, and answering one would answer the wrong agent. It is
// also what lets the answer be routed back without asking who to send it to.
func (g *Gate) register(kind, tool string) (string, chan Decision) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seq++
	prefix := "ap_"
	if kind == "question" {
		prefix = "q_"
	}
	id := g.log.agent + "." + prefix + pad(g.seq)
	// Buffered, so Resolve never blocks on a waiter that has already given up.
	ch := make(chan Decision, 1)
	g.pending[id] = &pending{answer: ch, tool: tool}
	return id, ch
}

// forget drops a pending interaction that timed out or was cancelled, and takes
// the raised hand down with it.
func (g *Gate) forget(id string) {
	g.mu.Lock()
	delete(g.pending, id)
	g.mu.Unlock()
	g.hub.Clear(id)
}

// Resolve delivers a human's answer, reporting whether anything was waiting.
func (g *Gate) Resolve(id string, d Decision) bool {
	g.mu.Lock()
	p, ok := g.pending[id]
	if ok {
		delete(g.pending, id)
		g.applyScope(p.tool, d)
	}
	g.mu.Unlock()
	if !ok {
		return false
	}
	g.hub.Clear(id)
	g.log.Append(Event{Type: "decision", ApprovalID: id, Decision: d.Decision})
	p.answer <- d
	return true
}

// raisedFrom turns the logged event into the card a person sees.
func raisedFrom(id, agent string, ev Event) agentapi.Raised {
	return agentapi.Raised{
		ID: id, Agent: agent, Kind: ev.Type, Tool: ev.Tool, Preview: ev.Preview,
		Input: ev.Input, Question: ev.Question, UI: ev.UI, Shot: ev.Shot,
	}
}

// IsPending reports whether an interaction with this id is still waiting.
func (g *Gate) IsPending(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	_, ok := g.pending[id]
	return ok
}

// applyScope turns an approval with scope "batch" into a grant. Caller holds
// g.mu. Unlike the TypeScript gate, which hardcoded a Bash grant whatever tool
// was approved, the grant is for the tool actually being asked about.
func (g *Gate) applyScope(tool string, d Decision) {
	if d.Scope != "batch" || d.Decision != "allow" || tool == "" {
		return
	}
	g.grants = append(g.grants, grant{
		tool:      tool,
		remaining: orZero(d.MaxUses, 10),
		expiresAt: time.Now().Add(time.Duration(orZero(d.TTLSeconds, 3600)) * time.Second),
	})
}

// consume spends one use of a matching grant, if the caller has one.
func (g *Gate) consume(tool string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := range g.grants {
		gr := &g.grants[i]
		if gr.tool == tool && gr.remaining > 0 && gr.expiresAt.After(time.Now()) {
			gr.remaining--
			return true
		}
	}
	return false
}

// RevokeAll clears every grant and denies everything pending, returning the
// number of grants dropped. Called on interrupt: consent given for one piece of
// work must not carry into whatever the person asks for next.
//
// The hub is cleared alongside, which every other exit from a wait already does
// -- forget on timeout or cancel, Resolve on an answer. Without it an interrupt
// left the raised hand up FOREVER: /pending kept advertising a card whose gate
// no longer existed, so it could never be answered and never went away.
func (g *Gate) RevokeAll() int {
	g.mu.Lock()
	n := len(g.grants)
	g.grants = nil
	waiting := g.pending
	g.pending = map[string]*pending{}
	g.mu.Unlock()
	for id, p := range waiting {
		g.hub.Clear(id)
		p.answer <- Decision{Decision: "deny", Reason: "the person interrupted"}
	}
	return n
}

// orDefault returns s, or fallback when s is empty.
func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// orZero returns n, or fallback when n is zero.
func orZero(n, fallback int) int {
	if n == 0 {
		return fallback
	}
	return n
}
