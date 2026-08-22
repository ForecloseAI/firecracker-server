// Package api exposes the control plane over HTTP. It holds no exec or
// syscall knowledge; all lifecycle work goes through the vm package.
package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"cracked/internal/agent"
	"cracked/internal/vm"
)

// Server wires the registry to HTTP handlers.
type Server struct {
	reg   *vm.Registry
	token string
	usage *agent.Accumulator
}

// New builds a Server guarded by the given bearer token.
func New(reg *vm.Registry, token string) *Server {
	return &Server{reg: reg, token: token, usage: agent.NewAccumulator()}
}

// Routes registers every endpoint using ServeMux method+wildcard patterns.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", s.handleRoot)
	mux.HandleFunc("GET /dashboard", s.authNoCookie(s.handleDashboard))
	// Cookie-free like the dashboard: neither route has an {id}, so a ?token=
	// here would set a root-scoped cookie. Nothing needs one -- the page sends
	// a header and so does Prometheus.
	mux.HandleFunc("GET /stats", s.authNoCookie(s.handleStats))
	mux.HandleFunc("GET /metrics", s.authNoCookie(s.handleMetrics))
	mux.HandleFunc("GET /capacity", s.auth(s.handleCapacity))
	mux.HandleFunc("POST /vms", s.auth(s.handleCreate))
	mux.HandleFunc("GET /vms", s.auth(s.handleList))
	mux.HandleFunc("GET /vms/{id}", s.auth(s.handleGet))
	mux.HandleFunc("GET /vms/{id}/stats", s.authNoCookie(s.handleVMStats))
	mux.HandleFunc("POST /vms/{id}/pause", s.auth(s.handlePause))
	mux.HandleFunc("POST /vms/{id}/resume", s.auth(s.handleResume))
	mux.HandleFunc("DELETE /vms/{id}", s.auth(s.handleDelete))
	mux.Handle("/vms/{id}/vnc/", s.auth(s.handleProxy(vm.VNCPort, "vnc")))
	mux.Handle("/vms/{id}/agent/", s.auth(s.handleProxy(vm.AgentPort, "agent")))
	return mux
}

// apiError is the single error shape used by every endpoint.
type apiError struct {
	Error    string `json:"error"`
	Message  string `json:"message"`
	Resource string `json:"resource,omitempty"`
}

// auth requires a valid bearer token from a header, query param, or cookie.
// A query token is echoed into a path-scoped cookie because browsers cannot
// set headers on subresource loads or WebSocket handshakes.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return s.guard(next, true)
}

// authNoCookie authenticates without ever setting one. The dashboard needs
// this: its route has no {id}, so the cookie would be scoped to "/", and the
// untrusted guest serves content on the SAME origin under /vms/{id}/agent/.
// An injected page there could ride that ambient cookie and drive the fleet.
func (s *Server) authNoCookie(next http.HandlerFunc) http.HandlerFunc {
	return s.guard(next, false)
}

// guard is the single token comparison site for every authenticated route.
func (s *Server) guard(next http.HandlerFunc, setCookie bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, fromQuery := extractToken(r)
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			writeErr(w, http.StatusUnauthorized, apiError{"unauthorized", "invalid or missing token", ""})
			return
		}
		if fromQuery && setCookie {
			setTokenCookie(w, r, tok)
		}
		next(w, r)
	}
}

// handleRoot sends a bare visit to the dashboard, preserving any ?token=.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	target := "/dashboard"
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// extractToken pulls the bearer token from the request, reporting whether it
// came from the query string.
func extractToken(r *http.Request) (string, bool) {
	if h := r.Header.Get("Authorization"); len(h) > 7 && h[:7] == "Bearer " {
		return h[7:], false
	}
	if q := r.URL.Query().Get("token"); q != "" {
		return q, true
	}
	if c, err := r.Cookie("cracked_token"); err == nil {
		return c.Value, false
	}
	return "", false
}

// setTokenCookie scopes the token to this VM's path so proxied assets and the
// WebSocket upgrade authenticate without a header.
func setTokenCookie(w http.ResponseWriter, r *http.Request, tok string) {
	path := "/"
	if id := r.PathValue("id"); id != "" {
		path = "/vms/" + id + "/"
	}
	http.SetCookie(w, &http.Cookie{
		Name: "cracked_token", Value: tok, Path: path,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
}

// writeJSON emits a JSON body with the given status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("write response: %v", err)
	}
}

// writeErr emits an apiError, adding Retry-After when capacity is exhausted.
func writeErr(w http.ResponseWriter, status int, e apiError) {
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "30")
	}
	writeJSON(w, status, e)
}

// writeVMErr maps a vm package sentinel error onto its status code.
func writeVMErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vm.ErrNotFound):
		writeErr(w, http.StatusNotFound, apiError{"not_found", err.Error(), ""})
	case errors.Is(err, vm.ErrDuplicate):
		writeErr(w, http.StatusConflict, apiError{"conflict", err.Error(), ""})
	case errors.Is(err, vm.ErrState):
		writeErr(w, http.StatusConflict, apiError{"conflict", err.Error(), ""})
	case errors.Is(err, vm.ErrNoSlots):
		writeErr(w, http.StatusServiceUnavailable,
			apiError{"capacity_exhausted", err.Error(), "slots"})
	default:
		writeErr(w, http.StatusInternalServerError, apiError{"internal", err.Error(), ""})
	}
}
