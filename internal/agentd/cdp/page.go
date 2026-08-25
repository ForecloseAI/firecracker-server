package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// discoverTimeout bounds the HTTP hop that finds Chrome's WebSocket URL. Chrome
// is on loopback in the same guest, so this only ever fires when it is down.
const discoverTimeout = 5 * time.Second

// actTimeout bounds one whole action -- a handful of loopback commands, plus
// however long it waits for the action lock. Generous for a healthy renderer,
// and short enough that an action against a wedged one reports back inside a
// single model turn instead of holding the browser for everybody.
const actTimeout = 15 * time.Second

// Browser is one attached Chrome page and everything needed to act on it.
//
// One page, not many: this agent shares a single browser with a person watching
// the same screen, and the TypeScript agent's own prompt is emphatic that it
// must use that browser rather than open a context of its own, which would be
// signed out of everything.
type Browser struct {
	conn      *Conn
	sessionID string

	mu     sync.Mutex
	gen    int
	snap   *Snapshot
	dialog *Dialog

	// act serialises one tool call's whole CDP sequence.
	//
	// Deliberately not b.mu: that one is taken by invalidate from the navigation
	// watcher, so holding it across network I/O would block navigation detection
	// behind a click. A channel rather than a sync.Mutex because a caller that
	// cannot get it must be able to give up when its context does.
	//
	// The hole it closes is real with a single agent, never mind two: the SDK's
	// tool runner calls handlers concurrently within one turn, and interleaving
	// two sequences -- scrollIntoView A, getContentQuads B, mousePressed at A's
	// coordinates -- clicks something belonging to a different element with
	// nothing anywhere reporting an error.
	//
	// It serialises SEQUENCES, not intents. One agent opening a menu and another
	// closing it is still wrong; a per-turn lease is what would fix that, and
	// this is not a substitute for one. Never call the approval Gate while
	// holding it -- Gate.Check blocks for up to thirty minutes, and one gated
	// browser tool added later would freeze Chrome for every agent here.
	act chan struct{}
}

// lock takes the action lock, giving up when the caller's context does.
func (b *Browser) lock(ctx context.Context) error {
	select {
	case b.act <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("the browser is busy with another action - try again")
	}
}

// unlock releases the action lock.
func (b *Browser) unlock() { <-b.act }

