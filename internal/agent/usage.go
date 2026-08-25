package agent

import (
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// recentCap is how many trailing events the detail view keeps per VM.
const recentCap = 50

// Totals is one VM's cumulative agent spend. It covers the lifetime of the
// workspace, not this boot: events.jsonl lives on the persisted overlay and
// survives DELETE without ?purge=true, and a cold accumulator reads from id 0.
type Totals struct {
	CostUSD             float64   `json:"cost_usd"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	Turns               int64     `json:"turns"`
	LastCostUSD         float64   `json:"last_cost_usd"`
	LastDurationMS      int64     `json:"last_duration_ms"`
	LastActivity        time.Time `json:"last_activity,omitzero"`
}

// TotalTokens is every token billed across all four categories.
func (t Totals) TotalTokens() int64 {
	return t.InputTokens + t.OutputTokens + t.CacheReadTokens + t.CacheCreationTokens
}

// entry is one VM's accumulator: a watermark into the guest's event log, the
// running totals, and a trailing window of events for the detail view.
type entry struct {
	mu        sync.Mutex
	watermark int
	totals    Totals
	recent    []agentapi.Event
}

// Accumulator folds guest usage events into per-VM totals, polling each guest
// incrementally from a watermark so no event is read or counted twice.
type Accumulator struct {
	mu   sync.Mutex
	byID map[string]*entry
}

// NewAccumulator builds an empty accumulator.
func NewAccumulator() *Accumulator {
	return &Accumulator{byID: map[string]*entry{}}
}

// entryFor returns the per-VM accumulator, creating it on first sight.
func (a *Accumulator) entryFor(id string) *entry {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.byID[id]
	if !ok {
		e = &entry{}
		a.byID[id] = e
	}
	return e
}

// Forget drops a VM's totals and watermark. Required on delete: a ?purge=true
// recreate resets the guest log to id 1, and a stale high watermark would then
// skip every event the new VM ever emits.
func (a *Accumulator) Forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.byID, id)
}

// Update pulls any new events from a guest and returns its totals plus the
// trailing event window. The whole fetch-and-advance holds the VM's lock, so
// two concurrent dashboard polls cannot both consume the same batch.
//
// This reads the BOSS only, so a delegated worker's spend is not yet counted.
// That is deliberate and temporary: the daemon is about to report its own
// totals across the whole roster, at which point this watermark machinery goes
// away rather than growing a fan-out it would only keep for one phase.
func (a *Accumulator) Update(id, guestIP string, port int) (Totals, []agentapi.Event, error) {
	e := a.entryFor(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	events, last, err := New(guestIP, port).EventsSince(agentapi.BossID, e.watermark)
	if err != nil {
		return e.totals, e.recent, err
	}
	e.absorb(events, last)
	return e.totals, e.recent, nil
}

// absorb folds a fresh batch in and advances the watermark. Caller holds e.mu.
func (e *entry) absorb(events []agentapi.Event, last int) {
	for _, ev := range events {
		if ev.Type == "usage" {
			foldUsage(&e.totals, ev)
		}
	}
	e.recent = appendRecent(e.recent, events)
	// Trust the reported id only when it moves forward: a guest whose log was
	// reset would otherwise leave us stuck reading nothing.
	if last > e.watermark {
		e.watermark = last
	}
}

// foldUsage adds one usage event to a VM's running totals.
//
// Cost is not folded here: the daemon reports tokens and a model id and nothing
// else, so there is no dollar figure to add until the host learns to price
// them. Tokens and turns are correct in the meantime; cost reads zero, which is
// why the caller now surfaces a usage error rather than letting a clean $0.00
// stand in for "nobody looked".
func foldUsage(t *Totals, e agentapi.Event) {
	t.Turns++
	t.LastDurationMS = e.DurationMS
	if !e.TS.IsZero() {
		t.LastActivity = e.TS
	}
	if e.Usage == nil {
		return
	}
	t.InputTokens += e.Usage.InputTokens
	t.OutputTokens += e.Usage.OutputTokens
	t.CacheReadTokens += e.Usage.CacheReadInputTokens
	t.CacheCreationTokens += e.Usage.CacheCreationInputTokens
}

// appendRecent keeps the newest recentCap events, oldest first.
func appendRecent(recent, fresh []agentapi.Event) []agentapi.Event {
	out := append(recent, fresh...)
	if len(out) > recentCap {
		out = append([]agentapi.Event(nil), out[len(out)-recentCap:]...)
	}
	return out
}

// Snapshot returns a VM's last known totals and event window without touching
// the guest. Used for VMs that are not running, where a poll would only buy a
// timeout, and for the detail view once Update has already run.
func (a *Accumulator) Snapshot(id string) (Totals, []agentapi.Event) {
	e := a.entryFor(id)
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.totals, e.recent
}
