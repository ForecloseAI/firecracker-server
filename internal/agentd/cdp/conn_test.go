package cdp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeChrome is a DevTools endpoint that answers commands however the test
// says. Real Chrome is not available on a developer machine or in CI, and the
// behaviour worth pinning here -- out-of-order replies, interleaved events,
// error objects -- is awkward to provoke from a real browser anyway.
func fakeChrome(t *testing.T, handle func(*websocket.Conn, message)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		serveFake(ws, handle)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// serveFake reads commands until the client goes away, handing each to handle.
func serveFake(ws *websocket.Conn, handle func(*websocket.Conn, message)) {
	for {
		_, data, err := ws.Read(context.Background())
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		handle(ws, m)
	}
}

// writeJSON sends one frame from the fake server.
func writeJSON(ws *websocket.Conn, v any) {
	b, _ := json.Marshal(v)
	ws.Write(context.Background(), websocket.MessageText, b)
}

// dialFake opens a Conn against a fake server and closes it with the test.
func dialFake(t *testing.T, url string) *Conn {
	t.Helper()
	c, err := Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A reply must reach the caller that asked for it even when Chrome answers out
// of order. CDP makes no ordering promise, so routing by id is the whole
// contract -- matching on arrival order would mis-deliver under any real load.
func TestCallRoutesRepliesByIDNotArrivalOrder(t *testing.T) {
	url := fakeChrome(t, func(ws *websocket.Conn, m message) {
		// Answer the slow method late, so the second command replies first.
		if m.Method == "Slow" {
			go func() {
				time.Sleep(80 * time.Millisecond)
				writeJSON(ws, message{ID: m.ID, Result: json.RawMessage(`{"who":"slow"}`)})
			}()
			return
		}
		writeJSON(ws, message{ID: m.ID, Result: json.RawMessage(`{"who":"fast"}`)})
	})
	c := dialFake(t, url)
	type who struct {
		Who string `json:"who"`
	}
	done := make(chan who, 1)
	go func() {
		var got who
		c.Call(context.Background(), "", "Slow", nil, &got)
		done <- got
	}()
	time.Sleep(10 * time.Millisecond)
	var fast who
	if err := c.Call(context.Background(), "", "Fast", nil, &fast); err != nil {
		t.Fatal(err)
	}
	if fast.Who != "fast" {
		t.Errorf("second call got %q, want the fast reply", fast.Who)
	}
	if slow := <-done; slow.Who != "slow" {
		t.Errorf("first call got %q, want the slow reply", slow.Who)
	}
}

// Chrome's own error text has to reach the model. "No node with given id found"
// tells it to take a fresh snapshot; a generic wrapper tells it nothing.
func TestCallSurfacesChromeErrorText(t *testing.T) {
	url := fakeChrome(t, func(ws *websocket.Conn, m message) {
		writeJSON(ws, message{ID: m.ID, Error: &wireError{
			Code: -32000, Message: "No node with given id found", Data: "backendNodeId 42"}})
	})
	c := dialFake(t, url)
	err := c.Call(context.Background(), "", "DOM.focus", nil, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "No node with given id found") ||
		!strings.Contains(err.Error(), "backendNodeId 42") {
		t.Errorf("error lost Chrome's wording: %v", err)
	}
}

// Events are interleaved with command replies on the same socket. A subscriber
// must receive them without the command path noticing.
func TestSubscribeReceivesInterleavedEvents(t *testing.T) {
	url := fakeChrome(t, func(ws *websocket.Conn, m message) {
		writeJSON(ws, message{Method: "Page.loadEventFired", Params: json.RawMessage(`{"n":1}`)})
		writeJSON(ws, message{ID: m.ID, Result: json.RawMessage(`{}`)})
	})
	c := dialFake(t, url)
	evs := c.Subscribe("Page.loadEventFired")
	if err := c.Call(context.Background(), "", "Page.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case p := <-evs:
		if !strings.Contains(string(p), `"n":1`) {
			t.Errorf("event params = %s", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event never arrived")
	}
}

// A dead browser must surface as an error on the next tool call. Before
// failAll existed, a parked caller waited on a channel nobody would ever write
// to, and the agent goroutine hung until systemd killed the process.
func TestConnectionLossWakesParkedCallers(t *testing.T) {
	url := fakeChrome(t, func(ws *websocket.Conn, m message) {
		ws.CloseNow() // die mid-command, as a crashing Chrome would
	})
	c := dialFake(t, url)
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "", "Page.navigate", nil, nil) }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("expected an error when the connection dropped")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("caller hung after the connection dropped")
	}
}

// A cancelled turn must not leave its id parked forever. The agent cancels
// mid-turn on interrupt, and a leaked entry per interrupt is a slow leak in a
// process meant to run for the life of a VM.
func TestCancelledCallRetiresItsPendingEntry(t *testing.T) {
	url := fakeChrome(t, func(ws *websocket.Conn, m message) {}) // never answers
	c := dialFake(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.Call(ctx, "", "Page.navigate", nil, nil); err == nil {
		t.Fatal("expected a context error")
	}
	c.mu.Lock()
	n := len(c.pending)
	c.mu.Unlock()
	if n != 0 {
		t.Errorf("%d pending entries left after cancellation, want 0", n)
	}
}

// The session id has to reach Chrome, or every command lands on the browser
// target instead of the page and silently does nothing to the tab.
func TestCallCarriesSessionID(t *testing.T) {
	seen := make(chan string, 1)
	url := fakeChrome(t, func(ws *websocket.Conn, m message) {
		seen <- m.SessionID
		writeJSON(ws, message{ID: m.ID, Result: json.RawMessage(`{}`)})
	})
	c := dialFake(t, url)
	if err := c.Call(context.Background(), "SESSION-7", "Page.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-seen; got != "SESSION-7" {
		t.Errorf("sessionId = %q, want SESSION-7", got)
	}
}
