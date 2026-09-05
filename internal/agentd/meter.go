package agentd

import (
	"cmp"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// meterFile is where the running total lives, beside the roster on the
// overlay. It shares the overlay's lifetime, so it survives DELETE and dies on
// ?purge=true -- the same rule the event logs and the workspace follow.
const meterFile = "usage.json"

// meterVersion is the file format. Version 1 had no field of that name and
// kept one row per model; a probe for it is how the two are told apart.
const meterVersion = 2

// meterKeepDays is how long a day keeps its own row before folding into the
// agent's lifetime carry. A year and a margin: "today" and "this week" are
// always exact, and the file stays kilobytes however long the machine lives.
const meterKeepDays = 400

// Meter is what this daemon has spent, in tokens, by agent, by model, by day.
//
// The event logs already hold this, but deriving it meant the host re-reading
// every agent's whole log on every dashboard poll and keeping a watermark per
// feed so nothing was counted twice. The daemon knows the number as it
// happens; reporting it directly deletes that machinery.
//
// Days are cut on the person's own clock, read through personNow rather than
// time.Local -- a zone change mid-life must not move the day, and nothing in
// this daemon reads time.Local. A row is stamped in the zone current when it
// was recorded; a later zone change does not re-bucket history.
//
// Not seeded from the logs at startup: a daemon that has never metered starts
// at zero rather than replaying history it would then have to deduplicate.
type Meter struct {
	path string
	now  func() time.Time

	mu   sync.Mutex
	rows map[rowKey]*usageRow
	// folded is the day the retention fold last ran on. The cutoff moves once
	// a day, so the scan is worth doing that often and no more.
	folded string
	state  meterState
}

// rowKey names one agent's spend on one model on one day. An empty day is the
// carry row: everything older than meterKeepDays, folded together. An empty
// model is the agent's turns for that day, which belong to no one model.
type rowKey struct{ agent, model, day string }

// usageRow is one rowKey's count. Calls is assistant messages, which is what
// the version-1 file called turns and what ModelUsage.Turns still means; Turns
// is set only on the model-less rows that count turns ended.
type usageRow struct {
	Agent string `json:"agent"`
	Model string `json:"model,omitempty"`
	Day   string `json:"day,omitempty"`
	agentapi.Usage
	Calls int64 `json:"calls,omitempty"`
	Turns int64 `json:"turns,omitempty"`
}

// meterState is the part of a report that is neither per-model nor per-agent.
type meterState struct {
	LastDurationMS int64     `json:"last_duration_ms,omitempty"`
	LastActivity   time.Time `json:"last_activity,omitzero"`
}

// meterFileV1 is the shape before agents were told apart.
type meterFileV1 struct {
	ByModel []agentapi.ModelUsage `json:"by_model"`
	Turns   int64                 `json:"turns"`
	meterState
}

// meterFileV2 is what is persisted now.
type meterFileV2 struct {
	Version int        `json:"version"`
	Rows    []usageRow `json:"rows"`
	meterState
}

// OpenMeter loads the running total from dir, starting empty if there is none.
//
// A corrupt or unreadable file starts empty rather than failing: losing a spend
// total is a reporting gap, and refusing to boot over one would turn it into
// an outage. A version-1 file is not corrupt: it is carried over whole.
func OpenMeter(dir string) *Meter {
	m := &Meter{path: filepath.Join(dir, meterFile), rows: map[rowKey]*usageRow{},
		now: func() time.Time { return personNow(dir) }}
	buf, err := os.ReadFile(m.path)
	if err != nil {
		return m
	}
	f, err := readMeterFile(buf)
	if err != nil {
		log.Printf("agentd: usage total unreadable, starting from zero: %v", err)
		return m
	}
	for i := range f.Rows {
		row := f.Rows[i]
		m.rows[rowKey{row.Agent, row.Model, row.Day}] = &row
	}
	m.state = f.meterState
	m.foldLocked(m.now())
	return m
}

// readMeterFile decodes either format, upgrading the old one on the way in.
func readMeterFile(buf []byte) (meterFileV2, error) {
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(buf, &probe); err != nil {
		return meterFileV2{}, err
	}
	switch probe.Version {
	case 0:
		var v1 meterFileV1
		err := json.Unmarshal(buf, &v1)
		return migrateV1(v1), err
	case meterVersion:
		var f meterFileV2
		err := json.Unmarshal(buf, &f)
		return f, err
	}
	return meterFileV2{}, fmt.Errorf("usage file version %d is newer than this daemon", probe.Version)
}

// migrateV1 keeps a version-1 total as the carry rows of an agent nobody was:
// the spend is real and the bill must not shrink, but there is no one to
// credit it to.
func migrateV1(v1 meterFileV1) meterFileV2 {
	f := meterFileV2{Version: meterVersion, meterState: v1.meterState,
		Rows: []usageRow{{Agent: agentapi.UnattributedAgent, Turns: v1.Turns}}}
	for _, row := range v1.ByModel {
		f.Rows = append(f.Rows, usageRow{Agent: agentapi.UnattributedAgent, Model: row.Model,
			Usage: row.Usage, Calls: row.Turns})
	}
	return f
}

// Record adds one assistant message's usage to an agent's row for today.
// Model is the id the API resolved the request to, which is what the price
// table is keyed on.
func (m *Meter) Record(agent, model string, u agentapi.Usage) {
	if m == nil {
		return
	}
	now := m.now()
	m.mu.Lock()
	row := m.rowLocked(agent, model, dateOf(now))
	addUsage(&row.Usage, u)
	row.Calls++
	m.state.LastActivity = time.Now().UTC()
	m.mu.Unlock()
	m.save(now)
}

// FinishTurn records that one agent's turn ended, which is what "turns" counts.
//
// Deliberately not counted in Record: the daemon emits one usage event per
// assistant message, so a turn with five tool calls produces six of them.
// Counting those as turns inflated the figure several-fold and made
// cost-per-turn meaningless while every token total stayed correct.
func (m *Meter) FinishTurn(agent string, d time.Duration) {
	if m == nil {
		return
	}
	now := m.now()
	m.mu.Lock()
	m.rowLocked(agent, "", dateOf(now)).Turns++
	m.state.LastDurationMS = d.Milliseconds()
	m.mu.Unlock()
	m.save(now)
}

// rowLocked finds or starts one row. Caller holds m.mu.
func (m *Meter) rowLocked(agent, model, day string) *usageRow {
	k := rowKey{agent, model, day}
	row, ok := m.rows[k]
	if !ok {
		row = &usageRow{Agent: agent, Model: model, Day: day}
		m.rows[k] = row
	}
	return row
}

// foldLocked moves rows older than the retention into each agent's lifetime
// carry, so the file cannot grow without bound. Once a day. Caller holds m.mu.
func (m *Meter) foldLocked(now time.Time) {
	today := dateOf(now)
	if today == m.folded {
		return
	}
	m.folded = today
	cutoff := dateOf(now.AddDate(0, 0, -meterKeepDays))
	for k, row := range m.rows {
		if k.day == "" || k.day >= cutoff {
			continue
		}
		carry := m.rowLocked(k.agent, k.model, "")
		addUsage(&carry.Usage, row.Usage)
		carry.Calls, carry.Turns = carry.Calls+row.Calls, carry.Turns+row.Turns
		delete(m.rows, k)
	}
}

// addUsage folds one usage block into a running one.
func addUsage(into *agentapi.Usage, u agentapi.Usage) {
	into.InputTokens += u.InputTokens
	into.OutputTokens += u.OutputTokens
	into.CacheCreationInputTokens += u.CacheCreationInputTokens
	into.CacheReadInputTokens += u.CacheReadInputTokens
	into.ClearedInputTokens += u.ClearedInputTokens
	into.ClearedToolUses += u.ClearedToolUses
	into.CostUSD += u.CostUSD
}

// Report is the machine-wide total the host's dashboard has always read:
// every row summed by model, and every turn ever ended, exactly as before the
// split. The per-agent view is ByAgent, on its own route, because this one is
// polled every few seconds and the windows are only ever wanted by a person.
func (m *Meter) Report() agentapi.UsageReport {
	if m == nil {
		return agentapi.UsageReport{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	byModel := map[string]*agentapi.ModelUsage{}
	var turns int64
	for _, row := range m.rows {
		turns += row.Turns
		if row.Model != "" {
			addModel(byModel, row)
		}
	}
	return agentapi.UsageReport{ByModel: sortedModels(byModel), Turns: turns,
		LastDurationMS: m.state.LastDurationMS, LastActivity: m.state.LastActivity}
}

// ByAgent cuts three windows per agent at a moment on the person's clock:
// today, the calendar week from Monday, and everything.
func (m *Meter) ByAgent(now time.Time) agentapi.AgentUsageReport {
	m.mu.Lock()
	defer m.mu.Unlock()
	today, week := dateOf(now), dateOf(mondayOf(now))
	out := agentapi.AgentUsageReport{Zone: now.Location().String(), Today: today, WeekStart: week,
		Agents: []agentapi.AgentUsage{}}
	for _, agent := range m.agentsLocked() {
		out.Agents = append(out.Agents, agentapi.AgentUsage{Agent: agent,
			Today: m.windowLocked(agent, today, today), Week: m.windowLocked(agent, week, today),
			Lifetime: m.windowLocked(agent, "", today)})
	}
	return out
}

// agentsLocked is every agent id with a row, in a stable order. Caller holds m.mu.
func (m *Meter) agentsLocked() []string {
	seen := map[string]bool{}
	for k := range m.rows {
		seen[k.agent] = true
	}
	out := make([]string, 0, len(seen))
	for agent := range seen {
		out = append(out, agent)
	}
	sort.Strings(out)
	return out
}

// windowLocked sums one agent's rows between two days inclusive: tokens by
// model, turns from the model-less rows. An empty from means the lifetime,
// which is the only window the carry rows join. Caller holds m.mu.
func (m *Meter) windowLocked(agent, from, to string) agentapi.UsageWindow {
	byModel := map[string]*agentapi.ModelUsage{}
	var turns int64
	for k, row := range m.rows {
		if k.agent != agent || !inWindow(k.day, from, to) {
			continue
		}
		if k.model == "" {
			turns += row.Turns
		} else {
			addModel(byModel, row)
		}
	}
	return agentapi.UsageWindow{ByModel: sortedModels(byModel), Turns: turns}
}

// inWindow says whether a row's day falls in [from, to]. Dates are ISO
// strings, so they compare as text; a carry row (no day) is lifetime only.
func inWindow(day, from, to string) bool {
	if from == "" {
		return true
	}
	return day != "" && from <= day && day <= to
}

// addModel folds one row into its model's running sum, starting it if needed.
func addModel(byModel map[string]*agentapi.ModelUsage, row *usageRow) {
	sum, ok := byModel[row.Model]
	if !ok {
		sum = &agentapi.ModelUsage{Model: row.Model}
		byModel[row.Model] = sum
	}
	addUsage(&sum.Usage, row.Usage)
	sum.Turns += row.Calls
}

// sortedModels copies per-model sums out in a stable order, never as nil.
func sortedModels(byModel map[string]*agentapi.ModelUsage) []agentapi.ModelUsage {
	out := make([]agentapi.ModelUsage, 0, len(byModel))
	for _, row := range byModel {
		out = append(out, *row)
	}
	slices.SortFunc(out, func(a, b agentapi.ModelUsage) int { return cmp.Compare(a.Model, b.Model) })
	return out
}

// mondayOf is local midnight of the Monday that starts now's week.
func mondayOf(now time.Time) time.Time {
	back := (int(now.Weekday()) + 6) % 7
	y, mo, d := now.AddDate(0, 0, -back).Date()
	return time.Date(y, mo, d, 0, 0, 0, 0, now.Location())
}

// dateOf is a moment as the day it fell on, in its own zone.
func dateOf(t time.Time) string { return t.Format("2006-01-02") }

// save persists the total. Written on every change rather than periodically:
// this is the billing record, and a crash between a turn and a flush would lose
// money that was really spent, silently.
func (m *Meter) save(now time.Time) {
	m.mu.Lock()
	m.foldLocked(now)
	buf, err := json.Marshal(m.fileLocked())
	m.mu.Unlock()
	if err != nil {
		log.Printf("agentd: cannot encode usage total: %v", err)
		return
	}
	if err := writeAtomic(m.path, buf); err != nil {
		log.Printf("agentd: cannot write %s: %v", m.path, err)
	}
}

// fileLocked is the file as it will be written, rows in a stable order so the
// file diffs cleanly. Caller holds m.mu.
func (m *Meter) fileLocked() meterFileV2 {
	f := meterFileV2{Version: meterVersion, Rows: make([]usageRow, 0, len(m.rows)), meterState: m.state}
	for _, row := range m.rows {
		f.Rows = append(f.Rows, *row)
	}
	slices.SortFunc(f.Rows, func(a, b usageRow) int {
		return cmp.Or(cmp.Compare(a.Agent, b.Agent), cmp.Compare(a.Model, b.Model), cmp.Compare(a.Day, b.Day))
	})
	return f
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
