package agentd

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeLog puts a log on disk for an agent that has never been started, which is
// the whole case this file is about.
func writeLog(t *testing.T, sup *Supervisor, id string, lines ...string) {
	t.Helper()
	dir := sup.dirFor(id)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

// Reading a transcript must not start the agent. The app draws every thread at
// once on open, and paying a cold start per thread would spawn the whole roster
// into memory just to render text that is already on disk.
func TestHistoryDoesNotStartTheAgent(t *testing.T) {
	sup := supervisorWith(t, 8)
	writeLog(t, sup, BossID,
		`{"id":1,"agent":"boss","type":"user","text":"hello"}`,
		`{"id":2,"agent":"boss","type":"text","text":"hi back"}`)

	before := sup.LiveCount()
	events, last, err := sup.History(BossID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := sup.LiveCount(); got != before {
		t.Fatalf("live count went %d -> %d; reading a log started an agent", before, got)
	}
	if len(events) != 2 || last != 2 {
		t.Fatalf("events = %+v, last = %d", events, last)
	}
	if events[1].Text != "hi back" {
		t.Errorf("text = %q", events[1].Text)
	}
}

// The since cursor still filters when the log comes off disk.
func TestHistorySinceFiltersFromDisk(t *testing.T) {
	sup := supervisorWith(t, 8)
	writeLog(t, sup, BossID,
		`{"id":1,"agent":"boss","type":"text","text":"one"}`,
		`{"id":2,"agent":"boss","type":"text","text":"two"}`,
		`{"id":3,"agent":"boss","type":"text","text":"three"}`)
	events, _, err := sup.History(BossID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "three" {
		t.Fatalf("events = %+v, want only the newest", events)
	}
}

// A hard kill can leave half a line behind. That must cost one line, not the
// whole transcript.
func TestHistorySurvivesATornFinalLine(t *testing.T) {
	sup := supervisorWith(t, 8)
	writeLog(t, sup, BossID,
		`{"id":1,"agent":"boss","type":"text","text":"kept"}`,
		`{"id":2,"agent":"boss","type":"te`)
	events, last, err := sup.History(BossID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || last != 1 {
		t.Fatalf("events = %+v, last = %d", events, last)
	}
}

// An unknown agent is an error, not an empty transcript that reads as "this
// conversation has nothing in it".
func TestHistoryRejectsAnUnknownAgent(t *testing.T) {
	sup := supervisorWith(t, 8)
	if _, _, err := sup.History("nobody", 0); err == nil {
		t.Fatal("an unknown agent returned a transcript")
	}
}

// GET /agents used to report last_event_id 0 for every agent that was not
// running, which tells a client that an evicted agent -- the one most likely to
// have a long history -- has none at all.
func TestStatusReportsLastEventIDWhenNotLive(t *testing.T) {
	sup := supervisorWith(t, 8)
	writeLog(t, sup, BossID, `{"id":7,"agent":"boss","type":"text","text":"x"}`)
	for _, st := range sup.List() {
		if st.ID != BossID {
			continue
		}
		if st.Live {
			t.Fatal("the boss should not be running")
		}
		if st.LastEventID != 7 {
			t.Errorf("last_event_id = %d, want 7", st.LastEventID)
		}
	}
}

// The poll route is the one the app uses to draw a thread, so it must not start
// anything either.
func TestPollRouteDoesNotStartTheAgent(t *testing.T) {
	sup := newTestSupervisor(t)
	writeLog(t, sup, BossID, `{"id":1,"agent":"boss","type":"text","text":"on disk"}`)
	srv := NewServer(sup)
	before := sup.LiveCount()

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/agents/"+BossID+"/events?poll=1&since=0", nil)
	srv.Routes().ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	if got := sup.LiveCount(); got != before {
		t.Errorf("live count went %d -> %d; a poll started an agent", before, got)
	}
	if !strings.Contains(w.Body.String(), "on disk") {
		t.Errorf("body = %s", w.Body)
	}
}

// Interrupting an agent that is waiting on a person must take the raised hand
// down. It used to leave it up forever: /pending kept advertising a card whose
// gate no longer existed, so it could never be answered and never disappeared.
func TestInterruptClearsTheRaisedHand(t *testing.T) {
	hub := NewInteractions()
	gate := gateFor(t, hub, "cody")
	raiseOne(t, gate, hub)

	gate.RevokeAll()
	if left := hub.List(); len(left) != 0 {
		t.Fatalf("interrupt left %d raised hands up: %+v", len(left), left)
	}
}
