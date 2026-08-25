package agentd

import (
	"os"
	"path/filepath"
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
	m.Record("claude-sonnet-5", sonnet(100, 20))
	m.FinishTurn(1500 * time.Millisecond)

	again := OpenMeter(dir)
	got := again.Report()
	if got.Turns != 1 || got.LastDurationMS != 1500 {
		t.Errorf("report = %+v, want 1 turn of 1500ms", got)
	}
	if len(got.ByModel) != 1 || got.ByModel[0].InputTokens != 100 {
		t.Fatalf("tokens did not survive the restart: %+v", got.ByModel)
	}
	// And it must keep counting from there, not start a second tally.
	again.Record("claude-sonnet-5", sonnet(100, 20))
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
		m.Record("claude-sonnet-5", sonnet(10, 2))
	}
	m.FinishTurn(time.Second)
	if got := m.Report().Turns; got != 1 {
		t.Errorf("turns = %d after six messages in one turn, want 1", got)
	}
	if got := m.Report().ByModel[0].InputTokens; got != 60 {
		t.Errorf("input tokens = %d, want all six messages counted", got)
	}
}

// A delegated worker on a different model is the reason the report is split by
// model at all: one token total cannot be priced when the rates differ tenfold.
func TestEveryModelIsReportedSeparately(t *testing.T) {
	m := OpenMeter(t.TempDir())
	m.Record("claude-sonnet-5", sonnet(100, 10))
	m.Record("claude-haiku-4-5", sonnet(300, 30))
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
	m.Record("claude-sonnet-5", sonnet(1, 1))
	if len(m.Report().ByModel) != 1 {
		t.Error("a meter that started from a corrupt file cannot record")
	}
}

// An agent built without a team has no meter. That is only ever a unit test,
// but a nil dereference there would fail every one of them.
func TestANilMeterIsSafeToUse(t *testing.T) {
	var m *Meter
	m.Record("claude-sonnet-5", sonnet(1, 1))
	m.FinishTurn(time.Second)
	if got := m.Report().Turns; got != 0 {
		t.Errorf("a nil meter reported %d turns", got)
	}
}
