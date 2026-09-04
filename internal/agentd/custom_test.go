package agentd

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
)

// ownModel is a model the person brought themselves, key included.
func ownModel() *agentapi.ModelConfig {
	return &agentapi.ModelConfig{URL: "https://models.example.com", APIKey: "sk-own-secret",
		Model: "their-model", Thinking: "high"}
}

// custom builds one custom agent on a supervisor.
func custom(t *testing.T, sup *Supervisor, role string, m *agentapi.ModelConfig) Record {
	t.Helper()
	rec, err := sup.CreateWith(agentapi.CreateAgentReq{Type: CustomType, Name: "Maya",
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
func TestACustomAgentKeepsItsRoleAndModelAndHidesTheKey(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec := custom(t, sup, "Plan trips. Be brief.", ownModel())
	again, err := LoadRoster(sup.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := again.Get(rec.ID)
	if !ok || got.Instructions != "Plan trips. Be brief." || got.Model == nil || got.Model.APIKey != "sk-own-secret" {
		t.Fatalf("reloaded record: %+v", got)
	}
	wire, _ := json.Marshal(sup.List())
	if strings.Contains(string(wire), "sk-own-secret") {
		t.Fatalf("the key reached the roster listing: %s", wire)
	}
	if !strings.Contains(string(wire), `"key_set":true`) || !strings.Contains(string(wire), "Plan trips") {
		t.Errorf("the listing lost the role or the key flag: %s", wire)
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

// A model needs everything the machine will dial with, and a thinking level the
// loop knows. Half a model would fail on the agent's first turn, which is the
// worst place to find out.
func TestACustomModelIsCheckedBeforeItIsStored(t *testing.T) {
	sup := supervisorWith(t, 8)
	for _, bad := range []*agentapi.ModelConfig{
		{URL: "http://models.example.com", APIKey: "k", Model: "m"}, // plaintext to the internet
		{URL: "https://models.example.com", Model: "m"},             // no key
		{URL: "https://models.example.com", APIKey: "k"},            // no model
		{URL: "https://models.example.com", APIKey: "k", Model: "m", Thinking: "max"},
	} {
		req := agentapi.CreateAgentReq{Type: CustomType, Name: "Maya", Instructions: "x", Model: bad}
		if _, err := sup.CreateWith(req); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// Only a custom agent's role and model can change: the gallery profiles are the
// product. Anyone may be renamed.
func TestOnlyACustomAgentsRoleAndModelCanChange(t *testing.T) {
	sup := supervisorWith(t, 8)
	ada, _ := sup.Create("coder", "Ada")
	role := "Write Rust."
	if _, err := sup.Update(ada.ID, agentapi.AgentPatch{Instructions: &role}); err == nil {
		t.Error("a coder's role was rewritten")
	}
	name := "Ada L"
	if got, err := sup.Update(ada.ID, agentapi.AgentPatch{Name: &name}); err != nil || got.Name != "Ada L" {
		t.Errorf("rename: %+v, %v", got, err)
	}
}

// The app never sees the key, so an edit that leaves it out keeps the stored
// one; and an edit can hand the model back to the default.
func TestAPatchKeepsTheStoredKeyAndCanClearTheModel(t *testing.T) {
	sup := supervisorWith(t, 8)
	rec := custom(t, sup, "x", ownModel())
	swap := &agentapi.ModelPatch{ModelConfig: agentapi.ModelConfig{URL: "https://other.example.com", Model: "m2"}}
	got, err := sup.Update(rec.ID, agentapi.AgentPatch{Model: swap})
	if err != nil || got.Model.APIKey != "sk-own-secret" || got.Model.Model != "m2" {
		t.Fatalf("patch without a key: %+v, %v", got.Model, err)
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
		if p.Key == CustomType {
			t.Fatal("the custom shell is offered as something to hire")
		}
	}
	if got := hire(sup, createAgentInput{Type: CustomType, Name: "Maya"}); !strings.Contains(got, "made by the person") {
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
	a := &Agent{system: "p", ep: endpoint{model: "m", thinking: "medium", anthropic: true}}
	p := a.params(nil).BetaMessageNewParams
	if p.MaxTokens != maxTokens+8192 || p.Thinking.OfEnabled == nil || p.Thinking.OfEnabled.BudgetTokens != 8192 {
		t.Fatalf("thinking request: max_tokens %d thinking %+v", p.MaxTokens, p.Thinking)
	}
	if !slices.Contains(p.Betas, anthropic.AnthropicBetaInterleavedThinking2025_05_14) {
		t.Error("interleaved thinking was not asked for")
	}
	plain := (&Agent{system: "p", ep: endpoint{model: "m", anthropic: true}}).params(nil).BetaMessageNewParams
	if plain.MaxTokens != maxTokens || plain.Thinking.OfEnabled != nil ||
		slices.Contains(plain.Betas, anthropic.AnthropicBetaInterleavedThinking2025_05_14) {
		t.Errorf("an agent that does not think got %d tokens, %+v, %v", plain.MaxTokens, plain.Thinking, plain.Betas)
	}
}

// An endpoint of the person's own speaks the API but not Anthropic's betas: it
// gets plain requests, and its compaction runs on its own model, since the
// cheap one may not exist there.
func TestAForeignEndpointGetsPlainRequests(t *testing.T) {
	a := &Agent{system: "p", ep: endpoint{baseURL: "https://models.example.com", model: "m", thinking: "low"}}
	p := a.params(nil).BetaMessageNewParams
	if len(p.Betas) != 0 || p.ContextManagement.Edits != nil {
		t.Errorf("a foreign endpoint was sent betas %v and context management %+v", p.Betas, p.ContextManagement)
	}
	if p.Thinking.OfEnabled == nil {
		t.Error("thinking is not a beta and should still be asked for")
	}
	if a.summaryModel() != "m" {
		t.Errorf("compaction on a foreign endpoint uses %q", a.summaryModel())
	}
	if (&Agent{ep: endpoint{anthropic: true}}).summaryModel() != summaryModel {
		t.Error("compaction on Anthropic left the cheap model")
	}
}
