package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// Routes builds the handler. Method+wildcard patterns, like internal/api.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /agents", s.handleList)
	mux.HandleFunc("POST /agents", s.handleCreate)
	mux.HandleFunc("DELETE /agents/{id}", s.handleDelete)
	mux.HandleFunc("GET /agent-types", s.handleTypes)
	mux.HandleFunc("GET /usage", s.handleUsage)
	mux.HandleFunc("POST /agents/{id}/messages", s.withAgent(s.handleMessage))
	mux.HandleFunc("POST /agents/{id}/interrupt", s.withAgent(s.handleInterrupt))
	mux.HandleFunc("POST /agents/{id}/approvals/{apid}", s.withAgent(s.handleApproval))
	mux.HandleFunc("GET /agents/{id}/events", s.withAgent(s.handleEvents))
	mux.HandleFunc("GET /debug/memstats", s.handleMemstats)
	mux.HandleFunc("POST /debug/exec", s.handleDebugExec)
	return mux
}

// agentHandler is a handler that has already had its agent resolved.
type agentHandler func(http.ResponseWriter, *http.Request, *Agent)

// withAgent resolves {id} to an agent, 404ing when there is no such one.
func (s *Server) withAgent(next agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		a, err := s.sup.Get(id)
		if errors.Is(err, ErrNoCapacity) {
			w.Header().Set("Retry-After", "10")
			fail(w, http.StatusServiceUnavailable, "capacity_exhausted", err.Error(), "agents")
			return
		}
		if err != nil {
			fail(w, http.StatusNotFound, "not_found", err.Error(), "agent")
			return
		}
		next(w, r, a)
	}
}

// handleList reports every agent and what it is doing. This is the poll a
// client makes: one call, every agent, its state and its current task title.
//
// It deliberately does not start anything. A dashboard refreshing every few
// seconds must not be able to spawn the whole roster into memory.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.sup.List())
}

// createReq is the body of POST /agents.
type createReq struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// handleCreate adds an agent to the roster. It does not start it: an agent
// runs when it is first addressed.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.Type == "" {
		fail(w, http.StatusBadRequest, "bad_request", "type is required", "")
		return
	}
	rec, err := s.sup.Create(req.Type, req.Name)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err.Error(), "agent")
		return
	}
	reply(w, http.StatusCreated, rec)
}

// handleDelete stops an agent and drops it from the roster. With ?purge=true
// its state goes too; without, recreating the same id gets its history back,
// mirroring what DELETE on a VM does with its workspace.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sup.Delete(id, r.URL.Query().Get("purge") == "true"); err != nil {
		fail(w, http.StatusConflict, "conflict", err.Error(), "agent")
		return
	}
	reply(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

// handleTypes lists the profiles an agent can be created from. This is what a
// client shows the person when they ask for a new specialist.
func (s *Server) handleTypes(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.sup.Catalog().List())
}

// handleHealth is what the control plane's boot probe reads, and what the
// dashboard's agent column renders.
//
// session_state mirrors the TypeScript agent's field so internal/agent.Health
// decodes it unchanged. It is computed from List(), never Get(): Get STARTS an
// agent, and a dashboard polling every few seconds must not be able to spawn the
// whole roster into memory.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	statuses := s.sup.List()
	working := 0
	for _, st := range statuses {
		if st.State == "working" {
			working++
		}
	}
	state := "idle"
	if working > 0 {
		state = "working"
	}
	reply(w, http.StatusOK, Health{
		OK: true, Ready: true, Agents: len(statuses),
		Live: s.sup.LiveCount(), Working: working, SessionState: state,
	})
}

// handleUsage reports what this machine has spent, in tokens, across every
// agent. Tokens and not dollars: the host owns the price table, so a rate change
// does not mean rebuilding a guest image.
func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	reply(w, http.StatusOK, s.sup.Meter().Report())
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
		reply(w, http.StatusOK, EventsPage{Events: events, LastEventID: a.Log().LastID()})
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
