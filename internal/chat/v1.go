package chat

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
	"cracked/internal/vm"
)

// The app's API, mounted under /v1. It is a separate mux from the web page's
// /api/* routes rather than an extension of them: the page's handlers all take a
// VM id in the body and address the boss, which is the shape this replaces.
//
// Auth is a Supabase access token, verified against the project's public keys.
// There is no sign-in route here: the app talks to Supabase directly and this
// service only ever checks what it is handed. Nothing else in the gateway
// depends on how that works -- every handler below is given a user id and does
// not care where it came from.
func (s *Server) v1Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/agents", s.apiGuard(s.listAgents))
	mux.HandleFunc("GET /v1/agent-types", s.apiGuard(s.listTypes))
	mux.HandleFunc("POST /v1/agents", s.apiGuard(s.createAgent))
	mux.HandleFunc("DELETE /v1/agents/{id}", s.apiGuard(s.deleteAgent))
	mux.HandleFunc("POST /v1/threads/{id}/messages", s.apiGuard(s.sendMessage))
	mux.HandleFunc("POST /v1/threads/{id}/files", s.apiGuard(s.uploadFile))
	mux.HandleFunc("GET /v1/threads/{id}/shots/{name}", s.apiGuard(s.getShot))
	mux.HandleFunc("GET /v1/schedules", s.apiGuard(s.listSchedules))
	mux.HandleFunc("DELETE /v1/schedules/{id}", s.apiGuard(s.cancelSchedule))
	mux.HandleFunc("GET /v1/profile", s.apiGuard(s.getProfile))
	mux.HandleFunc("PUT /v1/profile", s.apiGuard(s.putProfile))
	mux.HandleFunc("GET /v1/threads/{id}", s.apiGuard(s.getThread))
	mux.HandleFunc("GET /v1/stream", s.apiGuard(s.streamV1))
	mux.HandleFunc("DELETE /v1/account", s.apiGuard(s.deleteAccount))
	mux.HandleFunc("POST /v1/threads/{id}/messages/{messageId}/approval",
		s.apiGuard(s.resolveApproval))
}

// apiGuard requires a verified Supabase token and always answers 401, never a
// redirect: an app given a 302 to an HTML page would report an unreadable
// failure instead of "your token expired".
//
// The token was already verified by the logging middleware, which wraps
// everything; this reads that result rather than checking the signature twice.
// The string handed to each handler is the Supabase user id.
func (s *Server) apiGuard(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r, id.UserID)
	}
}

// listAgents returns the roster of the signed-in person's own machine, booting
// it if this is their first visit. That first call is slow -- a VM takes most of
// a minute to come up -- and every one after it is not.
func (s *Server) listAgents(w http.ResponseWriter, r *http.Request, user string) {
	agents, err := s.rosterOf(machineFor(user))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, agents)
}

