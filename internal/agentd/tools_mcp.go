package agentd

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpCallTimeout bounds one call into a registered server. The browser has
// none: it is a local process on the same machine, and a slow page action is
// still a real one. A third party over HTTPS is not owed the whole turn.
var mcpCallTimeout = 60 * time.Second

// MCPManager owns every registered remote server on this machine.
//
// One per daemon, beside the browser and for the same reason: a session is a
// connection to a third party, and one per agent would multiply them by the
// roster. Sessions are lazy -- registering a server must not cost a connection
// held open for the life of the VM.
type MCPManager struct {
	store *MCPStore

	// dial is how a session is obtained. A field so a test can stand a real MCP
	// server up in-process rather than over HTTP, which is also what makes the
	// disable-mid-turn path testable.
	dial func(context.Context, mcpRecord) (*mcpsdk.ClientSession, error)

	mu sync.Mutex
	by map[string]*remoteServer
}

// NewMCPManager prepares the manager. It connects to nothing.
func NewMCPManager(store *MCPStore) *MCPManager {
	return &MCPManager{store: store, dial: dialRemote, by: map[string]*remoteServer{}}
}

// Store exposes the registered set, for the HTTP surface.
func (m *MCPManager) Store() *MCPStore { return m.store }

// Tools is the whole registered surface, built from the STORE.
//
// It dials nothing. An agent must start when a registered server is down, and
// the schemas it needs were captured when the person registered it -- so this
// touches no network and no disk, and a slow third party cannot add a second to
// any agent's cold start.
func (m *MCPManager) Tools() []anthropic.BetaTool {
	var out []anthropic.BetaTool
	for _, rec := range m.store.Enabled() {
		out = append(out, wrapSpecs(rec.Tools, m.serverFor(rec), rec.ID, mcpCallTimeout)...)
	}
	return out
}

// serverFor returns the connection holder for a record, creating it on first
// use. Caller must not hold a remoteServer's own lock.
func (m *MCPManager) serverFor(rec mcpRecord) *remoteServer {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.by[rec.ID]; ok {
		return s
	}
	s := &remoteServer{rec: rec, dial: m.dial, enabled: true}
	m.by[rec.ID] = s
	return s
}

// Forget retires a server's connection, for disable, for re-enable and for
// removal alike.
//
// Two steps, and both matter. Dropping it from the map means the next build
// reads the store fresh. Disabling the retired holder is what makes the change
// bite MID-TURN: a running agent still holds wrappers pointing at this object,
// and without the flag its next call would quietly redial.
func (m *MCPManager) Forget(id string) {
	m.mu.Lock()
	s, ok := m.by[id]
	delete(m.by, id)
	m.mu.Unlock()
	if !ok {
		return
	}
	s.setEnabled(false)
	s.Close()
}

// Close retires every connection this machine has open.
func (m *MCPManager) Close() {
	m.mu.Lock()
	all := make([]*remoteServer, 0, len(m.by))
	for id, s := range m.by {
		all = append(all, s)
		delete(m.by, id)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.Close()
	}
}

// remoteServer is one registered server's connection: browserServer's shape,
// minus the process and plus the flag that turns it off.
type remoteServer struct {
	rec  mcpRecord
	dial func(context.Context, mcpRecord) (*mcpsdk.ClientSession, error)

	mu      sync.Mutex
	enabled bool
	sess    *mcpsdk.ClientSession
}

// label is what a failure message calls this server: the name the person gave
// it, so a Notion failure does not read as a browser failure.
func (s *remoteServer) label() string { return orDefault(s.rec.Name, "this server") }

// session hands back a live session, connecting on first use and refusing
// outright once the server has been turned off.
//
// The refusal is what makes disable mean something. Closing the session alone
// is not enough: mcpTool.call retries through a FRESH dial by design -- that is
// what survives Chrome restarting -- so a disabled server would quietly
// reconnect on the very next call, and disable would be decorative until the
// agent happened to be recycled.
func (s *remoteServer) session(ctx context.Context) (*mcpsdk.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return nil, errors.New("has been turned off on this machine")
	}
	if s.sess != nil {
		return s.sess, nil
	}
	sess, err := s.dial(ctx, s.rec)
	if err != nil {
		return nil, err
	}
	s.sess = sess
	return sess, nil
}

// drop retires a session that has died, so the next call reconnects.
func (s *remoteServer) drop(dead *mcpsdk.ClientSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == dead {
		s.sess = nil
	}
}

// setEnabled turns the server on or off for calls already in flight.
func (s *remoteServer) setEnabled(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = on
}

// Close ends the session, if one was ever opened.
func (s *remoteServer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess != nil {
		s.sess.Close()
		s.sess = nil
	}
}

// mcpTools is the surface every registered server contributes.
//
// No error return, because there is nothing here that can fail: the wrappers
// come off stored schemas, not off a network. That is tools_browser's "degrade,
// never refuse to start" taken one step further -- there is no degraded case.
//
// Registered servers need no profile opt-in: the person added them for this
// machine, and a tools: list written months ago cannot name a server registered
// this morning. If that ever needs narrowing, the smallest fix is a profile
// field handled in permitted(), not a per-agent store.
func mcpTools(d toolDeps) []anthropic.BetaTool {
	if d.mcp == nil {
		return nil
	}
	return d.mcp.Tools()
}
