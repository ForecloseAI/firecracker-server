package agentd

import (
	"context"
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
		{expr: "EVERY 30M"},           // case is not the model's problem
		{expr: "  daily at 23:59   "}, // nor is stray spacing
		{expr: "every 5m", wantErr: "tightest"},
		{expr: "every 14m59s", wantErr: "tightest"},
		{expr: "every banana", wantErr: "duration"},
		{expr: "daily at 25:00", wantErr: "time of day"},
		{expr: "weekly on funday at 09:00", wantErr: "day such as"},
		{expr: "0 9 * * *", wantErr: "every 30m"}, // cron gets told the real grammar
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

// A firing that is not strictly in the future fires again immediately, and the
// sweep would then loop on it every tick.
func TestNextIsAlwaysInTheFuture(t *testing.T) {
	now := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC) // exactly on a daily boundary
	for _, expr := range []string{"every 30m", "daily at 09:00", "weekly on sun at 09:00"} {
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

// The routes must not be a way around the limits the tool enforces.
func TestScheduleRoutesRejectBadInput(t *testing.T) {
	srv := NewServer(newTestSupervisor(t))
	for _, tc := range []struct{ name, body string }{
		{"too tight", `{"name":"x","agent":"boss","task":"t","expr":"every 1m"}`},
		{"not a schedule", `{"name":"x","agent":"boss","task":"t","expr":"0 9 * * *"}`},
		{"no task", `{"name":"x","agent":"boss","expr":"every 30m"}`},
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
