package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"cracked/internal/agentapi"
)

// stamp is a fixed event time, so LastActivity is assertable.
var stamp = time.Date(2026, 8, 22, 10, 0, 0, 0, time.UTC)

// usageEvent builds a usage event the way the daemon writes one: tokens and a
// model id, and no dollar figure -- pricing is the host's job.
func usageEvent(id int, model string, in, out int64) agentapi.Event {
	return agentapi.Event{
		ID: id, Type: "usage", TS: stamp, Model: model, DurationMS: 1500,
		Usage: &agentapi.Usage{InputTokens: in, OutputTokens: out, CacheReadInputTokens: 7},
	}
}

func TestFoldUsageAccumulates(t *testing.T) {
	var got Totals
	foldUsage(&got, usageEvent(1, "claude-sonnet-5", 100, 20))
	foldUsage(&got, usageEvent(2, "claude-sonnet-5", 300, 40))
	if got.InputTokens != 400 || got.OutputTokens != 60 || got.CacheReadTokens != 14 {
		t.Errorf("tokens = %+v", got)
	}
	if got.Turns != 2 {
		t.Errorf("turns = %d, want 2", got.Turns)
	}
	if !got.LastActivity.Equal(stamp) {
		t.Errorf("last activity = %v, want %v", got.LastActivity, stamp)
	}
}

// The daemon reports tokens and a model id and no dollar figure, so folding can
// only produce zero until the host prices them. This pins that the zero is a
// known gap and not an accumulation bug: tokens must still be right.
func TestFoldUsageLeavesCostUnpricedForNow(t *testing.T) {
	var got Totals
	foldUsage(&got, usageEvent(1, "claude-sonnet-5", 100, 20))
	if got.CostUSD != 0 {
		t.Errorf("cost = %v; nothing on the wire can produce one yet", got.CostUSD)
	}
	if got.InputTokens == 0 {
		t.Error("tokens must accumulate even while cost cannot")
	}
}

// A usage event with no usage block must not panic or corrupt the totals.
func TestFoldUsageNilUsageBlock(t *testing.T) {
	var got Totals
	foldUsage(&got, agentapi.Event{ID: 1, Type: "usage", Model: "claude-sonnet-5"})
	if got.Turns != 1 || got.InputTokens != 0 {
		t.Errorf("totals = %+v", got)
	}
}

func TestAbsorbIgnoresNonUsage(t *testing.T) {
	var e entry
	e.absorb([]agentapi.Event{{ID: 1, Type: "text", Text: "hi"}, {ID: 2, Type: "tool_use", Tool: "Bash"}}, 2)
	if e.totals.Turns != 0 {
		t.Errorf("turns = %d, want 0", e.totals.Turns)
	}
	if e.watermark != 2 || len(e.recent) != 2 {
		t.Errorf("watermark = %d, recent = %d", e.watermark, len(e.recent))
	}
}

// A reported id that does not move forward must not rewind the watermark, or
// the next poll would re-read events already counted.
func TestAbsorbWatermarkNeverRewinds(t *testing.T) {
	var e entry
	e.absorb([]agentapi.Event{usageEvent(1, "claude-sonnet-5", 10, 1)}, 9)
	e.absorb(nil, 3)
	if e.watermark != 9 {
		t.Errorf("watermark = %d, want 9", e.watermark)
	}
}

func TestAbsorbTrimsRecent(t *testing.T) {
	var e entry
	for i := 1; i <= recentCap+10; i++ {
		e.absorb([]agentapi.Event{{ID: i, Type: "text"}}, i)
	}
	if len(e.recent) != recentCap {
		t.Fatalf("recent = %d, want %d", len(e.recent), recentCap)
	}
	if e.recent[len(e.recent)-1].ID != recentCap+10 {
		t.Errorf("newest kept id = %d, want %d", e.recent[len(e.recent)-1].ID, recentCap+10)
	}
}

// fakeGuest serves the poll endpoint from a fixed log, honouring ?since=.
func fakeGuest(t *testing.T, log []agentapi.Event) (ip string, port int, hits *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		fresh := []agentapi.Event{}
		for _, e := range log {
			if e.ID > since {
				fresh = append(fresh, e)
			}
		}
		last := since
		if len(fresh) > 0 {
			last = fresh[len(fresh)-1].ID
		}
		writeJSON(t, w, fresh, last)
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(u.Port())
	return u.Hostname(), p, &n
}

// writeJSON emits the poll response shape by hand, so the test does not depend
// on the client's own decoding to define the contract.
func writeJSON(t *testing.T, w http.ResponseWriter, fresh []agentapi.Event, last int) {
	t.Helper()
	body := `{"events":[`
	for i, e := range fresh {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf(`{"id":%d,"agent":"boss","type":%q,"ts":%q,"model":%q,"duration_ms":%d,`+
			`"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d}}`,
			e.ID, e.Type, e.TS.Format(time.RFC3339Nano), e.Model, e.DurationMS,
			e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.CacheReadInputTokens)
	}
	fmt.Fprintf(w, body+`],"last_event_id":%d}`, last)
}

// The watermark is what stops a second poll from re-counting the first batch.
func TestUpdateDoesNotDoubleCount(t *testing.T) {
	log := []agentapi.Event{usageEvent(1, "claude-sonnet-5", 100, 20), usageEvent(2, "claude-sonnet-5", 200, 30)}
	ip, port, _ := fakeGuest(t, log)
	a := NewAccumulator()
	if _, _, err := a.Update("probe", ip, port); err != nil {
		t.Fatalf("first update: %v", err)
	}
	got, recent, err := a.Update("probe", ip, port)
	if err != nil {
		t.Fatalf("second update: %v", err)
	}
	if got.Turns != 2 || got.InputTokens != 300 {
		t.Errorf("after re-poll totals = %+v, want 2 turns / 300 input", got)
	}
	if len(recent) != 2 {
		t.Errorf("recent = %d, want 2", len(recent))
	}
}

// After Forget the watermark is gone, so the next poll re-reads from zero and
// rebuilds totals cleanly rather than skipping a recreated VM's whole log.
func TestForgetResetsWatermark(t *testing.T) {
	log := []agentapi.Event{usageEvent(1, "claude-sonnet-5", 100, 20)}
	ip, port, _ := fakeGuest(t, log)
	a := NewAccumulator()
	if _, _, err := a.Update("probe", ip, port); err != nil {
		t.Fatal(err)
	}
	a.Forget("probe")
	got, _, err := a.Update("probe", ip, port)
	if err != nil {
		t.Fatal(err)
	}
	if got.Turns != 1 || got.InputTokens != 100 {
		t.Errorf("after forget totals = %+v, want a clean single turn", got)
	}
}

// An unreachable guest must return the last known totals, not an empty set,
// so a blip does not blank the dashboard.
func TestUpdateUnreachableKeepsTotals(t *testing.T) {
	log := []agentapi.Event{usageEvent(1, "claude-sonnet-5", 100, 20)}
	ip, port, _ := fakeGuest(t, log)
	a := NewAccumulator()
	if _, _, err := a.Update("probe", ip, port); err != nil {
		t.Fatal(err)
	}
	got, _, err := a.Update("probe", "127.0.0.1", 1)
	if err == nil {
		t.Fatal("expected an error from an unreachable guest")
	}
	if got.Turns != 1 {
		t.Errorf("totals = %+v, want the last known single turn", got)
	}
}
