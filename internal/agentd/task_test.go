package agentd

import "testing"

// Nothing used to clear Task, so a title sat on the roster long after the work
// ended and a poll could not tell a running job from a finished one.
func TestFinishTaskClearsTheRoster(t *testing.T) {
	sup := supervisorWith(t, 8)
	if _, err := sup.StartTask(BossID, "Book flights", "book-flights"); err != nil {
		t.Fatal(err)
	}
	if sup.CurrentTask(BossID) == nil {
		t.Fatal("start_task recorded nothing")
	}
	task, ok := sup.FinishTask(BossID)
	if !ok || task.Title != "Book flights" {
		t.Fatalf("FinishTask = %+v, %v", task, ok)
	}
	if got := sup.CurrentTask(BossID); got != nil {
		t.Errorf("task still open: %+v", got)
	}
}

// The end of a task is an event, not just a roster edit: it is the only thing
// that tells a client watching the stream that the job is over.
func TestFinishTaskLogsTheEnd(t *testing.T) {
	sup := supervisorWith(t, 8)
	if _, err := sup.Get(BossID); err != nil { // logFor only reaches a live agent
		t.Fatal(err)
	}
	sup.StartTask(BossID, "Book flights", "book-flights")
	sup.FinishTask(BossID)

	events, _, err := sup.History(BossID, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ended *Event
	for i, e := range events {
		if e.Type == "task_end" {
			ended = &events[i]
		}
	}
	if ended == nil {
		t.Fatalf("no task_end in %d events", len(events))
	}
	if ended.TaskTitle != "Book flights" || ended.TaskSlug != "book-flights" {
		t.Errorf("task_end = %+v", *ended)
	}
}

// Closing nothing is not an error. A model may call finish_task twice, and the
// second call must not read as a second finished job.
func TestFinishTaskTwiceReportsNothingToClose(t *testing.T) {
	sup := supervisorWith(t, 8)
	sup.StartTask(BossID, "Book flights", "book-flights")
	sup.FinishTask(BossID)
	if _, ok := sup.FinishTask(BossID); ok {
		t.Error("a second finish reported a task")
	}
}

// An agent is on one piece of work at a time, so opening a new task is itself
// the news that the last one ended. Without this a forgotten finish_task
// strands the old title on the roster for good.
func TestStartTaskClosesTheOneBefore(t *testing.T) {
	sup := supervisorWith(t, 8)
	if _, err := sup.Get(BossID); err != nil {
		t.Fatal(err)
	}
	sup.StartTask(BossID, "Book flights", "book-flights")
	sup.StartTask(BossID, "Find a hotel", "find-hotel")

	got := sup.CurrentTask(BossID)
	if got == nil || got.Title != "Find a hotel" {
		t.Fatalf("current task = %+v", got)
	}
	events, _, _ := sup.History(BossID, 0)
	if !endedWith(events, "Book flights") {
		t.Error("the first task was replaced without being closed")
	}
}

// Only the boss can call start_task, so a specialist was the one agent whose
// row could never say what it was doing -- and delegated work is exactly the
// work someone would want to be asked about afterwards.
func TestDelegatedWorkBecomesTheWorkersTask(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec, err := sup.Create("coder", "Ada")
	if err != nil {
		t.Fatal(err)
	}
	sup.BeginTask(rec.ID, taskFor(Delegation{
		To: rec.ID, Title: "Write the parser", Task: "everything they need", TaskDir: "/w/trip"}))

	got := sup.CurrentTask(rec.ID)
	if got == nil {
		t.Fatal("the worker was given no task")
	}
	if got.Title != "Write the parser" || got.Dir != "/w/trip" || got.StartedAt.IsZero() {
		t.Errorf("worker task = %+v", *got)
	}
}

// A title that makes no valid slug must still produce a task, or the agent it
// was handed to goes back to having no visible work.
func TestDelegatedWorkWithAnUnslugabbleTitleStillTracks(t *testing.T) {
	got := taskFor(Delegation{To: "ada", Title: "!!!", Task: "do it"})
	if got == nil || got.Slug == "" {
		t.Fatalf("task = %+v", got)
	}
}

// Handing work over must not move the roster. An agent runs one turn at a time,
// so a second delegation waits in the inbox -- and recording it on hand-over
// said the agent had switched jobs while it was still on the last one, and
// closed that one, which the person saw as a finished task to rate.
func TestDelegatingDoesNotOpenTheTaskYet(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec, _ := sup.Create("coder", "Ada")
	first := taskFor(Delegation{To: rec.ID, Title: "Write the parser"})
	sup.BeginTask(rec.ID, first)

	// A second delegation is built, but nothing opens it until a turn does.
	second := taskFor(Delegation{To: rec.ID, Title: "Write the tests"})
	if got := sup.CurrentTask(rec.ID); got == nil || got.Title != "Write the parser" {
		t.Fatalf("current task = %+v, want the one still being worked on", got)
	}

	sup.BeginTask(rec.ID, second) // the turn finally picks it up
	if got := sup.CurrentTask(rec.ID); got == nil || got.Title != "Write the tests" {
		t.Fatalf("current task = %+v", got)
	}
}

// When the turn does move on, the job it left is closed rather than dropped.
// Replacing it silently meant no task_end for it ever reached anyone, and the
// worker's own finish_task then closed the second job under the first's name.
func TestStartingASecondDelegationClosesTheFirst(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec, _ := sup.Create("coder", "Ada")
	if _, err := sup.Get(rec.ID); err != nil { // logFor only reaches a live agent
		t.Fatal(err)
	}
	sup.BeginTask(rec.ID, taskFor(Delegation{To: rec.ID, Title: "Write the parser"}))
	sup.BeginTask(rec.ID, taskFor(Delegation{To: rec.ID, Title: "Write the tests"}))

	events, _, _ := sup.History(rec.ID, 0)
	if !endedWith(events, "Write the parser") {
		t.Error("the first delegation was replaced without being closed")
	}
}

// endedWith reports whether a task with this title was closed.
func endedWith(events []Event, title string) bool {
	for _, e := range events {
		if e.Type == "task_end" && e.TaskTitle == title {
			return true
		}
	}
	return false
}
