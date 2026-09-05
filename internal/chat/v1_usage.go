package chat

import (
	"errors"
	"math"
	"net/http"
	"sort"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
	"cracked/internal/vm"
)

// usageWindow is spend in one span, in money and in tokens.
type usageWindow struct {
	CostUSD          float64 `json:"costUsd"`
	InputTokens      int64   `json:"inputTokens"`
	OutputTokens     int64   `json:"outputTokens"`
	CacheReadTokens  int64   `json:"cacheReadTokens"`
	CacheWriteTokens int64   `json:"cacheWriteTokens"`
	Turns            int64   `json:"turns"`
}

// agentUsage is one agent's three windows, as the guest named it. Retired means
// the agent is gone but its spend is not. There is no longer an ownKey beside
// it: every agent runs on the fleet's key, so every figure here is what was
// charged rather than an estimate at our rates.
type agentUsage struct {
	AgentID  string      `json:"agentId"`
	Name     string      `json:"name"`
	Retired  bool        `json:"retired"`
	Today    usageWindow `json:"today"`
	Week     usageWindow `json:"week"`
	Lifetime usageWindow `json:"lifetime"`
}

// usageTotals is the whole machine, window by window.
type usageTotals struct {
	Today    usageWindow `json:"today"`
	Week     usageWindow `json:"week"`
	Lifetime usageWindow `json:"lifetime"`
}

// usageResponse is GET /v1/usage. Asleep means the machine was not running and
// nothing was asked of it; the lists are always lists, never null.
type usageResponse struct {
	Asleep    bool         `json:"asleep"`
	Zone      string       `json:"zone"`
	WeekStart string       `json:"weekStart"`
	Agents    []agentUsage `json:"agents"`
	Totals    usageTotals  `json:"totals"`
	Unpriced  []string     `json:"unpriced"`
}

// getUsage reports what each agent has cost today, this week and ever.
//
// It never boots the machine. The settings screen calls this on open, and it
// is also where "sign out and delete everything" lives: waking a machine that
// is about to be erased, or paying a minute of boot for a number, is wrong
// both ways. A sleeping machine answers asleep, and the app says so.
func (s *Server) getUsage(w http.ResponseWriter, r *http.Request, user string) {
	machine := machineFor(user)
	if machine == "" {
		fail(w, http.StatusBadGateway, ErrNoVM.Error())
		return
	}
	view, err := s.control.VM(machine)
	if errors.Is(err, ErrNoVM) || (err == nil && view.State != vm.StateRunning) {
		writeJSON(w, http.StatusOK, asleepUsage())
		return
	}
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	report, err := s.clientFor(r.Context(), user, view).AgentUsage()
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, projectUsage(report))
}

// asleepUsage is the answer for a machine that is not running.
func asleepUsage() usageResponse {
	return usageResponse{Asleep: true, Agents: []agentUsage{}, Unpriced: []string{}}
}

// projectUsage prices a machine's report, in the order the guest gave it.
func projectUsage(r agentapi.AgentUsageReport) usageResponse {
	unpriced := map[string]bool{}
	out := usageResponse{Zone: r.Zone, WeekStart: r.WeekStart, Agents: []agentUsage{}}
	for _, a := range r.Agents {
		row := agentUsage{AgentID: a.Agent, Name: a.Name, Retired: a.Retired,
			Today: projectWindow(a.Today, unpriced), Week: projectWindow(a.Week, unpriced),
			Lifetime: projectWindow(a.Lifetime, unpriced)}
		addWindow(&out.Totals.Today, row.Today)
		addWindow(&out.Totals.Week, row.Week)
		addWindow(&out.Totals.Lifetime, row.Lifetime)
		out.Agents = append(out.Agents, row)
	}
	out.Unpriced = make([]string, 0, len(unpriced))
	for model := range unpriced {
		out.Unpriced = append(out.Unpriced, model)
	}
	sort.Strings(out.Unpriced)
	return out
}

// projectWindow prices one window with the same table and the same warnings
// the dashboard's totals use, rounding to the cent a screen shows.
func projectWindow(w agentapi.UsageWindow, unpriced map[string]bool) usageWindow {
	t := agent.Price(agentapi.UsageReport{ByModel: w.ByModel, Turns: w.Turns})
	for _, model := range t.UnpricedModels {
		unpriced[model] = true
	}
	return usageWindow{CostUSD: cents(t.CostUSD), InputTokens: t.InputTokens,
		OutputTokens: t.OutputTokens, CacheReadTokens: t.CacheReadTokens,
		CacheWriteTokens: t.CacheCreationTokens, Turns: t.Turns}
}

// addWindow folds one window into a running total.
func addWindow(into *usageWindow, w usageWindow) {
	into.CostUSD = cents(into.CostUSD + w.CostUSD)
	into.InputTokens += w.InputTokens
	into.OutputTokens += w.OutputTokens
	into.CacheReadTokens += w.CacheReadTokens
	into.CacheWriteTokens += w.CacheWriteTokens
	into.Turns += w.Turns
}

// cents rounds a dollar figure to the cent, which is what a screen shows.
func cents(x float64) float64 { return math.Round(x*100) / 100 }
