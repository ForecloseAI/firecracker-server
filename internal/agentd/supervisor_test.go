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
	first.Add(Record{Type: "analyst", Name: "Bo"})

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

// liveWithoutGoroutine registers an agent with the supervisor but does NOT run
// it, so a test can leave a message sitting in its inbox.
//
// A running agent drains its inbox in microseconds and would then take a real
// turn against the API, so the queued-message guard cannot be tested any other
// way. The returned flag reports whether the agent's context was cancelled.
func liveWithoutGoroutine(t *testing.T, sup *Supervisor, id string) (*Agent, *bool) {
	t.Helper()
	a, err := New(Record{ID: id, Name: id}, t.TempDir(), t.TempDir(), testProfile(), sup)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	sup.mu.Lock()
	sup.agents[id] = &live{agent: a, cancel: func() { stopped = true }, lastUsed: time.Now()}
	sup.mu.Unlock()
	return a, &stopped
}

// An idle agent with nothing queued is dropped, so the next message to it
// rebuilds the prompt and picks up whatever it just learned.
func TestRecycleDropsAnIdleAgent(t *testing.T) {
	sup := supervisorWith(t, 8)
	_, stopped := liveWithoutGoroutine(t, sup, "helper")
	if !sup.Recycle("helper", nil) {
		t.Fatal("an idle agent with an empty inbox was not recycled")
	}
	if !*stopped {
		t.Error("the agent's context was never cancelled, so its goroutine would live on")
	}
	if _, ok := sup.liveAgent("helper"); ok {
		t.Error("the agent is still in the live map, so the next message reuses the stale prompt")
	}
}

// THE guard that matters. Cancelling a goroutine abandons its inbox, and a
// message is logged when it ARRIVES -- so a message lost here sits in the
// person's transcript with no reply ever coming. Refusing costs nothing,
// because the next start picks the skill up anyway.
func TestRecycleRefusesWhenBusyOrQueued(t *testing.T) {
	sup := supervisorWith(t, 8)
	working, workingStopped := liveWithoutGoroutine(t, sup, "working")
	working.setState("working")
	if sup.Recycle("working", nil) || *workingStopped {
		t.Error("an agent mid-turn was recycled, losing the work the person is waiting on")
	}
	queued, queuedStopped := liveWithoutGoroutine(t, sup, "queued")
	queued.inbox <- inbound{text: "already logged as theirs"}
	if sup.Recycle("queued", nil) || *queuedStopped {
		t.Error("an agent with a queued message was recycled; that message would never be answered")
	}
	if sup.Recycle("never-existed", nil) {
		t.Error("recycled an agent that is not running")
	}
	// A refused agent must still take messages: quiesce only marks the ones it
	// actually stopped.
	if err := queued.Send("and another"); err != nil {
		t.Errorf("Send to an agent that was NOT recycled = %v, want it queued", err)
	}
}

// The window the inbox check alone cannot close: a caller looks an agent up,
// the agent is recycled, and only then does the caller enqueue. The message has
// to bounce rather than land in an inbox nothing will ever drain -- otherwise it
// sits in the person's transcript with no reply coming.
func TestSendToARecycledAgentIsRefusedRatherThanLost(t *testing.T) {
	sup := supervisorWith(t, 8)
	stale, _ := liveWithoutGoroutine(t, sup, "helper")
	if !sup.Recycle("helper", nil) {
		t.Fatal("an idle agent with an empty inbox was not recycled")
	}
	if err := stale.Send("did this reach anybody?"); !errors.Is(err, ErrStopped) {
		t.Fatalf("Send through a recycled agent = %v, want ErrStopped", err)
	}
	if len(stale.inbox) != 0 {
		t.Error("the message was queued onto an agent whose goroutine is gone")
	}
	events, err := stale.Log().ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Type == "user" {
			t.Error("a refused message was still logged as the person's, so they wait on a reply nobody will send")
		}
	}
}

