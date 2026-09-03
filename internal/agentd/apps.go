package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	neturl "net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"cracked/internal/agentapi"

	"github.com/anthropics/anthropic-sdk-go"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// appsListTimeout bounds the dial and tools/list an agent pays for on its first
// construction. Kept short deliberately: building an agent is in the path of a
// person's message, so this is time they spend watching nothing happen.
const appsListTimeout = 15 * time.Second

// appsRetryAfter is how long a failed dial is remembered.
//
// The browser caches no failure, because it is a local process and a refusal
// comes back instantly. This one crosses the internet, where an unreachable host
// does not refuse -- it hangs until the timeout. Without a cooldown, a provider
// having a bad ten minutes would add appsListTimeout to EVERY message sent to an
// evicted agent for the whole ten minutes. Short enough that a blip heals on its
// own, long enough that an outage costs one stall rather than one per message.
const appsRetryAfter = 30 * time.Second

// appsPath is where the machine keeps the session its agents dial.
//
// One file for the whole machine, like the timezone and the roster: every agent
// here works for the same person, so they all reach the same connected accounts.
func appsPath(stateDir string) string {
	return filepath.Join(stateDir, "apps.json")
}

// ReadApps returns the app-integration session this machine holds, or a zero
// value when it has none -- which is the ordinary shape, not an error.
func ReadApps(stateDir string) agentapi.Apps {
	var a agentapi.Apps
	body, err := os.ReadFile(appsPath(stateDir))
	if err != nil {
		return agentapi.Apps{}
	}
	if json.Unmarshal(body, &a) != nil {
		return agentapi.Apps{}
	}
	return a
}

// WriteApps records the session the host pushed.
//
// Through writeAtomic like every other durable file here, because ReadApps
// swallows a parse error and returns "no session": a half-written apps.json
// would take the machine's app surface away silently, and the host would go on
// believing it had pushed one.
func WriteApps(stateDir string, a agentapi.Apps) error {
	body, err := json.Marshal(a)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o750); err != nil {
		return err
	}
	return writeAtomic(appsPath(stateDir), body)
}

// errNotDialable is what a session URL this machine could not safely dial is
// refused with. Declared once so the route and its test cannot drift.
var errNotDialable = errors.New(
	"a session url must be https, or http to a private address")

// sessionPlaceholder is what the session URL is replaced with on its way into
// anything a person or a model can read.
const sessionPlaceholder = "[the connected-apps session]"

// redactURL takes the session URL out of an error.
//
// Transport errors wrap Go's url.Error, which embeds the whole request URL --
// so `Post "https://.../mcp/sess_XXX": dial tcp ...` is what a failed call
// actually says. That string reaches the model as a tool result and lands in the
// event log, and the URL is a bearer credential for everything this person has
// connected. A prompt injection could then read it out of its own context.
func redactURL(err error, url string) error {
	if err == nil {
		return err
	}
	msg := err.Error()
	if url != "" {
		msg = strings.ReplaceAll(msg, url, sessionPlaceholder)
	}
	// The chain is walked as well as the string matched. A *url.Error prints its
	// own URL field, and if the client normalised what we dialled on the way --
	// a trailing slash, a redirect -- the substring above no longer matches and
	// the credential would sail straight through. Taking it out by the value the
	// error itself carries does not depend on the two agreeing.
	//
	// Note there is no stdlib switch for this: net/url's Error has Op, URL and
	// Err and nothing else as of Go 1.27, so the OmitURL field discussed in
	// golang/go#24572 is not something to reach for.
	var ue *neturl.Error
	if errors.As(err, &ue) && ue.URL != "" {
		msg = strings.ReplaceAll(msg, ue.URL, sessionPlaceholder)
	}
	if msg == err.Error() {
		return err
	}
	return redacted{msg: msg, cause: err}
}

