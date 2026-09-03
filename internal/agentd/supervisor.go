package agentd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// ErrNoCapacity is returned when the live-agent ceiling is reached and every
// live agent is busy, so none can be evicted to make room.
var ErrNoCapacity = errors.New("too many agents are working at once")

// live is one running agent: its goroutine, and when it was last needed.
type live struct {
	agent    *Agent
	cancel   context.CancelFunc
	lastUsed time.Time
}

// Supervisor owns the roster and the agents currently running from it.
//
// Existing and running are deliberately separate. A roster entry is a few
// hundred bytes on disk and lasts until deleted; a running agent costs a
// goroutine and a conversation in memory, and exists only while there is work.
// An evicted agent loses nothing: its conversation is already on disk and it
// resumes from there the next time it is addressed.
type Supervisor struct {
	stateDir  string
	workspace string
	catalog   *Catalog
	model     string // overrides the profile's model when set, for cheap testing
	maxLive   int
	roster    *Roster
	ctx       context.Context

	// wg tracks running agent goroutines so shutdown can wait for them.
	wg sync.WaitGroup

	mu     sync.Mutex
	agents map[string]*live

	// One chrome-devtools-mcp server for the whole machine, started on first
	// use. Agents share the browser the person is watching, so this must never
	// become one server per agent: a second one would be a second puppeteer
	// connection to the same Chrome.
	browser *browserServer

	// The machine's connected-apps session, pushed here by the host. One per
	// daemon, for the same reason the browser is: it is scoped to the person,
	// and every agent on this machine works for that same person.
	apps *appsServer

	// What this machine has spent, across every agent. Owned here rather than
	// per-agent because the question the host asks is "what did this VM cost",
	// and an evicted agent must not take its share of the answer with it.
	meter *Meter

	// Standing instructions to message an agent at a given time. Machine-wide
	// because the sweep has to see every agent's schedules in one pass, and
	// because a schedule must outlive the agent being evicted.
	schedules *ScheduleStore
	sweepDone chan struct{}
	sweepOnce sync.Once

	// Every agent currently waiting on a person. Machine-wide, because a raised
	// hand belongs to the team rather than to one agent's transcript, and the
	// person answering it must be able to reach whichever agent raised it.
	hub *Interactions
}

// ChromeURL is where the guest's Chrome exposes DevTools. A var so a test can
// point it at a stub, matching how the gate's timeouts are shrunk.
var ChromeURL = "http://127.0.0.1:9222"

// Browser is the machine's shared chrome-devtools-mcp server.
//
// Constructed with the supervisor but started on first use, because five of the
// six shipped profiles never open a page and a VM that never browses should
// never fork node.
func (s *Supervisor) Browser() *browserServer { return s.browser }

// Apps is the machine's connected-apps session. Never nil: a machine the host
// has pushed nothing to holds one with no URL, which advertises no tools.
func (s *Supervisor) Apps() *appsServer { return s.apps }

// NewSupervisor loads the roster and ensures this machine has a boss.
func NewSupervisor(ctx context.Context, stateDir, workspace string,
	catalog *Catalog, model string, maxLive int) (*Supervisor, error) {
	roster, err := LoadRoster(stateDir)
	if err != nil {
		return nil, err
	}
	if err := roster.EnsureBoss(BossID); err != nil {
		return nil, err
	}
	schedules, err := LoadSchedules(stateDir)
	if err != nil {
		return nil, err
	}
	s := &Supervisor{
		stateDir: stateDir, workspace: workspace, catalog: catalog,
		model: model, maxLive: maxLive, roster: roster, ctx: ctx,
		agents: map[string]*live{}, browser: newBrowserServer(ChromeURL, stateDir),
		apps:  newAppsServer(ReadApps(stateDir)),
		meter: OpenMeter(stateDir), hub: NewInteractions(), schedules: schedules,
		sweepDone: make(chan struct{}),
	}
	// Past occurrences are stepped over rather than fired: the VM was off, and a
	// machine asleep for a week must not wake into a week of backlog.
	s.rollForward(time.Now())
	s.wg.Add(1)
	go s.runSchedules()
	return s, nil
}

