package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The transports a person may register. Remote HTTP only: a stdio server would
// be an arbitrary command line the app could run as root inside the guest, and
// the browser is the one local server this daemon owns.
const (
	transportHTTP = "http" // streamable HTTP, the current spec
	transportSSE  = "sse"  // the legacy two-endpoint transport
)

// mcpProbeTimeout bounds a registration: connect, handshake, and every page of
// tools/list.
//
// It must stay well under internal/agent's 30s write client, or the host gives
// up first and reports a failure on a registration that SUCCEEDED -- and the
// person's retry then collides with the server they were just told was not
// registered. A var so a test can shrink it.
var mcpProbeTimeout = 15 * time.Second

// probeRetries disables the transport's own reconnect loop for a registration.
//
// Not a tuning knob. StreamableClientTransport detaches its connection from the
// caller's context, so mcpProbeTimeout does NOT bound its reconnect backoff: a
// closed port measured 75 seconds to come back, five times the deadline this
// package sets and well past internal/agent's 30s write client -- which is
// precisely how the host reports a failure on a registration that succeeded, and
// the person's retry then collides with the server they were told was not
// registered. Someone waiting on a registration wants the first answer anyway,
// not the fifth.
const probeRetries = -1

// mcpKeepAlive pings a registered server and closes one that stops answering.
//
// The browser deliberately has none: a node process that dies takes its pipe
// with it and the next write fails at once. A hung HTTPS connection to a third
// party does not fail at all -- it simply stops answering -- and without this
// every later call would wait out its own timeout for the life of the VM.
var mcpKeepAlive = 30 * time.Second

