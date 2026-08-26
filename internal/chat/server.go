package chat

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
)

//go:embed static/chat.html
var chatPage []byte

//go:embed static/login.html
var loginPage []byte

// Server is the browser-facing half of the chat service.
type Server struct {
	cfg     Config
	control *Control
	caps    *Caps

	mu      sync.Mutex
	bridges map[string]*Bridge
}

// NewServer wires the chat service together.
func NewServer(cfg Config, control *Control, caps *Caps) *Server {
	return &Server{cfg: cfg, control: control, caps: caps, bridges: map[string]*Bridge{}}
}

// Routes builds the handler, wrapped in stdlib CSRF protection.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.redirectHome)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("GET /logout", s.logout)
	mux.HandleFunc("GET /chat", s.guard(s.chat))
	mux.HandleFunc("GET /api/vms", s.guard(s.listVMs))
	mux.HandleFunc("GET /api/target", s.guard(s.target))
	mux.HandleFunc("GET /api/stream", s.guard(s.stream))
	mux.HandleFunc("POST /api/send", s.guard(s.send))
	mux.HandleFunc("POST /api/interrupt", s.guard(s.interrupt))
	mux.HandleFunc("POST /api/resume", s.guard(s.resume))
	mux.HandleFunc("POST /api/pending/{id}", s.guard(s.resolvePending))
	s.v1Routes(mux)
	cop := http.NewCrossOriginProtection()
	cop.AddTrustedOrigin(s.cfg.Origin)
	return logged(cop.Handler(mux), s.cfg.LogBodies)
}

// bridge returns the consumer for a VM, starting one on first use. A cached
// bridge may have stopped itself after an idle period; Subscribe revives it, so
// do not add eviction here. Removing it from this map would need s.mu while
// holding b.mu, which inverts the lock order every other path takes.
func (s *Server) bridge(id string) *Bridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.bridges[id]
	if !ok {
		b = newBridge(id, s.control, s.caps)
		s.bridges[id] = b
	}
	return b
}

// guard requires a session, redirecting pages and 401ing API calls.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := userFor(r); ok {
			next(w, r)
			return
		}
		if isAPI(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusFound)
	}
}

// isAPI reports whether a request wants JSON rather than a redirect.
func isAPI(r *http.Request) bool {
	return len(r.URL.Path) >= 5 && r.URL.Path[:5] == "/api/"
}

// writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// redirectHome sends the root at the chat page.
func (s *Server) redirectHome(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/chat", http.StatusFound)
}

// loginPage serves the form, or skips it if already signed in.
func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := userFor(r); ok {
		http.Redirect(w, r, "/chat", http.StatusFound)
		return
	}
	servePage(w, loginPage)
}

// login checks the password and starts a session.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/login?e=1", http.StatusSeeOther)
		return
	}
	tok, ok := login(r.FormValue("user"), r.FormValue("password"))
	if !ok {
		http.Redirect(w, r, "/login?e=1", http.StatusSeeOther)
		return
	}
	setSession(w, tok)
	http.Redirect(w, r, nextPath(r.FormValue("next")), http.StatusSeeOther)
}

// nextPath keeps a post-login redirect on this site.
func nextPath(next string) string {
	if len(next) > 1 && next[0] == '/' && next[1] != '/' {
		return next
	}
	return "/chat"
}

// logout ends the session both server-side and in the browser.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

// chat serves the page itself.
func (s *Server) chat(w http.ResponseWriter, r *http.Request) {
	servePage(w, chatPage)
}

// servePage writes an embedded HTML page.
func servePage(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}
