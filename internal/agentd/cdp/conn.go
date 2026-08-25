// Package cdp speaks the Chrome DevTools Protocol over a WebSocket.
//
// It exists because the Go agent has to drive the same Chrome the TypeScript
// agent drove through chrome-devtools-mcp, and that server cannot be reused:
// the Anthropic SDK's MCP support is the URL-based server-side connector, where
// the API dials the server itself, so it can never reach a stdio process on the
// guest's loopback.
//
// The scope here is deliberately narrow. This is a client, on loopback, talking
// to one trusted Chrome. There is no TLS, no compression, no server role.
package cdp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/coder/websocket"
)

// readLimit bounds one incoming message. A full-page screenshot arrives as one
// base64 payload, so the default 32 KiB would drop exactly the frames we care
// about most.
const readLimit = 32 << 20

// Conn multiplexes every command and event over one WebSocket to Chrome.
//
// Chrome answers commands out of order and interleaves events with them, so the
// only workable shape is a single read pump that routes by message id, with
// callers parked on a channel. Never write from two goroutines: the websocket
// library permits one concurrent writer, and agents are concurrent by design.
type Conn struct {
	ws *websocket.Conn

	mu      sync.Mutex
	nextID  int
	pending map[int]chan reply
	events  map[string][]chan json.RawMessage
	closed  bool

	wmu sync.Mutex // serialises writes; see the type doc
}

// reply is one command result, or the error Chrome answered with.
type reply struct {
	result json.RawMessage
	err    error
}

// message is the wire shape, covering both directions. A frame carrying an id
// is a command reply; one carrying a method is an event.
type message struct {
	ID     int             `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
	// SessionID targets one attached page. Absent means the browser itself.
	SessionID string `json:"sessionId,omitempty"`
}

// wireError is CDP's error object.
type wireError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    string `json:"data,omitempty"`
}

// Error renders a CDP error for a tool result, keeping Chrome's own wording:
// the model reacts better to "No node with given id found" than to a wrapper.
func (e *wireError) Error() string {
	if e.Data == "" {
		return e.Message
	}
	return e.Message + ": " + e.Data
}

// Dial opens a DevTools WebSocket. The URL comes from /json/version or
// /json/list, never hand-built -- Chrome mints a fresh path per browser session.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, _, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial devtools: %w", err)
	}
	ws.SetReadLimit(readLimit)
	c := &Conn{
		ws:      ws,
		pending: map[int]chan reply{},
		events:  map[string][]chan json.RawMessage{},
	}
	go c.readPump()
	return c, nil
}

// readPump owns the read side for the connection's whole life. It is the only
// reader, so nothing here needs a lock against another reader -- only against
// the maps that Call and Subscribe mutate.
func (c *Conn) readPump() {
	for {
		_, data, err := c.ws.Read(context.Background())
		if err != nil {
			c.failAll(err)
			return
		}
		var m message
		if json.Unmarshal(data, &m) != nil {
			continue // a frame we cannot parse is not worth dying over
		}
		c.route(m)
	}
}

// route delivers one decoded frame to whoever is waiting for it.
func (c *Conn) route(m message) {
	if m.ID != 0 {
		c.deliver(m)
		return
	}
	if m.Method != "" {
		c.publish(m)
	}
}

// deliver hands a command reply to its parked caller and retires the id.
func (c *Conn) deliver(m message) {
	c.mu.Lock()
	ch, ok := c.pending[m.ID]
	delete(c.pending, m.ID)
	c.mu.Unlock()
	if !ok {
		return // a caller that gave up; its context already fired
	}
	if m.Error != nil {
		ch <- reply{err: m.Error}
		return
	}
	ch <- reply{result: m.Result}
}

// publish fans an event out to its subscribers, dropping it for any subscriber
// that is not keeping up. A slow reader must not stall the whole connection.
func (c *Conn) publish(m message) {
	c.mu.Lock()
	subs := append([]chan json.RawMessage(nil), c.events[m.Method]...)
	c.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- m.Params:
		default:
		}
	}
}

// failAll wakes every parked caller when the connection dies, so a browser that
// crashes surfaces as an error on the next tool call rather than as a hang.
func (c *Conn) failAll(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for id, ch := range c.pending {
		ch <- reply{err: fmt.Errorf("devtools connection lost: %w", err)}
		delete(c.pending, id)
	}
}

// Call sends one command and waits for its reply. sessionID targets an attached
// page; empty addresses the browser itself. out may be nil when the result is
// not needed.
func (c *Conn) Call(ctx context.Context, sessionID, method string, params, out any) error {
	id, ch, err := c.register()
	if err != nil {
		return err
	}
	if err := c.send(ctx, id, sessionID, method, params); err != nil {
		c.retire(id)
		return err
	}
	return c.await(ctx, id, ch, out)
}

// register reserves an id and the channel its reply will arrive on.
func (c *Conn) register() (int, chan reply, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, nil, fmt.Errorf("devtools connection is closed")
	}
	c.nextID++
	ch := make(chan reply, 1)
	c.pending[c.nextID] = ch
	return c.nextID, ch, nil
}

// retire drops a pending id whose command never made it onto the wire.
func (c *Conn) retire(id int) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

// send marshals and writes one command. Writes are serialised because the
// websocket library allows only one at a time and agents run concurrently.
func (c *Conn) send(ctx context.Context, id int, sessionID, method string, params any) error {
	raw, err := encodeParams(params)
	if err != nil {
		return err
	}
	body, err := json.Marshal(message{ID: id, Method: method, Params: raw, SessionID: sessionID})
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.ws.Write(ctx, websocket.MessageText, body)
}

// encodeParams renders command params, omitting them entirely when there are
// none -- CDP rejects a null params on some domains.
func encodeParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	return json.Marshal(params)
}

// await parks until the reply lands or the caller's context ends.
func (c *Conn) await(ctx context.Context, id int, ch chan reply, out any) error {
	select {
	case <-ctx.Done():
		c.retire(id)
		return ctx.Err()
	case r := <-ch:
		if r.err != nil {
			return r.err
		}
		if out == nil || len(r.result) == 0 {
			return nil
		}
		return json.Unmarshal(r.result, out)
	}
}

// Subscribe returns a channel of params for one CDP event. The buffer is what
// lets publish drop rather than block; a subscriber that cares about every
// frame should drain promptly.
func (c *Conn) Subscribe(method string) <-chan json.RawMessage {
	ch := make(chan json.RawMessage, 32)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events[method] = append(c.events[method], ch)
	return ch
}

// Close shuts the connection down. Safe to call twice.
func (c *Conn) Close() error {
	c.mu.Lock()
	already := c.closed
	c.closed = true
	c.mu.Unlock()
	if already {
		return nil
	}
	return c.ws.Close(websocket.StatusNormalClosure, "")
}
