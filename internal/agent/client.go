// Package agent speaks the guest agent's HTTP API over the VM's TAP address.
// It mirrors internal/fc: one narrow client, no VM or routing knowledge. The
// host reaches guests directly here rather than through the api proxy, which
// exists for browsers and deliberately strips credentials.
package agent

import (
	"encoding/json"
	"fmt"
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
	ID           int     `json:"id"`
	Type         string  `json:"type"`
	TS           string  `json:"ts"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	Usage        *Usage  `json:"usage,omitempty"`
	DurationMS   int64   `json:"duration_ms,omitempty"`
	Text         string  `json:"text,omitempty"`
	Tool         string  `json:"tool,omitempty"`
	Message      string  `json:"message,omitempty"`
	Question     string  `json:"question,omitempty"`
	Preview      string  `json:"preview,omitempty"`
	ApprovalID   string  `json:"approval_id,omitempty"`
	Decision     string  `json:"decision,omitempty"`
	SessionState string  `json:"session_state,omitempty"`
	IsError      bool    `json:"is_error,omitempty"`
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
