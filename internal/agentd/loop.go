// Package agentd is the in-guest multi-agent daemon: one process running
// several named agents, each with its own tools, memory and event log.
//
// It is the only agent a guest runs. The TypeScript agent it replaced is gone
// from the image entirely; what remains of it here is the measurements its
// behaviour produced, kept where they justify a decision.
package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
)

// ErrBusy is returned when an agent's inbox is full. Reported to the caller as
// 503 rather than blocking an HTTP handler on a model call.
var ErrBusy = errors.New("agent queue is full")

// ErrStopped is returned when a message is handed to an agent whose goroutine
// has already been cancelled -- it was recycled or evicted between the caller
// looking it up and the caller using it.
//
// Refusing is the whole point: this instance's inbox is never drained again, so
// accepting would log the person's message into a transcript that gets no
// reply. The supervisor turns it into a retry on the replacement instance, so
// it should never reach a client.
var ErrStopped = errors.New("agent was recycled")

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
//
// 100 rather than 40, because 40 was measured too low for the browser. One UI
// action costs about three calls -- snapshot, act, screenshot -- so 40 bought
// roughly thirteen actions, and a live booking ran out one click into a login
// modal after five and a half minutes of real work. The turn ends where it
// stands, so the cost of being too low is a job abandoned mid-way, which is
// worse than the cost of being too high: the model stops on its own when it is
// finished, and this only binds when something has gone wrong.
const maxIterations = 100

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

	// stopped is set when the supervisor cancels this agent's goroutine, and is
	// what closes the window between a caller looking an agent up and enqueueing
	// to it. Under the SAME lock as state and the inbox check, so a recycle
	// cannot slip between "idle with nothing queued" and the message that would
	// have made that false.
	stopped bool

	// What the last request actually cost in input tokens, which is what
	// compaction triggers on. Read off the response rather than estimated from
	// convBytes: the two diverge badly once a browsing agent's history fills
	// with base64 the API is already clearing, and this is the number billed.
	lastInput int64

	// reload is shared with this agent's tools: create_skill sets it, and Run
	// acts on it once the turn is over. Not guarded by a.mu -- it has its own
	// lock, because a tool handler sets it from a goroutine the runner owns.
	reload *reloadFlag

	// How many people this agent is currently blocked on. A count rather than a
	// flag because the runner calls tool handlers concurrently, so two questions
	// can be open at once.
	waiting int
}

// inbound is one queued message. From is empty when it came from the person
// and names the sender when it came from another agent, which decides both how
// it is logged and how it is framed for the model.
type inbound struct {
	text string
	from string
	// schedule names the timer that started this, and is empty for everything
	// else. Deliberately not folded into from: that means "another agent on this
	// machine", and a timer rendered as a colleague is a lie in the transcript.
	schedule string
	// file is something the person attached. Kept beside the text rather than
	// pasted into it: the model needs to be told the path, and the transcript
	// needs to show what they actually typed.
	file *agentapi.File
	// replyTo is set when this message is delegated work. The delegator is
	// then told the outcome whatever happens, including when the agent runs
	// out of iterations mid-task -- otherwise a boss waits forever on a
	// worker that quietly stopped, which is exactly what happened the first
	// time this was run end to end.
	replyTo string
}

// New builds an agent rooted at dir, working in workspace, restoring its
// conversation and log from disk. The client reads ANTHROPIC_API_KEY from the
// environment.
//
// The agent owns its log, gate and tools rather than being handed them: the
// gate records into the log and the tools call the gate, so assembling them
// anywhere else just moves the knot.
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
	// Read once, then threaded. Everything downstream takes the directory as a
	// parameter, so there is one source of truth for where built-in skills are.
	r := roots{workspace: workspace, own: dir, builtin: BuiltinSkillsDir}
	skills, problems := LoadSkills(r.builtin, dir)
	reportBadSkills(log, problems)
	gate := NewGate(log, hubOf(team), dir)
	reload := &reloadFlag{}
	deps := toolDeps{gate: gate, team: team, self: id, log: log, browser: p.Browser,
		stateDir: stateDirOf(team), reload: reload, apps: appsOf(team)}
	if p.Browser {
		// A browser agent that cannot reach Chrome is still worth starting: it
		// keeps every other tool, and the failure shows up in its log rather
		// than as a VM that refuses to boot.
		if err := attachBrowser(&deps, dir, team); err != nil {
			log.Append(Event{Type: "error", Message: "no browser: " + err.Error()})
		}
	}
	tools, err := Tools(r, deps, p.Tools)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		id: id, dir: dir, client: anthropic.NewClient(), profile: p,
		model: p.Model, system: ComposeSystemPrompt(p, r, stateDirOf(team), skills),
		tools: tools, log: log, gate: gate, team: team, state: "idle",
		inbox: make(chan inbound, inboxDepth), reload: reload,
	}
	// Wired after the agent exists, because the gate is built before it and the
	// gate is what knows when a person has become the thing this agent is doing.
	gate.onWait = a.addWait
	if err := a.load(); err != nil {
		return nil, err
	}
	a.log.Append(Event{Type: "ready", Message: "agent " + id + " ready"})
	return a, nil
}

