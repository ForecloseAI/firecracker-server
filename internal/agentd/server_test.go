package agentd

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newTestServer builds a server over one idle agent with no tools.
func newTestServer(t *testing.T) (*Server, *Agent) {
	t.Helper()
	a := newTestAgent(t)
	return NewServer(a), a
}

// do runs one request against the routes and returns the recorder.
func do(t *testing.T, s *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	w := httptest.NewRecorder()
	s.Routes().ServeHTTP(w, req)
	return w
}

// GET /agents is the single call a polling client makes, so it has to carry
// state and the log watermark without needing a second request.
func TestListReportsStateAndWatermark(t *testing.T) {
	s, a := newTestServer(t)
	a.Log().Append(Event{Type: "text", Text: "something"})

	w := do(t, s, "GET", "/agents", "")
	var got []view
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "boss" || got[0].State != "idle" {
		t.Fatalf("GET /agents = %+v, want one idle boss", got)
	}
	if got[0].LastEventID != a.Log().LastID() {
		t.Errorf("last_event_id = %d, want %d", got[0].LastEventID, a.Log().LastID())
	}
}

// A turn can take minutes, so the send must return at once rather than waiting
// on the model.
func TestMessageIsAcceptedNotAwaited(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s, "POST", "/agents/boss/messages", `{"text":"hello"}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if got["message_id"] == "" || got["message_id"] == nil {
		t.Errorf("no message_id in %v", got)
	}
}

// A double-tapped send must collapse, or the agent runs the same instruction
// twice. The second call reports the first call's id and does not queue again.
func TestIdempotencyKeyCollapsesRepeat(t *testing.T) {
	s, a := newTestServer(t)
	send := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/agents/boss/messages", strings.NewReader(`{"text":"once"}`))
		req.Header.Set("Idempotency-Key", "same-key")
		w := httptest.NewRecorder()
		s.Routes().ServeHTTP(w, req)
		return w
	}
	first, second := send(), send()
	if first.Code != http.StatusAccepted || second.Code != http.StatusOK {
		t.Fatalf("codes = %d, %d; want 202 then 200", first.Code, second.Code)
	}
	if len(a.inbox) != 1 {
		t.Errorf("queued %d messages, want 1", len(a.inbox))
	}
}

// A full inbox must be reported, not silently buffered or blocked on: the
// person needs to know the agent is not keeping up.
func TestFullInboxReports503WithRetryAfter(t *testing.T) {
	s, a := newTestServer(t)
	for i := 0; i < inboxDepth; i++ {
		if err := a.Send("filler"); err != nil {
			t.Fatalf("filling inbox: %v", err)
		}
	}
	w := do(t, s, "POST", "/agents/boss/messages", `{"text":"one too many"}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("503 carried no Retry-After")
	}
}

// Errors use the control plane's shape, so one client-side decoder works
// against the whole system.
func TestUnknownAgentIs404InTheStandardShape(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s, "POST", "/agents/nobody/messages", `{"text":"hi"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var e apiError
	json.Unmarshal(w.Body.Bytes(), &e)
	if e.Error != "not_found" || e.Resource != "agent" {
		t.Errorf("body = %+v, want error=not_found resource=agent", e)
	}
}

// An empty message is a client bug, not an instruction to the model.
func TestEmptyTextIsRejected(t *testing.T) {
	s, _ := newTestServer(t)
	if w := do(t, s, "POST", "/agents/boss/messages", `{"text":""}`); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

// Interrupting an idle agent is not an error, it just reports that there was
// nothing to stop.
func TestInterruptIdleAgentReportsNothingStopped(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s, "POST", "/agents/boss/interrupt", "")
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || got["interrupted"] != false {
		t.Errorf("code=%d body=%v, want 200 and interrupted=false", w.Code, got)
	}
}

// The poll form exists for clients that cannot hold a connection open, and
// must honour the same watermark as the stream.
func TestPollReturnsOnlyEventsAfterWatermark(t *testing.T) {
	s, a := newTestServer(t)
	for _, txt := range []string{"a", "b", "c"} {
		a.Log().Append(Event{Type: "text", Text: txt})
	}
	last := a.Log().LastID()
	w := do(t, s, "GET", "/agents/boss/events?poll=1&since="+strconv.Itoa(last-1), "")
	var got struct {
		Events      []Event `json:"events"`
		LastEventID int     `json:"last_event_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Events) != 1 || got.Events[0].Text != "c" {
		t.Errorf("events = %+v, want only the last one", got.Events)
	}
	if got.LastEventID != last {
		t.Errorf("last_event_id = %d, want %d", got.LastEventID, last)
	}
}

// The stream must replay what a reconnecting client missed and then continue
// live, with no duplicate across the join.
//
// The subscription opens BEFORE the replay is read, so an event appended in
// between reaches neither path unless the overlap is handled -- which is why
// emit drops anything not newer than the high-water mark. This test drives both
// halves: two events replayed, one delivered live.
func TestStreamReplaysThenContinuesLiveWithoutDuplicates(t *testing.T) {
	_, a := newTestServer(t)
	for _, txt := range []string{"one", "two", "three"} {
		a.Log().Append(Event{Type: "text", Text: txt})
	}
	srv := httptest.NewServer(NewServer(a).Routes())
	defer srv.Close()

	// Resume from two events back, so exactly two are replayed.
	last := a.Log().LastID()
	since := last - 2

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET",
		srv.URL+"/agents/boss/events?since="+strconv.Itoa(since), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var liveID int
	ids := readIDs(t, resp.Body, 3, func() {
		liveID = a.Log().Append(Event{Type: "text", Text: "live"}).ID
	})
	want := []int{since + 1, last, liveID}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v (two replayed, then one live, no duplicates)", ids, want)
		}
	}
}

// readIDs reads n SSE id lines, running after() once the replay has arrived.
func readIDs(t *testing.T, body interface{ Read([]byte) (int, error) }, n int, after func()) []int {
	t.Helper()
	var ids []int
	sc := bufio.NewScanner(body)
	for sc.Scan() && len(ids) < n {
		line := sc.Text()
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		var id int
		json.Unmarshal([]byte(line[4:]), &id)
		ids = append(ids, id)
		if len(ids) == n-1 {
			after()
		}
	}
	if len(ids) < n {
		t.Fatalf("read only %v before the stream ended", ids)
	}
	return ids
}
