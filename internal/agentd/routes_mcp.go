package agentd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cracked/internal/agentapi"
)

// Body caps. A URL plus two headers is a few hundred bytes; a pathological
// bearer token runs to a few KB, so anything past these is a client bug.
const (
	mcpBodyCap  = 8 << 10
	mcpPatchCap = 1 << 10
)

// handleListMCP reports every registered server. It DELIBERATELY does not dial.
//
// Supervisor.List's rule, one step further: a poll must not be able to spawn
// work, and a poll that opens a TCP connection to a third party is worse than
// one that starts a goroutine -- an app on a two-second refresh would hammer
// somebody else's server from the person's own address. Reachable is the last
// registration probe, and the field says so.
func (s *Server) handleListMCP(w http.ResponseWriter, r *http.Request) {
	records := s.sup.MCP().Store().List()
	out := make([]agentapi.MCPServer, 0, len(records))
	for _, rec := range records {
		out = append(out, redact(rec))
	}
	reply(w, http.StatusOK, out)
}

// handleAddMCP validates a registration and hands it to the probe.
func (s *Server) handleAddMCP(w http.ResponseWriter, r *http.Request) {
	var in agentapi.MCPRegistration
	if !decode(w, r, mcpBodyCap, &in) {
		return
	}
	rec, err := recordFrom(in)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", err.Error(), "mcp")
		return
	}
	// The fast path: refuse a duplicate before spending a probe on it. Add
	// rechecks under the store's own lock, which is what catches a second
	// registration that arrives while this one is still dialling.
	if s.sup.MCP().Store().HasURL(rec.URL) {
		fail(w, http.StatusConflict, "conflict", errDuplicateURL.Error(), "mcp")
		return
	}
	s.registerMCP(w, rec)
}

// registerMCP connects, lists what the server offers, and stores it.
//
// Nothing is stored when the probe fails: a half-registered server whose tools
// never resolve would look successful in a list and do nothing forever.
func (s *Server) registerMCP(w http.ResponseWriter, rec mcpRecord) {
	ctx, cancel := context.WithTimeout(context.Background(), mcpProbeTimeout)
	defer cancel()
	specs, err := probeRemote(ctx, rec)
	if err != nil {
		fail(w, http.StatusBadGateway, "unreachable", probeReason(err), "mcp")
		return
	}
	rec.Tools, rec.ProbedAt = specs, time.Now().UTC()
	stored, err := s.sup.MCP().Store().Add(rec)
	if errors.Is(err, errDuplicateURL) {
		// The handler's check passed and then this probe took seconds, during
		// which another registration of the same address landed. Still a 409:
		// the person sees the conflict they would have got had they not raced.
		fail(w, http.StatusConflict, "conflict", err.Error(), "mcp")
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, "write_failed", err.Error(), "mcp")
		return
	}
	s.finishMCP(w, stored)
}

// finishMCP refuses a server none of whose tools can be offered, and otherwise
// makes the registration take effect now.
func (s *Server) finishMCP(w http.ResponseWriter, rec mcpRecord) {
	out := redact(rec)
	if len(out.Tools) == 0 {
		s.sup.MCP().Store().Remove(rec.ID)
		fail(w, http.StatusBadRequest, "bad_request",
			"none of this server's tools can be offered under a name the model can call", "mcp")
		return
	}
	s.applyMCP(rec.ID)
	reply(w, http.StatusCreated, out)
}

// handlePatchMCP is the enable switch, and only that. Changing a URL or a token
// is a delete and a re-add, so a live session can never outlive the credentials
// that opened it.
func (s *Server) handlePatchMCP(w http.ResponseWriter, r *http.Request) {
	var in agentapi.MCPUpdate
	if !decode(w, r, mcpPatchCap, &in) {
		return
	}
	if in.Enabled == nil {
		fail(w, http.StatusBadRequest, "bad_request",
			"say whether to enable or disable this server", "mcp")
		return
	}
	rec, err := s.sup.MCP().Store().SetEnabled(r.PathValue("id"), *in.Enabled)
	if err != nil {
		fail(w, http.StatusNotFound, "not_found", err.Error(), "mcp")
		return
	}
	s.applyMCP(rec.ID)
	reply(w, http.StatusOK, redact(rec))
}