// ...and the person never sees that bounce, because the supervisor puts the
// message on the instance that replaced the one it lost.
func TestSendToRetriesOntoTheReplacement(t *testing.T) {
	sup := supervisorWith(t, 8)
	stale, _ := liveWithoutGoroutine(t, sup, "helper")
	if !sup.Recycle("helper", nil) {
		t.Fatal("an idle agent with an empty inbox was not recycled")
	}
	// Registered again by hand rather than by letting Get start it: a real
	// goroutine would take the message straight to the API, which an offline
	// test has no key for. What is under test is that sendTo looks the agent up
	// AGAIN after a refusal, not what the replacement then does with it.
	var fresh *Agent
	sup.mu.Lock()
	sup.agents["helper"] = &live{agent: stale, cancel: func() {}, lastUsed: time.Now()}
	sup.mu.Unlock()
	took, err := sup.sendTo("helper", func(a *Agent) error {
		err := a.Send("did this reach anybody?")
		if errors.Is(err, ErrStopped) {
			fresh, _ = liveWithoutGoroutine(t, sup, "helper") // as a restart would
		}
		return err
	})
	if err != nil {
		t.Fatalf("sendTo after a recycle = %v, want the message taken by the replacement", err)
	}
	if took != fresh {
		t.Fatal("sendTo returned the recycled instance, so the reply would report a state nobody is in")
	}
	if len(fresh.inbox) != 1 {
		t.Error("the replacement never got the message")
	}
}

// The reload note is appended while the supervisor still owns the agent. After
// the entry goes, a message can start a second instance, and OpenLog takes its
// next event id from disk -- so an append made after that read hands out an id
// the new log has already given to something else, and SSE replay needs them
// unique.
func TestRecycleLogsBeforeTheSlotIsFree(t *testing.T) {
	sup := supervisorWith(t, 8)
	liveWithoutGoroutine(t, sup, "helper")
	noted := false
	sup.Recycle("helper", func() {
		noted = true
		if _, ok := sup.agents["helper"]; !ok { // safe: Recycle holds the lock
			t.Error("the agent was already out of the map, so a restart could be racing this append")
		}
	})
	if !noted {
		t.Fatal("the note was never appended")
	}
}

// Every cancel path has to check the inbox, not just the state. A message sits
// on the inbox from the moment enqueue returns until the goroutine picks it up,
// and the agent reads "idle" for that whole window -- so an eviction there
// drops a message already logged as the person's, with no reply ever coming.
func TestEvictionSparesAnAgentWithAQueuedMessage(t *testing.T) {
	sup := supervisorWith(t, 8)
	_, quietStopped := liveWithoutGoroutine(t, sup, "quiet")
	queued, queuedStopped := liveWithoutGoroutine(t, sup, "queued")
	queued.inbox <- inbound{text: "already logged as theirs"}

	sup.EvictIdle()
	if !*quietStopped {
		t.Error("an idle agent with an empty inbox survived eviction")
	}
	if *queuedStopped {
		t.Error("EvictIdle dropped an agent holding an unanswered message")
	}
}

// Same guard on the capacity path: making room must not free a slot by
// throwing away a message someone is waiting on.
func TestMakingRoomSparesAnAgentWithAQueuedMessage(t *testing.T) {
	sup := supervisorWith(t, 2)
	busy, busyStopped := liveWithoutGoroutine(t, sup, "busy")
	busy.inbox <- inbound{text: "already logged as theirs"}
	_, spareStopped := liveWithoutGoroutine(t, sup, "spare")

	sup.mu.Lock()
	err := sup.makeRoomLocked("newcomer")
	sup.mu.Unlock()
	if err != nil {
		t.Fatalf("makeRoomLocked: %v, want it to free the spare", err)
	}
	if *busyStopped {
		t.Error("made room by dropping an agent holding an unanswered message")
	}
	if !*spareStopped {
		t.Error("the genuinely idle agent was not the one evicted")
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

	// TestMain pins time.Local to an offset nothing here uses, so this proves the
	// stored zone is what was read rather than passing on a host that agreed.
	//
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