// Schedules exposes the machine's schedules, for the HTTP surface and the tools.
func (s *Supervisor) Schedules() *ScheduleStore { return s.schedules }

// Interactions exposes the machine's raised hands, for the HTTP surface.
func (s *Supervisor) Interactions() *Interactions { return s.hub }

// ResolveApproval delivers an answer to whichever agent raised it.
//
// The id names that agent, so nothing has to be told where to send it. Only
// LIVE agents are considered, deliberately: Get STARTS an agent, and a pending
// interaction can only exist on one that is running and blocked -- so "not
// live" correctly means "already settled", which is the existing 404 and a
// normal race rather than a client error.
func (s *Supervisor) ResolveApproval(apid string, d Decision) bool {
	id, _, ok := strings.Cut(apid, ".")
	if !ok {
		return false
	}
	s.mu.Lock()
	l, live := s.agents[id]
	s.mu.Unlock()
	return live && l.agent.Gate().Resolve(apid, d)
}

// Meter exposes the machine's running spend, for the HTTP surface.
func (s *Supervisor) Meter() *Meter { return s.meter }

// Roster exposes the durable roster.
func (s *Supervisor) Roster() *Roster { return s.roster }

// Catalog exposes the profile catalog.
func (s *Supervisor) Catalog() *Catalog { return s.catalog }

// List reports every agent without starting any of them. A poll must not be
// able to spawn work, or a dashboard refreshing every few seconds would hold
// the whole roster in memory forever.
func (s *Supervisor) List() []Status {
	out := make([]Status, 0)
	for _, rec := range s.roster.List() {
		out = append(out, s.statusFor(rec))
	}
	return out
}

// statusFor builds one row, reading live state only if the agent is running.
//
// A non-live agent still reports its real LastEventID, read off disk. Returning
// 0 -- as this used to -- tells a client that every idle agent has an empty
// history, which is exactly wrong: an evicted agent is the one most likely to
// have a long one.
func (s *Supervisor) statusFor(rec Record) Status {
	st := Status{ID: rec.ID, Name: rec.Name, Type: rec.Type, State: "idle", Task: rec.Task}
	l, ok := s.liveAgent(rec.ID)
	if !ok {
		_, last, _ := ReadLogSince(s.dirFor(rec.ID), 0)
		st.LastEventID = last
		return st
	}
	st.Live, st.State = true, l.agent.State()
	st.LastEventID, st.Conversation = l.agent.Log().LastID(), l.agent.ConversationBytes()
	return st
}

// liveAgent returns the running agent for an id, if there is one.
func (s *Supervisor) liveAgent(id string) (*live, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.agents[id]
	return l, ok
}

// History is one agent's log, WITHOUT starting it. A running agent's log is read
// in memory; otherwise the file is read directly. Only the live SSE stream needs
// a subscription, and paying a cold start for a snapshot would mean the app
// drawing six conversations spawned six agents to do it.
func (s *Supervisor) History(id string, since int) ([]Event, int, error) {
	if _, ok := s.roster.Get(id); !ok {
		return nil, 0, fmt.Errorf("no agent %s", id)
	}
	if l, ok := s.liveAgent(id); ok {
		events, err := l.agent.Log().Since(since)
		return events, l.agent.Log().LastID(), err
	}
	return ReadLogSince(s.dirFor(id), since)
}

// Get returns a running agent, starting it if it is not already running.
func (s *Supervisor) Get(id string) (*Agent, error) {
	s.mu.Lock()
	if l, ok := s.agents[id]; ok {
		l.lastUsed = time.Now()
		s.mu.Unlock()
		return l.agent, nil
	}
	s.mu.Unlock()
	rec, ok := s.roster.Get(id)
	if !ok {
		return nil, fmt.Errorf("no agent %s", id)
	}
	return s.start(rec)
}