// mcpToolSpec is one tool as the registration probe found it, frozen so agents
// can be built with no network at all.
//
// Schema stays raw JSON rather than a decoded struct: anything that round-trips
// through a Go type drops $defs, anyOf and additionalProperties, which is the
// exact bug schemaOf exists to avoid.
type mcpToolSpec struct {
	Name   string          `json:"name"`
	Desc   string          `json:"description,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// mcpRecord is one registered remote MCP server. Headers hold secrets in the
// clear, consistent with the API key already baked into this image; they are
// removed by redaction before anything encodes this for a client.
type mcpRecord struct {
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	URL       string            `json:"url"`
	Transport string            `json:"transport"`
	Headers   map[string]string `json:"headers,omitempty"`
	Enabled   bool              `json:"enabled"`
	CreatedAt time.Time         `json:"created_at"`
	Tools     []mcpToolSpec     `json:"tools"`

	// The LAST probe, from registration. Read routes report this rather than
	// dialling: a poll that opens a TCP connection to a third party is worse
	// than one that starts a goroutine, which Supervisor.List already forbids.
	ProbedAt time.Time `json:"probed_at"`
	ProbeErr string    `json:"probe_error,omitempty"`
}

// dialRemote opens a session to one registered server.
func dialRemote(ctx context.Context, rec mcpRecord) (*mcpsdk.ClientSession, error) {
	return connect(ctx, rec, 0) // 0 leaves the transport's own reconnect budget
}

// dialProbe is the registration dial: one attempt, no reconnect loop.
func dialProbe(ctx context.Context, rec mcpRecord) (*mcpsdk.ClientSession, error) {
	return connect(ctx, rec, probeRetries)
}

// dialed is one finished attempt, so the watchdog below can hand back a session
// or an error through a single channel and never leak the goroutine.
type dialed struct {
	sess *mcpsdk.ClientSession
	err  error
}

// connect opens a session with the given reconnect budget, under a watchdog.
//
// The watchdog is not belt and braces. Measured against an address that accepts
// nothing and says nothing -- the shape a wrong hostname or a firewalled server
// takes -- the SDK's streamable client took 63 SECONDS to return from a three
// second context. That is twenty times the deadline and well past
// internal/agent's 30s write client, which is exactly how the host comes to
// report a failure on a registration that actually stored, leaving the person's
// retry to collide with a server they were told was not registered. A caller's
// deadline has to mean something here, so it is enforced rather than trusted.
//
// The abandoned attempt closes whatever session it eventually gets, so a dial we
// stopped waiting for cannot leave a connection open behind us.
func connect(ctx context.Context, rec mcpRecord, retries int) (*mcpsdk.ClientSession, error) {
	tr, err := transportFor(rec, retries)
	if err != nil {
		return nil, err
	}
	done := start(ctx, tr)
	select {
	case d := <-done:
		if d.err != nil {
			return nil, fmt.Errorf("could not reach this server: %w", d.err)
		}
		return d.sess, nil
	case <-ctx.Done():
		go abandon(done)
		return nil, errors.New("this server did not answer in time")
	}
}

// start runs one dial in the background, reporting through a buffered channel so
// the attempt can finish even after nobody is waiting for it.
func start(ctx context.Context, tr mcpsdk.Transport) <-chan dialed {
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "agentd", Version: "1"},
		&mcpsdk.ClientOptions{KeepAlive: mcpKeepAlive})
	done := make(chan dialed, 1)
	go func() {
		sess, err := client.Connect(ctx, tr, nil)
		done <- dialed{sess, err}
	}()
	return done
}

// abandon retires the session from a dial nobody is waiting for any more.
func abandon(done <-chan dialed) {
	if d := <-done; d.sess != nil {
		d.sess.Close()
	}
}

// transportFor picks the client transport a record declared. An empty string is
// the streamable one, so a client that omits the field gets the current spec.
func transportFor(rec mcpRecord, retries int) (mcpsdk.Transport, error) {
	hc := remoteHTTPClient(rec)
	switch rec.Transport {
	case transportSSE:
		return &mcpsdk.SSEClientTransport{Endpoint: rec.URL, HTTPClient: hc}, nil
	case transportHTTP, "":
		return &mcpsdk.StreamableClientTransport{Endpoint: rec.URL, HTTPClient: hc,
			MaxRetries: retries}, nil
	}
	return nil, fmt.Errorf("unknown transport %q", rec.Transport)
}

// remoteHTTPClient carries the registered auth headers and refuses to follow a
// redirect off the host the server was registered on.
//
// The redirect rule is not caution: the round tripper below adds the person's
// token to every request it sees, so a server that 302s elsewhere would have
// their credential delivered to a host they never named, and nothing else in
// this system would catch it.
//
// No Timeout, deliberately. A streamable session holds a standalone SSE GET
// open for its whole life, and a client deadline would cut it -- which reads as
// a server that dies on a timer. Per-call bounding comes from the context.
func remoteHTTPClient(rec mcpRecord) *http.Client {
	host := hostOf(rec.URL)
	return &http.Client{
		Transport: headerRoundTripper{base: http.DefaultTransport, headers: rec.Headers},
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Host != host {
				return fmt.Errorf("this server redirected to %s, which is not where it was registered", r.URL.Host)
			}
			return nil
		},
	}
}

// headerRoundTripper adds the registered auth headers to every request.
//
// A header the person pasted is the supported way to authenticate: the SDK's own
// OAuth client sits behind the mcp_go_client_oauth build tag and does not
// compile in by default.
type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

// RoundTrip sets the headers on a COPY of the request. net/http reuses a request
// across retries and redirects, so mutating the caller's own header map is how a
// token ends up on a request that was never meant to carry it.
func (h headerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if len(h.headers) == 0 {
		return h.base.RoundTrip(r)
	}
	out := r.Clone(r.Context())
	for k, v := range h.headers {
		out.Header.Set(k, v)
	}
	return h.base.RoundTrip(out)
}

// probeRemote connects, lists every tool, and closes.
//
// Closing is the point: registration must not leave a connection open, or a
// person who adds four servers holds four idle sockets for the life of the VM.
func probeRemote(ctx context.Context, rec mcpRecord) ([]mcpToolSpec, error) {
	sess, err := dialProbe(ctx, rec)
	if err != nil {
		return nil, err
	}
	defer sess.Close()
	return listRemoteTools(ctx, sess)
}

// listRemoteTools reads every page of tools/list.
//
// Paging is not optional. ListToolsResult carries a NextCursor, so one call can
// return a partial list -- and a tool that never arrives simply never reaches
// the model, with nothing anywhere reporting an error.
func listRemoteTools(ctx context.Context, sess *mcpsdk.ClientSession) ([]mcpToolSpec, error) {
	var out []mcpToolSpec
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		out = append(out, specOf(tool))
	}
	if len(out) == 0 {
		return nil, errors.New("this server answered but advertised no tools")
	}
	return out, nil
}

// specOf freezes one advertised tool, keeping the schema exactly as it arrived.
func specOf(t *mcpsdk.Tool) mcpToolSpec {
	spec := mcpToolSpec{Name: t.Name, Desc: t.Description}
	if raw, err := json.Marshal(t.InputSchema); err == nil {
		spec.Schema = raw
	}
	return spec
}

// hostOf is a URL's host, or "" when it will not parse -- which the redirect
// check reads as "matches nothing", so an unparseable URL follows no redirect.
func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
