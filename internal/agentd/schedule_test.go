package agentd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// mustZone resolves a location or fails the test.
func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// The grammar is the whole surface a model has to hit, so every accepted form
// has to parse and every rejected one has to say what would work instead.
func TestParseSchedule(t *testing.T) {
	for _, tc := range []struct {
		expr    string
		wantErr string
	}{
		{expr: "every 30m"},
		{expr: "every 6h"},
		{expr: "daily at 09:00"},
		{expr: "weekly on mon at 09:00"},
		{expr: "once on 2026-09-02 at 11:00"},
		{expr: "EVERY 30M"},                          // case is not the model's problem
		{expr: "  daily at 23:59   "},                // nor is stray spacing
		{expr: "  ONCE ON 2026-09-02 AT 11:00     "}, // and the one-off is no different
		{expr: "every 5m", wantErr: "tightest"},
		{expr: "every 14m59s", wantErr: "tightest"},
		{expr: "every banana", wantErr: "duration"},
		{expr: "daily at 25:00", wantErr: "time of day"},
		{expr: "weekly on funday at 09:00", wantErr: "day such as"},
		{expr: "once on 02-09-2026 at 11:00", wantErr: "date such as"}, // not ISO
		{expr: "once on 2026-13-02 at 11:00", wantErr: "date such as"}, // no such month
		{expr: "once on 2026-09-02 at 25:00", wantErr: "time of day"},
		{expr: "once at 2026-09-02 11:00", wantErr: "once on"}, // near miss, told the form
		{expr: "0 9 * * *", wantErr: "every 30m"},              // cron gets told the real grammar
		{expr: "", wantErr: "every 30m"},
	} {
		_, err := parseSchedule(tc.expr, time.UTC)
		switch {
		case tc.wantErr == "" && err != nil:
			t.Errorf("%q: %v", tc.expr, err)
		case tc.wantErr != "" && err == nil:
			t.Errorf("%q was accepted; want an error mentioning %q", tc.expr, tc.wantErr)
		case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
			t.Errorf("%q: error %q does not mention %q", tc.expr, err, tc.wantErr)
		}
	}
}

// An expression the grammar does not know is the moment a model finds out what
// the grammar does know, and the one-off form has to be in that list. It is the
// form nothing else substitutes for: told only about the repeating three, a
// model asked for a single dated event books a daily job with a date check in
// its task text, which is the bug this form exists to remove.
func TestTheGrammarErrorNamesEveryForm(t *testing.T) {
	_, err := parseSchedule("0 9 * * *", time.UTC)
	if err == nil {
		t.Fatal("cron was accepted")
	}
	for _, want := range []string{"every 30m", "daily at", "weekly on", "once on"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the grammar error does not offer %q: %s", want, err)
		}
	}
}

// A firing that is not strictly in the future fires again immediately, and the
// sweep would then loop on it every tick.
func TestNextIsAlwaysInTheFuture(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) // exactly on a daily boundary
	for _, expr := range []string{"every 30m", "daily at 09:00", "weekly on sun at 09:00",
		"once on 2026-09-02 at 11:00"} {
		sp, err := parseSchedule(expr, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		if got := sp.next(now); !got.After(now) {
			t.Errorf("%q: next(%v) = %v, which is not in the future", expr, now, got)
		}
	}
}

// "daily at 09:00" means 9am where the person is. Built in their location rather
// than by adding 24h, so the 23-hour day over a spring-forward still lands on 9.
func TestDailyHoldsItsWallClockAcrossDST(t *testing.T) {
	london := mustZone(t, "Europe/London")
	sp, err := parseSchedule("daily at 09:00", london)
	if err != nil {
		t.Fatal(err)
	}
	// 2026-03-29 is the UK spring-forward. Start the evening before.
	before := time.Date(2026, 3, 28, 20, 0, 0, 0, london)
	got := sp.next(before).In(london)
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Errorf("next = %v, want 09:00 local", got)
	}
	if got.Day() != 29 {
		t.Errorf("next = %v, want the 29th", got)
	}
}

// Weekly has to reach the right day, not just the right hour.
func TestWeeklyLandsOnItsDay(t *testing.T) {
	sp, err := parseSchedule("weekly on fri at 17:30", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	got := sp.next(time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)) // a Monday
	if got.Weekday() != time.Friday || got.Hour() != 17 || got.Minute() != 30 {
		t.Errorf("next = %v, want the coming Friday at 17:30", got)
	}
}

