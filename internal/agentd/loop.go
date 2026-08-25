// Package agentd is the in-guest multi-agent daemon: one process running
// several named agents, each with its own tools, memory and event log.
//
// It replaces nothing yet. The TypeScript agent in rootfs/files/agent keeps
// running in every VM; this is built alongside it and switched on only after
// it has proven itself.
package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// ErrBusy is returned when an agent's inbox is full. Reported to the caller as
// 503 rather than blocking an HTTP handler on a model call.
var ErrBusy = errors.New("agent queue is full")

// inboxDepth is how many messages may wait while a turn runs. Small on
// purpose: a person queueing a dozen instructions at an agent that is stuck is
// better told so than silently buffered.
const inboxDepth = 8

// Context-editing thresholds. Starting values, to be tuned against real usage
// events from a browsing session: a snapshot costs a few thousand tokens, so
// the trigger sits well above a couple of them and only fires on a genuinely
// long conversation. clearAtLeast stops a clear being paid for a trivial saving.
const (
	clearTrigger = 30_000
	clearKeep    = 5
	clearAtLeast = 5_000
)

// maxTokens bounds one assistant response.
const maxTokens = 8192

// maxIterations bounds the API calls in a single turn. The SDK's default is 0,
// which means UNLIMITED -- the runner loops until the model stops calling
// tools. For an agent that runs unattended that is a runaway bill, so it is set
// explicitly here rather than left at the default.
const maxIterations = 40

// Agent is one conversation: its own system prompt, tool set, history and log.
//
// NOT safe for concurrent use. The SDK's runner requires that all its methods
// be called from a single goroutine, so every agent gets exactly one -- which
// is also how agents will run in parallel from Phase 6 on.
type Agent struct {
	id      string
	dir     string
	client  anthropic.Client
	model   string
	system  string
	profile Profile
	tools   []anthropic.BetaTool
	log     *Log
	gate    *Gate
	team    *Supervisor
	inbox   chan inbound

	// Guards everything the HTTP surface reads while the agent goroutine
	// writes it. The SDK runner itself stays confined to that one goroutine,
	// which is what the SDK requires.
	mu          sync.Mutex
	lastText    string
	turnStartID int
	messages    []anthropic.BetaMessageParam
	state       string
	cancel      context.CancelFunc
	convBytes   int
}

// New builds an agent rooted at dir, working in workspace, restoring its
// conversation and log from disk. The client reads ANTHROPIC_API_KEY from the
// environment.
//
// The agent owns its log, gate and tools rather than being handed them: the
// gate records into the log and the tools call the gate, so assembling them
// anywhere else just moves the knot.
// inbound is one queued message. From is empty when it came from the person
// and names the sender when it came from another agent, which decides both how
// it is logged and how it is framed for the model.
type inbound struct {
	text string
	from string
	// replyTo is set when this message is delegated work. The delegator is
	// then told the outcome whatever happens, including when the agent runs
	// out of iterations mid-task -- otherwise a boss waits forever on a
	// worker that quietly stopped, which is exactly what happened the first
	// time this was run end to end.
	replyTo string
}

func New(id, dir, workspace string, p Profile, team *Supervisor) (*Agent, error) {
	log, err := OpenLog(dir, id)
	if err != nil {
		return nil, err
	}
	if _, err := EnsureMemory(dir); err != nil {
		// A memory failure must degrade to "an agent with no memory", never to
		// "an agent that did not start".
		log.Append(Event{Type: "memory", Message: "could not seed memory: " + err.Error()})
	}
	gate := NewGate(log)
	deps := toolDeps{gate: gate, team: team, self: id, log: log, browser: p.Browser}
	if p.Browser {
		// A browser agent that cannot reach Chrome is still worth starting: it
		// keeps every other tool, and the failure shows up in its log rather
		// than as a VM that refuses to boot.
		if err := attachBrowser(&deps, dir, team); err != nil {
			log.Append(Event{Type: "error", Message: "no browser: " + err.Error()})
		}
	}
	tools, err := Tools(roots{workspace: workspace, own: dir}, deps, p.Tools)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		id: id, dir: dir, client: anthropic.NewClient(), profile: p,
		model: p.Model, system: ComposeSystemPrompt(p, dir),
		tools: tools, log: log, gate: gate, team: team, state: "idle",
		inbox: make(chan inbound, inboxDepth),
	}
	if err := a.load(); err != nil {
		return nil, err
	}
	a.log.Append(Event{Type: "ready", Message: "agent " + id + " ready"})
	return a, nil
}

