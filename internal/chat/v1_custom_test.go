package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// mayaBody is a custom agent as the app sends one, model and key included.
const mayaBody = `{"name":"Maya","instructions":"Plan my trips. Keep it brief.",` +
	`"model":{"url":"https://models.example.com","apiKey":"sk-own-secret","model":"their-model","thinking":"high"}}`

// Building an agent: the gateway forwards what the person wrote to their
// machine, key included -- and what it sends back to the app has the role, a
// key-set flag, and no key anywhere in it, on this reply or the next roster.
func TestACustomAgentIsBuiltFromWhatThePersonWrote(t *testing.T) {
	s, g, u := newFake(t)
	w := call(t, s, u, "POST", "/v1/agents", mayaBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if g.created.Type != "custom" || g.created.Model == nil || g.created.Model.APIKey != "sk-own-secret" ||
		g.created.Model.Thinking != "high" {
		t.Fatalf("the machine received %+v", g.created)
	}
	var made Agent
	json.Unmarshal(w.Body.Bytes(), &made)
	if !made.Custom || made.Role != "Plan my trips" || made.Model == nil || !made.Model.KeySet ||
		made.Instructions == "" || made.ID != "maya" {
		t.Fatalf("the app was told %+v", made)
	}
	roster := call(t, s, u, "GET", "/v1/agents", "").Body.String()
	for _, body := range []string{w.Body.String(), roster} {
		if strings.Contains(body, "sk-own-secret") {
			t.Fatalf("the key reached the app: %s", body)
		}
	}
	if !strings.Contains(roster, `"custom":true`) {
		t.Errorf("the roster does not mark the agent as custom: %s", roster)
	}
}

// A name and a role are the whole definition; a model is optional but must be
// whole when given. And the shell itself is not something to pick from the
// gallery.
func TestACustomAgentNeedsANameARoleAndAWholeModel(t *testing.T) {
	s, _, u := newFake(t)
	for _, body := range []string{
		`{"name":"","instructions":"x"}`,
		`{"name":"Maya","instructions":"  "}`,
		`{"name":"Maya","instructions":"x","model":{"url":"https://m.example.com","model":"m"}}`,
		`{"name":"Maya","instructions":"x","model":{"url":"https://m.example.com","apiKey":"k","model":"m","thinking":"max"}}`,
		`{"templateId":"custom"}`,
	} {
		if w := call(t, s, u, "POST", "/v1/agents", body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d", body, w.Code)
		}
	}
}

// The gallery is for picking; the shell a person's own role goes into is not a
// card, any more than the boss is.
func TestTheGalleryHidesTheCustomShell(t *testing.T) {
	s, _, u := newFake(t)
	var got []Template
	json.Unmarshal(call(t, s, u, "GET", "/v1/agent-types", "").Body.Bytes(), &got)
	for _, tpl := range got {
		if tpl.ID == "custom" {
			t.Fatal("the custom shell is offered as a template")
		}
	}
}

// Editing: only a custom agent's role or model changes; a model sent without a
// key keeps the one the machine holds; {"clear": true} goes back to the
// default model; a gallery agent's role is not the person's to edit.
func TestEditingACustomAgent(t *testing.T) {
	s, g, u := newFake(t)
	call(t, s, u, "POST", "/v1/agents", mayaBody)
	w := call(t, s, u, "PATCH", "/v1/agents/maya",
		`{"instructions":"Plan trips and hotels.","model":{"url":"https://m.example.com","model":"m2"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if g.patched.Model == nil || g.patched.Model.APIKey != "" || g.patched.Model.Model != "m2" {
		t.Fatalf("the machine received %+v", g.patched.Model)
	}
	var got Agent
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Instructions != "Plan trips and hotels." || got.Role != "Plan trips and hotels" {
		t.Errorf("edited row %+v", got)
	}
	w = call(t, s, u, "PATCH", "/v1/agents/maya", `{"model":{"clear":true}}`)
	var cleared Agent
	json.Unmarshal(w.Body.Bytes(), &cleared)
	if w.Code != http.StatusOK || cleared.Model != nil {
		t.Errorf("clear: status %d model %+v", w.Code, cleared.Model)
	}
	if w := call(t, s, u, "PATCH", "/v1/agents/boss", `{"instructions":"x"}`); w.Code != http.StatusConflict {
		t.Errorf("the boss's role was edited: %d", w.Code)
	}
	if w := call(t, s, u, "PATCH", "/v1/agents/nobody", `{"name":"x"}`); w.Code != http.StatusNotFound {
		t.Errorf("a missing agent answered %d", w.Code)
	}
}
