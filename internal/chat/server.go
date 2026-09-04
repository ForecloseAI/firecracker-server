package chat

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"

	"cracked/internal/composio"
)

//go:embed static/chat.html
var chatPage []byte

// Server is the browser-facing half of the chat service.
type Server struct {
	cfg     Config
	control *Control
	caps    *Caps
	auth    *Verifier

	// composio mints app-integration sessions, and apps remembers them. Both are
	// nil when no provider is configured, which is what turns the whole feature
	// off without a flag.
	composio *composio.Client
	apps     AppsStore
	// catalog is the featured apps' copy, shared by every person on the fleet.
	catalog *appCatalog
	// kinds is what kind of thing each connected-app action is, shared the same
	// way. It is what a person's policy is resolved against. Not `caps`, which
	// this struct already uses for the VNC grants.
	kinds *appCaps
	// gw is how a guest reaches its session without holding the credential.
	gw *AppsGateway
	// llm lends the host's model credential to guests, request by request.
	llm *LLMGateway

	mu      sync.Mutex
	bridges map[string]*Bridge
	// appsClaims is what this process has done about each machine's session.
	// One map, not two: pushed and failed have to stay consistent across three
	// call sites, and a delete forgotten in one of them is a machine that never
	// gets its apps back.
	appsClaims map[string]appsClaim
}

// NewServer wires the chat service together.
func NewServer(cfg Config, control *Control, caps *Caps, auth *Verifier) *Server {
	s := &Server{cfg: cfg, control: control, caps: caps, auth: auth,
		bridges: map[string]*Bridge{}, appsClaims: map[string]appsClaim{},
		composio: composio.New(cfg.ComposioKey, cfg.ComposioBase)}
	// Only alongside a provider. A store with nothing to remember would be a
	// live database dependency bought for nothing.
	// Assigned through a local, never straight into the field: a nil *pgApps put
	// behind the interface is not a nil interface, and every call on it would
	// panic instead of taking the "no store configured" path.
	if s.composio != nil {
		if store := newPGApps(cfg.SupabaseURL, cfg.SupabasePublishable); store != nil {
			s.apps = store
		}
		s.gw = NewAppsGateway(cfg.ComposioKey, cfg.AppsAddr)
		s.catalog = newAppCatalog(s.composio)
		s.kinds = newAppCaps(s.composio)
	}
	s.llm = NewLLMGateway(cfg.AnthropicKey, cfg.AnthropicUpstream)
	return s
}

// GuestRoutes is the one handler tree a guest can reach, or nil when there is
// nothing to broker: connected-apps tickets under /apps/, the model credential
// under /v1/.
//
// Its own listener and its own mux, deliberately: it is the one surface an
// untrusted guest can reach, and mounting it on the mux that serves the app's
// /v1 and the operator page would put it one routing mistake away from both.
// Never wrapped in the logging middleware either: the bodies are prompts.
func (s *Server) GuestRoutes() http.Handler {
	if s.gw == nil && s.llm == nil {
		return nil
	}
	mux := http.NewServeMux()
	if s.gw != nil {
		mux.HandleFunc(appsGatewayPrefix, s.gw.serve)
	}
	if s.llm != nil {
		mux.HandleFunc(llmGatewayPrefix, s.llm.serve)
	}
	mux.HandleFunc("/", http.NotFound)
	return mux
}

// Routes builds the handler, wrapped in stdlib CSRF protection.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.redirectHome)
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
	return logged(cop.Handler(mux), s.cfg.LogBodies, s.auth)
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

// dropBridge removes a machine's consumer and hands it back to be stopped.
//
// Stopping is the caller's job, deliberately: the bridge paths take b.mu and
// then s.mu, so calling into a bridge while holding s.mu inverts that order.
// Take the entry out here, release, then stop it.
//
// This matters on deletion because a bridge outlives the machine it watched. It
// holds that machine's event watermark, and the replacement boots under the same
// id with its log restarted at 1 -- so a bridge left in place would discard the
// new machine's entire transcript until its ids passed the old high mark.
func (s *Server) dropBridge(id string) *Bridge {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.bridges[id]
	delete(s.bridges, id)
	return b
}

// guard requires the fleet token. An operator arrives with ?token=<CRACKED_TOKEN>
// once and the cookie carries them from there, which is how the control plane's
// dashboard already works.
//
// There is no redirect to a login form any more: there is no form to send anyone
// to. A browser without the token gets 401 and an explanation, not a page that
// invites it to guess.
func (s *Server) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isOperator(r) {
			if isAPI(r) {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
			http.Error(w, "unauthorized: append ?token=<CRACKED_TOKEN>", http.StatusUnauthorized)
			return
		}
		// Arriving by link: move the token out of the address bar and into the
		// cookie, then bounce to the clean URL. Setting the cookie is not enough
		// on its own -- left in the address bar the fleet token rides along in
		// the Referer of every same-origin request, and survives in history, a
		// screenshot, or a copied link. The dashboard strips it the same way.
		//
		// Only page GETs are redirected: a redirect would silently drop the body
		// of a POST, and an API caller passing ?token= wants an answer, not a 302.
		if tok := r.URL.Query().Get("token"); tok != "" {
			setSession(w, tok)
			if r.Method == http.MethodGet && !isAPI(r) {
				clean := *r.URL
				q := clean.Query()
				q.Del("token")
				clean.RawQuery = q.Encode()
				http.Redirect(w, r, clean.RequestURI(), http.StatusFound)
				return
			}
		}
		next(w, r)
	}
}

// isAPI reports whether a request wants JSON rather than an HTML error.
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

// logout drops the operator cookie.
func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	clearSession(w)
	http.Error(w, "signed out", http.StatusUnauthorized)
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