// Log exposes the event log, for the HTTP surface.
func (a *Agent) Log() *Log { return a.log }

// Gate exposes the approval gate, so the HTTP surface can deliver answers.
func (a *Agent) Gate() *Gate { return a.gate }

// ID is the agent's slug.
func (a *Agent) ID() string { return a.id }

// Type is the profile key the agent was created from.
func (a *Agent) Type() string { return a.profile.Key }

// SystemPrompt is the composed prompt, exposed so a caller can inspect what an
// agent was actually given without reconstructing it.
func (a *Agent) SystemPrompt() string { return a.system }

// State reports whether the agent is idle or working.
func (a *Agent) State() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// ConversationBytes is the size of the persisted history, tracked at save time
// rather than measured on demand. This is the term that grows without bound,
// so it is reported by /debug/memstats from here on.
func (a *Agent) ConversationBytes() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.convBytes
}

// Messages exposes a copy of the conversation so far.
func (a *Agent) Messages() []anthropic.BetaMessageParam {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.messages)
}

// Run drives the agent until ctx is done, taking one queued message at a time.
// One goroutine per agent: the SDK runner is not safe for concurrent use, and
// this is also the shape many agents in parallel will take.
func (a *Agent) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case in := <-a.inbox:
			a.runTurn(ctx, in)
		}
	}
}

// runTurn executes one turn under a cancellable child context, so Interrupt can
// stop the turn without tearing down the agent.
func (a *Agent) runTurn(parent context.Context, in inbound) {
	ctx, cancel := context.WithCancel(parent)
	a.setCancel(cancel)
	defer func() {
		a.setCancel(nil)
		cancel()
	}()
	a.turn(ctx, in) // errors are already recorded in the event log
}

// Send queues a message from the person.
//
// A full inbox is reported rather than blocking the HTTP handler on a model
// call that may take a minute.
func (a *Agent) Send(text string) error {
	return a.enqueue(inbound{text: text})
}

// Deliver queues a message from another agent, recording it in this agent's
// log as well as the sender's. Each per-agent log is the only transcript of
// that agent, so it has to be self-contained.
func (a *Agent) Deliver(from, text string) error {
	return a.deliver(inbound{text: text, from: from})
}

// DeliverWork queues delegated work, which is reported back on when it ends.
func (a *Agent) DeliverWork(from, text string) error {
	return a.deliver(inbound{text: text, from: from, replyTo: from})
}

// deliver queues a message from another agent and records it in this agent's
// log as well as the sender's. Each per-agent log is the only transcript of
// that agent, so it has to be self-contained.
func (a *Agent) deliver(in inbound) error {
	if err := a.enqueue(in); err != nil {
		return err
	}
	a.log.Append(Event{Type: "agent_message", From: in.from, Text: in.text})
	return nil
}

// enqueue puts one message on the inbox without blocking.
func (a *Agent) enqueue(in inbound) error {
	select {
	case a.inbox <- in:
		return nil
	default:
		return ErrBusy
	}
}

// Interrupt cancels the in-flight turn, reporting whether there was one.
//
// The rollback rule is what makes this safe: the cancelled turn is discarded
// rather than adopted, so the conversation stays at its last complete boundary
// and the agent can take the next message immediately.
func (a *Agent) Interrupt() bool {
	// Consent given for one piece of work must not carry into whatever the
	// person asks for next, so every grant and every pending prompt goes too.
	a.gate.RevokeAll()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cancel == nil {
		return false
	}
	a.cancel()
	return true
}

// setCancel records (or clears) the in-flight turn's canceller.
func (a *Agent) setCancel(c context.CancelFunc) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cancel = c
}

// convPath is where the message history is persisted.
func (a *Agent) convPath() string { return filepath.Join(a.dir, "conversation.json") }

// load restores the conversation. A missing file is a fresh agent, not an
// error; an unreadable one is fatal, because silently starting from nothing
// would look like amnesia rather than a bug.
func (a *Agent) load() error {
	buf, err := os.ReadFile(a.convPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	a.convBytes = len(buf)
	return json.Unmarshal(buf, &a.messages)
}

// save persists the conversation, writing to a temp file and renaming so a
// crash mid-write cannot leave a half-written history behind.
func (a *Agent) save() error {
	buf, err := json.Marshal(a.messages)
	if err != nil {
		return err
	}
	tmp := a.convPath() + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o640); err != nil {
		return err
	}
	a.mu.Lock()
	a.convBytes = len(buf)
	a.mu.Unlock()
	return os.Rename(tmp, a.convPath())
}