// redacted is an error whose message has been rewritten but whose cause is
// intact.
//
// errors.New would have been simpler and was what shipped, but it throws the
// chain away -- and the case that most wants classifying downstream, a timeout
// on a URL we had to redact, is exactly the case where the message changed. So
// errors.Is(err, context.DeadlineExceeded) went false precisely when it
// mattered.
type redacted struct {
	msg   string
	cause error
}

// Error is the rewritten message, with nothing secret left in it.
func (r redacted) Error() string { return r.msg }

// Unwrap keeps errors.Is and errors.As working through the redaction.
func (r redacted) Unwrap() error { return r.cause }

// errRecentlyFailed is what an agent built during a cooldown is told. It reads
// into the agent's log, never to the model: the agent simply has no app tools
// this time round, and the next one after the cooldown tries again.
var errRecentlyFailed = errors.New("the connected-apps session was unreachable a moment ago")

// errNoSession is what a machine the host has pushed nothing to answers with.
var errNoSession = errors.New("this machine has no connected-apps session")

// errRepointed means the host moved this machine to another session while a
// dial to the old one was in flight. The next call picks up the new address.
var errRepointed = errors.New("the connected-apps session moved while dialling")

// appsServer owns this machine's connection to the app-integration session.
//
// One per daemon, like the browser: the session is scoped to the person, and
// every agent on the machine works for that same person. Dialled lazily, so a
// machine that never touches a connected app never opens the socket.
type appsServer struct {
	// dial is how a session is obtained. A field so a test can stand a real MCP
	// server up in-process rather than reaching the internet.
	dial func(context.Context, string) (*mcpsdk.ClientSession, error)

	// mu guards the four fields below and is held for microseconds at a time.
	// dialMu serialises the dial itself, which happens with mu RELEASED.
	mu     sync.Mutex
	dialMu sync.Mutex

	url  string
	sess *mcpsdk.ClientSession
	// failedAt is when the last dial gave up, so the next agent does not pay the
	// same timeout again straight away. Zero once a dial succeeds.
	failedAt time.Time

	// listed is the tools/list result, cached because it costs a round trip and
	// cannot change under a pinned session.
	//
	// The LISTING is cached and the WRAPPING is not, which is the one place this
	// deliberately differs from browserServer. A wrapped tool closes over one
	// agent's deps -- its gate, its log -- so a surface built for the first agent
	// and handed to the second would raise that agent's approvals in someone
	// else's transcript.
	listed []*mcpsdk.Tool

	// actions is what each connected-app action needs from this person, resolved
	// on the host and pushed. Absent means ask -- see agentapi.Apps.
	actions map[string]string
}

// newAppsServer prepares the manager. It starts nothing, and a blank url is the
// ordinary state of a machine whose host has no integration provider configured.
//
// Takes the whole pushed record rather than the URL alone: the resolved answer is
// on the same disk and a machine that came back without it would ask about every
// read until its next push.
func newAppsServer(a agentapi.Apps) *appsServer {
	s := &appsServer{dial: dialApps}
	s.SetConfig(a.SessionURL, a.Actions)
	return s
}

// SetConfig installs a pushed URL and policy as one state transition, and is the
// only writer of either.
//
// One transition because installing them apart lets a call observe a refreshed
// session URL while still classifying against the previous session's answer.
//
// The policy is installed unconditionally and the URL only when it moved. A push
// carrying a fresher answer on the SAME session is the ordinary case -- the one
// surfaceChanged is pinned to allow, and now also how a person's setting takes
// effect -- so gating it on the URL having changed would drop exactly the update
// this design exists to deliver. Nor does a repoint clear it: it describes the
// provider's catalogue and this person's settings, not this session.
func (s *appsServer) SetConfig(url string, actions map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actions = actions
	if url != s.url {
		s.url, s.listed, s.failedAt = url, nil, time.Time{}
		if s.sess != nil {
			s.sess.Close()
			s.sess = nil
		}
	}
	if len(actions) == 0 && url != "" {
		log.Printf("agentd: session but no resolved actions; every app call will ask")
	}
}

