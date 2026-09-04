package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"cracked/internal/agentapi"
)

// handlePending lists every agent currently waiting on a person.
//
// Machine-wide rather than per-agent, because a person is answering the team,
// not polling each specialist in turn to find out which one has its hand up.
func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.sup.Interactions().List())
}

// handlePendingEvents streams raised hands as they happen.
//
// The current set is sent first, so a client that connects mid-wait sees what
// is already outstanding rather than only what happens next. Pending state is
// live, not a log, so there is no resume cursor: reconnecting re-reads the set
// and is correct again, which is what makes dropping a frame to a slow reader
// safe here.
func (s *Server) handlePendingEvents(w http.ResponseWriter, r *http.Request) {
	ch, unsubscribe := s.sup.Interactions().Subscribe()
	defer unsubscribe()
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{})
	writeSSEHeaders(w)
	for _, raised := range s.sup.Interactions().List() {
		emitChange(w, agentapi.PendingChange{Raised: &raised})
	}
	rc.Flush()
	pump(r, w, rc, ch, func(c agentapi.PendingChange) { emitChange(w, c) })
}

// emitChange writes one hub change as an SSE frame.
func emitChange(w http.ResponseWriter, c agentapi.PendingChange) {
	buf, err := json.Marshal(c)
	if err != nil {
		return
	}
	event := "raised"
	if c.ClearedID != "" {
		event = "cleared"
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, buf)
}

// handleResolve delivers a person's answer to whichever agent raised it.
//
// Not scoped to an agent in the path: the id already names one, so there is no
// way for a client to send an answer to the wrong agent, and no way for a
// worker's approval to be routed through the boss.
//
// A 404 means the interaction already settled -- answered elsewhere, timed out,
// or revoked by an interrupt. That is a normal race with a second client rather
// than a client error, so it says so.
func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var d Decision
	if json.NewDecoder(r.Body).Decode(&d) != nil {
		fail(w, http.StatusBadRequest, "bad_request", "could not read the decision", "")
		return
	}
	d = normalise(d)
	id := r.PathValue("apid")
	if !s.sup.ResolveApproval(id, d) {
		fail(w, http.StatusNotFound, "not_found", "no pending decision", "approval")
		return
	}
	reply(w, http.StatusOK, map[string]any{"approval_id": id, "decision": d.Decision})
}