// SendFile queues a message from the person, returning the agent that took it.
//
// Through here rather than Get-then-Send, because the message has to reach the
// agent that is actually running: recycling can cancel the instance a caller
// just looked up, and the reply reports the taker's state and last event id.
func (s *Supervisor) SendFile(id, text string, file *agentapi.File) (*Agent, error) {
	return s.sendTo(id, func(a *Agent) error { return a.SendFile(text, file) })
}

// sendTo enqueues onto a live agent, starting it again if it was stopped
// between the lookup and the enqueue.
//
// One retry is enough: only an idle agent is ever recycled, and the fresh
// instance cannot be recycled again before it has taken a turn. The retry has
// to happen out here rather than inside enqueue because the log entry for the
// message belongs to whichever instance accepted it -- an event appended
// through the old instance's log would reuse an id the new one already gave out.
func (s *Supervisor) sendTo(id string, put func(*Agent) error) (*Agent, error) {
	for attempt := 0; attempt < 2; attempt++ {
		a, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		if err := put(a); !errors.Is(err, ErrStopped) {
			if err != nil {
				return nil, err
			}
			return a, nil
		}
	}
	return nil, ErrStopped
}

// start builds an agent and its goroutine, making room first if needed.
func (s *Supervisor) start(rec Record) (*Agent, error) {
	profile, err := s.profileFor(rec)
	if err != nil {
		return nil, err
	}
	agent, err := New(rec.ID, s.dirFor(rec.ID), s.workspace, profile, s)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.agents[rec.ID]; ok {
		return existing.agent, nil // another caller won the race
	}
	if err := s.makeRoomLocked(rec.ID); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.agents[rec.ID] = &live{agent: agent, cancel: cancel, lastUsed: time.Now()}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		agent.Run(ctx)
	}()
	return agent, nil
}

// profileFor resolves a record's profile, applying the model override.
func (s *Supervisor) profileFor(rec Record) (Profile, error) {
	p, ok := s.catalog.Get(rec.Type)
	if !ok {
		return Profile{}, fmt.Errorf("agent %s has unknown type %q", rec.ID, rec.Type)
	}
	if s.model != "" {
		p.Model = s.model
	}
	return p, nil
}

// makeRoomLocked evicts an idle agent when the ceiling is reached. Caller holds
// s.mu.
//
// Eviction is safe because it loses nothing: the conversation is on disk, and
// the agent resumes from there. If every live agent is working, there is
// nothing safe to evict and the caller is told to retry.
// The boss is exempt from the ceiling, though not from eviction -- it resumes
// from disk, so losing its slot costs nothing. What it must never hit is the
// refusal below: it is the agent a person addresses directly, so declining to
// start it because every specialist is busy would lock them out of their own
// conversation exactly when the work they asked for is running.
func (s *Supervisor) makeRoomLocked(want string) error {
	if len(s.agents) < s.maxLive || want == BossID {
		return nil
	}
	// Through quiesce, like every other cancel path. State() alone says only
	// that no turn is RUNNING: a message sits on the inbox from the moment
	// enqueue returns until the goroutine picks it up, and the agent reads
	// "idle" for that whole window. Cancelling there drops a message already
	// logged as the person's, which is the failure Recycle refuses to risk.
	for _, id := range s.idleByAgeLocked() {
		if s.agents[id].agent.quiesce() {
			s.stopLocked(id)
			return nil
		}
	}
	return ErrNoCapacity
}

// idleByAgeLocked lists the agents reporting idle, least recently used first.
// Caller holds s.mu.
func (s *Supervisor) idleByAgeLocked() []string {
	var out []string
	for id, l := range s.agents {
		if l.agent.State() == "idle" {
			out = append(out, id)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return s.agents[out[i]].lastUsed.Before(s.agents[out[j]].lastUsed)
	})
	return out
}

// Create adds an agent of the given type and starts nothing: it runs when it
// is first addressed.
func (s *Supervisor) Create(typeKey, name string) (Record, error) {
	if _, ok := s.catalog.Get(typeKey); !ok {
		return Record{}, fmt.Errorf("no profile %q", typeKey)
	}
	return s.roster.Add(typeKey, name)
}