// versionInfo is the part of /json/version we need.
type versionInfo struct {
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// Connect finds Chrome's browser-level WebSocket and dials it.
//
// The URL is read from /json/version rather than built by hand: Chrome mints a
// fresh path per browser session, so a hardcoded one works exactly until the
// first restart.
func Connect(ctx context.Context, baseURL string) (*Browser, error) {
	wsURL, err := discover(ctx, baseURL)
	if err != nil {
		return nil, err
	}
	conn, err := Dial(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	b := &Browser{conn: conn, act: make(chan struct{}, 1)}
	if err := b.attach(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return b, nil
}

// discover reads the browser WebSocket URL out of /json/version.
func discover(ctx context.Context, baseURL string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/json/version", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("chrome is not answering on %s: %w", baseURL, err)
	}
	defer resp.Body.Close()
	var v versionInfo
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", err
	}
	return v.WebSocketDebuggerURL, nil
}

// targetInfo is one Chrome target from Target.getTargets.
type targetInfo struct {
	TargetID string `json:"targetId"`
	Type     string `json:"type"`
	URL      string `json:"url"`
}

// attach binds to the first page target and turns on the domains we use.
//
// flatten:true is what makes one connection enough: without it, a page session
// needs its own socket, and every command would have to be routed to whichever
// socket owned that target.
func (b *Browser) attach(ctx context.Context) error {
	target, err := b.firstPage(ctx)
	if err != nil {
		return err
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	err = b.conn.Call(ctx, "", "Target.attachToTarget",
		map[string]any{"targetId": target, "flatten": true}, &attached)
	if err != nil {
		return err
	}
	b.sessionID = attached.SessionID
	return b.enableDomains(ctx)
}

// firstPage returns the target id of a page, ignoring workers and extensions.
func (b *Browser) firstPage(ctx context.Context) (string, error) {
	var out struct {
		TargetInfos []targetInfo `json:"targetInfos"`
	}
	if err := b.conn.Call(ctx, "", "Target.getTargets", nil, &out); err != nil {
		return "", err
	}
	for _, t := range out.TargetInfos {
		if t.Type == "page" {
			return t.TargetID, nil
		}
	}
	return "", fmt.Errorf("chrome has no page target open")
}

// enableDomains turns on the two domains we read from and starts watching for
// navigation, which is what retires a generation of uids.
func (b *Browser) enableDomains(ctx context.Context) error {
	for _, domain := range []string{"Page.enable", "Accessibility.enable"} {
		if err := b.conn.Call(ctx, b.sessionID, domain, nil, nil); err != nil {
			return err
		}
	}
	go b.watchNavigation()
	go b.watchDialogs()
	return nil
}

// watchNavigation retires the current snapshot whenever the page moves.
//
// A uid is only meaningful against the DOM it was read from, and the person
// shares this browser -- they can navigate it themselves at any moment. Tying
// invalidation to Chrome's own events rather than to our commands is what makes
// a stale uid impossible to act on rather than merely unlikely.
func (b *Browser) watchNavigation() {
	navigated, stopNav := b.conn.Subscribe("Page.frameNavigated")
	loaded, stopLoad := b.conn.Subscribe("Page.loadEventFired")
	defer stopNav()
	defer stopLoad()
	for {
		select {
		case params, ok := <-navigated:
			if !ok {
				return
			}
			if !isMainFrame(params) {
				continue
			}
		case _, ok := <-loaded:
			if !ok {
				return
			}
		}
		b.invalidate()
	}
}

// isMainFrame reports whether a Page.frameNavigated event is the page itself.
//
// The parentId check is the whole point. An advertising iframe navigates
// several times a minute, so invalidating on every frameNavigated would retire
// every uid seconds after any commercial page finished loading, and the model
// would be stuck in a snapshot-act-stale loop it cannot get out of.
func isMainFrame(params json.RawMessage) bool {
	var ev struct {
		Frame struct {
			ParentID string `json:"parentId"`
		} `json:"frame"`
	}
	if json.Unmarshal(params, &ev) != nil {
		return true // an event we cannot read is safest treated as the page moving
	}
	return ev.Frame.ParentID == ""
}

// invalidate drops the current snapshot, so every uid from it goes stale.
func (b *Browser) invalidate() {
	b.mu.Lock()
	b.snap = nil
	b.mu.Unlock()
}

// Alive reports whether this browser can still be used. Chrome restarts on
// Restart=always, so a cached connection outlives the browser it was made to.
func (b *Browser) Alive() bool { return !b.conn.isClosed() }

// Close releases the connection.
func (b *Browser) Close() error { return b.conn.Close() }

// Navigate drives the page to a URL and reports where it ended up, which is not
// always where it was sent: redirects, consent walls and login gates all move
// it, and an agent that assumes otherwise acts on the wrong page.
func (b *Browser) Navigate(ctx context.Context, url string) (string, error) {
	// The lock matters most here. Another agent computing an element's quads and
	// then dispatching a press across this call would click those coordinates on
	// a different page entirely.
	ctx, cancel := context.WithTimeout(ctx, actTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return "", err
	}
	defer b.unlock()
	if err := b.goTo(ctx, url); err != nil {
		return "", err
	}
	b.invalidate()
	return b.currentURL(ctx)
}

// goTo sends the navigation, surfacing the failure Chrome reports in its result
// rather than as a protocol error -- a blocked or unreachable URL comes back as
// errorText on an otherwise successful command.
func (b *Browser) goTo(ctx context.Context, url string) error {
	var out struct {
		ErrorText string `json:"errorText"`
	}
	if err := b.conn.Call(ctx, b.sessionID, "Page.navigate",
		map[string]any{"url": url}, &out); err != nil {
		return err
	}
	if out.ErrorText != "" {
		return fmt.Errorf("%s", out.ErrorText)
	}
	return nil
}

// currentURL asks Chrome where the page actually is.
func (b *Browser) currentURL(ctx context.Context) (string, error) {
	var out struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
	}
	err := b.conn.Call(ctx, b.sessionID, "Runtime.evaluate",
		map[string]any{"expression": "location.href", "returnByValue": true}, &out)
	return out.Result.Value, err
}
