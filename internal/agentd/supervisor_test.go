package agentd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// supervisorWith builds a supervisor with a given live-agent ceiling.
func supervisorWith(t *testing.T, maxLive int) *Supervisor {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-not-a-real-key-for-offline-tests")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	sup, err := NewSupervisor(ctx, t.TempDir(), t.TempDir(), testCatalog(t), "claude-haiku-4-5", maxLive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sup.Close)
	return sup
}

// A machine always has a boss: the person needs someone to talk to, and
// something has to be accountable for the whole result.
func TestBossExistsOnFirstBootAndCannotBeDeleted(t *testing.T) {
	sup := supervisorWith(t, 8)
	if _, ok := sup.Roster().Get(BossID); !ok {
		t.Fatal("no boss after first boot")
	}
	if err := sup.Delete(BossID, false); err == nil {
		t.Error("the boss was deleted")
	}
}

// Existing and running are separate. Creating an agent must not start it, or a
// roster of twenty would cost twenty goroutines the moment it was written.
func TestCreateDoesNotStartTheAgent(t *testing.T) {
	sup := supervisorWith(t, 8)
	if _, err := sup.Create("coder", "Ada"); err != nil {
		t.Fatal(err)
	}
	if got := sup.LiveCount(); got != 0 {
		t.Errorf("live agents after create = %d, want 0", got)
	}
	if len(sup.List()) != 2 {
		t.Errorf("roster = %d, want boss plus Ada", len(sup.List()))
	}
}

// Listing is what a client polls every few seconds. It must never start an
// agent, or a dashboard left open would pull the whole roster into memory.
func TestListDoesNotStartAnything(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Create("coder", "Ada")
	sup.Create("analyst", "Bo")
	for i := 0; i < 5; i++ {
		sup.List()
	}
	if got := sup.LiveCount(); got != 0 {
		t.Errorf("polling started %d agents", got)
	}
}

// Addressing an agent is what starts it, and asking twice must reuse the one
// already running rather than starting a second goroutine over the same files.
func TestGetStartsOnceAndReuses(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.Create("coder", "Ada")
	first, err := sup.Get("ada")
	if err != nil {
		t.Fatal(err)
	}
	second, err := sup.Get("ada")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Error("a second Get built a second agent over the same state")
	}
	if got := sup.LiveCount(); got != 1 {
		t.Errorf("live = %d, want 1", got)
	}
}

// An id is derived from the name so a person can address an agent by something
// they chose, and collisions get a suffix rather than overwriting.
func TestIDsComeFromNamesAndDoNotCollide(t *testing.T) {
	sup := supervisorWith(t, 8)
	first, _ := sup.Create("coder", "Ada Lovelace")
	second, _ := sup.Create("coder", "Ada Lovelace")
	if first.ID != "ada-lovelace" {
		t.Errorf("id = %q, want ada-lovelace", first.ID)
	}
	if second.ID != "ada-lovelace-2" {
		t.Errorf("second id = %q, want a suffixed id", second.ID)
	}
}

// The ceiling is a backstop against a guest with swap off, where an OOM reaps
// the whole VM rather than one process. Reaching it evicts an idle agent, which
// costs nothing: its conversation is on disk and it resumes from there.
func TestCeilingEvictsAnIdleAgent(t *testing.T) {
	sup := supervisorWith(t, 2)
	sup.Create("coder", "Ada")
	sup.Create("analyst", "Bo")

	if _, err := sup.Get(BossID); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Get("ada"); err != nil {
		t.Fatal(err)
	}
	if _, err := sup.Get("bo"); err != nil {
		t.Fatalf("third Get at a ceiling of 2 failed instead of evicting: %v", err)
	}
	if got := sup.LiveCount(); got != 2 {
		t.Errorf("live = %d, want the ceiling of 2", got)
	}
}

// An evicted agent must come back with its history, or eviction would be data
// loss rather than a memory saving.
func TestEvictedAgentResumesItsConversation(t *testing.T) {
	sup := supervisorWith(t, 1)
	sup.Create("coder", "Ada")

	boss, _ := sup.Get(BossID)
	boss.Log().Append(Event{Type: "text", Text: "before eviction"})
	before := boss.Log().LastID()

	if _, err := sup.Get("ada"); err != nil { // evicts the idle boss
		t.Fatal(err)
	}
	again, err := sup.Get(BossID)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Log().LastID(); got < before {
		t.Errorf("log id after revival = %d, want at least %d", got, before)
	}
}