// appsOf is the machine's connected-apps session, or nil when this agent has no
// team -- which is only ever the case in a unit test.
func appsOf(team *Supervisor) *appsServer {
	if team == nil {
		return nil
	}
	return team.Apps()
}

// hubOf is the machine's interaction hub, or nil when this agent has no team --
// which is only ever the case in a unit test.
func hubOf(team *Supervisor) *Interactions {
	if team == nil {
		return nil
	}
	return team.Interactions()
}

// stateDirOf is where the machine keeps what it knows about the person, or ""
// when this agent has no team -- which is only ever the case in a unit test.
func stateDirOf(team *Supervisor) string {
	if team == nil {
		return ""
	}
	return team.stateDir
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
			// After runTurn, so the turn's canceller is already cleared: this
			// is bookkeeping between turns, and Interrupt must not appear to
			// stop it. On the agent's own context, so shutdown still does.
			a.compactIfNeeded(ctx)
			a.recycleIfStale()
		}
	}
}

// recycleIfStale asks the supervisor to drop this agent when its prompt no
// longer matches what is on disk, so the next message rebuilds it.
//
// Placed at the turn boundary and nowhere else. Inside `turn`, a.save() is the
// LAST statement -- after finish and after report -- so this is the only point
// at which the event log and conversation.json are both settled. It is also
// safe from here: the lock order in the supervisor is one-way, and report()
// already calls back into it from this same goroutine on every delegated turn.
//
// Recycle does the deciding, because whether an agent may go depends on state
// only the supervisor can check without racing.
func (a *Agent) recycleIfStale() {
	if !a.reload.take() {
		return
	}
	if a.team == nil {
		return // a unit test, which has no supervisor to recycle it
	}
	// The note is appended by Recycle, while the supervisor still owns this
	// agent: after the entry is dropped, a message arriving in that window
	// starts a second instance, and OpenLog reads the last id off DISK -- so an
	// append made after that read hands out an id the new log is about to reuse,
	// and SSE replay needs them unique.
	if a.team.Recycle(a.id, func() {
		a.log.Append(Event{Type: "skill", Message: "reloading so a new skill is available"})
	}) {
		return
	}
	// Refused because a message arrived while the turn was ending. Put the
	// request back rather than dropping it: the queued turn runs on the old
	// prompt either way, but without this the skill the agent just wrote stays
	// invisible until some unrelated eviction. The next turn boundary tries again.
	a.reload.set()
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
func (a *Agent) Send(text string) error { return a.SendFile(text, nil) }

// SendFile queues a message from the person with a file attached to it.
func (a *Agent) SendFile(text string, file *agentapi.File) error {
	if err := a.enqueue(inbound{text: text, file: file}); err != nil {
		return err
	}
	// Logged at receipt, not when the turn dequeues it. A message sent while
	// the agent is busy would otherwise be invisible for however long that turn
	// runs, which reads as "my message did not send" -- and appending BEFORE the
	// enqueue would do the opposite, showing a message the agent refused.
	a.log.Append(Event{Type: "user", Text: text, File: file})
	return nil
}

// SendScheduled queues a turn started by a timer rather than by the person.
//
// Logged as "scheduled" and not "user", because the transcript must never show
// the person saying words they did not type. Event.Type is a free string and
// TaskTitle already exists, so this needs nothing new on the wire and an older
// client renders an unknown event rather than breaking.
func (a *Agent) SendScheduled(name, text string) error {
	if err := a.enqueue(inbound{text: text, schedule: name}); err != nil {
		return err
	}
	a.log.Append(Event{Type: "scheduled", TaskTitle: name, Text: text})
	return nil
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
//
// Under a.mu, and not because the channel needs it: the lock is what makes the
// refusal above meaningful. quiesce checks the inbox and marks the agent
// stopped while holding the same lock, so a message either lands before the
// recycle -- which then refuses -- or is refused itself. Nothing can fall
// between the two. The send never blocks, so holding the lock across it cannot
// stall a turn.
func (a *Agent) enqueue(in inbound) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return ErrStopped
	}
	select {
	case a.inbox <- in:
		return nil
	default:
		return ErrBusy
	}
}

