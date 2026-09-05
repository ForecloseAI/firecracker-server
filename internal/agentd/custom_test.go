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

// ownModel is a model the person brought themselves, key included.
func ownModel() *agentapi.ModelConfig {
	return &agentapi.ModelConfig{URL: "https://models.example.com", APIKey: "sk-own-secret",
		Model: "their-model", Thinking: "high"}
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

// The machine is the one place a custom agent is checked -- the host relays
// its refusal -- so every rule lives here: a name that fits a row, a role that
// exists and fits a page or two, and a model that is whole. Half a model would
// fail on the agent's first turn, which is the worst place to find out.
func TestACustomAgentIsCheckedBeforeItIsStored(t *testing.T) {
	sup := supervisorWith(t, 8)
	for _, bad := range []*agentapi.ModelConfig{
		{URL: "http://models.example.com", APIKey: "k", Model: "m"}, // plaintext to the internet
		{URL: "https://models.example.com", Model: "m"},             // no key
		{URL: "https://models.example.com", APIKey: "k"},            // no model
		{URL: "https://models.example.com", APIKey: "k", Model: "m", Thinking: "max"},
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
	plain := (&Agent{system: "p", ep: endpoint{model: "m"}}).params(nil).BetaMessageNewParams
	if plain.MaxTokens != maxTokens || plain.Thinking.OfEnabled != nil ||
		slices.Contains(plain.Betas, anthropic.AnthropicBetaInterleavedThinking2025_05_14) {
		t.Errorf("an agent that does not think got %d tokens, %+v, %v", plain.MaxTokens, plain.Thinking, plain.Betas)
	}
}

// An endpoint of the person's own speaks the API but not Anthropic's betas: it
// gets plain requests, and its compaction runs on its own model, since the
// cheap one may not exist there.
func TestAPastedEndpointGetsPlainRequests(t *testing.T) {
	a := &Agent{system: "p", ep: endpoint{baseURL: "https://models.example.com", model: "m", thinking: "low", plain: true, bearer: true}}
	p := a.params(nil).BetaMessageNewParams
	if len(p.Betas) != 0 || p.ContextManagement.Edits != nil {
		t.Errorf("a pasted endpoint was sent betas %v and context management %+v", p.Betas, p.ContextManagement)
	}
	if p.Thinking.OfEnabled == nil {
		t.Error("thinking is not a beta and should still be asked for")
	}
	if a.compactModel() != "m" {
		t.Errorf("compaction on a pasted endpoint uses %q", a.compactModel())
	}
}

// The assertion that used to live above this was `(&Agent{}).compactModel()`,
// which builds an endpoint with no host at all: it proved the default branch was
// taken and nothing about which id that branch names. That is precisely how a
// rename of the summariser to an OpenRouter slug passed the whole suite while
// pointing a bring-your-own-Anthropic agent at a model Anthropic does not have.
//
// Compaction is the call nobody watches -- a bad id there does not fail a turn,
// it logs inside compactIfNeeded and returns, so the conversation quietly stops
// being trimmed -- so the id each endpoint actually sends is worth pinning.
func TestEachEndpointCompactsWithAnIDItsOwnHostKnows(t *testing.T) {
	// The ids are written out rather than referred to by constant. Asserting
	// against the same constant the code reads moves both sides together, which
	// is the flaw that let the original bug through -- verified by swapping the
	// constants and watching a symbolic version of this test still pass. These
	// two literals were confirmed against their own hosts on 2026-09-06.
	base := endpoint{baseURL: "http://172.16.0.1:8092", key: brokerKey, summary: summaryOpenRouter}
	if got := (&Agent{ep: base}).compactModel(); string(got) != "anthropic/claude-haiku-4.5" {
		t.Errorf("brokered compaction uses %q, want anthropic/claude-haiku-4.5", got)
	}
	own := &agentapi.ModelConfig{URL: "https://api.anthropic.com", APIKey: "k", Model: "m"}
	if got := (&Agent{ep: base.forAgent("x", own)}).compactModel(); string(got) != "claude-haiku-4-5" {
		t.Errorf("Anthropic compaction uses %q, want its own unprefixed claude-haiku-4-5", got)
	}
	pasted := &agentapi.ModelConfig{URL: "https://models.example.com", APIKey: "k", Model: "m"}
	if got := (&Agent{ep: base.forAgent("x", pasted)}).compactModel(); string(got) != "m" {
		t.Errorf("an unknown endpoint compacts with %q, want the agent's own model", got)
	}
}

// The per-agent view is named from the roster and ordered the way the roster
// is: live agents first, then agents that were retired, by id, then what was
// spent before agents were told apart. A custom agent on its own model is
// marked, so the host can say its price is an estimate rather than the bill.
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
	if got[0].Name != "Boss" || got[1].Name != "Maya" || !got[1].OwnKey || got[0].OwnKey {
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
		model: "anthropic/claude-sonnet-5", summary: summaryOpenRouter}}
	p := a.params(nil).BetaMessageNewParams
	if !slices.Contains(p.Betas, anthropic.AnthropicBetaContextManagement2025_06_27) {
		t.Errorf("brokered betas = %v, want the context-management beta", p.Betas)
	}
	if p.ContextManagement.Edits == nil {
		t.Error("the brokered path sends no context management; every tool result is re-billed")
	}
	if string(a.compactModel()) != summaryOpenRouter {
		t.Errorf("brokered compaction uses %q, want the cheap model", a.compactModel())
	}
}