// The one-off form names an instant rather than a rhythm, so it has to land on
// exactly that instant in the person's own zone. This is the case that was
// asked for and could not be said: a sale opening at 11:00 IST on 2 September.
func TestOnceLandsOnItsNamedInstant(t *testing.T) {
	kolkata := mustZone(t, "Asia/Kolkata")
	sp, err := parseSchedule("once on 2026-09-02 at 11:00", kolkata)
	if err != nil {
		t.Fatal(err)
	}

	got := sp.next(time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)).UTC()

	want := time.Date(2026, 9, 2, 5, 30, 0, 0, time.UTC) // 11:00 +05:30
	if !got.Equal(want) {
		t.Errorf("next = %v, want %v", got, want)
	}
}

// A one-off has exactly one occurrence. next has to say so rather than invent a
// second one, because everything downstream reads a stored NextRunAt as a
// promise that there is still a run to come.
func TestOnceHasNoRunAfterItsOwn(t *testing.T) {
	sp, err := parseSchedule("once on 2026-09-02 at 11:00", time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	at := sp.next(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC))

	if got := sp.next(at); !got.IsZero() {
		t.Errorf("next after the single occurrence = %v, want the zero time", got)
	}
}

// A date that has gone by parses perfectly well and still has nothing to book.
// Caught at creation, because a schedule stored with a zero NextRunAt reads as
// due on every sweep for the rest of the machine's life.
func TestAPastOneOffIsRefusedWhenItIsCreated(t *testing.T) {
	now := time.Date(2026, 9, 3, 9, 0, 0, 0, time.UTC)

	if _, err := parseSchedule("once on 2026-09-02 at 11:00", time.UTC); err != nil {
		t.Fatalf("the expression itself is well-formed: %v", err)
	}
	_, at, err := planSchedule("once on 2026-09-02 at 11:00", time.UTC, now)
	if err == nil {
		t.Fatalf("a one-off in the past was booked for %v", at)
	}
	if !strings.Contains(err.Error(), "already passed") {
		t.Errorf("error = %q, want it to say the time has passed", err)
	}
	if _, at, err := planSchedule("daily at 09:00", time.UTC, now); err != nil || !at.After(now) {
		t.Errorf("a repeating form was caught by the past-date check: %v %v", at, err)
	}
}

// The zone cannot be guessed in the guest, which runs UTC. A named zone beats a
// stamp because an offset is only true for the instant it was sampled.
func TestRememberZonePrefersTheNameOverTheOffset(t *testing.T) {
	dir := t.TempDir()
	RememberZone(dir, "Asia/Kolkata", "2026-08-29T14:03:11+09:00")
	if got := loadZone(dir).String(); got != "Asia/Kolkata" {
		t.Errorf("zone = %q, want Asia/Kolkata: the name must win over the stamp", got)
	}
}

// A client that can only send a stamp still gets its own hours.
func TestRememberZoneFallsBackToTheOffset(t *testing.T) {
	dir := t.TempDir()
	RememberZone(dir, "", "2026-08-29T14:03:11+05:30")
	sp, err := parseSchedule("daily at 09:00", loadZone(dir))
	if err != nil {
		t.Fatal(err)
	}
	got := sp.next(time.Date(2026, 8, 29, 0, 0, 0, 0, time.UTC)).UTC()
	if got.Hour() != 3 || got.Minute() != 30 {
		t.Errorf("09:00 at +05:30 = %v UTC, want 03:30", got)
	}
}

// Nothing sent, or nonsense sent, must not leave the zone in a broken state.
func TestZoneDefaultsToUTC(t *testing.T) {
	dir := t.TempDir()
	if got := loadZone(dir); got != time.UTC {
		t.Errorf("zone with no file = %v, want UTC", got)
	}
	RememberZone(dir, "Mars/Olympus", "not a time")
	if got := loadZone(dir); got != time.UTC {
		t.Errorf("zone after nonsense = %v, want UTC", got)
	}
}

