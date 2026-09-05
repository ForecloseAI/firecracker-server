package chat

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"

	"cracked/internal/hostnet"
)

// llmGatewayPrefix is where a guest's model calls arrive. The Messages API
// lives entirely under /v1/, so a guest is handed the listener's root as its
// base URL and the SDK's own paths land here unchanged.
const llmGatewayPrefix = "/v1/"

// llmForwardedHeaders is everything a guest may say to the model service.
//
// An ALLOW list, for the same reason forwardedHeaders is: the guest is
// untrusted and a strip list is a promise to have thought of every header.
//
// The two anthropic-* headers are forwarded because the SDK sends them on every
// request and this is an Anthropic-shaped endpoint. Anthropic-Version is
// required there and tolerated by OpenRouter. Anthropic-Beta is how the SDK
// renders Betas, and OpenRouter documents no such header -- it takes
// context_management in the BODY instead, which rides through untouched. What
// an undocumented header actually does there has not been observed, so it is
// forwarded rather than guessed at: interleaved thinking is the only thing that
// rides on it alone, and forwarding costs nothing. Not a header to remove
// without checking which of the two endpoints is on the other end.
var llmForwardedHeaders = []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta"}

// llmAppName is what OpenRouter files this traffic under, beside the origin.
// A constant, not config: it says who we are, which does not vary per machine.
const llmAppName = "AutoBots"

// llmAllowedPaths is what a guest may call: one endpoint, POST only. Files,
// batches, model listing, generation lookups and everything else the key could
// reach are refused, so the broker lends the key for turns and nothing wider.
//
// count_tokens used to be here. Nothing in this repo has ever called it and
// OpenRouter does not serve it, so it was an opening kept for a caller that
// never arrived.
var llmAllowedPaths = map[string]bool{
	"/v1/messages": true,
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
// validated the upstream, so there is nothing here that can fail. origin is
// this service's own URL, which is how OpenRouter attributes the traffic.
func NewLLMGateway(key string, upstream *url.URL, origin string) *LLMGateway {
	return &LLMGateway{proxy: llmProxy(key, upstream, origin)}
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

// llmProxy forwards to the model service. SetURL joins the upstream's path with
// the request's own -- /api plus /v1/messages -- and keeps the query, the SDK's
// ?beta=true included. The header map is replaced rather than edited, so
// whatever the guest added is gone before the credential is set.
//
// OpenRouter authenticates with a bearer token, not the x-api-key Anthropic
// wants, and attributes traffic by the two headers below. All three are set
// here rather than in the guest, which is the point of the broker: the guest
// holds no credential and cannot claim to be somebody else.
//
// The upstream is named in a failure: it is no secret (it sits in the systemd
// unit), and when OPENROUTER_UPSTREAM points at a test host, which host failed
// is the one fact the line is for.
func llmProxy(key string, upstream *url.URL, origin string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(upstream)
			pr.Out.Header = keepHeaders(pr.Out.Header, llmForwardedHeaders)
			pr.Out.Header.Set("Authorization", "Bearer "+key)
			pr.Out.Header.Set("HTTP-Referer", origin)
			pr.Out.Header.Set("X-Title", llmAppName)
		},
		FlushInterval: -1, // responses may stream; buffering would stall them
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("llm gateway: the model service could not be reached: %v", err)
			http.Error(w, "the model service is not answering", http.StatusBadGateway)
		},
	}
}
