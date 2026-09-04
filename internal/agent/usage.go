package agent

import (
	"sync"
	"time"

	"cracked/internal/agentapi"
)

// Totals is one VM's cumulative agent spend. It covers the lifetime of the
// workspace, not this boot: the daemon keeps its running total on the persisted
// overlay, so it survives DELETE and resets on ?purge=true.
type Totals struct {
	CostUSD             float64   `json:"cost_usd"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CacheReadTokens     int64     `json:"cache_read_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	Turns               int64     `json:"turns"`
	LastDurationMS      int64     `json:"last_duration_ms"`
	LastActivity        time.Time `json:"last_activity,omitzero"`
	// UnpricedModels names any model the price table did not recognise. Without
	// it an unknown id contributes zero and the row reads like a cheap VM.
	UnpricedModels []string `json:"unpriced_models,omitempty"`
}

// Accumulator caches each VM's last known spend.
//
// It used to rebuild these totals itself, folding an event stream behind a
// per-VM watermark so nothing was counted twice, and re-reading the guest's
// whole log on every poll to do it. The daemon now reports what it has spent,
// so all that is left is fetching, pricing and remembering the answer for a VM
// that is no longer running to be asked.
type Accumulator struct {
	mu   sync.Mutex
	byID map[string]Totals
}

// NewAccumulator builds an empty accumulator.
func NewAccumulator() *Accumulator {
	return &Accumulator{byID: map[string]Totals{}}
}

// Forget drops a VM's totals. Required on delete, so a recreated VM reports its
// own spend rather than inheriting the last one's.
func (a *Accumulator) Forget(id string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.byID, id)
}

// Update fetches a guest's spend, prices it, and caches the result.
//
// On failure the last known totals are returned alongside the error, so a blip
// shows stale spend rather than blanking a live VM's cost to zero.
func (a *Accumulator) Update(id, guestIP string, port int) (Totals, error) {
	report, err := New(guestIP, port).Usage()
	if err != nil {
		return a.Snapshot(id), err
	}
	t := Price(report)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.byID[id] = t
	return t, nil
}

// Snapshot returns a VM's last known totals without touching the guest. Used
// for VMs that are not running, where a poll would only buy a timeout.
func (a *Accumulator) Snapshot(id string) Totals {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.byID[id]
}

// Price turns a daemon's token report into money.
func Price(r agentapi.UsageReport) Totals {
	t := Totals{Turns: r.Turns, LastDurationMS: r.LastDurationMS, LastActivity: r.LastActivity}
	for _, row := range r.ByModel {
		addTokens(&t, row.Usage)
		cost, ok := costOf(row.Model, row.Usage)
		if !ok {
			warnUnpriced(row.Model)
			t.UnpricedModels = append(t.UnpricedModels, row.Model)
			continue
		}
		t.CostUSD += cost
	}
	return t
}

// addTokens folds one model's token counts into a VM's totals.
func addTokens(t *Totals, u agentapi.Usage) {
	t.InputTokens += u.InputTokens
	t.OutputTokens += u.OutputTokens
	t.CacheReadTokens += u.CacheReadInputTokens
	t.CacheCreationTokens += u.CacheCreationInputTokens
}
