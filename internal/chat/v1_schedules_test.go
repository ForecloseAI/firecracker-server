package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"cracked/internal/agentapi"
)

// A scheduled fire is the person's own agent acting on a timer. Projecting it
// as a text message would put words in the transcript they never typed, so it
// has to arrive as an event -- and it has to carry the job's name, because
// "check the deploy queue" appearing at 3am means nothing without it.
func TestAScheduledFireIsAnEventNamingTheJob(t *testing.T) {
	got, ok := projectMessage(agentapi.Event{
		ID: 7, Type: "scheduled", TaskTitle: "morning sweep", Text: "check the deploy queue",
	})
	if !ok {
		t.Fatal("a scheduled fire was dropped; the app would never see it")
	}
	if got.Kind != kindEvent {
		t.Errorf("kind = %q, want %q", got.Kind, kindEvent)
	}
	if got.From == fromMe {
		t.Error("a timer was rendered as the person speaking")
	}
	if !strings.Contains(got.Text, "morning sweep") {
		t.Errorf("text = %q, want it to name the schedule", got.Text)
	}
}

// A fire with no name must still be legible rather than rendering as an empty
// pill.
func TestAnUnnamedScheduledFireStillReads(t *testing.T) {
	got, ok := projectMessage(agentapi.Event{ID: 8, Type: "scheduled"})
	if !ok || strings.TrimSpace(got.Text) == "" {
		t.Errorf("unnamed fire projected to %+v, want a readable line", got)
	}
}

// Compaction is how an agent forgets. It is quiet housekeeping, but the person
// needs some way to see why the agent no longer remembers something.
func TestCompactionReachesTheTranscript(t *testing.T) {
	got, ok := projectMessage(agentapi.Event{
		ID: 9, Type: "compaction", Message: "summarized 412 of 500 messages",
	})
	if !ok {
		t.Fatal("compaction was dropped")
	}
	if got.Kind != kindEvent {
		t.Errorf("kind = %q, want %q", got.Kind, kindEvent)
	}
	if !strings.Contains(got.Text, "412") {
		t.Errorf("text = %q, want the event's own message", got.Text)
	}
}

// The default arm is what keeps this surface small: a guest that learns a new
// event type must not start pushing it at an app that cannot render it.
func TestUnknownEventTypesAreStillDropped(t *testing.T) {
	for _, kind := range []string{"usage", "state", "turn_complete", "tool_use", "ready"} {
		if _, ok := projectMessage(agentapi.Event{ID: 1, Type: kind}); ok {
			t.Errorf("%q reached the transcript; only conversation lines belong there", kind)
		}
	}
}

// schedules serves a fixed set from the fake guest.
func (g *fakeGuest) schedules(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	json.NewEncoder(w).Encode(g.sched)
}

// dropSchedule removes one, or 404s, as the daemon does.
func (g *fakeGuest) dropSchedule(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	id := r.PathValue("id")
	for i, sc := range g.sched {
		if sc.ID == id {
			g.sched = append(g.sched[:i], g.sched[i+1:]...)
			json.NewEncoder(w).Encode(map[string]any{"id": id, "deleted": true})
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

// The list is what the person opens to see what runs while they are away.
func TestSchedulesRoundTrip(t *testing.T) {
	s, g, u := newFake(t)
	g.sched = []agentapi.Schedule{
		{ID: "sch-0001", Name: "morning sweep", Agent: "boss", Expr: "every 30m", Enabled: true},
	}

	var got []agentapi.Schedule
	w := call(t, s, u, "GET", "/v1/schedules", "")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("status %d: %v", w.Code, err)
	}
	if len(got) != 1 || got[0].Name != "morning sweep" {
		t.Fatalf("list = %+v, want the one schedule", got)
	}

	if code := call(t, s, u, "DELETE", "/v1/schedules/sch-0001", "").Code; code != http.StatusNoContent {
		t.Errorf("delete = %d, want 204", code)
	}
	if len(g.sched) != 0 {
		t.Errorf("%d schedules survived the delete", len(g.sched))
	}
}

// An empty machine must answer with an empty list, never null: the app renders
// this straight into a list, where null is a crash and [] is an empty state.
func TestNoSchedulesIsAnEmptyListNotNull(t *testing.T) {
	s, _, u := newFake(t)
	body := strings.TrimSpace(call(t, s, u, "GET", "/v1/schedules", "").Body.String())
	if body != "[]" {
		t.Errorf("body = %q, want []", body)
	}
}

// Cancelling something already gone is the person tapping twice, not a broken
// gateway -- it must not surface as a 502.
func TestCancellingAMissingScheduleIs404(t *testing.T) {
	s, _, u := newFake(t)
	if code := call(t, s, u, "DELETE", "/v1/schedules/sch-nope", "").Code; code != http.StatusNotFound {
		t.Errorf("delete of a missing schedule = %d, want 404", code)
	}
}

// The profile is the only way a zone reaches the guest. The country picked at
// onboarding resolves to an IANA name here, and the guest puts itself on that
// clock -- so this one call is what decides whose 9am "daily at 09:00" means.
func TestProfileCarriesTheTimezone(t *testing.T) {
	s, g, u := newFake(t)
	code := call(t, s, u, "PUT", "/v1/profile",
		`{"name":"Sam","work":"ops","onboarded":true,"tz":"Asia/Kolkata"}`).Code
	if code != http.StatusNoContent {
		t.Fatalf("put profile = %d, want 204", code)
	}
	if g.person.TZ != "Asia/Kolkata" {
		t.Errorf("the guest received tz %q, want Asia/Kolkata", g.person.TZ)
	}
}

// A message carries no zone, and an old client that still sends one is ignored
// rather than obeyed.
//
// This is the invariant the whole design rests on: one writer. A zone arriving
// on ordinary traffic is a second source for a fact the profile already owns,
// and it is the one that drifts -- a VPN, a second device or a wrong clock
// would silently re-anchor every "daily at 09:00" the person had booked.
func TestAMessageNeverCarriesATimezone(t *testing.T) {
	s, g, u := newFake(t)
	code := call(t, s, u, "POST", "/v1/threads/boss/messages",
		`{"text":"ship it","tz":"America/Los_Angeles"}`).Code
	if code != http.StatusOK {
		t.Fatalf("post message = %d, want 200", code)
	}
	if strings.Contains(g.body, "tz") {
		t.Errorf("the guest was sent %s, which still carries a zone: only the profile may set it", g.body)
	}
}