// systemBlocks renders the system prompt as one cached block.
//
// The cache breakpoint is the point. Tools and system render before messages,
// so a breakpoint here caches the whole fixed prefix; without it every turn
// re-pays full input price on it. Note it silently does nothing below a ~1024
// token prefix, so a short prompt cannot prove the setup works.
func (a *Agent) systemBlocks() []anthropic.BetaTextBlockParam {
	return []anthropic.BetaTextBlockParam{{
		Text:         a.system,
		CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
	}}
}

// params builds the request for one turn from a candidate history.
func (a *Agent) params(msgs []anthropic.BetaMessageParam) anthropic.BetaToolRunnerParams {
	return anthropic.BetaToolRunnerParams{
		MaxIterations: maxIterations,
		BetaMessageNewParams: anthropic.BetaMessageNewParams{
			Model:             anthropic.Model(a.model),
			MaxTokens:         maxTokens,
			System:            a.systemBlocks(),
			Messages:          msgs,
			ContextManagement: contextManagement(),
			Betas:             []anthropic.AnthropicBeta{anthropic.AnthropicBetaContextManagement2025_06_27},
		},
	}
}

// contextManagement drops old tool results out of the request once a
// conversation gets long.
//
// This is available in the Go SDK and was NOT available to the TypeScript
// agent -- its own commit says context editing is not exposed by the Agent SDK,
// which is exactly why that agent had to digest snapshots through a PostToolUse
// hook instead. It is the one thing the rewrite gets for free.
//
// Clearing rewrites the prefix below the clear point, which would invalidate a
// cache breakpoint sitting there. There is exactly one breakpoint here and it
// is on the last system block, above the messages, so nothing in the history is
// cached and clearing costs nothing. That is also why it matters: every tool
// result left in history is re-billed at full uncached price on every turn.
func contextManagement() anthropic.BetaContextManagementConfigParam {
	return anthropic.BetaContextManagementConfigParam{
		Edits: []anthropic.BetaContextManagementConfigEditUnionParam{{
			OfClearToolUses20250919: &anthropic.BetaClearToolUses20250919EditParam{
				Trigger: anthropic.BetaClearToolUses20250919EditTriggerUnionParam{
					OfInputTokens: &anthropic.BetaInputTokensTriggerParam{Value: clearTrigger},
				},
				Keep:         anthropic.BetaToolUsesKeepParam{Value: clearKeep},
				ClearAtLeast: anthropic.BetaInputTokensClearAtLeastParam{Value: clearAtLeast},
				// A person's answer is the one result that cannot be recovered by
				// running the tool again: they have walked away, and re-asking
				// spends them twice. Snapshots are deliberately NOT protected --
				// they are the fattest thing in history, so excluding them would
				// defeat the whole point of turning this on.
				ExcludeTools: []string{"ask_human"},
			},
		}},
	}
}

// Turn runs one user message to completion.
//
// The candidate history is built separately and only adopted on success. That
// is the rollback rule: a cancelled or failed turn leaves a.messages exactly
// as it was, so the next request is never malformed. See drain for why that
// matters.
func (a *Agent) Turn(ctx context.Context, text string) error {
	return a.turn(ctx, inbound{text: text})
}

// turn runs one queued message, whoever it came from.
func (a *Agent) turn(ctx context.Context, in inbound) error {
	started := time.Now()
	a.mu.Lock()
	a.turnStartID, a.lastText = a.log.LastID(), ""
	a.mu.Unlock()
	if in.from == "" {
		a.log.Append(Event{Type: "user", Text: in.text})
	}
	a.setState("working")
	candidate := slices.Clone(a.messages)
	candidate = append(candidate, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(frame(in))))
	runner := a.client.Beta.Messages.NewToolRunner(a.tools, a.params(candidate))
	if err := a.drain(ctx, runner); err != nil {
		a.finish(started, err)
		a.report(in, runner, err)
		return err
	}
	a.mu.Lock()
	a.messages = runner.Messages()
	a.mu.Unlock()
	a.finish(started, nil)
	a.report(in, runner, nil)
	return a.save()
}

