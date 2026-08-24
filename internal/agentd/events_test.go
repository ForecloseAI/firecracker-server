package agentd

import (
	"os"
	"path/filepath"
	"testing"
)

// Ids must continue across a restart. If a reopened log restarted at 1, an SSE
// client resuming from Last-Event-ID would silently skip every new event,
// because they would all look older than what it had already seen.
func TestLogResumesIDsAfterReopen(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenLog(dir, "boss")
	if err != nil {
		t.Fatal(err)
	}
	first.Append(Event{Type: "user", Text: "one"})
	first.Append(Event{Type: "user", Text: "two"})

	again, err := OpenLog(dir, "boss")
	if err != nil {
		t.Fatal(err)
	}
	if got := again.LastID(); got != 2 {
		t.Fatalf("LastID after reopen = %d, want 2", got)
	}
	if got := again.Append(Event{Type: "user"}).ID; got != 3 {
		t.Errorf("next id after reopen = %d, want 3", got)
	}
}

// A hard kill can leave a half-written final line. That must cost one event,
// not the whole transcript.
func TestReadAllSkipsTruncatedLine(t *testing.T) {
	dir := t.TempDir()
	log, err := OpenLog(dir, "boss")
	if err != nil {
		t.Fatal(err)
	}
	log.Append(Event{Type: "user", Text: "kept"})
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"id":2,"type":"tex`)
	f.Close()

	events, err := log.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "kept" {
		t.Errorf("got %d events %+v, want the one intact event", len(events), events)
	}
}

// Since is what drives Last-Event-ID replay, so it must be strictly greater
// than the watermark: returning the boundary event again would duplicate it.
func TestSinceIsStrictlyAfterWatermark(t *testing.T) {
	log, err := OpenLog(t.TempDir(), "boss")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"a", "b", "c"} {
		log.Append(Event{Type: "text", Text: s})
	}
	events, err := log.Since(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Text != "c" {
		t.Errorf("Since(2) = %+v, want only the third event", events)
	}
}

// A subscriber that stops reading must not wedge the agent mid-turn, so the
// fan-out drops for a full channel instead of blocking.
func TestAppendDoesNotBlockOnFullSubscriber(t *testing.T) {
	log, err := OpenLog(t.TempDir(), "boss")
	if err != nil {
		t.Fatal(err)
	}
	ch, unsubscribe := log.Subscribe()
	defer unsubscribe()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 500; i++ { // far more than the channel buffer
			log.Append(Event{Type: "text", Text: "flood"})
		}
		close(done)
	}()
	<-done
	if len(ch) == 0 {
		t.Error("subscriber received nothing at all")
	}
}

// Unsubscribing must actually detach, or a finished SSE connection would keep
// its channel in the fan-out set forever.
func TestUnsubscribeDetaches(t *testing.T) {
	log, err := OpenLog(t.TempDir(), "boss")
	if err != nil {
		t.Fatal(err)
	}
	ch, unsubscribe := log.Subscribe()
	unsubscribe()
	log.Append(Event{Type: "text", Text: "after"})
	if len(ch) != 0 {
		t.Error("an unsubscribed channel still received an event")
	}
}
