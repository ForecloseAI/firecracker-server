package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeGuest serves GET /usage from a fixed body, counting how often it is hit.
func fakeGuest(t *testing.T, body string) (ip string, port int, hits *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			t.Errorf("path = %q, want /usage", r.URL.Path)
		}
		n++
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	ip, port = hostPort(t, srv)
	return ip, port, &n
}

// The daemon's report is hand-written here rather than marshalled from the Go
// type, so the test defines the contract instead of inheriting it.
const guestUsage = `{"by_model":[
  {"model":"claude-sonnet-5","input_tokens":1000000,"output_tokens":0,
   "cache_creation_input_tokens":0,"cache_read_input_tokens":0,"turns":2}],
 "turns":2,"last_duration_ms":4210,"last_activity":"2026-08-26T09:00:00Z"}`

// The headline: a guest's tokens must come back as money. This is the figure
// that read $0.00 on every go VM for the whole rollout.
func TestUpdatePricesWhatTheGuestReports(t *testing.T) {
	ip, port, _ := fakeGuest(t, guestUsage)
	a := NewAccumulator()
	got, err := a.Update("probe", ip, port)
	if err != nil {
		t.Fatal(err)
	}
	if want := priceTable["claude-sonnet-5"].in; !near(got.CostUSD, want) {
		t.Errorf("cost = $%v, want $%v", got.CostUSD, want)
	}
	if got.Turns != 2 || got.LastDurationMS != 4210 || got.LastActivity.IsZero() {
		t.Errorf("totals = %+v", got)
	}
}

// Repeated polls report the same number. The old accumulator folded an event
// stream and needed a watermark to avoid counting a batch twice; a report is
// idempotent by construction, and this pins that the replacement kept the
// property rather than merely losing the machinery that provided it.
func TestPollingTwiceDoesNotDoubleCount(t *testing.T) {
	ip, port, hits := fakeGuest(t, guestUsage)
	a := NewAccumulator()
	first, _ := a.Update("probe", ip, port)
	second, _ := a.Update("probe", ip, port)
	if first.CostUSD != second.CostUSD || first.InputTokens != second.InputTokens {
		t.Errorf("two polls disagreed: %+v then %+v", first, second)
	}
	if *hits != 2 {
		t.Errorf("guest hit %d times, want 2", *hits)
	}
}

// A blip must show the last known spend, not blank a live VM's cost to zero --
// and it must still say that it failed, or the stale figure looks current.
func TestUpdateUnreachableKeepsTotalsAndReportsWhy(t *testing.T) {
	ip, port, _ := fakeGuest(t, guestUsage)
	a := NewAccumulator()
	good, _ := a.Update("probe", ip, port)

	stale, err := a.Update("probe", "192.0.2.1", 9)
	if err == nil {
		t.Error("an unreachable guest reported success")
	}
	if stale.CostUSD != good.CostUSD {
		t.Errorf("cost blanked to %v on a blip, want the last known %v", stale.CostUSD, good.CostUSD)
	}
}

// A recreated VM must report its own spend. The daemon's total lives on the
// overlay and ?purge=true wipes it, so a cached figure here would leave the
// dashboard showing a dead VM's bill against a fresh one.
func TestForgetDropsAVMsTotals(t *testing.T) {
	ip, port, _ := fakeGuest(t, guestUsage)
	a := NewAccumulator()
	a.Update("probe", ip, port)
	a.Forget("probe")
	if got := a.Snapshot("probe"); got.CostUSD != 0 || got.Turns != 0 {
		t.Errorf("totals survived a delete: %+v", got)
	}
}
