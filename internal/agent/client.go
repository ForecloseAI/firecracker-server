// Package agent speaks the guest agent's HTTP API over the VM's TAP address.
// It mirrors internal/fc: one narrow client, no VM or routing knowledge. The
// host reaches guests directly here rather than through the api proxy, which
// exists for browsers and deliberately strips credentials.
package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"
)

// timeout bounds every guest call. A wedged guest must never stall a dashboard
// poll, so this is short and failures degrade to "unreachable".
const timeout = 2 * time.Second

// Client talks to one guest agent.
type Client struct {
	base string
	http *http.Client
}

// New builds a client for the agent listening on a guest's port.
func New(guestIP string, port int) *Client {
	return &Client{
		base: "http://" + net.JoinHostPort(guestIP, strconv.Itoa(port)),
		http: &http.Client{Timeout: timeout},
	}
}

// Health is the guest's readiness report, from GET /health.
type Health struct {
	OK           bool   `json:"ok"`
	Ready        bool   `json:"ready"`
	SessionState string `json:"session_state"`
}

// Usage mirrors the SDK usage block the guest passes through verbatim.
type Usage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

// Event is one entry of the guest's append-only log. The fields are the union
// of what events.append emits; only those matching an event's type are set.
type Event struct {
	ID         int     `json:"id"`
	Type       string  `json:"type"`
	TS         string  `json:"ts"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	Usage      *Usage  `json:"usage,omitempty"`
	DurationMS int64   `json:"duration_ms,omitempty"`
	Text       string  `json:"text,omitempty"`
	Tool       string  `json:"tool,omitempty"`
	// Tool arguments, kept opaque: the shape is whatever the tool declares,
	// and the dashboard only ever shows a truncated preview of it.
	Input        json.RawMessage `json:"input,omitempty"`
	Message      string          `json:"message,omitempty"`
	Question     string          `json:"question,omitempty"`
	Preview      string          `json:"preview,omitempty"`
	ApprovalID   string          `json:"approval_id,omitempty"`
	Decision     string          `json:"decision,omitempty"`
	SessionState string          `json:"session_state,omitempty"`
	IsError      bool            `json:"is_error,omitempty"`
	// Kind and UI describe a pending interaction for the chat page. UI stays
	// opaque for the same reason Input does: the guest owns its shape.
	Kind string          `json:"kind,omitempty"`
	UI   json.RawMessage `json:"ui,omitempty"`
}

// Health reports whether the agent is up and what its session is doing.
func (c *Client) Health() (Health, error) {
	var h Health
	err := c.get("/health", &h)
	return h, err
}

// eventsResp is the shape of GET /session/events?poll=1.
type eventsResp struct {
	Events      []Event `json:"events"`
	LastEventID int     `json:"last_event_id"`
}

// EventsSince fetches every event after id, using the poll form rather than SSE
// because this is a one-shot read, not a subscription.
func (c *Client) EventsSince(id int) ([]Event, int, error) {
	var r eventsResp
	err := c.get("/session/events?poll=1&since="+strconv.Itoa(id), &r)
	return r.Events, r.LastEventID, err
}

// get performs one request and decodes a success response.
func (c *Client) get(path string, out any) error {
	resp, err := c.http.Get(c.base + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("agent %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// post sends a JSON body and discards a success response.
func (c *Client) post(path string, body any, hdr map[string]string) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return c.send(req)
}

// send executes a request and turns a non-2xx into an error.
func (c *Client) send(req *http.Request) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("agent %s: %s", req.URL.Path, resp.Status)
	}
	return nil
}

// SendMessage queues a user turn. The idempotency key lets a retried or
// double-clicked send collapse instead of queueing the message twice.
func (c *Client) SendMessage(text, idempotencyKey string) error {
	hdr := map[string]string{}
	if idempotencyKey != "" {
		hdr["Idempotency-Key"] = idempotencyKey
	}
	return c.post("/session/messages", map[string]string{"text": text}, hdr)
}

// Resolve answers a pending approval or question.
func (c *Client) Resolve(approvalID string, body map[string]any) error {
	return c.post("/session/approvals/"+approvalID, body, nil)
}

// Interrupt stops the current turn and revokes outstanding consent grants.
func (c *Client) Interrupt() error {
	return c.post("/session/interrupt", map[string]string{}, nil)
}
