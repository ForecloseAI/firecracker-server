package chat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cracked/internal/agentapi"
)

// sonnetRow is one model's tokens in a window.
func sonnetRow(model string, in, out int64) agentapi.ModelUsage {
	return agentapi.ModelUsage{Model: model, Usage: agentapi.Usage{InputTokens: in, OutputTokens: out}, Turns: 1}
}

// window is a usage window with one turn per row.
func window(rows ...agentapi.ModelUsage) agentapi.UsageWindow {
	if rows == nil {
		rows = []agentapi.ModelUsage{}
	}
	return agentapi.UsageWindow{ByModel: rows, Turns: int64(len(rows))}
}

// usageFixture is what a machine reports: the boss on the roster, an agent that
// was retired, and what was spent before agents were told apart, named and
// ordered by the guest.
func usageFixture() agentapi.AgentUsageReport {
	today := sonnetRow("claude-sonnet-5", 123_456, 1_000)
	more := sonnetRow("claude-sonnet-5", 1_000_000, 0)
	return agentapi.AgentUsageReport{
		Zone: "Asia/Kolkata", Today: "2026-09-05", WeekStart: "2026-08-31",
		Agents: []agentapi.AgentUsage{
			{Agent: "boss", Name: "Boss", Today: window(today), Week: window(today, more), Lifetime: window(today, more)},
			{Agent: "cody-old", Name: "cody-old", Retired: true, Today: window(), Week: window(),
				Lifetime: window(sonnetRow("claude-sonnet-5", 10, 1))},
			{Agent: agentapi.UnattributedAgent, Name: "Before per-agent tracking", Today: window(), Week: window(),
				Lifetime: window(sonnetRow("claude-sonnet-5", 500, 50))},
		}}
}

// usageOf asks the gateway for the machine's spend, decoded.
func usageOf(t *testing.T, s *Server, tok string) usageResponse {
	t.Helper()
	w := call(t, s, tok, "GET", "/v1/usage", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got usageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Money is computed here, on the host's price table, and rounded to the cent a
// screen shows -- per agent and per window, with the totals as their sum.
func TestUsageIsPricedInCentsPerAgentAndWindow(t *testing.T) {
	s, g, u := newFake(t)
	g.usage = usageFixture()
	got := usageOf(t, s, u)
	boss := got.Agents[0]
	if boss.AgentID != "boss" || boss.Name != "Boss" || boss.Retired {
		t.Fatalf("the roster's agent came back as %+v", boss)
	}
	// 123,456 in at $2/M is 0.246912; 1,000 out at $10/M is 0.01: 0.26 to the cent.
	if boss.Today.CostUSD != 0.26 || boss.Today.InputTokens != 123_456 || boss.Today.Turns != 1 {
		t.Errorf("today: %+v", boss.Today)
	}
	if boss.Week.CostUSD != 2.26 || boss.Week.Turns != 2 || boss.Lifetime.CostUSD != 2.26 {
		t.Errorf("week %+v lifetime %+v", boss.Week, boss.Lifetime)
	}
	if got.Totals.Today.CostUSD != 0.26 || got.Totals.Lifetime.CostUSD != 2.26 || got.Totals.Lifetime.Turns != 4 {
		t.Errorf("totals: %+v", got.Totals)
	}
	if got.Zone != "Asia/Kolkata" || got.WeekStart != "2026-08-31" || got.Asleep {
		t.Errorf("header: zone=%s week=%s asleep=%v", got.Zone, got.WeekStart, got.Asleep)
	}
}

// What the guest says about a row -- its name, that it was retired, that it
// is the pre-split bucket -- comes through untouched and in the guest's order.
func TestTheGuestsNamesAndOrderComeThrough(t *testing.T) {
	s, g, u := newFake(t)
	g.usage = usageFixture()
	got := usageOf(t, s, u)
	if len(got.Agents) != 3 {
		t.Fatalf("agents: %+v", got.Agents)
	}
	if old := got.Agents[1]; old.AgentID != "cody-old" || !old.Retired || old.Name != "cody-old" {
		t.Errorf("the retired agent came back as %+v", old)
	}
	if last := got.Agents[2]; last.AgentID != agentapi.UnattributedAgent || last.Name != "Before per-agent tracking" {
		t.Errorf("the pre-split bucket came back as %+v", last)
	}
}

// An unknown model must not read as free. Its tokens are counted, its cost is
// zero, and it is named so the screen can say why.
func TestUnpricedModelsAreNamedNotZeroed(t *testing.T) {
	s, g, u := newFake(t)
	row := window(sonnetRow("claude-something-7", 1_000, 10))
	g.usage = agentapi.AgentUsageReport{
		Agents: []agentapi.AgentUsage{{Agent: "boss", Name: "Boss", Today: row, Week: row, Lifetime: row}}}
	got := usageOf(t, s, u)
	if got.Agents[0].Today.InputTokens != 1_000 || got.Agents[0].Today.CostUSD != 0 {
		t.Errorf("today: %+v", got.Agents[0].Today)
	}
	if len(got.Unpriced) != 1 || got.Unpriced[0] != "claude-something-7" {
		t.Errorf("unpriced: %v", got.Unpriced)
	}
}

// An agent on the person's own model is marked by the guest, so the screen
// can say the figure is an estimate at our rates rather than our bill.
func TestAnAgentOnItsOwnKeyIsMarked(t *testing.T) {
	s, g, u := newFake(t)
	g.usage = agentapi.AgentUsageReport{Agents: []agentapi.AgentUsage{
		{Agent: "maya", Name: "Maya", OwnKey: true, Today: window(), Week: window(), Lifetime: window()}}}
	got := usageOf(t, s, u)
	if len(got.Agents) != 1 || !got.Agents[0].OwnKey || got.Agents[0].Name != "Maya" {
		t.Errorf("agents: %+v", got.Agents)
	}
}

// Settings opens on this, and it is also where "delete everything" lives, so
// a number must never cost a minute of boot: a sleeping machine is reported
// asleep and is not asked anything.
func TestASleepingMachineIsNotWokenForANumber(t *testing.T) {
	g := &fakeGuest{usage: usageFixture()}
	srv := httptest.NewServer(g.routes())
	t.Cleanup(srv.Close)
	v, mint := testAuth(t)
	s := &Server{control: stubControl(t, srv.URL, "paused"), auth: v,
		cfg: Config{Origin: "https://chat.example.com", Token: "fleet-token"}}
	w := call(t, s, mint(testUserID, "tester@example.com"), "GET", "/v1/usage", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"asleep":true`) {
		t.Fatalf("status %d body %s", w.Code, w.Body)
	}
	if g.usageHits != 0 {
		t.Error("a sleeping machine was asked for its spend")
	}
}

// A machine that has spent nothing answers with lists, not nulls.
func TestEmptyUsageIsListsNotNull(t *testing.T) {
	s, _, u := newFake(t)
	body := call(t, s, u, "GET", "/v1/usage", "").Body.String()
	if !strings.Contains(body, `"agents":[]`) || !strings.Contains(body, `"unpriced":[]`) {
		t.Errorf("an empty report encoded as %s", body)
	}
}
