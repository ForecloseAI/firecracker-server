package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// usageEvent builds a turn-result event like the guest's emitResult writes.
func usageEvent(id int, cost float64, in, out int64) Event {
	return Event{
		ID: id, Type: "usage", TS: "2026-08-22T10:00:00.000Z", CostUSD: cost,
		DurationMS: 1500, Usage: &Usage{InputTokens: in, OutputTokens: out, CacheReadInputTokens: 7},
	}
}

func TestFoldUsageAccumulates(t *testing.T) {
	var got Totals
	foldUsage(&got, usageEvent(1, 0.02, 100, 20))
	foldUsage(&got, usageEvent(2, 0.03, 200, 30))
	if got.Turns != 2 || got.InputTokens != 300 || got.OutputTokens != 50 || got.CacheReadTokens != 14 {
		t.Errorf("totals = %+v", got)
	}
	if got.CostUSD < 0.049 || got.CostUSD > 0.051 {
		t.Errorf("cost = %v, want ~0.05", got.CostUSD)
	}
	if got.LastCostUSD != 0.03 {
		t.Errorf("last cost = %v, want 0.03", got.LastCostUSD)
	}
}

// A usage event with no usage block still counts as a turn and still bills.
func TestFoldUsageNilUsageBlock(t *testing.T) {
	var got Totals
	foldUsage(&got, Event{ID: 1, Type: "usage", CostUSD: 0.01})
	if got.Turns != 1 || got.TotalTokens() != 0 {
		t.Errorf("totals = %+v", got)
	}
}

// Only usage events move the counters; text and tool_use must not.
func TestAbsorbIgnoresNonUsage(t *testing.T) {
	var e entry
	e.absorb([]Event{{ID: 1, Type: "text", Text: "hi"}, {ID: 2, Type: "tool_use", Tool: "Bash"}}, 2)
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
	e.absorb([]Event{usageEvent(1, 0.01, 10, 1)}, 9)
	e.absorb(nil, 3)
	if e.watermark != 9 {
		t.Errorf("watermark = %d, want 9", e.watermark)
	}
}

func TestAbsorbTrimsRecent(t *testing.T) {
	var e entry
	for i := 1; i <= recentCap+10; i++ {
		e.absorb([]Event{{ID: i, Type: "text"}}, i)
	}
	if len(e.recent) != recentCap {
		t.Fatalf("recent = %d, want %d", len(e.recent), recentCap)
	}
	if e.recent[len(e.recent)-1].ID != recentCap+10 {
		t.Errorf("newest kept id = %d, want %d", e.recent[len(e.recent)-1].ID, recentCap+10)
	}
}

// fakeGuest serves the poll endpoint from a fixed log, honouring ?since=.
func fakeGuest(t *testing.T, log []Event) (ip string, port int, hits *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		since, _ := strconv.Atoi(r.URL.Query().Get("since"))
		fresh := []Event{}
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
func writeJSON(t *testing.T, w http.ResponseWriter, fresh []Event, last int) {
	t.Helper()
	body := `{"events":[`
	for i, e := range fresh {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf(`{"id":%d,"type":%q,"ts":%q,"cost_usd":%v,"duration_ms":%d,`+
			`"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d}}`,
			e.ID, e.Type, e.TS, e.CostUSD, e.DurationMS,
			e.Usage.InputTokens, e.Usage.OutputTokens, e.Usage.CacheReadInputTokens)
	}
	fmt.Fprintf(w, body+`],"last_event_id":%d}`, last)
}

// The watermark is what stops a second poll from re-counting the first batch.
func TestUpdateDoesNotDoubleCount(t *testing.T) {
	log := []Event{usageEvent(1, 0.02, 100, 20), usageEvent(2, 0.03, 200, 30)}
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
	log := []Event{usageEvent(1, 0.02, 100, 20)}
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
	log := []Event{usageEvent(1, 0.02, 100, 20)}
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
