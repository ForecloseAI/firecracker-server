package agentd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cracked/internal/agentapi"
)

// sonnet is a usage block worth a known number of tokens.
func sonnet(in, out int64) agentapi.Usage {
	return agentapi.Usage{InputTokens: in, OutputTokens: out, CacheReadInputTokens: 5}
}

// The one that matters. The meter is the billing record, and it lives on the
// same overlay as the event logs -- so it must come back after a restart. A
// meter that reset to zero would make every agentd restart silently forgive
// whatever the VM had spent, and the only symptom is a total that drops.
func TestUsageSurvivesADaemonRestart(t *testing.T) {
	dir := t.TempDir()
	m := OpenMeter(dir)
	m.Record("boss", "claude-sonnet-5", sonnet(100, 20))
	m.FinishTurn("boss", 1500*time.Millisecond)

	again := OpenMeter(dir)
	got := again.Report()
	if got.Turns != 1 || got.LastDurationMS != 1500 {
		t.Errorf("report = %+v, want 1 turn of 1500ms", got)
	}
	if len(got.ByModel) != 1 || got.ByModel[0].InputTokens != 100 {
		t.Fatalf("tokens did not survive the restart: %+v", got.ByModel)
	}
	// And it must keep counting from there, not start a second tally.
	again.Record("boss", "claude-sonnet-5", sonnet(100, 20))
	if n := again.Report().ByModel[0].InputTokens; n != 200 {
		t.Errorf("input tokens = %d after restart + one turn, want 200", n)
	}
}

// Turns count TURNS. The daemon emits one usage event per assistant message, so
// a turn with five tool calls produces six of them; counting those inflated the
// figure several-fold while every token total stayed correct, which made
// cost-per-turn quietly meaningless.
func TestTurnsCountTurnsNotAssistantMessages(t *testing.T) {
	m := OpenMeter(t.TempDir())
	for range 6 {
		m.Record("boss", "claude-sonnet-5", sonnet(10, 2))
	}
	m.FinishTurn("boss", time.Second)
	if got := m.Report().Turns; got != 1 {
		t.Errorf("turns = %d after six messages in one turn, want 1", got)
	}
	if got := m.Report().Agents[0].Lifetime.Turns; got != 1 {
		t.Errorf("the agent's own turns = %d, want 1", got)
	}
	if got := m.Report().ByModel[0].InputTokens; got != 60 {
		t.Errorf("input tokens = %d, want all six messages counted", got)
	}
}

// A delegated worker on a different model is the reason the report is split by
// model at all: one token total cannot be priced when the rates differ tenfold.
func TestEveryModelIsReportedSeparately(t *testing.T) {
	m := OpenMeter(t.TempDir())
	m.Record("boss", "claude-sonnet-5", sonnet(100, 10))
	m.Record("boss", "claude-haiku-4-5", sonnet(300, 30))
	rows := m.Report().ByModel
	if len(rows) != 2 {
		t.Fatalf("got %d model rows, want 2: %+v", len(rows), rows)
	}
	// Sorted, so the output is stable for a diff and for a person reading it.
	if rows[0].Model != "claude-haiku-4-5" || rows[1].Model != "claude-sonnet-5" {
		t.Errorf("rows are not in a stable order: %+v", rows)
	}
}

// A corrupt total is a reporting gap. Refusing to boot over one would turn it
// into an outage, which is a far worse trade for a number on a dashboard.
func TestCorruptMeterFileStartsFromZeroRatherThanFailing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, meterFile), []byte("{not json"), 0o640); err != nil {
		t.Fatal(err)
	}
	m := OpenMeter(dir)
	if got := m.Report().Turns; got != 0 {
		t.Errorf("turns = %d, want a clean zero", got)
	}
	m.Record("boss", "claude-sonnet-5", sonnet(1, 1))
	if len(m.Report().ByModel) != 1 {
		t.Error("a meter that started from a corrupt file cannot record")
	}
}

// An agent built without a team has no meter. That is only ever a unit test,
// but a nil dereference there would fail every one of them.
func TestANilMeterIsSafeToUse(t *testing.T) {
	var m *Meter
	m.Record("boss", "claude-sonnet-5", sonnet(1, 1))
	m.FinishTurn("boss", time.Second)
	if got := m.Report().Turns; got != 0 {
		t.Errorf("a nil meter reported %d turns", got)
	}
}

