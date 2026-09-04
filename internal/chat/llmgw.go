package chat

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"time"

	"cracked/internal/hostnet"
)

// llmGatewayPrefix is where a guest's model calls arrive. The Anthropic API
// lives entirely under /v1/, so a guest is handed the listener's root as its
// base URL and the SDK's own paths land here unchanged.
const llmGatewayPrefix = "/v1/"

// guestCIDR is every address a guest can have. Slot N is 172.16.(4N+2), see
// hostnet.SlotAddrs; nothing else is ever on the guest side of a tap.
var guestCIDR = netip.MustParsePrefix("172.16.0.0/16")

// llmForwardedHeaders is everything a guest may say to the model service.
//
// An ALLOW list, for the same reason forwardedHeaders is: the guest is
// untrusted and a strip list is a promise to have thought of every header. The
// two anthropic-* headers are load-bearing -- the SDK versions every request,
// and agentd depends on a beta for context management -- so dropping either
// would fail every turn.
var llmForwardedHeaders = []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta"}

// llmAllowedPaths is what a guest may call: the two message endpoints, POST
// only. Files, batches, model listing and everything else the key could reach
// are refused, so the broker lends the key for turns and nothing wider.
var llmAllowedPaths = map[string]bool{
	"/v1/messages":              true,
	"/v1/messages/count_tokens": true,
}

// LLMGateway lends the host's model credential to guests, one request at a time.
//
// The apps broker next door needs a ticket because each guest goes to a
// DIFFERENT upstream: the ticket is what says which session. Here every guest
// goes to the same place with the same key, so a ticket would route nothing.
// What protects the key is that it is added on this side of the tap and never
// sent back. Who may ask is decided by where the request comes from: the
// firewall admits this port only from a tap device, and a tap is a
// point-to-point /30 the host owns, so a source address in guestCIDR is a
// guest and nothing else can claim to be one.
//
// That also makes it boot-order proof. There is no route to register, so a
// guest whose first turn is a schedule firing seconds after boot -- before
// anything on the host has spoken to it -- still reaches the model, and a
// restart of this process forgets nothing a guest depends on.
type LLMGateway struct {
	key      string
	upstream *url.URL
	proxy    *httputil.ReverseProxy
}

// NewLLMGateway prepares the broker, or nil when there is no key to lend. The
// upstream was validated by Config, so a parse failure here is a bug and is
// treated like a missing key: no broker, which the startup log then says.
func NewLLMGateway(key, upstream string) *LLMGateway {
	if key == "" {
		return nil
	}
	target, err := url.Parse(upstream)
	if err != nil || target.Host == "" {
		return nil
	}
	g := &LLMGateway{key: key, upstream: target}
	g.proxy = g.newProxy()
	return g
}

// serve gates one request on where it came from and what it asks for, then
// proxies it and writes one line saying how it went. Never the body: prompts
// are the person's own words, and the journal is read aloud when debugging.
func (g *LLMGateway) serve(w http.ResponseWriter, r *http.Request) {
	slot, ok := guestFrom(r.RemoteAddr)
	if !ok {
		log.Printf("llm gateway: refused a request from %s", r.RemoteAddr)
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost || !llmAllowedPaths[r.URL.Path] {
		log.Printf("llm gateway: slot %d asked for %s %s, refused", slot, r.Method, r.URL.Path)
		http.NotFound(w, r)
		return
	}
	started := time.Now()
	sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
	g.proxy.ServeHTTP(sw, r)
	log.Printf("llm gateway: slot %d %s %s -> %d in %s",
		slot, r.Method, r.URL.Path, sw.code, time.Since(started).Round(time.Millisecond))
}

// guestFrom says which guest a request came from, or false when the address is
// not a guest's. The slot is for the log line; the decision is the CIDR.
func guestFrom(remote string) (int, bool) {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil || !guestCIDR.Contains(ap.Addr()) {
		return 0, false
	}
	slot, ok := hostnet.SlotOf(ap.Addr().String())
	if !ok {
		// Inside the range but off the grid: still tap-side, so still a guest
		// as far as the firewall is concerned. It just has no name in the log.
		return -1, true
	}
	return slot, true
}

// newProxy builds the one proxy every request goes through. SetURL keeps the
// request's own path and query -- the SDK's ?beta=true included -- and the
// header map is replaced rather than edited, so whatever the guest added is
// gone before the credential is set.
func (g *LLMGateway) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(g.upstream)
			pr.Out.Header = keepHeaders(pr.Out.Header, llmForwardedHeaders)
			pr.Out.Header.Set("x-api-key", g.key)
		},
		FlushInterval: -1, // responses may stream; buffering would stall them
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("llm gateway: the model service could not be reached: %s",
				hideURL(err, g.upstream.String()))
			http.Error(w, "the model service is not answering", http.StatusBadGateway)
		},
	}
}

// statusWriter remembers the status the proxy wrote, for the log line.
type statusWriter struct {
	http.ResponseWriter
	code int
}

// WriteHeader records the status and passes it on.
func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the flusher underneath, which is
// what makes FlushInterval work through this wrapper.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