// needs is what this action requires from the person before it runs.
//
// Everything unknown resolves to asking: an action absent from the answer, a
// value we do not recognise, a machine never pushed at all. This is the one
// lookup that can make a machine LESS capable than intended, so nothing may
// reach auto by accident and nothing may reach never by accident either.
func (s *appsServer) needs(slug string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch got := s.actions[slug]; got {
	case agentapi.ActionAuto, agentapi.ActionNever:
		return got
	}
	return agentapi.ActionAsk
}

// Current is the address this server is pointed at, or "" for none. Read by
// tests and by nothing in the daemon, which resolves its session per call.
func (s *appsServer) Current() string {
	_, url := s.held()
	return url
}

// Tools lists the session's surface for one agent, dialling on first use.
func (s *appsServer) Tools(ctx context.Context, d toolDeps) ([]anthropic.BetaTool, error) {
	listed, url, err := s.listing(ctx)
	if err != nil {
		return nil, redactURL(err, url)
	}
	if listed == nil {
		return nil, nil // this machine has no session, which is not an error
	}
	return wrapAll(listed, s, appsNoun, s.hook(d), d), nil
}

// hook decides every action in a call against what this person allows.
//
// Closed over ONE agent's deps, sound because the wrapping is per agent even
// though the listing is cached -- see the note on listed. Nothing here reads a
// tool NAME to decide; the classifier that did was deleted from this package.
func (s *appsServer) hook(d toolDeps) beforeHook {
	return func(ctx context.Context, name string, args map[string]any) error {
		calls, err := callsIn(name, args)
		if err != nil {
			return err
		}
		for _, c := range calls {
			if err := s.permit(ctx, d.gate, c); err != nil {
				return err
			}
		}
		return nil
	}
}

// permit decides one action against what this person allows: run it, ask them
// and wait, or refuse it outright. It runs nothing: the batch goes out
// afterwards, whole.
//
// So a refusal anywhere aborts ALL of it, reads included -- the call never
// reaches the provider. That is the safe direction and the only coherent one,
// since the provider executes the batch as one request and cannot be asked for
// part of it.
//
// Asked per action rather than per call, so every card names one thing and every
// grant is scoped to one slug. The key handed to Check is the SLUG and never the
// meta-tool: grants key on that string, so passing the wrapper would make one tap
// buy an hour of every action in every connected app.
// Refusing BEFORE Gate.Check, never through it. Check's first act is to consume
// a standing batch grant -- "allow the next ten for an hour" -- keyed on the same
// slug this passes, so routing a refusal through it would let a tap made before
// the setting was changed run the very thing the setting forbids.
func (s *appsServer) permit(ctx context.Context, g *Gate, c appCall) error {
	switch s.needs(c.Slug) {
	case agentapi.ActionAuto:
		return nil
	case agentapi.ActionNever:
		return refusedByPolicy(c.Slug)
	}
	return g.Check(ctx, c.Slug, previewOf(c), c.Args)
}

// listing returns the session's tool list, fetching it once if it is cold.
//
// The network happens with NO lock held. Only the cache read and the cache
// write take s.mu, so an agent being built while the provider hangs cannot
// block another agent's tool call, the error path that reports the failure, or
// the host's own PUT /apps.
func (s *appsServer) listing(ctx context.Context) ([]*mcpsdk.Tool, string, error) {
	cached, url, cooling := s.cached()
	if url == "" || cached != nil || cooling != nil {
		return cached, url, cooling
	}
	sess, err := s.connect(ctx)
	if err == nil {
		var listed []*mcpsdk.Tool
		if listed, err = listAppTools(ctx, sess); err == nil {
			if s.store(url, listed) {
				return listed, url, nil
			}
			return nil, url, errRepointed
		}
	}
	if !s.noteFailure(url) {
		return nil, url, errRepointed
	}
	return nil, url, err
}

// cached reports the tool list this server holds, the session it belongs to,
// and any cooldown still standing after a failed attempt.
func (s *appsServer) cached() ([]*mcpsdk.Tool, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listed == nil && time.Since(s.failedAt) < appsRetryAfter {
		return nil, s.url, errRecentlyFailed
	}
	return s.listed, s.url, nil
}

