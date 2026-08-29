package chat

import (
	"encoding/json"
	"errors"
	"net/http"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
)

// mcpBodyV1 bounds a registration. The guest enforces the same number, so
// anything over it is a client that skipped its own check.
const mcpBodyV1 = 8 << 10

// listMCP reports the remote MCP servers registered on the person's machine.
func (s *Server) listMCP(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	servers, err := cl.MCPServers()
	if err != nil {
		mcpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, servers)
}

// addMCP registers a server and answers with what it offers.
func (s *Server) addMCP(w http.ResponseWriter, r *http.Request, user string) {
	var reg agentapi.MCPRegistration
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, mcpBodyV1)).Decode(&reg) != nil {
		fail(w, http.StatusBadRequest, "could not read the registration")
		return
	}
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	// Passed through unchanged: the headers are the person's credentials and the
	// guest is what stores them. Redaction happens on the way BACK.
	server, err := cl.AddMCPServer(reg)
	if err != nil {
		mcpError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, server)
}

// updateMCP turns one registered server on or off.
func (s *Server) updateMCP(w http.ResponseWriter, r *http.Request, user string) {
	var in agentapi.MCPUpdate
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&in) != nil || in.Enabled == nil {
		fail(w, http.StatusBadRequest, "say whether to enable or disable this server")
		return
	}
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	server, err := cl.SetMCPEnabled(r.PathValue("id"), *in.Enabled)
	if err != nil {
		mcpError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, server)
}

// removeMCP forgets a server entirely.
func (s *Server) removeMCP(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := cl.RemoveMCPServer(r.PathValue("id")); err != nil {
		mcpError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// mcpError passes the guest's own refusal through instead of flattening it.
//
// sendError passes only 503 and 404 because that is all a message send can
// mean. Registration has four answers a client must act on differently: 400 is
// "fix the URL or the token", 409 is "you already have this one", 404 is "it is
// gone", 502 is "your server did not answer, try again". Flattened into one 502
// the app can only ever say "something went wrong", and a client retrying a 409
// would retry forever.
func mcpError(w http.ResponseWriter, err error) {
	var se *agent.StatusError
	if !errors.As(err, &se) {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	switch se.Code {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusBadGateway:
		fail(w, se.Code, orGuestSilence(se.Message))
	default:
		fail(w, http.StatusBadGateway, orGuestSilence(se.Message))
	}
}

// orGuestSilence keeps a refusal with no explanation from reaching the app as an
// empty string, which renders as a blank error toast.
func orGuestSilence(msg string) string {
	if msg == "" {
		return "the machine refused that and did not say why"
	}
	return msg
}