// drain pumps the runner one turn at a time, recording each message.
//
// Iterating with NextMessage rather than RunToCompletion is deliberate: this
// loop body is where the approval gate and the browser lease will hook in. It
// is also why the rollback rule exists -- NextMessage appends the assistant
// message immediately but appends the matching tool_result only at the START of
// the next call, so mid-loop the history ends with an orphan tool_use that
// would make the next request malformed.
func (a *Agent) drain(ctx context.Context, runner *anthropic.BetaToolRunner) error {
	for {
		msg, err := runner.NextMessage(ctx)
		if err != nil {
			return err
		}
		if msg == nil {
			return nil
		}
		a.record(msg)
	}
}

// record turns one assistant message into events.
func (a *Agent) record(msg *anthropic.BetaMessage) {
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			a.mu.Lock()
			a.lastText = b.Text
			a.mu.Unlock()
			a.log.Append(Event{Type: "text", Text: b.Text})
		case anthropic.BetaToolUseBlock:
			a.log.Append(Event{Type: "tool_use", Tool: b.Name, Input: encode(b.Input)})
		}
	}
	cleared, uses := appliedEdits(msg)
	a.log.Append(Event{Type: "usage", Model: msg.Model, Usage: &Usage{
		InputTokens:              msg.Usage.InputTokens,
		OutputTokens:             msg.Usage.OutputTokens,
		CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		ClearedInputTokens:       cleared,
		ClearedToolUses:          uses,
	}})
}

// appliedEdits totals what context editing removed from this request, so the
// event log can show it happening rather than merely being configured.
func appliedEdits(msg *anthropic.BetaMessage) (tokens, uses int64) {
	for _, e := range msg.ContextManagement.AppliedEdits {
		tokens += e.ClearedInputTokens
		uses += e.ClearedToolUses
	}
	return tokens, uses
}

// finish closes a turn, reporting the error if there was one.
func (a *Agent) finish(started time.Time, err error) {
	if err != nil {
		a.log.Append(Event{Type: "error", Message: err.Error()})
	}
	a.log.Append(Event{
		Type:       "turn_complete",
		IsError:    err != nil,
		DurationMS: time.Since(started).Milliseconds(),
	})
	a.setState("idle")
}

// report tells the delegator how delegated work ended, unless the agent
// already messaged them itself during the turn.
//
// Without this a worker that stops early -- an error, or simply running out of
// iterations mid-task -- leaves the delegator waiting on a message that will
// never come. A turn that ended is news whether or not it went well.
func (a *Agent) report(in inbound, runner *anthropic.BetaToolRunner, turnErr error) {
	if in.replyTo == "" || a.team == nil || a.messagedDuring(in.replyTo, a.turnStartID) {
		return
	}
	a.team.Deliver(a.id, in.replyTo, a.outcome(runner, turnErr))
}

// outcome describes how a turn ended, for the delegator.
func (a *Agent) outcome(runner *anthropic.BetaToolRunner, turnErr error) string {
	if turnErr != nil {
		return "I stopped before finishing: " + turnErr.Error() +
			". Nothing further will happen unless you ask again."
	}
	if runner != nil && runner.IterationCount() >= maxIterations {
		return "I ran out of steps before finishing, so the work is incomplete. " +
			"Check what is in the folder and send me a narrower piece if you still need it."
	}
	return "I have finished my turn. " + orDefault(a.lastText, "There was nothing to report.")
}

// messagedDuring reports whether this agent messaged `to` after event id since.
func (a *Agent) messagedDuring(to string, since int) bool {
	events, err := a.log.ReadAll()
	if err != nil {
		return false
	}
	for _, e := range events {
		if e.ID > since && e.Type == "agent_message" && e.To == to {
			return true
		}
	}
	return false
}

// setState records a transition. The old agent flipped this variable silently
// and only logged it on interrupt, so a streaming client had to infer busy from
// turn_complete; every transition is an event here.
func (a *Agent) setState(s string) {
	a.mu.Lock()
	if a.state == s {
		a.mu.Unlock()
		return
	}
	a.state = s
	a.mu.Unlock()
	a.log.Append(Event{Type: "state", SessionState: s})
}

// frame renders a queued message for the model. A message from another agent
// is labelled as such: the limits already say colleagues are not a chain of
// command, and that rule only works if the model can tell the difference.
func frame(in inbound) string {
	if in.from == "" {
		return in.text
	}
	return "Message from the agent \"" + in.from + "\", a colleague on this machine:\n\n" + in.text
}

// encode renders a tool's input for the log, since Input is an untyped any.
func encode(in any) json.RawMessage {
	buf, err := json.Marshal(in)
	if err != nil {
		return json.RawMessage(`"<unencodable input>"`)
	}
	return buf
}