// store keeps a freshly read tool list and clears the cooldown, provided the
// server still points at the session that produced it.
func (s *appsServer) store(url string, listed []*mcpsdk.Tool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.url != url {
		return false
	}
	s.listed, s.failedAt = listed, time.Time{}
	return true
}

// noteFailure starts the cooldown, so the next agent built does not pay the
// same timeout again straight away. A failure from a session that was replaced
// in flight must not cool down its replacement.
func (s *appsServer) noteFailure(url string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.url != url {
		return false
	}
	s.failedAt = time.Now()
	return true
}

// session hands back a live session, reconnecting if the last one died.
func (s *appsServer) session(ctx context.Context) (*mcpsdk.ClientSession, error) {
	sess, err := s.connect(ctx)
	return sess, s.redact(err)
}

// redact takes the session URL out of an error a wrapped tool is about to hand
// back. Safe from anywhere: it holds s.mu only long enough to read the url.
func (s *appsServer) redact(err error) error {
	_, url := s.held()
	return redactURL(err, url)
}

// connect returns a live session, dialling at most one at a time and never
// holding the state lock while it does.
//
// browserServer can afford to dial under its state lock because that dial is a
// local exec of a few milliseconds. This one crosses the internet, so the same
// shape would block every other caller for the full timeout.
func (s *appsServer) connect(ctx context.Context) (*mcpsdk.ClientSession, error) {
	sess, url := s.held()
	if sess != nil {
		return sess, nil
	}
	if url == "" {
		return nil, errNoSession
	}
	s.dialMu.Lock()
	defer s.dialMu.Unlock()
	if sess, url = s.held(); sess != nil {
		return sess, nil // somebody else dialled while we queued
	}
	if url == "" {
		return nil, errNoSession
	}
	return s.dialTo(ctx, url)
}

// dialTo opens a session and keeps it, unless the machine was repointed while
// the dial was in flight -- a session for an address nobody wants any more is
// closed rather than handed out. Caller holds s.dialMu.
func (s *appsServer) dialTo(ctx context.Context, url string) (*mcpsdk.ClientSession, error) {
	sess, err := s.dial(ctx, url)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.url != url {
		sess.Close()
		return nil, errRepointed
	}
	s.sess = sess
	return sess, nil
}

// held reports the live session and the address it belongs to, dialling nothing.
func (s *appsServer) held() (*mcpsdk.ClientSession, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sess, s.url
}

// drop retires a session that has died, so the next call reconnects.
func (s *appsServer) drop(dead *mcpsdk.ClientSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess == dead {
		s.sess = nil
	}
}

// Close hangs up, if a connection was ever made.
func (s *appsServer) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sess != nil {
		s.sess.Close()
		s.sess = nil
	}
}

// dialApps opens the streamable HTTP transport and completes the handshake.
//
// The URL is the credential: it is scoped by the provider to this machine's
// person, and it must never appear in an event, a tool result or a log line.
func dialApps(ctx context.Context, url string) (*mcpsdk.ClientSession, error) {
	client := mcpsdk.NewClient(&mcpsdk.Implementation{Name: "agentd", Version: "1"}, nil)
	return client.Connect(ctx, &mcpsdk.StreamableClientTransport{Endpoint: url}, nil)
}

// listAppTools reads every page of tools/list.
//
// Unfiltered, unlike the browser's: the session was configured host-side to
// advertise exactly the meta-tools we want, so a second allow list here would be
// a place for the two to disagree silently. Paging is not optional -- a partial
// list would drop a tool with nothing reporting it.
func listAppTools(ctx context.Context, sess *mcpsdk.ClientSession) ([]*mcpsdk.Tool, error) {
	var out []*mcpsdk.Tool
	for tool, err := range sess.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		out = append(out, tool)
	}
	if len(out) == 0 {
		return nil, errors.New("the connected-apps session advertised no tools")
	}
	return out, nil
}
