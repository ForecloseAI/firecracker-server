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
	inbox   chan string

	// Guards everything the HTTP surface reads while the agent goroutine
	// writes it. The SDK runner itself stays confined to that one goroutine,
	// which is what the SDK requires.
	mu        sync.Mutex
	messages  []anthropic.BetaMessageParam
	state     string
	cancel    context.CancelFunc
	convBytes int
}

// New builds an agent rooted at dir, working in workspace, restoring its
// conversation and log from disk. The client reads ANTHROPIC_API_KEY from the
// environment.
//
// The agent owns its log, gate and tools rather than being handed them: the
// gate records into the log and the tools call the gate, so assembling them
// anywhere else just moves the knot.
func New(id, dir, workspace string, p Profile) (*Agent, error) {
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
	tools, err := Tools(roots{workspace: workspace, own: dir}, gate, p.Tools)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		id: id, dir: dir, client: anthropic.NewClient(), profile: p,
		model: p.Model, system: ComposeSystemPrompt(p, dir),
		tools: tools, log: log, gate: gate, state: "idle",
		inbox: make(chan string, inboxDepth),
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
		case text := <-a.inbox:
			a.runTurn(ctx, text)
		}
	}
}

// runTurn executes one turn under a cancellable child context, so Interrupt can
// stop the turn without tearing down the agent.
func (a *Agent) runTurn(parent context.Context, text string) {
	ctx, cancel := context.WithCancel(parent)
	a.setCancel(cancel)
	defer func() {
		a.setCancel(nil)
		cancel()
	}()
	a.Turn(ctx, text) // errors are already recorded in the event log
}

// Send queues a user message. A full inbox is reported rather than blocking the
// HTTP handler on a model call that may take a minute.
func (a *Agent) Send(text string) error {
	select {
	case a.inbox <- text:
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
			Model:     anthropic.Model(a.model),
			MaxTokens: maxTokens,
			System:    a.systemBlocks(),
			Messages:  msgs,
		},
	}
}

// Turn runs one user message to completion.
//
// The candidate history is built separately and only adopted on success. That
// is the rollback rule: a cancelled or failed turn leaves a.messages exactly
// as it was, so the next request is never malformed. See drain for why that
// matters.
func (a *Agent) Turn(ctx context.Context, text string) error {
	started := time.Now()
	a.log.Append(Event{Type: "user", Text: text})
	a.setState("working")
	candidate := slices.Clone(a.messages)
	candidate = append(candidate, anthropic.NewBetaUserMessage(anthropic.NewBetaTextBlock(text)))
	runner := a.client.Beta.Messages.NewToolRunner(a.tools, a.params(candidate))
	if err := a.drain(ctx, runner); err != nil {
		a.finish(started, err)
		return err
	}
	a.mu.Lock()
	a.messages = runner.Messages()
	a.mu.Unlock()
	a.finish(started, nil)
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
			a.log.Append(Event{Type: "text", Text: b.Text})
		case anthropic.BetaToolUseBlock:
			a.log.Append(Event{Type: "tool_use", Tool: b.Name, Input: encode(b.Input)})
		}
	}
	a.log.Append(Event{Type: "usage", Model: msg.Model, Usage: &Usage{
		InputTokens:              msg.Usage.InputTokens,
		OutputTokens:             msg.Usage.OutputTokens,
		CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
		CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
	}})
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

// encode renders a tool's input for the log, since Input is an untyped any.
func encode(in any) json.RawMessage {
	buf, err := json.Marshal(in)
	if err != nil {
		return json.RawMessage(`"<unencodable input>"`)
	}
	return buf
}