// Delete stops an agent and removes it from the roster. With purge, its whole
// state directory goes too -- conversation, event log, memory. Without it, the
// state survives, so recreating an agent with the same id gets its history
// back, which mirrors what DELETE on a VM already does with its workspace.
func (s *Supervisor) Delete(id string, purge bool) error {
	if err := s.roster.Remove(id); err != nil {
		return err
	}
	s.mu.Lock()
	s.stopLocked(id)
	s.mu.Unlock()
	if purge {
		return os.RemoveAll(s.dirFor(id))
	}
	return nil
}

// stopLocked ends one agent's goroutine. Caller holds s.mu.
//
// The agent is marked stopped as well as cancelled, because the pointer outlives
// the map entry: a caller that looked it up a moment ago must be told to look it
// up again rather than dropping its message into an inbox nothing will drain.
func (s *Supervisor) stopLocked(id string) {
	l, ok := s.agents[id]
	if !ok {
		return
	}
	l.agent.stop()
	l.cancel()
	delete(s.agents, id)
}

// Close stops every running agent and waits for them to finish.
//
// Waiting matters: a turn that is mid-write when the process exits leaves a
// truncated log or conversation behind. The lock is released before the wait
// because a turn winding down can still need it -- reporting back to a
// delegator goes through Deliver, which takes s.mu.
func (s *Supervisor) Close() {
	// Before the wait, and not left to s.ctx: the sweep is tracked by the same
	// wg as the agents, and a caller whose context outlives the supervisor --
	// every test builds one from context.Background() -- would otherwise wait
	// on a goroutine with nothing to stop it.
	s.stopSweep()
	s.mu.Lock()
	for id := range s.agents {
		s.stopLocked(id)
	}
	s.mu.Unlock()
	s.wg.Wait()
	// After the agents, not before: one of them may be mid-action on the page.
	s.browser.Close()
}

// EvictIdle stops every agent that is not mid-turn, so the next message to one
// rebuilds it.
//
// A prompt is composed once when an agent starts and then frozen, which is what
// keeps the cached prefix stable. So a change to what the machine knows about the
// person reaches nobody until they are recycled -- and onboarding that only takes
// effect on some later restart is onboarding that looks broken. A working agent
// is left alone: interrupting a turn to refresh a prompt would cost the person
// the work they are waiting on.
func (s *Supervisor) EvictIdle() {
	s.mu.Lock()
	defer s.mu.Unlock()
	// quiesce rather than State(), for the reason makeRoomLocked gives: an
	// agent with a message already on its inbox still reports idle, and
	// cancelling it there loses that message for good.
	for id, l := range s.agents {
		if l.agent.quiesce() {
			s.stopLocked(id)
		}
	}
}

// Recycle stops one agent so the next message to it rebuilds its prompt,
// reporting whether it actually did.
//
// The narrow version of EvictIdle: a skill belongs to the agent that wrote it,
// so recycling the whole machine to pick one up would throw away five other
// agents' warm state for nothing.
//
// Both guards matter, and both are checked by quiesce under the agent's OWN
// lock. A working agent must not be interrupted mid-task. And an agent with a
// queued message must not be dropped either: cancelling the goroutine abandons
// its inbox, and Send logs the person's message when it ARRIVES, so a message
// lost here would sit in their transcript with no reply coming, forever.
// Checking from out here would not be enough -- a caller holding a pointer from
// a moment ago can enqueue between the check and the cancel. Refusing is free:
// the next start picks the skill up anyway.
//
// note, when set, is appended while the agent is still in the map. The window
// after the delete belongs to whichever instance a new message starts, and
// OpenLog takes its next event id from disk: an append made in that window
// duplicates an id the new log has already handed out.
func (s *Supervisor) Recycle(id string, note func()) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.agents[id]
	if !ok || !l.agent.quiesce() {
		return false
	}
	if note != nil {
		note()
	}
	s.stopLocked(id)
	return true
}

// LiveCount is how many agents currently hold a goroutine, for /debug/memstats.
func (s *Supervisor) LiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.agents)
}

