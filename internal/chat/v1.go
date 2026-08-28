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
// Auth here is a bearer token and deliberately throwaway -- an email-to-machine
// mapping in the users file -- because it is being replaced by a real identity
// provider. Nothing else in the gateway depends on how it works.
func (s *Server) v1Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/auth/sign-in", s.signIn)
	mux.HandleFunc("POST /v1/auth/sign-out", s.apiGuard(s.signOut))
	mux.HandleFunc("GET /v1/agents", s.apiGuard(s.listAgents))
	mux.HandleFunc("GET /v1/agent-types", s.apiGuard(s.listTypes))
	mux.HandleFunc("POST /v1/agents", s.apiGuard(s.createAgent))
	mux.HandleFunc("DELETE /v1/agents/{id}", s.apiGuard(s.deleteAgent))
	mux.HandleFunc("POST /v1/threads/{id}/messages", s.apiGuard(s.sendMessage))
	mux.HandleFunc("POST /v1/threads/{id}/files", s.apiGuard(s.uploadFile))
	mux.HandleFunc("GET /v1/threads/{id}/shots/{name}", s.apiGuard(s.getShot))
	mux.HandleFunc("GET /v1/profile", s.apiGuard(s.getProfile))
	mux.HandleFunc("PUT /v1/profile", s.apiGuard(s.putProfile))
	mux.HandleFunc("GET /v1/threads/{id}", s.apiGuard(s.getThread))
	mux.HandleFunc("GET /v1/stream", s.apiGuard(s.streamV1))
	mux.HandleFunc("POST /v1/threads/{id}/messages/{messageId}/approval",
		s.apiGuard(s.resolveApproval))
	mux.HandleFunc("POST /v1/feedback", s.apiGuard(s.postFeedback))
}

// apiGuard requires a session and always answers 401, never a redirect. The web
// page's guard sends a browser to /login, and an app given a 302 to an HTML page
// would report an unreadable failure instead of "your token expired".
func (s *Server) apiGuard(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := userFor(r)
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		next(w, r, user)
	}
}

// signInReq is the sign-in body. Email is simply the users-file key.
type signInReq struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// session is what the app stores and replays as a bearer token.
type sessionResp struct {
	UserID string `json:"userId"`
	Email  string `json:"email"`
	Token  string `json:"token"`
}

// signIn checks the password and mints a token. The cookie is set as well so the
// existing web page keeps working against the same session store.
func (s *Server) signIn(w http.ResponseWriter, r *http.Request) {
	var req signInReq
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	token, ok := login(req.Email, req.Password)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}
	setSession(w, token)
	writeJSON(w, http.StatusOK, sessionResp{UserID: req.Email, Email: req.Email, Token: token})
}

// signOut clears the cookie and answers 204 with no body, per the client's rule
// that any other 2xx has res.json() called on it.
//
// Tokens are fixed constants, so there is nothing to revoke server-side: the
// client drops its stored copy and that is the whole of it. Real revocation
// arrives with real auth.
func (s *Server) signOut(w http.ResponseWriter, r *http.Request, _ string) {
	clearSession(w)
	w.WriteHeader(http.StatusNoContent)
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

// fail writes an error status. The client reads only the code, never the body,
// so the text is for us and the number is the contract.
func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
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

// sendReqV1 is the body of POST /v1/threads/{id}/messages. The client's own time
// is ignored -- the server stamps the message. File names something already
// uploaded through POST /v1/threads/{id}/files, not the bytes themselves.
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
	sent, err := cl.PostFile(r.PathValue("id"), req.Text, req.File)
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