// quiesce marks the agent stopped if it is idle with an empty inbox, reporting
// whether it did. The supervisor calls it before cancelling.
//
// Both guards live here rather than in the supervisor so that they are checked
// under the lock enqueue takes. Reading len(inbox) from outside proves nothing:
// a caller holding a pointer from a moment ago can push onto it between that
// read and the cancel, and the message would then sit in the person's
// transcript with no reply ever coming.
func (a *Agent) quiesce() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped || a.state != "idle" || len(a.inbox) > 0 {
		return false
	}
	a.stopped = true
	return true
}

// stop marks the agent as no longer running, so a caller still holding it is
// told to look it up again rather than enqueueing into a dead inbox.
func (a *Agent) stop() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.stopped = true
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
	if err := json.Unmarshal(buf, &a.messages); err != nil {
		return err
	}
	// A history written before repairTail existed can end in an orphan tool_use,
	// which 400s every turn from then on. Healing here is what unwedges an agent
	// that has already stopped taking input.
	a.messages = repairTail(a.messages)
	return nil
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
	a.setState("working")
	candidate := slices.Clone(a.messages)
	candidate = append(candidate, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(frame(in))))
	runner := a.client.Beta.Messages.NewToolRunner(a.tools, a.params(candidate))
	// The watermark starts at the restored history, not at zero: everything below
	// it already has its results, and re-reading them would append the whole of a
	// long conversation's tool output to the log on every single turn.
	if err := a.drain(ctx, runner, len(candidate)); err != nil {
		a.finish(started, err)
		a.report(in, runner, err)
		return err
	}
	a.adopt(runner)
	a.finish(started, nil)
	a.report(in, runner, nil)
	return a.save()
}

// adopt takes the runner's history as the agent's own, answering any tool call
// the turn stopped before running.
func (a *Agent) adopt(runner *anthropic.BetaToolRunner) {
	msgs := runner.Messages()
	fixed := repairTail(msgs)
	a.mu.Lock()
	a.messages = fixed
	a.mu.Unlock()
	a.noteStop(runner, len(fixed) != len(msgs))
}

// noteStop records a turn that ended at a ceiling rather than because the model
// was finished. Only a delegator heard about this before, so a person reading the
// log saw a turn that looked complete and a task that silently was not.
func (a *Agent) noteStop(runner *anthropic.BetaToolRunner, repaired bool) {
	if runner.IterationCount() >= maxIterations {
		a.log.Append(Event{Type: "error", Message: "stopped at the step limit for one turn"})
	}
	if repaired {
		a.log.Append(Event{Type: "error", Message: "answered a tool call the turn never ran"})
	}
}

// drain pumps the runner one turn at a time, recording each message.
//
// Iterating with NextMessage rather than RunToCompletion is deliberate: this
// loop body is where the approval gate and the browser lease will hook in. It
// is also why the rollback rule exists -- NextMessage appends the assistant
// message immediately but appends the matching tool_result only at the START of
// the next call, so mid-loop the history ends with an orphan tool_use that
// would make the next request malformed.
func (a *Agent) drain(ctx context.Context, runner *anthropic.BetaToolRunner, seen int) error {
	for {
		msg, err := runner.NextMessage(ctx)
		// Before the error check: a call that failed may still have executed its
		// tools, and the result is what says why.
		seen = a.recordResults(runner.Messages(), seen)
		if err != nil {
			return err
		}
		if msg == nil {
			return nil
		}
		a.record(msg)
	}
}

// resultCap bounds one logged tool result. A page snapshot is thousands of
// tokens and the log is read line by line by a person.
const resultCap = 2_000