// Deleting keeps the agent's state by default, so recreating the same id gets
// its history back -- the same bargain DELETE on a VM makes with its workspace.
// Purge is the explicit way to actually destroy it.
func TestDeleteKeepsStateUnlessPurged(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec, _ := sup.Create("coder", "Ada")
	agent, _ := sup.Get(rec.ID)
	agent.Log().Append(Event{Type: "text", Text: "some history"})
	dir := sup.dirFor(rec.ID)

	if err := sup.Delete(rec.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "events.jsonl")); err != nil {
		t.Errorf("a plain delete destroyed the agent's log: %v", err)
	}

	again, _ := sup.Create("coder", "Ada")
	if err := sup.Delete(again.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("purge left the agent's state behind")
	}
}

// Deleting must also stop the goroutine, or a removed agent would keep running
// and keep writing to a log nobody is listening to.
func TestDeleteStopsTheRunningAgent(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec, _ := sup.Create("coder", "Ada")
	sup.Get(rec.ID)
	if sup.LiveCount() != 1 {
		t.Fatal("agent did not start")
	}
	if err := sup.Delete(rec.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := sup.LiveCount(); got != 0 {
		t.Errorf("live after delete = %d, want 0", got)
	}
}

// The roster is the durable half, so it has to survive a restart intact.
func TestRosterSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadRoster(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.EnsureBoss("boss")
	first.Add("analyst", "Bo")

	again, err := LoadRoster(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.List()) != 2 {
		t.Fatalf("reloaded %d agents, want 2", len(again.List()))
	}
	if again.List()[0].ID != BossID {
		t.Errorf("first row is %q, want the boss to lead", again.List()[0].ID)
	}
}

// An unknown profile is a client mistake worth naming, not a broken agent
// discovered at its first turn.
func TestCreateRejectsAnUnknownProfile(t *testing.T) {
	sup := supervisorWith(t, 8)
	if _, err := sup.Create("astrologer", "Mystic"); err == nil {
		t.Error("an unknown profile was accepted")
	}
}

// A name that reduces to nothing usable must be refused rather than producing
// an agent with an id that cannot appear in a path or a URL.
func TestUnusableNamesAreRejected(t *testing.T) {
	sup := supervisorWith(t, 8)
	if _, err := sup.Create("coder", "!!! ???"); err == nil {
		t.Error("a name with no usable characters produced an agent")
	}
}

// Every live agent working means there is nothing safe to evict, and the
// caller is told to retry rather than being handed a ninth goroutine.
func TestNoCapacityWhenEverythingIsBusy(t *testing.T) {
	sup := supervisorWith(t, 1)
	sup.Create("coder", "Ada")
	boss, err := sup.Get(BossID)
	if err != nil {
		t.Fatal(err)
	}
	boss.setState("working")

	_, err = sup.Get("ada")
	if !errors.Is(err, ErrNoCapacity) {
		t.Errorf("Get with everything busy = %v, want ErrNoCapacity", err)
	}
	if !strings.Contains(ErrNoCapacity.Error(), "working") {
		t.Error("the capacity error does not say what is actually wrong")
	}
}

// A task folder is dated where the person is, not where the guest booted.
//
// AdoptZone sets time.Local at startup, so a machine onboarded since is still
// running UTC in-process until it restarts. Reading the stored zone instead is
// what keeps the folder name and the `date` an agent runs in its own turn from
// disagreeing -- they only ever differ near midnight, which is exactly when
// nobody is looking.
func TestTaskFoldersAreDatedInThePersonsZone(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Asia/Kolkata")

	// Pinned to UTC, which is what a machine onboarded since its last restart
	// still has: without it this would pass on a host that already ran Kolkata
	// and prove nothing about which of the two was read.
	local := time.Local
	t.Cleanup(func() { time.Local = local })
	time.Local = time.UTC

	// 20:00 UTC is already the next day in Kolkata, which is +05:30.
	evening := time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC)
	if got := sup.localDate(evening); got != "2026-09-01" {
		t.Errorf("task folder date = %q, want 2026-09-01: it must follow the stored zone", got)
	}
}

// With no zone stored the machine is on UTC, and must stay there rather than
// following whatever the host that built it happened to run.
func TestTaskFoldersFallBackToUTC(t *testing.T) {
	sup := newTestSupervisor(t)
	evening := time.Date(2026, 8, 31, 20, 0, 0, 0, time.UTC)
	if got := sup.localDate(evening); got != "2026-08-31" {
		t.Errorf("task folder date = %q, want 2026-08-31", got)
	}
}
