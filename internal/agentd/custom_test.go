package agentd

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
)

// ownModel is a model the person picked for themselves.
func ownModel() *agentapi.ModelConfig {
	return &agentapi.ModelConfig{Model: "openai/gpt-4o", Thinking: "high"}
}

// custom builds one custom agent on a supervisor.
func custom(t *testing.T, sup *Supervisor, role string, m *agentapi.ModelConfig) Record {
	t.Helper()
	rec, err := sup.CreateWith(agentapi.CreateAgentReq{Type: agentapi.CustomType, Name: "Maya",
		Instructions: role, Model: m})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

// A custom agent is defined by what the person wrote and chose, and both must
// come back after a restart -- the key too, since the machine dials with it.
// But the key stops at the roster's own file: what GET /agents reports says
// only that a key is set.
func TestACustomAgentKeepsItsRoleAndModel(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec := custom(t, sup, "Plan trips. Be brief.", ownModel())
	again, err := LoadRoster(sup.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get(rec.ID)
	if !ok || got.Instructions != "Plan trips. Be brief." || got.Model == nil || got.Model.Model == "" {
		t.Fatalf("reloaded record: %+v", got)
	}
	wire, _ := json.Marshal(sup.List())
	if !strings.Contains(string(wire), `"model":"openai/gpt-4o"`) || !strings.Contains(string(wire), "Plan trips") {
		t.Errorf("the listing lost the role or the model: %s", wire)
	}
	// Nothing in a model config is a secret any more, so nothing has to be held
	// back -- but the listing must not sprout the fields that used to carry one.
	for _, gone := range []string{"key_set", "api_key", "url"} {
		if strings.Contains(string(wire), gone) {
			t.Errorf("the listing still carries %q: %s", gone, wire)
		}
	}
}

// Two custom agents may share a name: the ids diverge, and a second Maya is not
// a duplicate the way a second coder would be.
func TestAnyNumberOfCustomAgentsMayExist(t *testing.T) {
	sup := supervisorWith(t, 8)
	a := custom(t, sup, "x", nil)
	b := custom(t, sup, "y", nil)
	if a.ID == b.ID {
		t.Fatalf("two custom agents share id %s", a.ID)
	}
}

// The machine is the one place a custom agent is checked -- the host relays its
// refusal -- so every rule lives here: a name that fits a row, a role that
// exists and fits a page or two, and a model that is named at a level we know.
//
// The model ID itself is deliberately unchecked: it goes to OpenRouter's whole
// catalogue, which this machine holds no list of, so a name that does not exist
// there fails the first turn in the gateway's words rather than being guessed at
// in ours.
func TestACustomAgentIsCheckedBeforeItIsStored(t *testing.T) {
	sup := supervisorWith(t, 8)
	for _, bad := range []*agentapi.ModelConfig{
		{Model: ""},    // no model
		{Model: "   "}, // whitespace is not a model
		{Model: "openai/gpt-4o", Thinking: "max"}, // not a thinking level
	} {
		req := agentapi.CreateAgentReq{Type: agentapi.CustomType, Name: "Maya", Instructions: "x", Model: bad}
		if _, err := sup.CreateWith(req); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	for _, bad := range []agentapi.CreateAgentReq{
		{Type: agentapi.CustomType, Name: "", Instructions: "x"},
		{Type: agentapi.CustomType, Name: strings.Repeat("n", nameCap+1), Instructions: "x"},
		{Type: agentapi.CustomType, Name: "Maya", Instructions: "   "},
		{Type: agentapi.CustomType, Name: "Maya", Instructions: strings.Repeat("r", roleCap+1)},
	} {
		if _, err := sup.CreateWith(bad); err == nil {
			t.Errorf("accepted name %q with a %d-rune role", bad.Name, len([]rune(bad.Instructions)))
		}
	}
	if _, err := sup.Create("coder", ""); err != nil {
		t.Errorf("a gallery agent with no name was refused: %v", err) // it takes the type's
	}
}

// Only a custom agent's role and model can change: the gallery profiles are the
// product. Anyone may be renamed.
func TestOnlyACustomAgentsRoleAndModelCanChange(t *testing.T) {
	sup := supervisorWith(t, 8)
	ada, _ := sup.Create("coder", "Ada")
	role := "Write Rust."
	if _, err := sup.Update(ada.ID, agentapi.AgentPatch{Instructions: &role}); !errors.Is(err, errNotCustom) {
		t.Errorf("a coder's role was rewritten, or refused for the wrong reason: %v", err)
	}
	name := "Ada L"
	if got, err := sup.Update(ada.ID, agentapi.AgentPatch{Name: &name}); err != nil || got.Name != "Ada L" {
		t.Errorf("rename: %+v, %v", got, err)
	}
}

// An edit swaps the model outright, and can hand it back to the default.
//
// There is no longer a stored secret for a patch to preserve, which is the
// whole of what this used to have to be careful about: an edit that left the
// key out once meant "keep the one you hold", and now an edit is simply the
// model the person chose.
func TestAPatchSwapsTheModelAndCanClearIt(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec := custom(t, sup, "x", ownModel())
	swap := &agentapi.ModelPatch{ModelConfig: agentapi.ModelConfig{Model: "google/gemini-2.5-flash"}}
	got, err := sup.Update(rec.ID, agentapi.AgentPatch{Model: swap})
	if err != nil || got.Model.Model != "google/gemini-2.5-flash" {
		t.Fatalf("patch: %+v, %v", got.Model, err)
	}
	got, err = sup.Update(rec.ID, agentapi.AgentPatch{Model: &agentapi.ModelPatch{Clear: true}})
	if err != nil || got.Model != nil {
		t.Fatalf("clear: %+v, %v", got.Model, err)
	}
}

// An edit has to reach the running agent, whose prompt was composed at start.
// Idle, it is recycled on the spot; mid-turn, it is marked to recycle when the
// turn ends, exactly as create_skill does -- so the next reply is on the new
// prompt either way, never "whenever it happens to be evicted".
func TestAnEditRecyclesAnIdleAgentAndMarksABusyOne(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec := custom(t, sup, "x", nil)
	if _, err := sup.Get(rec.ID); err != nil {
		t.Fatal(err)
	}
	role := "y"
	if _, err := sup.Update(rec.ID, agentapi.AgentPatch{Instructions: &role}); err != nil {
		t.Fatal(err)
	}
	if sup.LiveCount() != 0 {
		t.Fatal("an idle agent was not recycled by its edit")
	}
	a, _ := sup.Get(rec.ID)
	a.mu.Lock()
	a.state = "working"
	a.mu.Unlock()
	if _, err := sup.Update(rec.ID, agentapi.AgentPatch{Instructions: &role}); err != nil {
		t.Fatal(err)
	}
	if !a.reload.take() {
		t.Error("a busy agent was not marked to recycle at the end of its turn")
	}
	if sup.LiveCount() != 1 {
		t.Error("a busy agent was stopped mid-turn")
	}
	a.mu.Lock()
	a.state = "idle"
	a.mu.Unlock()
}

// Agents hire from the gallery; a custom agent is something only the person
// writes. Offering the shell as a type would make a nameless-role agent.
func TestAgentsCannotHireACustomAgent(t *testing.T) {
	sup := supervisorWith(t, 8)
	for _, p := range hireable(sup.Catalog()) {
		if p.Key == agentapi.CustomType {
			t.Fatal("the custom shell is offered as something to hire")
		}
	}
	if got := hire(sup, createAgentInput{Type: agentapi.CustomType, Name: "Maya"}); !strings.Contains(got, "made by the person") {
		t.Errorf("hire answered %q", got)
	}
	if _, ok := sup.Roster().Get("maya"); ok {
		t.Fatal("an agent hired a custom agent")
	}
}

// A thinking budget has to fit under max_tokens, so turning thinking on raises
// the ceiling by exactly the budget; reasoning between tool calls needs a beta
// of its own, which only Anthropic understands.
func TestThinkingRaisesTheCeilingAndAsksForTheBeta(t *testing.T) {
	a := &Agent{system: "p", ep: endpoint{model: "m", thinking: "medium"}}
	p := a.params(nil).BetaMessageNewParams
	if p.MaxTokens != maxTokens+8192 || p.Thinking.OfEnabled == nil || p.Thinking.OfEnabled.BudgetTokens != 8192 {
		t.Fatalf("thinking request: max_tokens %d thinking %+v", p.MaxTokens, p.Thinking)
	}
	if !slices.Contains(p.Betas, anthropic.AnthropicBetaInterleavedThinking2025_05_14) {
		t.Error("interleaved thinking was not asked for")
	}
	plain := (&Agent{system: "p", ep: endpoint{model: "m"}}).
		params(nil).BetaMessageNewParams
	if plain.MaxTokens != maxTokens || plain.Thinking.OfEnabled != nil ||
		slices.Contains(plain.Betas, anthropic.AnthropicBetaInterleavedThinking2025_05_14) {
		t.Errorf("an agent that does not think got %d tokens, %+v, %v", plain.MaxTokens, plain.Thinking, plain.Betas)
	}
}

// A custom agent's own model is now a first-class citizen rather than a poor
// relation, and this is the whole point of routing it through the broker.
//
// It used to get a client of its own pointed at a URL the person pasted, which
// meant no betas, no context management -- so its history was never trimmed and
// every tool result in it was re-billed uncached on every turn -- and compaction
// ran on its own model, because the cheap one might not exist wherever they had
// pointed it. All three were the price of not knowing where the request went.
// We know now: it goes where every other request goes.
func TestACustomAgentsModelGetsTheSameTreatmentAsAnyOther(t *testing.T) {
	base := endpoint{baseURL: "http://172.16.0.1:8092", key: brokerKey}
	own := &agentapi.ModelConfig{Model: "openai/gpt-4o", Thinking: "low"}
	ep := base.forAgent("anthropic/claude-sonnet-5", own)

	if ep.model != "openai/gpt-4o" || ep.thinking != "low" {
		t.Fatalf("the person's choice did not take: %+v", ep)
	}
	if ep.baseURL != base.baseURL || ep.key != brokerKey {
		t.Errorf("a custom agent left the broker: %+v", ep)
	}
	p := (&Agent{system: "p", ep: ep}).params(nil).BetaMessageNewParams
	if !slices.Contains(p.Betas, anthropic.AnthropicBetaContextManagement2025_06_27) {
		t.Errorf("custom betas = %v, want the context-management beta", p.Betas)
	}
	if p.ContextManagement.Edits == nil {
		t.Error("a custom agent's history is never trimmed; every tool result is re-billed")
	}
	// And it condenses on the cheap model rather than on gpt-4o, which is the
	// bill this used to quietly run up on the longest conversations.
	if got := (&Agent{ep: ep}).compactModel(); string(got) != "anthropic/claude-haiku-4.5" {
		t.Errorf("custom compaction uses %q, want the cheap model", got)
	}
}

// The per-agent view is named from the roster and ordered the way the roster
// is: live agents first, then agents that were retired, by id, then what was
// spent before agents were told apart.
func TestAgentUsageIsNamedAndOrderedByTheRoster(t *testing.T) {
	sup := supervisorWith(t, 8)
	maya := custom(t, sup, "x", ownModel())
	m := sup.Meter()
	m.Record(agentapi.UnattributedAgent, "claude-sonnet-5", sonnet(4, 4))
	m.Record("gone", "claude-sonnet-5", sonnet(1, 1))
	m.Record(maya.ID, "claude-sonnet-5", sonnet(2, 2))
	m.Record(BossID, "claude-sonnet-5", sonnet(3, 3))
	got := sup.AgentUsage().Agents
	var ids []string
	for _, a := range got {
		ids = append(ids, a.Agent)
	}
	if want := []string{BossID, maya.ID, "gone", agentapi.UnattributedAgent}; !slices.Equal(ids, want) {
		t.Fatalf("order %v, want %v", ids, want)
	}
	if got[0].Name != "Boss" || got[1].Name != "Maya" {
		t.Errorf("live rows: %+v %+v", got[0], got[1])
	}
	if !got[2].Retired || got[2].Name != "gone" || got[3].Retired || got[3].Name != "Before per-agent tracking" {
		t.Errorf("retired and pre-split rows: %+v %+v", got[2], got[3])
	}
}

// The counterpart, and the one this cutover could most easily have broken.
//
// The broker's upstream is OpenRouter now, so the old rule -- "betas go only to
// Anthropic" -- would have switched context editing off for the whole fleet.
// Nothing would have failed: turns would keep working and every tool result in
// a long history would quietly be re-billed uncached on every turn. OpenRouter
// documents context_management as a request field, so the fleet keeps it, and
// this test is what says so out loud.
func TestABrokeredTurnKeepsItsBetasAndContextManagement(t *testing.T) {
	a := &Agent{system: "p", ep: endpoint{
		baseURL: "http://172.16.0.1:8092", key: brokerKey,
		model: "anthropic/claude-sonnet-5"}}
	p := a.params(nil).BetaMessageNewParams
	if !slices.Contains(p.Betas, anthropic.AnthropicBetaContextManagement2025_06_27) {
		t.Errorf("brokered betas = %v, want the context-management beta", p.Betas)
	}
	if p.ContextManagement.Edits == nil {
		t.Error("the brokered path sends no context management; every tool result is re-billed")
	}
}
