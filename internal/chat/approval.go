package chat

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
)

// Verdicts the client may send. Nothing else is accepted, so a typo cannot be
// read as consent.
const (
	verdictApproved = "approved"
	verdictDenied   = "denied"
)

// declined is the reason a refusal carries into the agent's transcript.
const declined = "the person declined"

// approvalReq is what the client sends. Answer is only meaningful for a text or
// choice ask, and is refused everywhere else.
type approvalReq struct {
	Verdict string `json:"verdict"`
	Answer  string `json:"answer"`
}

// resolveApproval delivers a person's decision to the agent waiting on it.
//
// messageId IS the guest's event id, and that event carries the approval id --
// so there is no mapping table to keep, and nothing to lose across a restart.
func (s *Server) resolveApproval(w http.ResponseWriter, r *http.Request, user string) {
	var req approvalReq
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req) != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	cl, ok := s.guestOf(w, r, user)
	if !ok {
		return
	}
	s.deliver(w, cl, r.PathValue("id"), r.PathValue("messageId"), req)
}

// deliver finds the ask, authors the body, and sends it.
func (s *Server) deliver(w http.ResponseWriter, cl *agent.Client,
	agentID, messageID string, req approvalReq) {
	ask, ok := findAsk(cl, agentID, messageID)
	if !ok {
		fail(w, http.StatusNotFound, "no such ask")
		return
	}
	body, ok := decisionBody(askUIOf(ask), req)
	if !ok {
		fail(w, http.StatusBadRequest, "that answer does not fit this ask")
		return
	}
	forwardDecision(w, cl, ask.ApprovalID, body)
}

// forwardDecision sends the authored body and maps the guest's answer.
func forwardDecision(w http.ResponseWriter, cl *agent.Client, apid string, body map[string]any) {
	err := cl.Resolve(apid, body)
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// The guest 404s an interaction that is already settled -- answered on
	// another device, timed out, or revoked by an interrupt. That is a stale
	// card, not a wrong route, so the client is told 409 and can re-fetch.
	if isNotFound(err) {
		fail(w, http.StatusConflict, "already resolved")
		return
	}
	fail(w, http.StatusBadGateway, err.Error())
}

// findAsk locates the ask a message id names, in that agent's own log.
//
// Scoped to the agent in the path, so a message id from one conversation can
// never settle an ask in another.
func findAsk(cl *agent.Client, agentID, messageID string) (agentapi.Event, bool) {
	want, err := strconv.Atoi(messageID)
	if err != nil {
		return agentapi.Event{}, false
	}
	events, _, err := cl.EventsSince(agentID, 0)
	if err != nil {
		return agentapi.Event{}, false
	}
	for _, ev := range events {
		if ev.ID == want && (ev.Type == "approval_required" || ev.Type == "question") {
			return ev, ev.ApprovalID != ""
		}
	}
	return agentapi.Event{}, false
}

// decisionBody turns a verdict into the body the guest receives. The client
// never authors this, and that is the point.
//
// agentd reads ANY non-empty answer as consent, so a client-supplied string
// arriving at a tool gate would approve a shell command with nobody having
// pressed anything. Free text is therefore confined to the two kinds that ask
// for it, and refused -- not ignored -- everywhere else.
func decisionBody(ui *AskUI, req approvalReq) (map[string]any, bool) {
	if req.Verdict != verdictApproved && req.Verdict != verdictDenied {
		return nil, false
	}
	wantsText := ui.Kind == askText || ui.Kind == askChoice
	if req.Answer != "" && !wantsText {
		return nil, false // a client bug whose failure mode is approving a command
	}
	if req.Verdict == verdictDenied {
		return denyBody(ui), true
	}
	return allowBody(ui, req.Answer)
}

// denyBody is how a refusal reaches the agent for each kind.
func denyBody(ui *AskUI) map[string]any {
	switch ui.Kind {
	case askConfirm:
		return map[string]any{"answer": "no"}
	case askHandoff, askConnect:
		return map[string]any{"answer": "not now"}
	}
	return map[string]any{"decision": "deny", "reason": declined}
}

// allowBody is how consent reaches the agent, and where an answer is validated.
func allowBody(ui *AskUI, answer string) (map[string]any, bool) {
	switch ui.Kind {
	case askApproval:
		return map[string]any{"decision": "allow"}, true
	case askConfirm:
		return map[string]any{"answer": "yes"}, true
	case askHandoff, askConnect:
		// Deliberately not in wantsText above. The guest reads ANY non-empty
		// answer as consent, so a connect card that accepted free text would let
		// a client approve one by typing into it.
		return map[string]any{"answer": "done"}, true
	case askText:
		// An empty answer reads as a refusal to the guest, so it would tell the
		// agent "no" on behalf of someone who meant to say something.
		return map[string]any{"answer": answer}, answer != ""
	case askChoice:
		// Only an answer the person was actually offered.
		return map[string]any{"answer": answer}, slices.Contains(ui.Options, answer)
	}
	return nil, false
}