// ist is the person's zone in these tests: far enough from UTC that a day cut
// on the wrong clock shows up as the wrong date.
var ist = time.FixedZone("IST", 5*3600+30*60)

// meterAt opens a meter whose clock is pinned to one moment.
func meterAt(t *testing.T, dir string, at time.Time) *Meter {
	t.Helper()
	m := OpenMeter(dir)
	m.now = func() time.Time { return at }
	return m
}

// v1File is a usage.json as the daemon wrote it before agents were told apart.
const v1File = `{"by_model":[{"model":"claude-sonnet-5","input_tokens":100,"output_tokens":20,` +
	`"cache_creation_input_tokens":0,"cache_read_input_tokens":5,"turns":6}],"turns":5,"last_duration_ms":1500}`

// The upgrade must not touch the bill. Before this, a file the daemon could not
// decode started from zero -- and the new format is exactly such a file to the
// old decoder, so an upgrade would silently have forgiven everything the
// machine had spent. The old total becomes the unattributed agent's lifetime.
func TestAV1MeterFileMigratesWithoutLosingTheTotal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, meterFile), []byte(v1File), 0o640)
	at := time.Date(2026, 9, 5, 10, 0, 0, 0, ist)
	m := meterAt(t, dir, at)
	got := m.Report()
	if got.Turns != 5 || got.LastDurationMS != 1500 || len(got.ByModel) != 1 ||
		got.ByModel[0].InputTokens != 100 || got.ByModel[0].Turns != 6 {
		t.Fatalf("the machine total changed on upgrade: %+v", got)
	}
	if len(got.Agents) != 1 || got.Agents[0].Agent != agentapi.UnattributedAgent ||
		got.Agents[0].Lifetime.ByModel[0].InputTokens != 100 || len(got.Agents[0].Today.ByModel) != 0 {
		t.Fatalf("the old total is not the unattributed lifetime: %+v", got.Agents)
	}
	m.Record("boss", "claude-sonnet-5", sonnet(100, 20))
	again := meterAt(t, dir, at)
	if n := again.Report().ByModel[0].InputTokens; n != 200 {
		t.Errorf("after one turn and a reopen the total is %d, want 200", n)
	}
	buf, _ := os.ReadFile(filepath.Join(dir, meterFile))
	if !strings.Contains(string(buf), `"version":2`) || strings.Count(string(buf), `"legacy"`) != 1 {
		t.Errorf("the file was not rewritten as version 2 with one legacy block: %s", buf)
	}
}

// Spend is credited to the agent that made the call, on the day it was made
// where the person lives: at one in the morning in Kolkata it is still
// yesterday in UTC, and a day cut on the wrong clock is the wrong bill.
func TestSpendIsBucketedByAgentAndLocalDay(t *testing.T) {
	at := time.Date(2026, 9, 1, 1, 0, 0, 0, ist)
	m := meterAt(t, t.TempDir(), at)
	m.Record("boss", "claude-sonnet-5", sonnet(100, 10))
	m.Record("cody", "claude-sonnet-5", sonnet(50, 5))
	r := m.Report()
	if r.Today != "2026-09-01" || r.Zone != "IST" {
		t.Fatalf("the day was cut on the wrong clock: today=%s zone=%s", r.Today, r.Zone)
	}
	if len(r.Agents) != 2 || r.Agents[0].Agent != "boss" || r.Agents[1].Agent != "cody" ||
		r.Agents[0].Today.ByModel[0].InputTokens != 100 || r.Agents[1].Today.ByModel[0].InputTokens != 50 {
		t.Fatalf("per-agent windows: %+v", r.Agents)
	}
	if r.ByModel[0].InputTokens != 150 {
		t.Errorf("the machine total is %d, want both agents", r.ByModel[0].InputTokens)
	}
}

