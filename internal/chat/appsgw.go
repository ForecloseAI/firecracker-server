package chat

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
)

// appsGatewayPrefix is where a guest dials its own session.
const appsGatewayPrefix = "/apps/"

// AppsGateway lets a guest reach its own app-integration session without ever
// holding the credential that opens it.
//
// The provider's MCP endpoint turned out to need the PROJECT api key -- the
// session URL alone answers Unauthorized -- and that key is authority over every
// user's connected accounts. A guest is a machine its owner has root on, so a
// key that reads everybody's email cannot live there. So the host holds the
// credential and the guest holds a ticket to its own session. The model broker
// beside it (LLMGateway) has the same shape minus the ticket, and says why.
//
// A ticket is safe to hand over because it is worthless anywhere else. It names
// no user, it is pinned to one guest's address, and the port it works on is
// reachable only from a tap device on this host.
type AppsGateway struct {
	key  string
	port string

	mu     sync.Mutex
	routes map[string]appsRoute
}

// errNoGuestAddress is what registering a machine with no address is refused
// with. Its ticket could never be served, so minting one would be a lie.
var errNoGuestAddress = errors.New("cannot route to a machine with no address")

// appsRouteCap bounds the table. Only MaxVMs machines are ever live, but a
// long-running process accumulates a route per machine it has ever pushed to,
// and an unbounded map on a service that runs for weeks is a leak by another
// name. Far above the fleet, so nothing live is ever evicted in practice.
const appsRouteCap = 256

// appsRoute is one machine's way through to its session.
type appsRoute struct {
	machine  string
	guestIP  string
	upstream *url.URL
}

// NewAppsGateway prepares the broker, or nil when no provider is configured.
func NewAppsGateway(key, addr string) *AppsGateway {
	if key == "" || addr == "" {
		return nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	return &AppsGateway{key: key, port: port, routes: map[string]appsRoute{}}
}

// Register points one machine at its session and returns the URL it should dial.
//
// A fresh ticket every time, and the machine's old one is dropped: slots are
// recycled, so a ticket that outlived its machine is exactly how one person's
// agent would end up acting as another.
func (g *AppsGateway) Register(machine, guestIP, hostIP, upstream string) (string, error) {
	// A machine still coming up has no address yet, and route() rejects a ticket
	// whose guestIP is empty -- so registering one mints a URL that can only ever
	// 404, while the caller is told the push succeeded. It also skips the
	// address half of dropLocked, leaving any stale route for a recycled address
	// in place.
	if guestIP == "" {
		return "", errNoGuestAddress
	}
	target, err := url.Parse(upstream)
	if err != nil {
		return "", err
	}
	ticket, err := newTicket()
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dropLocked(machine, guestIP)
	evictTo(g.routes, appsRouteCap)
	g.routes[ticket] = appsRoute{machine: machine, guestIP: guestIP, upstream: target}
	return "http://" + net.JoinHostPort(hostIP, g.port) + appsGatewayPrefix + ticket, nil
}

// Forget drops a machine's way through, so a recreated one cannot inherit it.
func (g *AppsGateway) Forget(machine string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.dropLocked(machine, "")
}

// dropLocked removes every route for a machine or an address. Caller holds g.mu.
//
// Both keys, because either one being stale is a way to serve the wrong person:
// the machine may have been recreated, and the address may have been recycled
// into a different machine entirely.
func (g *AppsGateway) dropLocked(machine, guestIP string) {
	for ticket, route := range g.routes {
		if route.machine == machine || (guestIP != "" && route.guestIP == guestIP) {
			delete(g.routes, ticket)
		}
	}
}

// route resolves a ticket, but only for the guest it was issued to.
//
// Why a route left behind by a machine deleted on the control plane directly --
// where this service never hears about it -- is still not a leak: serving it
// needs the TICKET as well as the address, the ticket is 128 unguessable bits,
// and the only copies were on that machine's own disk and in this map. A machine
// that later lands on the recycled address has a different id, so a different
// workspace, so no copy of it. The two checks are a conjunction on purpose;
// neither is load-bearing alone.
//
// Two checks, not one. The ticket says which session; the source address says
// who is asking. A guest cannot choose its own source address -- each tap is a
// point-to-point /30 owned by the host -- so requiring both means a leaked
// ticket is useless from anywhere but the one machine it was minted for.
func (g *AppsGateway) route(ticket, remote string) (appsRoute, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	r, ok := g.routes[ticket]
	if !ok || r.guestIP == "" || r.guestIP != remote {
		return appsRoute{}, false
	}
	return r, true
}

// serve resolves the ticket and, if it names a route this guest may use, hands
// the request to a proxy built for that one route.
//
// The proxy is built per request so it can close over the route. Passing it
// through a context value instead meant the rewrite step had an "if the gate
// refused this, do nothing" branch that could never run, and the setter lived
// in a third file -- four pieces that had to agree, two of them unreachable.
func (g *AppsGateway) serve(w http.ResponseWriter, r *http.Request) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	route, ok := g.route(ticketIn(r.URL.Path), host)
	if !ok {
		log.Printf("apps gateway: refused a request from %s", host)
		http.NotFound(w, r)
		return
	}
	g.proxyTo(route).ServeHTTP(w, r)
}

// ticketIn reads the ticket out of a request path, ignoring anything after it.
func ticketIn(path string) string {
	ticket := strings.TrimPrefix(path, appsGatewayPrefix)
	if i := strings.IndexByte(ticket, '/'); i >= 0 {
		return ticket[:i]
	}
	return ticket
}

// forwardedHeaders is everything a guest is allowed to send upstream.
//
// An ALLOW list, not a list of things to strip. The guest composes these and it
// is untrusted, so a strip list is a promise to have thought of every header
// worth removing -- and the next one the provider starts honouring is a header
// nobody remembered to add. These five are exactly what the MCP streamable
// client sets; everything else, authentication included, is dropped.
var forwardedHeaders = []string{
	"Content-Type", "Accept", "Mcp-Protocol-Version", "Mcp-Session-Id", "Last-Event-Id",
}

// proxyTo forwards one guest's MCP traffic to its session, adding the credential
// the guest deliberately does not hold.
func (g *AppsGateway) proxyTo(route appsRoute) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL, r.Out.Host = route.upstream, route.upstream.Host
			r.Out.Header = keepHeaders(r.Out.Header, forwardedHeaders)
			r.Out.Header.Set("x-api-key", g.key)
		},
		FlushInterval: -1, // the MCP transport streams; buffering would stall it
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			// Neither the guest nor the journal learns where the session lives.
			// An error string names the host, the path and the session id in one
			// line, and the journal is what gets read aloud when debugging.
			log.Printf("apps gateway: %s could not reach its session: %s",
				route.machine, hideURL(err, route.upstream.String()))
			http.Error(w, "the app service is not answering", http.StatusBadGateway)
		},
	}
}

// hideURL renders an error with the upstream taken out of it.
//
// Two layers, for the reason agentd's own redaction has two: a transport failure
// is a *url.Error, which prints its whole URL field, and if the client normalised
// what we handed it the plain substring no longer matches. Deliberately a second
// copy rather than a shared helper -- the two live either side of the tap and
// neither package should have to import the other to keep a secret.
func hideURL(err error, upstream string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if upstream != "" {
		msg = strings.ReplaceAll(msg, upstream, "[the session]")
	}
	var ue *url.Error
	if errors.As(err, &ue) && ue.URL != "" {
		msg = strings.ReplaceAll(msg, ue.URL, "[the session]")
	}
	return msg
}

// newTicket mints an unguessable name for one machine's route.
func newTicket() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
