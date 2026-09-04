package chat

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"

	"cracked/internal/hostnet"
)

// llmGatewayPrefix is where a guest's model calls arrive. The Anthropic API
// lives entirely under /v1/, so a guest is handed the listener's root as its
// base URL and the SDK's own paths land here unchanged.
const llmGatewayPrefix = "/v1/"

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
// goes to the same place with the same key, so a ticket would route nothing --
// and having no route to register is what makes this boot-order proof (README,
// "The model key goes the same way"). What protects the key is that it is
// added on this side of the tap and never sent back. Who may ask is decided by
// the source address: the firewall admits this port only from a tap device,
// and a tap is a point-to-point /30 the host owns, so an address on the guest
// grid is a guest and nothing else can claim to be one.
type LLMGateway struct {
	proxy *httputil.ReverseProxy
}

// NewLLMGateway prepares the broker. Config has already required the key and
// validated the upstream, so there is nothing here that can fail.
func NewLLMGateway(key string, upstream *url.URL) *LLMGateway {
	return &LLMGateway{proxy: llmProxy(key, upstream)}
}

// serve gates one request on where it came from and what it asks for. Both
// refusals are the same bare 404, so nothing about the listener can be probed.
func (g *LLMGateway) serve(w http.ResponseWriter, r *http.Request) {
	if !fromGuest(r.RemoteAddr) || r.Method != http.MethodPost || !llmAllowedPaths[r.URL.Path] {
		http.NotFound(w, r)
		return
	}
	g.proxy.ServeHTTP(w, r)
}

// fromGuest says whether an address is a guest's: on the /30 grid hostnet hands
// out, and nothing else. The host's own tap addresses are not guests.
func fromGuest(remote string) bool {
	ap, err := netip.ParseAddrPort(remote)
	if err != nil {
		return false
	}
	_, ok := hostnet.SlotOf(ap.Addr())
	return ok
}

// llmProxy forwards to the model service. SetURL keeps the request's own path
// and query -- the SDK's ?beta=true included -- and the header map is replaced
// rather than edited, so whatever the guest added is gone before the credential
// is set. The upstream is named in a failure: it is no secret (it sits in the
// systemd unit), and when ANTHROPIC_UPSTREAM points at a test host, which host
// failed is the one fact the line is for.
func llmProxy(key string, upstream *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Header = keepHeaders(pr.Out.Header, llmForwardedHeaders)
			pr.Out.Header.Set("x-api-key", key)
		},
		FlushInterval: -1, // responses may stream; buffering would stall them
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("llm gateway: the model service could not be reached: %v", err)
			http.Error(w, "the model service is not answering", http.StatusBadGateway)
		},
	}
}
