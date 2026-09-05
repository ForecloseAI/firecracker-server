package chat

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// mayaBody is a custom agent as the app sends one: a model id and how hard it
// should think, and nothing else to say about where the model lives.
const mayaBody = `{"name":"Maya","instructions":"Plan my trips. Keep it brief.",` +
	`"model":{"model":"openai/gpt-4o","thinking":"high"}}`

// legacyBody is what an app built before the broker sends: a model of the
// person's own, with an endpoint and a key. Both are gone from the wire type,
// so they are dropped rather than stored -- which is what makes an old client
// safe to leave installed rather than something that has to be forced upgraded.
const legacyBody = `{"name":"Maya","instructions":"Plan my trips. Keep it brief.",` +
	`"model":{"url":"https://models.example.com","api_key":"sk-own-secret",` +
	`"model":"openai/gpt-4o","thinking":"high"}}`

// Building an agent: the gateway forwards the model the person picked to their
// machine, and what comes back names it.
func TestACustomAgentIsBuiltFromWhatThePersonWrote(t *testing.T) {
	s, g, u := newFake(t)
	w := call(t, s, u, "POST", "/v1/agents", mayaBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if g.created.Type != "custom" || g.created.Model == nil ||
		g.created.Model.Model != "openai/gpt-4o" || g.created.Model.Thinking != "high" {
		t.Fatalf("the machine received %+v", g.created)
	}
	var made Agent
	json.Unmarshal(w.Body.Bytes(), &made)
	if !made.Custom || made.Role != "Plan my trips" || made.Model == nil ||
		made.Model.Model != "openai/gpt-4o" || made.Instructions == "" || made.ID != "maya" {
		t.Fatalf("the app was told %+v", made)
	}
	if roster := call(t, s, u, "GET", "/v1/agents", "").Body.String(); !strings.Contains(roster, `"custom":true`) {
		t.Errorf("the roster does not mark the agent as custom: %s", roster)
	}
}

// An app from before the broker still sends an endpoint and a key. Neither is
// on the wire type any anymore, so both are dropped on the way through -- the
// agent is built on the model id and the credential is not stored, forwarded or
// echoed. An old client gets the new behaviour rather than an error.
func TestAnOldClientsEndpointAndKeyAreDropped(t *testing.T) {
	s, g, u := newFake(t)
	w := call(t, s, u, "POST", "/v1/agents", legacyBody)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if g.created.Model == nil || g.created.Model.Model != "openai/gpt-4o" {
		t.Fatalf("the model choice did not survive: %+v", g.created.Model)
	}
	roster := call(t, s, u, "GET", "/v1/agents", "").Body.String()
	for _, body := range []string{w.Body.String(), roster} {
		if strings.Contains(body, "sk-own-secret") || strings.Contains(body, "models.example.com") {
			t.Fatalf("an old client's credential came back: %s", body)
		}
	}
}

// The machine is the one authority on what a custom agent may be, and its
// refusal reaches the app as it was said -- code and message -- rather than
// as a bad gateway. The shell itself is not something to pick from the
// gallery either.
func TestTheMachinesRefusalReachesTheAppAsItWasSaid(t *testing.T) {
	s, g, u := newFake(t)
	g.createStatus, g.createMessage = http.StatusBadRequest, "instructions: must be 1 to 8000 characters"
	w := call(t, s, u, "POST", "/v1/agents", `{"name":"Maya","instructions":""}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "1 to 8000") {
		t.Errorf("status %d body %s", w.Code, w.Body)
	}
	if w := call(t, s, u, "POST", "/v1/agents", `{"templateId":"custom"}`); w.Code != http.StatusBadRequest {
		t.Errorf("the shell was offered as a template: %d", w.Code)
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

// Editing: only a custom agent's role or model changes; {"clear": true} goes
// back to the default model; a gallery agent's role is not the person's to edit.
func TestEditingACustomAgent(t *testing.T) {
	s, g, u := newFake(t)
	call(t, s, u, "POST", "/v1/agents", mayaBody)
	w := call(t, s, u, "PATCH", "/v1/agents/maya",
		`{"instructions":"Plan trips and hotels.","model":{"model":"google/gemini-2.5-flash"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	if g.patched.Model == nil || g.patched.Model.Model != "google/gemini-2.5-flash" {
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
