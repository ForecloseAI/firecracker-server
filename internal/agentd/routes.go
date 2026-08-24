package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Routes builds the handler. Method+wildcard patterns, like internal/api.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /agents", s.handleList)
	mux.HandleFunc("POST /agents/{id}/messages", s.withAgent(s.handleMessage))
	mux.HandleFunc("POST /agents/{id}/interrupt", s.withAgent(s.handleInterrupt))
	mux.HandleFunc("POST /agents/{id}/approvals/{apid}", s.withAgent(s.handleApproval))
	mux.HandleFunc("GET /agents/{id}/events", s.withAgent(s.handleEvents))
	mux.HandleFunc("GET /debug/memstats", s.handleMemstats)
	return mux
}

// agentHandler is a handler that has already had its agent resolved.
type agentHandler func(http.ResponseWriter, *http.Request, *Agent)

// withAgent resolves {id} to an agent, 404ing when there is no such one.
func (s *Server) withAgent(next agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a, ok := s.agents[id]
		if !ok {
			fail(w, http.StatusNotFound, "not_found", "no agent "+id, "agent")
			return
		}
		next(w, r, a)
	}
}

// view is one agent's row in GET /agents: everything a polling client needs in
// a single call. The task title arrives in Phase 6 with the roster.
type view struct {
	ID           string `json:"id"`
	State        string `json:"state"`
	LastEventID  int    `json:"last_event_id"`
	Conversation int    `json:"conversation_bytes"`
}

// handleList reports every agent and what it is doing. This is the poll.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	out := make([]view, 0, len(s.agents))
	for _, a := range s.agents {
		out = append(out, view{ID: a.ID(), State: a.State(),
			LastEventID: a.Log().LastID(), Conversation: a.ConversationBytes()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	reply(w, http.StatusOK, out)
}

// handleHealth is what the control plane's boot probe will read.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	working := 0
	for _, a := range s.agents {
		if a.State() == "working" {
			working++
		}
	}
	reply(w, http.StatusOK, map[string]any{
		"ok": true, "ready": true, "agents": len(s.agents), "working": working,
	})
}

// sendReq is the body of POST /agents/{id}/messages.
type sendReq struct {
	Text string `json:"text"`
}

// handleMessage queues a user turn and returns immediately. The turn itself can
// take minutes, so the HTTP request must not wait on it.
func (s *Server) handleMessage(w http.ResponseWriter, r *http.Request, a *Agent) {
	var req sendReq
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Text == "" {
		fail(w, http.StatusBadRequest, "bad_request", "text is required", "")
		return
	}
	id, replayed := s.nextMessageID(r.Header.Get("Idempotency-Key"))
	if replayed {
		reply(w, http.StatusOK, map[string]any{"message_id": id, "replayed": true})
		return
	}
	if err := a.Send(req.Text); err != nil {
		s.sendFailed(w, err)
		return
	}
	reply(w, http.StatusAccepted, map[string]any{
		"message_id": id, "session_state": a.State(), "last_event_id": a.Log().LastID(),
	})
}

// sendFailed maps a queueing failure onto a status.
func (s *Server) sendFailed(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrBusy) {
		w.Header().Set("Retry-After", "5")
		fail(w, http.StatusServiceUnavailable, "busy", err.Error(), "agent")
		return
	}
	fail(w, http.StatusInternalServerError, "internal", err.Error(), "")
}

// handleApproval delivers a human's answer to a waiting tool call.
//
// A 404 here means the interaction already settled -- answered by someone else,
// timed out, or revoked by an interrupt. That is a normal race with a second
// client, not a client error, so it says so rather than failing silently.
func (s *Server) handleApproval(w http.ResponseWriter, r *http.Request, a *Agent) {
	var d Decision
	if json.NewDecoder(r.Body).Decode(&d) != nil {
		fail(w, http.StatusBadRequest, "bad_request", "could not read the decision", "")
		return
	}
	id := r.PathValue("apid")
	if !a.Gate().Resolve(id, normalise(d)) {
		fail(w, http.StatusNotFound, "not_found", "no pending decision", "approval")
		return
	}
	reply(w, http.StatusOK, map[string]any{"approval_id": id, "decision": normalise(d).Decision})
}

// normalise fills in the decision a bare answer implies, so a client answering
// a question does not have to also say "allow".
func normalise(d Decision) Decision {
	if d.Decision == "" {
		d.Decision = "deny"
		if d.Answer != "" {
			d.Decision = "allow"
		}
	}
	return d
}

// handleInterrupt stops the turn in flight, if there is one.
func (s *Server) handleInterrupt(w http.ResponseWriter, r *http.Request, a *Agent) {
	reply(w, http.StatusOK, map[string]any{
		"interrupted": a.Interrupt(), "session_state": a.State(),
	})
}

// handleEvents streams the agent's log as SSE, or returns a snapshot when
// ?poll= is set, for a client that cannot hold a connection open.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, a *Agent) {
	since := watermark(r)
	if r.URL.Query().Has("poll") {
		events, err := a.Log().Since(since)
		if err != nil {
			fail(w, http.StatusInternalServerError, "internal", err.Error(), "")
			return
		}
		reply(w, http.StatusOK, map[string]any{
			"events": events, "last_event_id": a.Log().LastID(),
		})
		return
	}
	s.stream(w, r, a, since)
}

// watermark reads the resume point from the header EventSource sends on its own
// reconnect, falling back to the query parameter.
func watermark(r *http.Request) int {
	for _, raw := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("since")} {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			return n
		}
	}
	return 0
}

// stream replays what the client missed, then forwards live events.
//
// Subscribing BEFORE the replay is what closes the gap: an event appended
// between the two would otherwise reach neither. That means the replay and the
// live feed overlap, so anything not newer than the high-water mark is dropped.
func (s *Server) stream(w http.ResponseWriter, r *http.Request, a *Agent, since int) {
	ch, unsubscribe := a.Log().Subscribe()
	defer unsubscribe()
	rc := http.NewResponseController(w)
	rc.SetWriteDeadline(time.Time{}) // an absolute deadline would cut the stream
	writeSSEHeaders(w)
	sent := since
	replayed, err := a.Log().Since(since)
	if err != nil {
		return
	}
	for _, e := range replayed {
		sent = emit(w, e, sent)
	}
	rc.Flush()
	pump(r, w, rc, ch, sent)
}

// pump forwards live events until the client goes away.
func pump(r *http.Request, w http.ResponseWriter, rc *http.ResponseController,
	ch <-chan Event, sent int) {
	tick := time.NewTicker(beat)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			sent = emit(w, e, sent)
			rc.Flush()
		case <-tick.C:
			fmt.Fprint(w, ": beat\n\n")
			rc.Flush()
		}
	}
}

// emit writes one event if it is newer than the last, returning the new mark.
func emit(w http.ResponseWriter, e Event, sent int) int {
	if e.ID <= sent {
		return sent
	}
	buf, err := json.Marshal(e)
	if err != nil {
		return sent
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, buf)
	return e.ID
}

// writeSSEHeaders opens the stream and sets the client's reconnect interval.
func writeSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 2000\n\n")
}