// "This week" is the calendar week from Monday, on the person's clock, and a
// window's membership is decided by date: a Sunday belongs to the week before.
func TestWeekStartsOnMondayInTheLocalZone(t *testing.T) {
	m := OpenMeter(t.TempDir())
	days := []time.Time{
		time.Date(2026, 8, 30, 12, 0, 0, 0, ist), // Sunday, the week before
		time.Date(2026, 8, 31, 12, 0, 0, 0, ist), // Monday
		time.Date(2026, 9, 1, 12, 0, 0, 0, ist),  // Tuesday, past the month boundary
	}
	for _, d := range days {
		m.now = func() time.Time { return d }
		m.Record("boss", "m", sonnet(1, 1))
		m.FinishTurn("boss", time.Second)
	}
	sunday := m.ByAgent(time.Date(2026, 9, 6, 23, 0, 0, 0, ist))
	boss := sunday.Agents[0]
	if sunday.WeekStart != "2026-08-31" || boss.Week.ByModel[0].InputTokens != 2 || boss.Week.Turns != 2 {
		t.Fatalf("week from Monday: start=%s week=%+v", sunday.WeekStart, boss.Week)
	}
	if len(boss.Today.ByModel) != 0 || boss.Lifetime.ByModel[0].InputTokens != 3 {
		t.Errorf("today=%+v lifetime=%+v", boss.Today, boss.Lifetime)
	}
	if tue := m.ByAgent(days[2]).Agents[0]; tue.Today.ByModel[0].InputTokens != 1 {
		t.Errorf("Tuesday's today window holds %+v", tue.Today)
	}
}

// A row per agent per model per day would grow without bound. Past the
// retention, days fold into the agent's lifetime carry: the lifetime and the
// machine total keep every token, the dated windows drop it, and the file no
// longer names the day.
func TestRowsOlderThanTheRetentionFoldIntoLifetime(t *testing.T) {
	dir := t.TempDir()
	old := meterAt(t, dir, time.Date(2025, 1, 1, 12, 0, 0, 0, ist))
	old.Record("boss", "m", sonnet(7, 1))
	old.FinishTurn("boss", time.Second)
	m := meterAt(t, dir, time.Date(2026, 9, 5, 12, 0, 0, 0, ist))
	for k := range m.rows {
		if k.day == "2025-01-01" {
			t.Fatal("a row past the retention kept its day")
		}
	}
	r := m.Report()
	boss := r.Agents[0]
	if r.ByModel[0].InputTokens != 7 || r.Turns != 1 || boss.Lifetime.ByModel[0].InputTokens != 7 || boss.Lifetime.Turns != 1 {
		t.Fatalf("folding lost tokens or turns: total=%+v lifetime=%+v", r.ByModel, boss.Lifetime)
	}
	if len(boss.Week.ByModel) != 0 || boss.Week.Turns != 0 {
		t.Errorf("a folded row leaked into this week: %+v", boss.Week)
	}
	m.Record("boss", "m", sonnet(1, 1))
	buf, _ := os.ReadFile(filepath.Join(dir, meterFile))
	if strings.Contains(string(buf), "2025-01-01") {
		t.Error("the fold was not persisted")
	}
}

// The machine-wide figure the host's dashboard reads is the same number it was
// before the split: the legacy total plus every agent's rows, per model, and
// every turn on the machine.
func TestReportIsUnchangedByTheSplit(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, meterFile), []byte(v1File), 0o640)
	m := meterAt(t, dir, time.Date(2026, 9, 5, 12, 0, 0, 0, ist))
	m.Record("boss", "claude-sonnet-5", sonnet(10, 1))
	m.Record("cody", "claude-sonnet-5", sonnet(20, 2))
	m.Record("cody", "claude-haiku-4-5", sonnet(5, 1))
	m.FinishTurn("boss", time.Second)
	r := m.Report()
	if len(r.ByModel) != 2 || r.ByModel[0].Model != "claude-haiku-4-5" || r.ByModel[0].InputTokens != 5 ||
		r.ByModel[1].InputTokens != 130 || r.ByModel[1].Turns != 8 {
		t.Fatalf("by model: %+v", r.ByModel)
	}
	if r.Turns != 6 {
		t.Errorf("turns = %d, want the 5 old ones and 1 new", r.Turns)
	}
}

// The app maps a null list to nothing at all, so an agent with no spend this
// week -- and a machine with no agents -- must still send lists.
func TestUsageWindowsNeverEncodeNull(t *testing.T) {
	m := meterAt(t, t.TempDir(), time.Date(2026, 9, 5, 12, 0, 0, 0, ist))
	buf, _ := json.Marshal(m.Report())
	if !strings.Contains(string(buf), `"agents":[]`) {
		t.Errorf("an empty meter encoded %s", buf)
	}
	m.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, ist) }
	m.Record("boss", "m", sonnet(1, 1))
	m.now = func() time.Time { return time.Date(2026, 9, 5, 12, 0, 0, 0, ist) }
	buf, _ = json.Marshal(m.Report())
	if !strings.Contains(string(buf), `"today":{"by_model":[],"turns":0}`) {
		t.Errorf("an empty window encoded as %s", buf)
	}
}