// Ids must not be reused after a deletion: a client holding the old one would
// otherwise cancel a schedule it has never seen.
func TestScheduleIDsAreNotReused(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadSchedules(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := store.Add(Schedule{Name: "one"})
	second, _ := store.Add(Schedule{Name: "two"})
	store.Delete(second.ID)
	third, _ := store.Add(Schedule{Name: "three"})
	if third.ID == second.ID || third.ID == first.ID {
		t.Errorf("id %s was reused after a deletion", third.ID)
	}
}

// A cancellation that only happened in memory is worse than a refused one: the
// job comes back at the next restart while the person has been told it is gone.
func TestDeleteReportsAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadSchedules(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.Add(Schedule{Name: "sweep", Agent: BossID, Expr: "every 30m"})
	if err != nil {
		t.Fatal(err)
	}
	// A directory where the temporary file goes makes the write fail the way a
	// full or read-only disk would.
	if err := os.Mkdir(store.path+".tmp", 0o750); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.Delete(added.ID)

	if err == nil {
		t.Fatal("a delete that could not be written reported success")
	}
	if deleted {
		t.Error("delete reported the schedule gone despite the failure")
	}
	if got := store.List(); len(got) != 1 {
		t.Errorf("the store holds %d schedules, want the one that could not be deleted", len(got))
	}
}

// The store is the durable half of the feature; a schedule that does not survive
// a restart is one the person agreed to and never gets.
func TestSchedulesSurviveAReload(t *testing.T) {
	dir := t.TempDir()
	store, err := LoadSchedules(dir)
	if err != nil {
		t.Fatal(err)
	}
	added, err := store.Add(Schedule{Name: "sweep", Agent: BossID, Expr: "every 30m"})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := LoadSchedules(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.List()
	if len(got) != 1 || got[0].ID != added.ID || got[0].Name != "sweep" {
		t.Fatalf("reload returned %+v, want the schedule that was added", got)
	}
	if !got[0].Enabled {
		t.Error("a reloaded schedule came back disabled")
	}
}

// A damaged file must stop the daemon rather than start empty, or the next write
// silently deletes everything the person agreed to.
func TestCorruptScheduleFileIsFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "schedules.json"), []byte("{not json"), 0o640)
	if _, err := LoadSchedules(dir); err == nil {
		t.Error("a corrupt schedules.json loaded cleanly; it must refuse")
	}
}

// The framing is what stops the model reading a timer as the person speaking,
// and what tells it that asking a question costs a night rather than a moment.
func TestScheduledFramingSaysNobodyMayBeWatching(t *testing.T) {
	got := frame(inbound{text: "check the deploy", schedule: "morning sweep"})
	for _, want := range []string{"morning sweep", "not the person", "at once"} {
		if !strings.Contains(got, want) {
			t.Errorf("scheduled framing does not mention %q: %s", want, got)
		}
	}
	if plain := frame(inbound{text: "check the deploy"}); plain != "check the deploy" {
		t.Errorf("an ordinary message was reframed: %q", plain)
	}
}

// The transcript must never show the person saying words they did not type.
func TestAScheduledFireIsNotLoggedAsTheUser(t *testing.T) {
	a := newTestAgent(t)
	if err := a.SendScheduled("morning sweep", "check the deploy"); err != nil {
		t.Fatal(err)
	}
	events, err := a.log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range events {
		if e.Type == "user" {
			t.Error("a scheduled fire was logged as a message from the person")
		}
		if e.Type == "scheduled" {
			found = true
			if e.TaskTitle != "morning sweep" {
				t.Errorf("the scheduled event does not name the job: %+v", e)
			}
		}
	}
	if !found {
		t.Error("a scheduled fire left no scheduled event")
	}
}

