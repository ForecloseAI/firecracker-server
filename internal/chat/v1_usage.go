package chat

import (
	"errors"
	"log"
	"math"
	"net/http"
	"slices"
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

// agentUsage is one agent's three windows, named for the roster. Retired means
// the agent is gone but its spend is not; OwnKey means it ran on the person's
// own model, so the cost is an estimate at Anthropic's rates, not our bill.
type agentUsage struct {
	AgentID  string      `json:"agentId"`
	Name     string      `json:"name"`
	Retired  bool        `json:"retired"`
	OwnKey   bool        `json:"ownKey"`
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
	cl := s.clientFor(r.Context(), user, view)
	report, err := cl.Usage()
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	roster, err := cl.Agents()
	if err != nil {
		log.Printf("chat: usage for %s has no names: %v", machine, err)
	}
	writeJSON(w, http.StatusOK, projectUsage(report, roster))
}

// asleepUsage is the answer for a machine that is not running.
func asleepUsage() usageResponse {
	return usageResponse{Asleep: true, Agents: []agentUsage{}, Unpriced: []string{}}
}

// projectUsage prices a machine's report and names its agents from the roster.
func projectUsage(r agentapi.UsageReport, roster []agentapi.Status) usageResponse {
	unpriced := map[string]bool{}
	out := usageResponse{Zone: r.Zone, WeekStart: r.WeekStart, Agents: []agentUsage{}}
	for _, a := range orderAgents(r.Agents, roster) {
		row := agentRow(a, roster, unpriced)
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

// orderAgents puts the roster's agents first in roster order, then retired
// ones by id, then what was spent before agents were told apart.
func orderAgents(in []agentapi.AgentUsage, roster []agentapi.Status) []agentapi.AgentUsage {
	byID := map[string]agentapi.AgentUsage{}
	for _, a := range in {
		byID[a.Agent] = a
	}
	var out []agentapi.AgentUsage
	for _, st := range roster {
		if a, ok := byID[st.ID]; ok {
			out = append(out, a)
			delete(byID, st.ID)
		}
	}
	rest := make([]agentapi.AgentUsage, 0, len(byID))
	for _, a := range byID {
		rest = append(rest, a)
	}
	sort.Slice(rest, func(i, j int) bool { return rest[i].Agent < rest[j].Agent })
	i := slices.IndexFunc(rest, isUnattributed)
	if i >= 0 {
		rest = append(slices.Delete(slices.Clone(rest), i, i+1), rest[i])
	}
	return append(out, rest...)
}

// isUnattributed picks out the legacy bucket, which orderAgents puts last.
func isUnattributed(a agentapi.AgentUsage) bool { return a.Agent == agentapi.UnattributedAgent }

// agentRow prices one agent's windows and names it.
func agentRow(a agentapi.AgentUsage, roster []agentapi.Status, unpriced map[string]bool) agentUsage {
	row := agentUsage{AgentID: a.Agent, Name: a.Agent, Retired: true,
		Today: projectWindow(a.Today, unpriced), Week: projectWindow(a.Week, unpriced),
		Lifetime: projectWindow(a.Lifetime, unpriced)}
	if a.Agent == agentapi.UnattributedAgent {
		row.Name, row.Retired = "Unattributed", false
		return row
	}
	if st, ok := find(roster, a.Agent); ok {
		row.Name, row.Retired, row.OwnKey = st.Name, false, st.Model != nil
	}
	return row
}

// projectWindow sums a window's tokens and prices them per model, rounding to
// cents. A model the table does not know is named rather than counted as free.
func projectWindow(w agentapi.UsageWindow, unpriced map[string]bool) usageWindow {
	var out usageWindow
	var usd float64
	for _, row := range w.ByModel {
		out.InputTokens += row.InputTokens
		out.OutputTokens += row.OutputTokens
		out.CacheReadTokens += row.CacheReadInputTokens
		out.CacheWriteTokens += row.CacheCreationInputTokens
		cost, ok := agent.PriceUsage(row.Model, row.Usage)
		if !ok {
			unpriced[row.Model] = true
		}
		usd += cost
	}
	out.CostUSD, out.Turns = cents(usd), w.Turns
	return out
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
