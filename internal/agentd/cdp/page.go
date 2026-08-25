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

// Browser is one attached Chrome page and everything needed to act on it.
//
// One page, not many: this agent shares a single browser with a person watching
// the same screen, and the TypeScript agent's own prompt is emphatic that it
// must use that browser rather than open a context of its own, which would be
// signed out of everything.
type Browser struct {
	conn      *Conn
	sessionID string

	mu   sync.Mutex
	gen  int
	snap *Snapshot
}

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
	b := &Browser{conn: conn}
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
	return nil
}

// watchNavigation retires the current snapshot whenever the page moves.
//
// A uid is only meaningful against the DOM it was read from, and the person
// shares this browser -- they can navigate it themselves at any moment. Tying
// invalidation to Chrome's own events rather than to our commands is what makes
// a stale uid impossible to act on rather than merely unlikely.
func (b *Browser) watchNavigation() {
	navigated := b.conn.Subscribe("Page.frameNavigated")
	loaded := b.conn.Subscribe("Page.loadEventFired")
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

// Close releases the connection.
func (b *Browser) Close() error { return b.conn.Close() }

// Navigate drives the page to a URL and reports where it ended up, which is not
// always where it was sent: redirects, consent walls and login gates all move
// it, and an agent that assumes otherwise acts on the wrong page.
func (b *Browser) Navigate(ctx context.Context, url string) (string, error) {
	var out struct {
		ErrorText string `json:"errorText"`
	}
	err := b.conn.Call(ctx, b.sessionID, "Page.navigate", map[string]any{"url": url}, &out)
	if err != nil {
		return "", err
	}
	if out.ErrorText != "" {
		return "", fmt.Errorf("%s", out.ErrorText)
	}
	b.invalidate()
	return b.currentURL(ctx)
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