// due books a schedule that is already overdue.
func due(t *testing.T, sup *Supervisor, expr string) Schedule {
	t.Helper()
	sc, err := sup.Schedules().Add(Schedule{Name: "sweep", Agent: BossID,
		Task: "check the deploy", Expr: expr, NextRunAt: time.Now().Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// The basic contract: a due schedule reaches the agent, and moves on.
func TestSweepFiresADueScheduleAndAdvancesIt(t *testing.T) {
	sup := newTestSupervisor(t)
	sc := due(t, sup, "every 30m")
	now := time.Now()

	sup.sweep(now)

	after := sup.Schedules().List()[0]
	if !after.NextRunAt.After(now) {
		t.Errorf("next run %v is not in the future", after.NextRunAt)
	}
	if after.Fires != 1 {
		t.Errorf("fires = %d, want 1", after.Fires)
	}
	if !loggedType(agentOf(t, sup, sc.Agent), "scheduled") {
		t.Error("the fire left no scheduled event in the agent's transcript")
	}
}

// Advancing at fire time is what stops a second tick re-firing the same
// occurrence.
func TestASecondSweepDoesNotRefire(t *testing.T) {
	sup := newTestSupervisor(t)
	due(t, sup, "every 30m")

	sup.sweep(time.Now())
	sup.sweep(time.Now())

	if got := sup.Schedules().List()[0].Fires; got != 1 {
		t.Errorf("fires = %d after two sweeps, want 1", got)
	}
}

// A busy agent is skipped -- but the occurrence is still spent. Leaving it due
// would bring it back every tick and deliver a burst the moment the agent frees
// up, which is the backlog the skip exists to avoid.
func TestABusyAgentIsSkippedButTheOccurrenceIsStillSpent(t *testing.T) {
	sup := newTestSupervisor(t)
	due(t, sup, "every 30m")
	a, err := sup.Get(BossID)
	if err != nil {
		t.Fatal(err)
	}
	a.setState("working")
	now := time.Now()

	sup.sweep(now)

	after := sup.Schedules().List()[0]
	if after.Fires != 0 {
		t.Errorf("fires = %d, want 0: a busy agent must not be sent work", after.Fires)
	}
	if !after.NextRunAt.After(now) {
		t.Errorf("a skipped occurrence stayed due (%v); it would repeat every tick", after.NextRunAt)
	}
	if loggedType(a, "scheduled") {
		t.Error("a busy agent was sent scheduled work anyway")
	}
}

// The whole point of the form: it runs, and then it is over. A repeating job
// left standing after its one useful morning spends a turn a day forever, and
// the person has to notice and cancel it.
func TestAOneOffRunsOnceAndThenStops(t *testing.T) {
	sup := newTestSupervisor(t)
	sc := due(t, sup, oneOffAt(time.Now().Add(-time.Minute)))
	now := time.Now()

	sup.sweep(now)
	sup.sweep(now.Add(time.Minute)) // and again, the way the ticker would

	after := byID(t, sup, sc.ID)
	if after.Fires != 1 {
		t.Errorf("fires = %d after two sweeps, want exactly 1", after.Fires)
	}
	if after.Enabled {
		t.Error("a one-off is still enabled after its run; it would be swept forever")
	}
	if !loggedType(agentOf(t, sup, sc.Agent), "scheduled") {
		t.Error("the fire left no scheduled event in the agent's transcript")
	}
}

// The opposite of the repeating case, and deliberately so. A repeating job that
// finds the agent busy spends the occurrence and waits for the next one; a
// one-off that did the same would simply never happen.
func TestABusyAgentDoesNotSpendAOneOff(t *testing.T) {
	sup := newTestSupervisor(t)
	dueAt := time.Now().Add(-time.Minute)
	sc := due(t, sup, oneOffAt(dueAt))
	a := agentOf(t, sup, BossID)
	a.setState("working")

	sup.sweep(time.Now())

	held := byID(t, sup, sc.ID)
	if held.Fires != 0 || loggedType(a, "scheduled") {
		t.Fatal("a busy agent was sent scheduled work anyway")
	}
	if !held.Enabled || !held.NextRunAt.Equal(sc.NextRunAt) {
		t.Fatalf("the one-off was spent on a busy agent: %+v", held)
	}

	a.setState("idle")
	sup.sweep(time.Now())

	if got := byID(t, sup, sc.ID); got.Fires != 1 {
		t.Errorf("fires = %d once the agent freed up, want 1: the run was dropped", got.Fires)
	}
}

// Retrying cannot go on forever. A reminder for an 11:00 sale is worth having at
// 11:20 and is noise by the evening, and a one-off left permanently due would be
// swept every 30s until the machine is rebuilt.
func TestAStaleOneOffIsTurnedOffRatherThanRunLate(t *testing.T) {
	sup := newTestSupervisor(t)
	missed := time.Now().Add(-oneShotGrace - time.Minute)
	sc := due(t, sup, oneOffAt(missed))
	sup.Schedules().Update(sc.ID, func(x *Schedule) { x.NextRunAt = missed })

	sup.sweep(time.Now())

	after := byID(t, sup, sc.ID)
	if after.Fires != 0 {
		t.Errorf("fires = %d, want 0: hours-late is not worth delivering", after.Fires)
	}
	if after.Enabled {
		t.Error("a one-off past its grace is still enabled; it would be swept forever")
	}
}

// A machine that was off over the instant and came back within the grace window
// should still run it -- that is the difference between "missed by four minutes"
// and a week of backlog, and it is the sweep's call, not rollForward's.
func TestRollForwardLeavesAOneOffForTheSweep(t *testing.T) {
	sup := newTestSupervisor(t)
	missed := time.Now().Add(-4 * time.Minute)
	sc := due(t, sup, oneOffAt(missed))
	sup.Schedules().Update(sc.ID, func(x *Schedule) { x.NextRunAt = missed })

	sup.rollForward(time.Now())

	held := byID(t, sup, sc.ID)
	if !held.NextRunAt.Equal(missed) {
		t.Fatalf("rollForward moved a one-off from %v to %v", missed, held.NextRunAt)
	}
	sup.sweep(time.Now())
	if got := byID(t, sup, sc.ID); got.Fires != 1 {
		t.Errorf("fires = %d, want 1: a run missed by minutes was thrown away", got.Fires)
	}
}

// oneOffAt is the expression that names a given instant, in UTC.
func oneOffAt(at time.Time) string {
	return "once on " + at.UTC().Format("2006-01-02") + " at " + at.UTC().Format("15:04")
}

// A week asleep must not wake into a week of backlog.
func TestRollForwardSkipsWhatWasMissedWhileOff(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.Schedules().Add(Schedule{Name: "sweep", Agent: BossID, Task: "check",
		Expr: "every 30m", NextRunAt: time.Now().Add(-7 * 24 * time.Hour)})
	now := time.Now()

	sup.rollForward(now)

	after := sup.Schedules().List()[0]
	if !after.NextRunAt.After(now) {
		t.Errorf("next run %v is still in the past", after.NextRunAt)
	}
	if after.Fires != 0 {
		t.Errorf("fires = %d, want 0: missed runs are skipped, not replayed", after.Fires)
	}
}

// A schedule whose agent has been deleted would otherwise fail on every tick
// forever.
func TestAScheduleForAMissingAgentTurnsItselfOff(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.Schedules().Add(Schedule{Name: "sweep", Agent: "ghost", Task: "check",
		Expr: "every 30m", NextRunAt: time.Now().Add(-time.Minute)})

	sup.sweep(time.Now())

	if sup.Schedules().List()[0].Enabled {
		t.Error("a schedule for a missing agent is still enabled")
	}
}

// agentOf starts an agent and hands it back, failing the test if it cannot.
func agentOf(t *testing.T, sup *Supervisor, id string) *Agent {
	t.Helper()
	a, err := sup.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Nothing is written until the person says yes. This is the one tool that
// commits the machine to spending money later with nobody watching, so a denial
// has to leave the store exactly as it was.
func TestSchedulingIsRefusedUntilThePersonAgrees(t *testing.T) {
	sup := newTestSupervisor(t)
	gate := NewGate(mustLog(t), sup.Interactions(), t.TempDir())
	in := scheduleInput{Name: "sweep", When: "every 30m", Task: "check the deploy"}

	for _, tc := range []struct {
		decision  string
		wantStore int
	}{
		{decision: "deny", wantStore: 0},
		{decision: "allow", wantStore: 1},
	} {
		done := make(chan string, 1)
		go func() { done <- sup.CreateSchedule(context.Background(), gate, BossID, in) }()
		gate.Resolve(pendingID(t, gate, gate.log), Decision{Decision: tc.decision})
		answer := <-done

		if got := len(sup.Schedules().List()); got != tc.wantStore {
			t.Errorf("%s: store holds %d schedules, want %d -- answer was %q",
				tc.decision, got, tc.wantStore, answer)
		}
	}
}

// An invalid expression must be caught before the person is asked: interrupting
// someone to approve a schedule that cannot be stored wastes the one thing this
// design spends carefully.
func TestAnInvalidScheduleNeverReachesThePerson(t *testing.T) {
	sup := newTestSupervisor(t)
	gate := NewGate(mustLog(t), sup.Interactions(), t.TempDir())

	got := sup.CreateSchedule(context.Background(), gate,
		BossID, scheduleInput{Name: "hammer", When: "every 1m", Task: "check"})

	if !strings.Contains(got, "tightest") {
		t.Errorf("answer = %q, want it to explain the limit", got)
	}
	if events, _ := gate.log.ReadAll(); len(events) != 0 {
		t.Errorf("the person was asked about an invalid schedule: %+v", events)
	}
}

// The description is the only place a model learns what it may ask for, and it
// is where this went wrong: offered three repeating forms and nothing else, an
// agent told to act on one named day booked "daily at 05:30" and put the date
// check in its own task text. The one-off form has to be on the menu, and the
// menu has to survive tag parsing -- a comma in a jsonschema tag truncates the
// description at the comma with no error, which would silently take it back off.
func TestScheduleToolOffersTheOneOffForm(t *testing.T) {
	tools, err := scheduleTools(toolDeps{team: newTestSupervisor(t), self: BossID})
	if err != nil {
		t.Fatal(err)
	}
	buf, err := json.Marshal(tools[0].InputSchema())
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Properties map[string]struct{ Description string } `json:"properties"`
	}
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatal(err)
	}
	when := got.Properties["when"].Description
	for _, want := range []string{"every 30m", "daily at", "weekly on", "once on", "15m"} {
		if !strings.Contains(when, want) {
			t.Errorf("the \"when\" description does not offer %q: %q", want, when)
		}
	}
	if strings.Contains(when, ",") {
		t.Error("a comma in the jsonschema tag truncates the description the model sees")
	}
}

// A one-off is confirmed as a single run, so the model can see it does not have
// to arrange a cancellation of its own afterwards.
func TestCreateSaysWhenAOneOffRuns(t *testing.T) {
	sup := newTestSupervisor(t)
	gate := NewGate(mustLog(t), sup.Interactions(), t.TempDir())
	day := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
	in := scheduleInput{Name: "sale", When: "once on " + day + " at 11:00", Task: "queue up"}

	done := make(chan string, 1)
	go func() { done <- sup.CreateSchedule(context.Background(), gate, BossID, in) }()
	gate.Resolve(pendingID(t, gate, gate.log), Decision{Decision: "allow"})
	answer := <-done

	if !strings.Contains(answer, "Runs once at") {
		t.Errorf("answer = %q, want it to say the schedule runs once", answer)
	}
	if got := len(sup.Schedules().List()); got != 1 {
		t.Errorf("store holds %d schedules, want 1", got)
	}
}

// cancelAs runs cancel_schedule as one agent.
func cancelAs(t *testing.T, sup *Supervisor, self, id string) string {
	t.Helper()
	tools, err := scheduleTools(toolDeps{team: sup, self: self})
	if err != nil {
		t.Fatal(err)
	}
	return runTool(t, tools, "cancel_schedule", `{"id":"`+id+`"}`)[0].OfText.Text
}

// list_schedules filters by owner, so an id belonging to a colleague can only
// have arrived out of band. Cancelling on it would undo work its owner is
// relying on and never told anyone about.
func TestCancelOnlyReachesTheCallersOwnSchedules(t *testing.T) {
	sup := newTestSupervisor(t)
	sc, err := sup.Schedules().Add(Schedule{Name: "sweep", Agent: "colleague",
		Task: "check the deploy", Expr: "every 30m", NextRunAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	got := cancelAs(t, sup, BossID, sc.ID)

	if !strings.Contains(got, "no schedule") {
		t.Errorf("answer = %q, want a colleague's id to read like an unknown one", got)
	}
	if len(sup.Schedules().List()) != 1 {
		t.Error("an agent cancelled a schedule belonging to someone else")
	}
	if got := cancelAs(t, sup, "colleague", sc.ID); !strings.Contains(got, "Cancelled") {
		t.Errorf("the owner could not cancel its own schedule: %q", got)
	}
}

// A person who travels must not get one firing on the zone they left. The
// stored instant is absolute, so a London 09:00 read in Los Angeles is 01:00
// local unless it is recomputed when the zone lands.
func TestAChangedZoneMovesTheClockSchedules(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London", "")
	daily, err := sup.Schedules().Add(Schedule{Name: "brief", Agent: BossID, Task: "brief me",
		Expr: "daily at 09:00", NextRunAt: nextOf(t, "daily at 09:00", mustZone(t, "Europe/London"))})
	if err != nil {
		t.Fatal(err)
	}
	every, err := sup.Schedules().Add(Schedule{Name: "sweep", Agent: BossID, Task: "check",
		Expr: "every 30m", NextRunAt: time.Now().Add(30 * time.Minute)})
	if err != nil {
		t.Fatal(err)
	}

	sup.RememberZone("America/Los_Angeles", "")

	la := mustZone(t, "America/Los_Angeles")
	got := byID(t, sup, daily.ID).NextRunAt.In(la)
	if got.Hour() != 9 || got.Minute() != 0 {
		t.Errorf("next run = %v, want 09:00 in the zone the person moved to", got)
	}
	// An interval is not anchored to a wall clock; moving it would only push the
	// next run further out.
	if moved := byID(t, sup, every.ID).NextRunAt; !moved.Equal(every.NextRunAt) {
		t.Errorf("an \"every 30m\" schedule moved from %v to %v on a zone change",
			every.NextRunAt, moved)
	}
}

// A one-off names a wall-clock instant, so it moves with the person exactly as a
// daily does: someone who books "once on 2026-09-02 at 11:00" in London and then
// lands in Kolkata means 11:00 where they now are.
func TestAChangedZoneMovesAOneOff(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London", "")
	london := mustZone(t, "Europe/London")
	expr := "once on " + time.Now().In(london).AddDate(0, 0, 30).Format("2006-01-02") + " at 11:00"
	sc, err := sup.Schedules().Add(Schedule{Name: "sale", Agent: BossID, Task: "queue up",
		Expr: expr, NextRunAt: nextOf(t, expr, london)})
	if err != nil {
		t.Fatal(err)
	}

	sup.RememberZone("Asia/Kolkata", "")

	got := byID(t, sup, sc.ID).NextRunAt.In(mustZone(t, "Asia/Kolkata"))
	if got.Hour() != 11 || got.Minute() != 0 {
		t.Errorf("next run = %v, want 11:00 in the zone the person moved to", got)
	}
}

// A one-off whose instant has already gone by has no next run to move it to.
// Rezoning must leave the stored instant alone rather than write a zero, which
// the sweep would read as due and hold forever.
func TestAChangedZoneLeavesAPastOneOffAlone(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London", "")
	missed := time.Now().Add(-time.Minute)
	sc, err := sup.Schedules().Add(Schedule{Name: "sale", Agent: BossID, Task: "queue up",
		Expr: oneOffAt(missed), NextRunAt: missed})
	if err != nil {
		t.Fatal(err)
	}

	sup.RememberZone("Asia/Kolkata", "")

	if got := byID(t, sup, sc.ID).NextRunAt; !got.Equal(missed) {
		t.Errorf("next run moved from %v to %v; a past one-off has no next", missed, got)
	}
}

// The same zone arrives on every message, so the common case must not rewrite
// the store -- a recompute on each message would walk a daily job forward a day
// at a time and it would never fire.
func TestAnUnchangedZoneLeavesSchedulesAlone(t *testing.T) {
	sup := newTestSupervisor(t)
	sup.RememberZone("Europe/London", "")
	sc, err := sup.Schedules().Add(Schedule{Name: "brief", Agent: BossID, Task: "brief me",
		Expr: "daily at 09:00", NextRunAt: nextOf(t, "daily at 09:00", mustZone(t, "Europe/London"))})
	if err != nil {
		t.Fatal(err)
	}

	sup.RememberZone("Europe/London", "2026-08-29T14:03:11+01:00")

	if got := byID(t, sup, sc.ID).NextRunAt; !got.Equal(sc.NextRunAt) {
		t.Errorf("next run moved from %v to %v with no change of zone", sc.NextRunAt, got)
	}
}

// nextOf is the first firing of an expression, for a test that needs a schedule
// already pointing at the right instant.
func nextOf(t *testing.T, expr string, loc *time.Location) time.Time {
	t.Helper()
	sp, err := parseSchedule(expr, loc)
	if err != nil {
		t.Fatal(err)
	}
	return sp.next(time.Now())
}

// byID reads back one schedule, failing the test if it has gone.
func byID(t *testing.T, sup *Supervisor, id string) Schedule {
	t.Helper()
	for _, sc := range sup.Schedules().List() {
		if sc.ID == id {
			return sc
		}
	}
	t.Fatalf("schedule %s is gone", id)
	return Schedule{}
}

// The app creates schedules directly rather than through a conversation, so the
// routes have to round-trip and to reject what the tools reject.
func TestScheduleRoutesRoundTrip(t *testing.T) {
	sup := newTestSupervisor(t)
	srv := NewServer(sup)

	w := do(t, srv, "POST", "/schedules",
		`{"name":"sweep","agent":"boss","task":"check the deploy","expr":"every 30m","tz":"Asia/Kolkata"}`)
	if w.Code != 201 {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
	if got := do(t, srv, "GET", "/schedules", ""); !strings.Contains(got.Body.String(), "sweep") {
		t.Errorf("the created schedule is not listed: %s", got.Body)
	}
	// The zone came in on the request, so it must now be what schedules use.
	if got := loadZone(sup.stateDir).String(); got != "Asia/Kolkata" {
		t.Errorf("zone = %q, want the one the client sent", got)
	}

	id := sup.Schedules().List()[0].ID
	if got := do(t, srv, "DELETE", "/schedules/"+id, ""); got.Code != 200 {
		t.Fatalf("delete = %d, want 200: %s", got.Code, got.Body)
	}
	if got := len(sup.Schedules().List()); got != 0 {
		t.Errorf("%d schedules survived the delete", got)
	}
	if got := do(t, srv, "DELETE", "/schedules/"+id, ""); got.Code != 404 {
		t.Errorf("deleting twice = %d, want 404", got.Code)
	}
}

// The app can book a one-off too, and the route has to store the instant the
// person's own zone makes of it rather than the guest's UTC.
func TestScheduleRoutesBookAOneOff(t *testing.T) {
	sup := newTestSupervisor(t)
	srv := NewServer(sup)

	kolkata := mustZone(t, "Asia/Kolkata")
	day := time.Now().In(kolkata).AddDate(0, 0, 30).Format("2006-01-02")

	w := do(t, srv, "POST", "/schedules", `{"name":"sale","agent":"boss","task":"queue up",`+
		`"expr":"once on `+day+` at 11:00","tz":"Asia/Kolkata"}`)

	if w.Code != 201 {
		t.Fatalf("create = %d, want 201: %s", w.Code, w.Body)
	}
	got := sup.Schedules().List()[0].NextRunAt
	if in := got.In(kolkata); in.Hour() != 11 || in.Minute() != 0 || in.Format("2006-01-02") != day {
		t.Errorf("next run = %v, want %s 11:00 in the zone the client sent", in, day)
	}
	if in := got.UTC(); in.Hour() != 5 || in.Minute() != 30 {
		t.Errorf("next run = %v UTC, want 05:30: the guest's own UTC was used", in)
	}
}

// The routes must not be a way around the limits the tool enforces.
func TestScheduleRoutesRejectBadInput(t *testing.T) {
	srv := NewServer(newTestSupervisor(t))
	for _, tc := range []struct{ name, body string }{
		{"too tight", `{"name":"x","agent":"boss","task":"t","expr":"every 1m"}`},
		{"not a schedule", `{"name":"x","agent":"boss","task":"t","expr":"0 9 * * *"}`},
		{"no task", `{"name":"x","agent":"boss","expr":"every 30m"}`},
		{"a date gone by", `{"name":"x","agent":"boss","task":"t","expr":"once on 2020-01-01 at 09:00"}`},
	} {
		if got := do(t, srv, "POST", "/schedules", tc.body); got.Code != 400 {
			t.Errorf("%s = %d, want 400: %s", tc.name, got.Code, got.Body)
		}
	}
	if got := do(t, srv, "POST", "/schedules",
		`{"name":"x","agent":"ghost","task":"t","expr":"every 30m"}`); got.Code != 404 {
		t.Errorf("unknown agent = %d, want 404", got.Code)
	}
}
