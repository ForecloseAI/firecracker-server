package cdp

import (
	"encoding/json"
	"strconv"
	"sync"
	"testing"

	"github.com/coder/websocket"
)

// recorder collects the commands a test sent to Chrome, which is what makes
// these assertions about the METHOD SEQUENCE rather than about a return value.
type recorder struct {
	mu   sync.Mutex
	msgs []message
}

func (r *recorder) add(m message) {
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.mu.Unlock()
}

// methods lists the command names seen, in order.
func (r *recorder) methods() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.msgs))
	for _, m := range r.msgs {
		out = append(out, m.Method)
	}
	return out
}

// sent returns the params of the nth call to a method, or nil.
func (r *recorder) sent(method string, n int) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.msgs {
		if m.Method != method {
			continue
		}
		if n > 0 {
			n--
			continue
		}
		var p map[string]any
		json.Unmarshal(m.Params, &p)
		return p
	}
	return nil
}

// count reports how many times a method was called.
func (r *recorder) count(method string) int {
	n := 0
	for _, m := range r.methods() {
		if m == method {
			n++
		}
	}
	return n
}

// fakeBrowser attaches a Browser to a fake DevTools server. Connect is skipped
// because its only extra work is an HTTP hop to find the socket URL.
func fakeBrowser(t *testing.T, handle func(*websocket.Conn, message)) (*Browser, *recorder) {
	t.Helper()
	rec := &recorder{}
	url := fakeChrome(t, func(ws *websocket.Conn, m message) {
		rec.add(m)
		handle(ws, m)
	})
	return &Browser{conn: dialFake(t, url), sessionID: "S", act: make(chan struct{}, 1)}, rec
}

// answers replies to each command with a canned result, "{}" by default.
func answers(canned map[string]string) func(*websocket.Conn, message) {
	return func(ws *websocket.Conn, m message) {
		body, ok := canned[m.Method]
		if !ok {
			body = "{}"
		}
		writeJSON(ws, message{ID: m.ID, Result: json.RawMessage(body)})
	}
}

// seed gives the browser a snapshot holding one actionable node, and returns
// the uid the model would have been shown.
func seed(b *Browser, role string, backendID int64) string {
	b.gen = 1
	b.snap = build(1, []axNode{node(role, "Sign in", backendID, false)})
	return "1_" + strconv.FormatInt(backendID, 10)
}

// oneQuad is a 20x20 box whose centre is (20,30).
const oneQuad = `{"quads":[[10,20,30,20,30,40,10,40]]}`
