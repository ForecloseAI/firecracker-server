package agent

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// hostPort splits a test server's address the way a guest is addressed: by IP
// and port, never by URL.
func hostPort(t *testing.T, srv *httptest.Server) (string, int) {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := strconv.Atoi(u.Port())
	return u.Hostname(), p
}

// clientFor points a Client at a test server standing in for a guest.
func clientFor(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(hostPort(t, srv))
}

func TestHealth(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q, want /health", r.URL.Path)
		}
		w.Write([]byte(`{"ok":true,"ready":true,"session_state":"idle"}`))
	})
	h, err := c.Health()
	if err != nil {
		t.Fatal(err)
	}
	if !h.Ready || h.SessionState != "idle" {
		t.Errorf("health = %+v", h)
	}
}

// Every field the dashboard renders must survive decoding, including the
// opaque tool input: dropping it silently degrades the view without any
// compile or test failure elsewhere.
func TestEventsSinceDecodesEveryRenderedField(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/agents/coder/events" {
			t.Errorf("path = %q; events are per-agent now", got)
		}
		if got := r.URL.Query().Get("since"); got != "12" {
			t.Errorf("since = %q, want 12", got)
		}
		w.Write([]byte(`{"events":[
			{"id":13,"agent":"coder","type":"tool_use","ts":"2026-08-22T10:00:00Z","tool":"Bash","input":{"command":"ls -la"}},
			{"id":14,"agent":"coder","type":"approval_required","approval_id":"ap_001","preview":"Run shell command: rm -rf /tmp"},
			{"id":15,"agent":"coder","type":"usage","model":"claude-sonnet-5","duration_ms":4210,
			 "usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3,"cache_creation_input_tokens":4}}
		],"last_event_id":15}`))
	})
	events, last, err := c.EventsSince("coder", 12)
	if err != nil {
		t.Fatal(err)
	}
	if last != 15 || len(events) != 3 {
		t.Fatalf("last = %d, events = %d", last, len(events))
	}
	if got := string(events[0].Input); got != `{"command":"ls -la"}` {
		t.Errorf("tool input = %q, want the raw arguments", got)
	}
	if events[1].Preview == "" || events[1].ApprovalID != "ap_001" {
		t.Errorf("approval event lost fields: %+v", events[1])
	}
	if events[2].Usage.CacheReadInputTokens != 3 || events[2].Model != "claude-sonnet-5" {
		t.Errorf("usage event = %+v", events[2])
	}
	if events[0].Agent != "coder" {
		t.Errorf("attribution lost: %+v", events[0])
	}
}

// A guest that errors must surface it, not decode into an empty success.
func TestEventsSinceError(t *testing.T) {
	c := clientFor(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
	})
	if _, _, err := c.EventsSince("boss", 0); err == nil {
		t.Error("want an error from a 404")
	}
}