// Deliver hands a message from one agent to another, waking the recipient.
//
// Delivery starts the recipient if it was not running, which is the point: an
// agent that has been evicted must still be reachable by a colleague, or work
// would silently stall depending on what happened to be in memory.
func (s *Supervisor) Deliver(from, to, text string) error {
	if from == to {
		return fmt.Errorf("an agent cannot message itself")
	}
	_, err := s.sendTo(to, func(target *Agent) error { return target.Deliver(from, text) })
	return err
}

// Delegation is one piece of work handed from the boss to a specialist.
type Delegation struct {
	To      string
	Title   string
	Task    string
	TaskDir string
}

// Delegate hands work to another agent and returns without waiting.
//
// Asynchronous by default is what makes parallel work possible: the boss keeps
// its turn, the specialist starts its own, and the result comes back as a
// message rather than a return value.
func (s *Supervisor) Delegate(from string, d Delegation) error {
	if _, ok := s.roster.Get(d.To); !ok {
		return fmt.Errorf("no agent %s; create one first or check list_agents", d.To)
	}
	brief := delegationBrief(from, d)
	if _, err := s.sendTo(d.To, func(target *Agent) error { return target.DeliverWork(from, brief) }); err != nil {
		return err
	}
	s.logFor(from, Event{Type: "delegation", To: d.To, TaskTitle: d.Title, TaskDir: d.TaskDir})
	return nil
}

// delegationBrief is what the specialist actually receives. It has to carry
// everything: a worker cannot see the boss's conversation with the person.
func delegationBrief(from string, d Delegation) string {
	brief := "You have been given a task: " + d.Title + "\n\n" + d.Task
	if d.TaskDir != "" {
		brief += "\n\nWork in this folder, which already exists: " + d.TaskDir +
			"\nDo not open a folder of your own. Others are working in this one too," +
			" so keep to your own files and read anything you did not write before changing it."
	}
	return brief + "\n\nWhen you are done, use message_agent to tell \"" + from +
		"\" what you produced, where it is, and anything you could not finish."
}

// StartTask opens a dated folder for a piece of work and records it on the
// roster, so a poll can say what an agent is doing rather than just that it is
// busy.
func (s *Supervisor) StartTask(agentID, title, taskSlug string) (Task, error) {
	name := slug(taskSlug)
	if name == "" {
		return Task{}, fmt.Errorf("could not make a folder name from %q", taskSlug)
	}
	dir := filepath.Join(s.workspace, s.localDate(time.Now())+"-"+name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return Task{}, err
	}
	task := Task{Slug: name, Title: title, Dir: dir, StartedAt: time.Now().UTC()}
	s.logFor(agentID, Event{Type: "task_start", TaskSlug: name, TaskTitle: title, TaskDir: dir})
	return task, s.roster.SetTask(agentID, &task)
}

// localDate is today's date where the person is.
//
// Taken from the stored zone and not from time.Local, which AdoptZone sets at
// startup and which therefore still reads UTC on a machine onboarded since. A
// person who picks their country at 11pm would otherwise get a folder dated
// tomorrow, named differently from what `date` prints inside their own turn --
// the subprocess reads TZ, which onboarding moves at once. The schedule math
// takes its location the same way, and for the same reason: the file is the
// machine's zone and it is current the moment onboarding writes it.
func (s *Supervisor) localDate(now time.Time) string {
	return now.In(loadZone(s.stateDir)).Format("2006-01-02")
}

// CurrentTask reports what an agent is working on, if anything.
func (s *Supervisor) CurrentTask(agentID string) *Task {
	rec, ok := s.roster.Get(agentID)
	if !ok {
		return nil
	}
	return rec.Task
}

// logFor appends to a running agent's log, if it is running. Best effort: a
// missing log line must never fail the operation it was describing.
func (s *Supervisor) logFor(id string, e Event) {
	s.mu.Lock()
	l, ok := s.agents[id]
	s.mu.Unlock()
	if ok {
		l.agent.Log().Append(e)
	}
}

// dirFor is where one agent's state lives.
func (s *Supervisor) dirFor(id string) string {
	return filepath.Join(s.stateDir, "agents", id)
}