// deleteAccount erases everything the person has on their machine.
//
// Not guestOf: that boots the machine on the way in, which is absurd when the
// next thing is deleting it, and would turn a wipe of a stopped machine into a
// minute of booting one first. machineFor is a pure derivation and needs nothing
// running.
//
// The Supabase account itself is untouched -- this service can verify tokens and
// nothing else. So the person can sign in again and gets a blank machine booted
// on demand, which is the intended shape: a clean start, not a locked door.
func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request, user string) {
	machine := machineFor(user)
	if machine == "" {
		fail(w, http.StatusBadRequest, "no machine for this account")
		return
	}
	if err := s.control.DeleteVM(machine); err != nil {
		// Deliberately not 204. The app tells someone their data is gone on the
		// strength of this status, so a failure has to read as one.
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	// The machine id is derived from the account, so the replacement reuses it.
	// Anything still keyed on it would attach to the new machine as if it were
	// the old one.
	if b := s.dropBridge(machine); b != nil {
		b.Close()
	}
	if s.caps != nil {
		s.caps.Revoke(machine)
	}
	log.Printf("chat: erased %s at the account holder's request", machine)
	w.WriteHeader(http.StatusNoContent)
}

// fail writes an error status. The client reads only the code, never the body,
// so the text is for us and the number is the contract.
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// listSchedules reports every standing job on the person's machine.
//
// Machine-wide and not per-thread, unlike the rest of this surface: the question
// the person asks is "what runs on its own while I am not here", and an answer
// split across one call per agent could not be asked at all.
func (s *Server) listSchedules(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	list, err := cl.Schedules()
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	// Never null: the client renders this straight into a list, and a null body
	// is a crash where an empty one is an empty state.
	if list == nil {
		list = []agentapi.Schedule{}
	}
	writeJSON(w, http.StatusOK, list)
}

// cancelSchedule stops a standing job.
//
// Not gated by an approval, unlike the agent's own schedule_task tool: this
// request IS the person, and undoing a commitment they made needs no permission
// from them.
func (s *Server) cancelSchedule(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := cl.DeleteSchedule(r.PathValue("id")); err != nil {
		var se *agent.StatusError
		if errors.As(err, &se) && se.Code == http.StatusNotFound {
			fail(w, http.StatusNotFound, "no such schedule")
			return
		}
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// guestOf resolves a person to their machine's daemon, booting the machine if
// this is their first call.
func guestOf(s *Server, user string) (*agent.Client, error) {
	machine := machineFor(user)
	if machine == "" {
		return nil, ErrNoVM
	}
	view, err := s.ensureMachine(machine)
	if err != nil {
		return nil, err
	}
	return agent.New(view.GuestIP, guestPort), nil
}

// listTypes is the gallery: what this person can still add.
func (s *Server) listTypes(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	profiles, err := cl.AgentTypes()
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	roster, _ := cl.Agents() // an unreadable roster only costs the active flags
	writeJSON(w, http.StatusOK, projectTemplates(profiles, roster))
}

// createReq is the body of POST /v1/agents. TemplateId is a profile key
// straight from the gallery, so there is no mapping table to drift.
type createReq struct {
	TemplateID string `json:"templateId"`
}

// createAgent activates one kind of agent on the person's machine.
func (s *Server) createAgent(w http.ResponseWriter, r *http.Request, user string) {
	var req createReq
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil {
		fail(w, http.StatusBadRequest, "bad request")
		return
	}
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	s.activate(w, cl, req.TemplateID, machineFor(user))
}

// activate refuses a duplicate, creates, and returns the finished roster row.
//
// One of each kind: a second agent of the same type would get id "coder-2",
// which the gallery has no card for and the person never asked for.
func (s *Server) activate(w http.ResponseWriter, cl *agent.Client, typeKey, machine string) {
	profile, roster, err := lookupType(cl, typeKey)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if profile.Key == "" || typeKey == agentapi.BossID {
		fail(w, http.StatusBadRequest, "unknown templateId")
		return
	}
	for _, st := range roster {
		if st.Type == typeKey {
			fail(w, http.StatusConflict, "already active")
			return
		}
	}
	s.finishActivate(w, cl, profile, machine)
}

// finishActivate creates the agent and projects it as the app's roster row.
func (s *Server) finishActivate(w http.ResponseWriter, cl *agent.Client,
	p agentapi.Profile, machine string) {
	rec, err := cl.CreateAgent(p.Key, p.Title)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	st := agentapi.Status{ID: rec.ID, Name: rec.Name, Type: rec.Type}
	writeJSON(w, http.StatusOK, projectAgent(st, p, machine, true))
}

// lookupType finds one profile and the current roster in one place, since every
// decision about activating needs both.
func lookupType(cl *agent.Client, typeKey string) (agentapi.Profile, []agentapi.Status, error) {
	profiles, err := cl.AgentTypes()
	if err != nil {
		return agentapi.Profile{}, nil, err
	}
	roster, err := cl.Agents()
	if err != nil {
		return agentapi.Profile{}, nil, err
	}
	for _, p := range profiles {
		if p.Key == typeKey {
			return p, roster, nil
		}
	}
	return agentapi.Profile{}, roster, nil
}

// deleteAgent retires one agent, keeping its history.
//
// The roster is checked here rather than trusting the guest's status: agentd
// answers 409 for both "no such agent" and "that is the boss", and a client
// retrying on 409 would retry a missing agent forever.
func (s *Server) deleteAgent(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	id := r.PathValue("id")
	if code, msg := retirable(cl, id); code != 0 {
		fail(w, code, msg)
		return
	}
	if err := cl.DeleteAgent(id); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// retirable reports why an agent cannot be retired, or 0 when it can.
func retirable(cl *agent.Client, id string) (int, string) {
	if id == agentapi.BossID {
		return http.StatusConflict, "the boss cannot be retired"
	}
	roster, err := cl.Agents()
	if err != nil {
		return http.StatusBadGateway, err.Error()
	}
	for _, st := range roster {
		if st.ID == id {
			return 0, ""
		}
	}
	return http.StatusNotFound, "no such agent"
}

// sendReqV1 is the body of POST /v1/threads/{id}/messages. File names something
// already uploaded through POST /v1/threads/{id}/files, not the bytes
// themselves.
//
// The message carries no clock and no zone. The server stamps it, so a device
// with a wrong clock cannot reorder a transcript, and the guest is already on
// the person's own timezone -- PUT /v1/profile is where that is decided, once.
type sendReqV1 struct {
	Text string         `json:"text"`
	File *agentapi.File `json:"file,omitempty"`
}

// sendMessage delivers one instruction and echoes it back as a stored message.
func (s *Server) sendMessage(w http.ResponseWriter, r *http.Request, user string) {
	var req sendReqV1
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil {
		fail(w, http.StatusBadRequest, "could not read the message")
		return
	}
	if req.Text == "" && req.File == nil {
		fail(w, http.StatusBadRequest, "text is required")
		return
	}
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	sent, err := cl.PostMessage(r.PathValue("id"), agent.Send{Text: req.Text, File: req.File})
	if err != nil {
		sendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, storedMessage(sent, req))
}

// storedMessage is what the client appends to the thread. The id is the guest's
// event id, so it matches what a later history fetch reports for the same line.
func storedMessage(sent agent.Sent, req sendReqV1) Message {
	return Message{Kind: "text", ID: strconv.Itoa(sent.LastEventID),
		Time: time.Now().UTC(), From: "me", Text: req.Text, File: req.File}
}

// sendError passes the guest's own refusal through instead of flattening it.
// A busy agent is 503 and worth retrying; an unknown one is 404 and is not.
func sendError(w http.ResponseWriter, err error) {
	var se *agent.StatusError
	if errors.As(err, &se) && (se.Code == http.StatusServiceUnavailable || se.Code == http.StatusNotFound) {
		if se.Code == http.StatusServiceUnavailable {
			w.Header().Set("Retry-After", "5")
		}
		fail(w, se.Code, "agent unavailable")
		return
	}
	fail(w, http.StatusBadGateway, err.Error())
}

// ensureMachine returns the person's VM, booting it if it does not exist yet.
//
// Blocking is the simple correct thing here: the roster screen has nothing to
// show until the machine is up, and doing it in the background would need a job
// table and a way to tell the app when it finished. Every later call is a plain
// lookup.
func (s *Server) ensureMachine(machine string) (vmView, error) {
	view, err := s.control.VM(machine)
	if errors.Is(err, ErrNoVM) {
		log.Printf("chat: booting %s on first sign-in", machine)
		return s.control.CreateVM(machine)
	}
	return view, err
}

// rosterOf asks one machine's daemon who lives on it.
//
// GET /agents on the guest is deliberately not wrapped in withAgent, so listing
// costs nothing: the sibling event route IS wrapped, and reading the roster
// through that would start every agent on the machine just to draw a list.
func (s *Server) rosterOf(machine string) ([]Agent, error) {
	if machine == "" {
		return []Agent{}, nil
	}
	view, err := s.ensureMachine(machine)
	if err != nil {
		return nil, err
	}
	if view.State != vm.StateRunning {
		return []Agent{}, nil
	}
	cl := agent.New(view.GuestIP, guestPort)
	roster, err := cl.Agents()
	if err != nil {
		return nil, err
	}
	profiles, _ := cl.AgentTypes() // display metadata only; a failure just costs labels
	return projectRoster(roster, profiles, machine, true), nil
}
