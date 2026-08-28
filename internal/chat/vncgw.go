package chat

import (
	"crypto/rand"
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	capTTL    = 15 * time.Minute
	capCookie = "vnc_cap"
)

// grant is one capability to view one VM's screen.
type grant struct {
	vmID    string
	expires time.Time
}

// Caps mints and checks short-lived VNC capabilities. A capability is the only
// thing the VNC origin ever holds: no chat session, no fleet token.
type Caps struct {
	mu     sync.Mutex
	grants map[string]grant
	origin string
}

// NewCaps builds a capability store issuing URLs under the given origin.
func NewCaps(origin string) *Caps {
	return &Caps{grants: map[string]grant{}, origin: origin}
}

// Mint issues a capability URL for one VM. Minted when the card is built, so a
// replayed event never carries a stale one.
func (c *Caps) Mint(vmID string) string {
	buf := make([]byte, 32)
	rand.Read(buf)
	key := base64.RawURLEncoding.EncodeToString(buf)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep()
	c.grants[key] = grant{vmID: vmID, expires: time.Now().Add(capTTL)}
	return c.origin + "/v/" + vmID + "?k=" + key
}

// check reports whether a key grants access to a VM.
func (c *Caps) check(key, vmID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	g, ok := c.grants[key]
	return ok && g.vmID == vmID && time.Now().Before(g.expires)
}

// Revoke drops every capability for one VM, for when that VM is deleted.
//
// Expiry alone is not enough here. A grant lives 15 minutes and check only
// compares key, VM id and expiry -- so a capability minted just before a delete
// would still be valid afterwards, and the machine id is derived from the
// account, meaning the replacement machine reuses it. Without this, a handoff
// link from before the wipe opens the screen of the machine after it.
func (c *Caps) Revoke(vmID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, g := range c.grants {
		if g.vmID == vmID {
			delete(c.grants, k)
		}
	}
}

// sweep drops expired capabilities.
func (c *Caps) sweep() {
	now := time.Now()
	for k, g := range c.grants {
		if now.After(g.expires) {
			delete(c.grants, k)
		}
	}
}

// VNCGateway serves noVNC on its own origin. It is a separate hostname on
// purpose: it renders guest-authored HTML, and the chat cookie's __Host-
// prefix makes that cookie structurally unable to reach here.
type VNCGateway struct {
	caps  *Caps
	proxy *httputil.ReverseProxy
}

// NewVNCGateway builds the gateway, injecting the fleet token server-side.
func NewVNCGateway(caps *Caps, controlURL, token string) (*VNCGateway, error) {
	target, err := url.Parse(controlURL)
	if err != nil {
		return nil, err
	}
	g := &VNCGateway{caps: caps}
	g.proxy = &httputil.ReverseProxy{
		Rewrite:       rewriteVNC(target, token),
		FlushInterval: -1,
		ErrorHandler:  vncError,
	}
	return g, nil
}

// rewriteVNC maps /v/{id}/... onto the control plane's VNC proxy subtree.
func rewriteVNC(target *url.URL, token string) func(*httputil.ProxyRequest) {
	return func(r *httputil.ProxyRequest) {
		id, rest := splitVNCPath(r.In.URL.Path)
		r.Out.URL.Scheme, r.Out.URL.Host = target.Scheme, target.Host
		r.Out.URL.Path = "/vms/" + id + "/vnc/" + rest
		r.Out.Host = target.Host
		r.Out.Header.Set("Authorization", "Bearer "+token)
		r.Out.Header.Del("Cookie")
	}
}

// splitVNCPath pulls the VM id and remainder out of /v/{id}/rest.
func splitVNCPath(p string) (string, string) {
	trimmed := strings.TrimPrefix(p, "/v/")
	id, rest, found := strings.Cut(trimmed, "/")
	if !found {
		return id, ""
	}
	return id, rest
}

// vncError reports an unreachable VM as plain text; this origin has no UI.
func vncError(w http.ResponseWriter, r *http.Request, err error) {
	log.Printf("vnc proxy %s: %v", r.URL.Path, err)
	http.Error(w, "screen unavailable", http.StatusBadGateway)
}

// Routes builds the gateway's handler.
func (g *VNCGateway) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v/{id}", g.enter)
	mux.Handle("/v/{id}/", http.HandlerFunc(g.serve))
	return mux
}

// enter validates ?k=, exchanges it for a path-scoped cookie, and redirects so
// the capability never lingers in the address bar or a referrer.
func (g *VNCGateway) enter(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	key := r.URL.Query().Get("k")
	if !g.caps.check(key, id) {
		http.Error(w, "expired or invalid link", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: capCookie, Value: key, Path: "/v/" + id + "/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(capTTL / time.Second),
	})
	http.Redirect(w, r, "/v/"+id+"/vnc.html?path=v/"+id+"/websockify&autoconnect=true&resize=scale", http.StatusFound)
}

// serve proxies the noVNC subtree once the capability cookie checks out.
func (g *VNCGateway) serve(w http.ResponseWriter, r *http.Request) {
	id, _ := splitVNCPath(r.URL.Path)
	ck, err := r.Cookie(capCookie)
	if err != nil || !g.caps.check(ck.Value, id) {
		http.Error(w, "expired or invalid link", http.StatusUnauthorized)
		return
	}
	g.proxy.ServeHTTP(w, r)
}