// recordResults logs the tool results appended since the last pass, returning
// the new watermark.
//
// The SDK produces these inside NextMessage and they never reach record, which
// walks assistant messages only. That left the log able to show that fill was
// called and what the model said next, but never what the browser answered --
// the one line that explains a failure.
func (a *Agent) recordResults(msgs []anthropic.BetaMessageParam, from int) int {
	for _, msg := range msgs[min(from, len(msgs)):] {
		for _, block := range msg.Content {
			if block.OfToolResult == nil {
				continue
			}
			a.log.Append(Event{Type: "tool_result", Text: resultBlockText(*block.OfToolResult),
				IsError: block.OfToolResult.IsError.Or(false)})
		}
	}
	return len(msgs)
}

// resultBlockText renders a tool result for the log, capped, naming any part that
// is not text rather than dropping it silently.
func resultBlockText(res anthropic.BetaToolResultBlockParam) string {
	var out strings.Builder
	for _, c := range res.Content {
		if c.OfText != nil {
			out.WriteString(c.OfText.Text)
			continue
		}
		out.WriteString("[non-text result]")
	}
	if out.Len() <= resultCap {
		return out.String()
	}
	return out.String()[:resultCap] + "\n[truncated]"
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
	used := Usage{
		InputTokens:              msg.Usage.InputTokens,
		OutputTokens:             msg.Usage.OutputTokens,
		CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		ClearedInputTokens:       cleared,
		ClearedToolUses:          uses,
	}
	a.mu.Lock()
	a.lastInput = msg.Usage.InputTokens
	a.mu.Unlock()
	a.bookUsage(msg.Model, used)
}

// bookUsage puts one response's tokens in both places that account for spend.
//
// The log is this agent's transcript; the meter is the machine's total. Both are
// written, because the host must not have to read every agent's log back to
// answer what the VM has cost. Separate from record so that a call the person
// never sees -- the compaction summary -- is still paid for in both, rather than
// spending real tokens that /usage cannot account for.
func (a *Agent) bookUsage(model string, used Usage) {
	a.log.Append(Event{Type: "usage", Model: model, Usage: &used})
	a.meter().Record(model, used)
}

// meter is the machine's spend counter, or nil when this agent has no team --
// which is only ever the case in a unit test.
func (a *Agent) meter() *Meter {
	if a.team == nil {
		return nil
	}
	return a.team.Meter()
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
	took := time.Since(started)
	a.log.Append(Event{
		Type:       "turn_complete",
		IsError:    err != nil,
		DurationMS: took.Milliseconds(),
	})
	a.meter().FinishTurn(took)
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

// addWait moves the count of people blocking this agent, and publishes the state
// that follows from it.
//
// Waiting is not working, and the difference is visible to a person: the app
// draws its typing animation from the working state, so an agent parked on a
// question showed one for however long they took to answer. It looked like an
// agent busy composing a reply when it was in fact waiting on them, and the one
// thing it could not do was type.
func (a *Agent) addWait(delta int) {
	a.mu.Lock()
	a.waiting += delta
	blocked := a.waiting > 0
	a.mu.Unlock()
	if blocked {
		a.setState("waiting")
		return
	}
	a.setState("working")
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
	if in.file != nil {
		return withFile(in.text, in.file)
	}
	if in.schedule != "" {
		return scheduleNote(in.schedule) + "\n\n" + in.text
	}
	if in.from == "" {
		return in.text
	}
	return "Message from the agent \"" + in.from + "\", a colleague on this machine:\n\n" + in.text
}

// scheduleNote tells the model a timer started this and nobody may be watching.
//
// The instruction to ask everything at once is the load-bearing part: an
// approval raised here waits for the person rather than failing, so a run that
// asks one question at a time spends a night per question.
func scheduleNote(name string) string {
	return "[Scheduled task \"" + name + "\" - a timer started this, not the person." +
		" They may not be at the keyboard. If you need approval the run will wait" +
		" for them, so ask for everything you need at once rather than one thing" +
		" at a time.]"
}

// withFile tells the model where an attachment landed. The path is the only part
// of an attachment it can act on, and it is added here rather than to the message
// itself so the transcript still shows what the person actually typed.
func withFile(text string, f *agentapi.File) string {
	note := "The person attached a file: " + f.Path
	if text == "" {
		return note
	}
	return text + "\n\n" + note
}

// encode renders a tool's input for the log, since Input is an untyped any.
func encode(in any) json.RawMessage {
	buf, err := json.Marshal(in)
	if err != nil {
		return json.RawMessage(`"<unencodable input>"`)
	}
	return buf
}
