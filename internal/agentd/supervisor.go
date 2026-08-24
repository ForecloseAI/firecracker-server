package agentd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
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

	mu     sync.Mutex
	agents map[string]*live
}

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
	return &Supervisor{
		stateDir: stateDir, workspace: workspace, catalog: catalog,
		model: model, maxLive: maxLive, roster: roster, ctx: ctx,
		agents: map[string]*live{},
	}, nil
}

// Roster exposes the durable roster.
func (s *Supervisor) Roster() *Roster { return s.roster }

// Catalog exposes the profile catalog.
func (s *Supervisor) Catalog() *Catalog { return s.catalog }

// Status is one agent's row in the API: identity from the roster, state from
// whether it is running and what it is doing.
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
func (s *Supervisor) statusFor(rec Record) Status {
	st := Status{ID: rec.ID, Name: rec.Name, Type: rec.Type, State: "idle", Task: rec.Task}
	s.mu.Lock()
	l, ok := s.agents[rec.ID]
	s.mu.Unlock()
	if !ok {
		return st
	}
	st.Live, st.State = true, l.agent.State()
	st.LastEventID, st.Conversation = l.agent.Log().LastID(), l.agent.ConversationBytes()
	return st
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

// start builds an agent and its goroutine, making room first if needed.
func (s *Supervisor) start(rec Record) (*Agent, error) {
	profile, err := s.profileFor(rec)
	if err != nil {
		return nil, err
	}
	agent, err := New(rec.ID, s.dirFor(rec.ID), s.workspace, profile)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.agents[rec.ID]; ok {
		return existing.agent, nil // another caller won the race
	}
	if err := s.makeRoomLocked(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.agents[rec.ID] = &live{agent: agent, cancel: cancel, lastUsed: time.Now()}
	go agent.Run(ctx)
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
func (s *Supervisor) makeRoomLocked() error {
	if len(s.agents) < s.maxLive {
		return nil
	}
	var oldest string
	for id, l := range s.agents {
		if l.agent.State() != "idle" {
			continue
		}
		if oldest == "" || l.lastUsed.Before(s.agents[oldest].lastUsed) {
			oldest = id
		}
	}
	if oldest == "" {
		return ErrNoCapacity
	}
	s.stopLocked(oldest)
	return nil
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
func (s *Supervisor) stopLocked(id string) {
	l, ok := s.agents[id]
	if !ok {
		return
	}
	l.cancel()
	delete(s.agents, id)
}

// Close stops every running agent.
func (s *Supervisor) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.agents {
		s.stopLocked(id)
	}
}

// LiveCount is how many agents currently hold a goroutine, for /debug/memstats.
func (s *Supervisor) LiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.agents)
}

// dirFor is where one agent's state lives.
func (s *Supervisor) dirFor(id string) string {
	return filepath.Join(s.stateDir, "agents", id)
}
