package agentd

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// meterFile is where the running total lives, beside the roster on the overlay.
// It shares the overlay's lifetime, so it survives DELETE and dies on
// ?purge=true -- the same rule the event logs and the workspace follow.
const meterFile = "usage.json"

// Meter is what this daemon has spent, in tokens, across every agent.
//
// The event logs already hold this, but deriving it meant the host re-reading
// every agent's whole log on every dashboard poll and keeping a watermark per
// feed so nothing was counted twice. The daemon knows the number as it happens;
// reporting it directly deletes that machinery and the double-count hazard with
// it.
//
// Not seeded from the logs at startup: a daemon that has never metered starts
// at zero rather than replaying history it would then have to deduplicate. The
// logs remain the durable record of what happened.
type Meter struct {
	path string

	mu    sync.Mutex
	byKey map[string]*agentapi.ModelUsage
	state meterState
}

// meterState is the part of a report that is not per-model.
type meterState struct {
	Turns          int64     `json:"turns"`
	LastDurationMS int64     `json:"last_duration_ms,omitempty"`
	LastActivity   time.Time `json:"last_activity,omitzero"`
}

// meterFileFormat is what is persisted: the report, verbatim.
type meterFileFormat struct {
	ByModel []agentapi.ModelUsage `json:"by_model"`
	meterState
}

// OpenMeter loads the running total from dir, starting empty if there is none.
//
// A corrupt or unreadable file starts empty rather than failing: losing a spend
// total is a reporting gap, and refusing to boot over one would turn it into an
// outage.
func OpenMeter(dir string) *Meter {
	m := &Meter{path: filepath.Join(dir, meterFile), byKey: map[string]*agentapi.ModelUsage{}}
	buf, err := os.ReadFile(m.path)
	if err != nil {
		return m
	}
	var f meterFileFormat
	if err := json.Unmarshal(buf, &f); err != nil {
		log.Printf("agentd: usage total unreadable, starting from zero: %v", err)
		return m
	}
	for i := range f.ByModel {
		row := f.ByModel[i]
		m.byKey[row.Model] = &row
	}
	m.state = f.meterState
	return m
}

// Record adds one assistant message's usage. Model is the id the API resolved
// the request to, which is what the price table is keyed on.
func (m *Meter) Record(model string, u agentapi.Usage) {
	if m == nil {
		return
	}
	m.mu.Lock()
	row, ok := m.byKey[model]
	if !ok {
		row = &agentapi.ModelUsage{Model: model}
		m.byKey[model] = row
	}
	addUsage(&row.Usage, u)
	row.Turns++
	m.state.LastActivity = time.Now().UTC()
	m.mu.Unlock()
	m.save()
}

// FinishTurn records that a turn ended, which is what "turns" counts.
//
// Deliberately not counted in Record: the daemon emits one usage event per
// assistant message, so a turn with five tool calls produces six of them.
// Counting those as turns inflated the figure several-fold and made
// cost-per-turn meaningless while every token total stayed correct.
func (m *Meter) FinishTurn(d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.state.Turns++
	m.state.LastDurationMS = d.Milliseconds()
	m.mu.Unlock()
	m.save()
}

// addUsage folds one usage block into a running one.
func addUsage(into *agentapi.Usage, u agentapi.Usage) {
	into.InputTokens += u.InputTokens
	into.OutputTokens += u.OutputTokens
	into.CacheCreationInputTokens += u.CacheCreationInputTokens
	into.CacheReadInputTokens += u.CacheReadInputTokens
	into.ClearedInputTokens += u.ClearedInputTokens
	into.ClearedToolUses += u.ClearedToolUses
}

// Report is the current total, ordered by model so the output is stable.
func (m *Meter) Report() agentapi.UsageReport {
	if m == nil {
		return agentapi.UsageReport{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return agentapi.UsageReport{
		ByModel: m.rowsLocked(), Turns: m.state.Turns,
		LastDurationMS: m.state.LastDurationMS, LastActivity: m.state.LastActivity,
	}
}

// rowsLocked copies the per-model rows in a stable order. Caller holds m.mu.
func (m *Meter) rowsLocked() []agentapi.ModelUsage {
	out := make([]agentapi.ModelUsage, 0, len(m.byKey))
	for _, row := range m.byKey {
		out = append(out, *row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Model < out[j].Model })
	return out
}

// save persists the total. Written on every change rather than periodically:
// this is the billing record, and a crash between a turn and a flush would lose
// money that was really spent, silently.
func (m *Meter) save() {
	m.mu.Lock()
	buf, err := json.Marshal(meterFileFormat{ByModel: m.rowsLocked(), meterState: m.state})
	m.mu.Unlock()
	if err != nil {
		log.Printf("agentd: cannot encode usage total: %v", err)
		return
	}
	if err := writeAtomic(m.path, buf); err != nil {
		log.Printf("agentd: cannot write %s: %v", m.path, err)
	}
}

// writeAtomic replaces a file via a temp file and a rename, so a crash mid-write
// leaves the previous contents rather than a truncated one.
//
// Reports the failure rather than logging it, because the callers want
// different things from one: the meter carries on with a stale file on disk,
// while a tool has to tell the model its write did not happen. Swallowing it
// here would make create_skill report a success it never had.
func writeAtomic(path string, buf []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o640); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
