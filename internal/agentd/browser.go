package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"cracked/internal/util"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// The chrome-devtools-mcp server, as the node agent has always launched it.
// Vars rather than consts so a test can point at something else, matching how
// ChromeURL and the gate's timeouts are made testable.
var (
	// MCPServerPath is the stdio server agentd drives Chrome through.
	MCPServerPath = "/opt/agent/node_modules/chrome-devtools-mcp/build/src/bin/chrome-devtools-mcp.js"

	// MCPNodePath is absolute rather than bare "node": under systemd this
	// process inherits the default PATH, and depending on that is a boot-order
	// failure waiting to happen. The image installs Node solely for this.
	MCPNodePath = "/usr/bin/node"

	// MCPServerArgs are the flags beyond the browser URL. The three categories
	// are dead weight for a browsing agent and their schemas are charged on
	// every turn -- the node agent measured dropping four tools at ~1,100
	// tokens off the prefix.
	MCPServerArgs = []string{
		"--no-category-performance", "--no-category-network", "--no-category-emulation",
	}
)

// browserAllowed is the tool surface an agent with browser access gets.
//
// Mirrors the node agent's BROWSER_TOOLS minus the four it disallows for cost.
// 1.8.0 advertises about 53 tools -- heapsnapshot, extension, PWA and webmcp
// families -- and the ones not named here never reach the model at all.
//
// evaluate_script is in node's list and deliberately absent from this one: the
// agent browses untrusted pages, the API key is in the guest, and arbitrary
// page-driven JS is the sharpest edge of the standing exfiltration TODO.
// type_text is here for the editors fill cannot drive. fill on a contenteditable
// sets innerText and dispatches a bare synthetic input event; a rich-text editor
// like the one behind WhatsApp's message box ignores that and reconciles the DOM
// straight back to its own empty state. The tool then reports "Successfully
// filled out the element" and nothing was typed, which is the worst shape a
// failure can take: the first live run had an agent fall back to pressing keys
// one character at a time, 60 of them, to send a single message. type_text goes
// through keyboard.type, so the page sees real key events, and its submitKey
// sends in the same call.
var browserAllowed = map[string]bool{
	"navigate_page": true, "take_snapshot": true, "click": true, "fill": true,
	"fill_form": true, "type_text": true, "press_key": true, "wait_for": true,
	"take_screenshot": true, "list_pages": true, "handle_dialog": true,
	"new_page": true, "select_page": true, "close_page": true, "drag": true,
	"upload_file": true, "hover": true,
}

// browserListTimeout bounds the one-off spawn and tools/list an agent pays for
// on its first construction.
const browserListTimeout = 30 * time.Second

// browserServer owns the single chrome-devtools-mcp process for this machine.
//
// One per daemon, not one per agent: every agent shares the browser the person
// is watching, and a second server would be a second puppeteer connection to
// the same Chrome. Spawned lazily, because five of the six shipped profiles
// never open a page and a VM that never browses should never fork node.
type browserServer struct {
	url string

	// Where the server's own stderr goes. Empty in a test, which keeps it on
	// /dev/null exactly as before.
	logPath string

	// dial is how a session is obtained. A field so a test can stand a real MCP
	// server up in-process rather than forking node, which also makes the
	// respawn path testable -- it is the one that would otherwise only fail in
	// production, hours in, when Chrome restarts.
	dial func(context.Context) (*mcpsdk.ClientSession, error)

	mu    sync.Mutex
	sess  *mcpsdk.ClientSession
	tools []anthropic.BetaTool
}

// newBrowserServer prepares the manager. It starts nothing.
func newBrowserServer(chromeURL, stateDir string) *browserServer {
	s := &browserServer{url: chromeURL, logPath: mcpLogPath(stateDir)}
	s.dial = s.spawn
	return s
}