// handleRemoveMCP forgets a server and closes its session.
func (s *Server) handleRemoveMCP(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.sup.MCP().Store().Remove(id); err != nil {
		fail(w, http.StatusNotFound, "not_found", err.Error(), "mcp")
		return
	}
	s.applyMCP(id)
	w.WriteHeader(http.StatusNoContent)
}

// applyMCP makes a change reach the agents.
//
// Two steps, and both are needed. Refresh retires the live connection and points
// the holder at what the store now says, so a disabled or deleted server stops
// answering an agent that is mid-turn and a re-enabled one starts again for that
// same agent. Evicting covers the frozen surface: tools are assembled once when
// an agent is built, so without this a change to WHICH tools exist reaches
// nobody until one happens to be recycled -- exactly what PUT /person already
// does, and for the same reason. An agent that is mid-turn is left alone by
// EvictIdle and keeps its old surface until it is next recycled.
func (s *Server) applyMCP(id string) {
	s.sup.MCP().Refresh(id)
	s.sup.EvictIdle()
}

// recordFrom validates a registration and turns it into a record to probe.
func recordFrom(in agentapi.MCPRegistration) (mcpRecord, error) {
	if err := checkURL(in.URL); err != nil {
		return mcpRecord{}, err
	}
	transport := orDefault(in.Transport, transportHTTP)
	if transport != transportHTTP && transport != transportSSE {
		return mcpRecord{}, fmt.Errorf("%q is not a transport this understands: http or sse", transport)
	}
	return mcpRecord{Name: strings.TrimSpace(in.Name), URL: in.URL,
		Transport: transport, Headers: in.Headers}, nil
}

// checkURL refuses a URL this guest provably cannot reach, and one that would be
// a bad idea if it could.
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errors.New("this is not a URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q is not a scheme this can use: http or https", u.Scheme)
	}
	if unreachableHost(u.Hostname()) {
		return errors.New("this address is on a private network, which this machine's " +
			"agents cannot reach; a server they can use has to be on the public internet")
	}
	return nil
}

// unreachableHost reports whether a literal address is one the guest firewall
// drops.
//
// Egress is masqueraded to the internet, but link-local and the RFC1918 ranges
// are dropped and the host refuses new inbound connections, so a URL naming the
// person's laptop or our own VPC can never connect. Caught here it is an
// instant, honest refusal; missed, it is a fifteen second timeout that reads as
// "your server is broken". A HOSTNAME that resolves into one of these still
// degrades to a timeout, which is why probeReason names the possibility too.
//
// Loopback is deliberately NOT on this list. The firewall's drops sit on FORWARD
// and on INPUT from the tap, so nothing stops a guest talking to itself -- and a
// server the agent installed in its own workspace is a real thing to register.
// Refusing it here would be a rule invented rather than enforced.
func unreachableHost(host string) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLinkLocalUnicast() || ip.IsPrivate() || ip.IsUnspecified()
}

// probeReason explains a failed registration in terms the person can act on.
//
// The two causes need opposite answers and would otherwise arrive as the same
// 502: a server that refuses the handshake means the token is wrong, and a
// server that never answers may be somewhere this machine can never reach.
// Handing both back as "your server did not answer" makes the commonest mistake
// -- a mistyped header -- the least diagnosable one.
func probeReason(err error) string {
	msg := err.Error()
	for _, refusal := range []string{"401", "Unauthorized", "403", "Forbidden"} {
		if strings.Contains(msg, refusal) {
			return "this server refused the credentials it was given: " + msg
		}
	}
	return msg + " - if this address is only reachable from a private network, " +
		"this machine's agents cannot get to it"
}