// Tools lists the browser surface, spawning the server on first use.
//
// Cached once obtained, because a tools/list costs a round trip and the surface
// cannot change under a pinned server. A failure is not cached: the next agent
// to be built retries, which is what makes a Chrome that is not up yet a delay
// rather than a browser-less agent for the life of the process.
func (s *browserServer) Tools(ctx context.Context, d toolDeps) ([]anthropic.BetaTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tools != nil {
		return s.tools, nil
	}
	sess, err := s.sessionLocked(ctx)
	if err != nil {
		return nil, err
	}
	listed, err := listBrowserTools(ctx, sess)
	if err != nil {
		return nil, err
	}
	s.tools = wrapAll(listed, s, d)
	return s.tools, nil
}

// session hands back a live session, reconnecting if the last one died.
func (s *browserServer) session(ctx context.Context) (*mcpsdk.ClientSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionLocked(ctx)
}

// sessionLocked connects on first use. Caller holds s.mu.
func (s *browserServer) sessionLocked(ctx context.Context) (*mcpsdk.ClientSession, error) {
	if s.sess != nil {
		return s.sess, nil
	}
	sess, err := s.dial(ctx)
	if err != nil {
		return nil, err
	}
	s.sess = sess
	return sess, nil
}

// mcpLogPath is where the browser server's stderr is kept, or "" for nowhere.
func mcpLogPath(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return filepath.Join(stateDir, "mcp.log")
}

// mcpLogCap bounds the browser server's log. The guest writes to a 5 GiB overlay
// it shares with the person's Chrome profile and downloads, so nothing here is
// allowed to grow without a limit.
const mcpLogCap = 8 << 20

// stderrTo opens the server's log, or nil to leave stderr on /dev/null.
//
// The old log is dropped rather than appended to: a respawn is what you are
// debugging, and the previous boot's noise only makes it harder to find. Best
// effort throughout -- a browser that runs unlogged beats one that will not start
// because a log file could not be opened.
func (s *browserServer) stderrTo() io.Writer {
	if s.logPath == "" {
		return nil
	}
	_ = os.Remove(s.logPath)
	w, err := util.NewCappedWriter(s.logPath, mcpLogCap)
	if err != nil {
		return nil
	}
	return w
}

// spawn starts the server and completes the MCP handshake.
//
// Wiring stderr is not cosmetic. os/exec sends a nil Stderr to /dev/null, so
// until this the server's own diagnostics -- the only account of why a click or
// a fill did not take -- existed nowhere at all, and a browser failure could be
// seen but never explained.
func (s *browserServer) spawn(ctx context.Context) (*mcpsdk.ClientSession, error) {
	cmd := exec.Command(MCPNodePath, append([]string{MCPServerPath, "--browserUrl", s.url},
		MCPServerArgs...)...)
	cmd.Env = serverEnv()
	if f := s.stderrTo(); f != nil {
		cmd.Stderr = f
	}
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "agentd", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcpsdk.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("could not start the browser server: %w", err)
	}
	return sess, nil
}

// serverEnv turns off the two things the server does on its own behalf.
//
// The update check is awaited before the server parses its arguments and shells
// out to `npm config get registry` -- and the image has no npm once trimmed, so
// it would burn the five second execSync timeout on every start. The usage
// statistics are Clearcut telemetry; this guest drives a person's signed-in
// browser and should not be reporting on it.
func serverEnv() []string {
	return append(os.Environ(),
		"CHROME_DEVTOOLS_MCP_NO_UPDATE_CHECKS=1",
		"CHROME_DEVTOOLS_MCP_NO_USAGE_STATISTICS=1")
}

// drop retires a session that has died, so the next call reconnects.
func (s *browserServer) drop(dead *mcpsdk.ClientSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == dead {
		s.sess = nil
	}
}

// Close stops the server, if one was ever started.
func (s *browserServer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess != nil {
		s.sess.Close()
		s.sess = nil
	}
}

// listBrowserTools reads every page of tools/list and keeps the allowed ones.
//
// Paging is not optional. ListToolsResult carries a NextCursor, so a single
// call can return a partial list -- and a tool that never arrives simply never
// reaches the model, with nothing anywhere reporting an error.
func listBrowserTools(ctx context.Context, sess *mcpsdk.ClientSession) ([]*mcpsdk.Tool, error) {
	var out []*mcpsdk.Tool
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		if browserAllowed[tool.Name] {
			out = append(out, tool)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("the browser server advertised none of the tools we use")
	}
	return out, nil
}
