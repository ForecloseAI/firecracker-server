// Package agent speaks the guest daemon's HTTP API over the VM's TAP address.
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

	"cracked/internal/agentapi"
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

// Health reports whether the daemon is up and what its roster is doing.
func (c *Client) Health() (agentapi.Health, error) {
	var h agentapi.Health
	err := c.get("/health", &h)
	return h, err
}

// Usage is what the daemon has spent, in tokens, across every agent it runs.
func (c *Client) Usage() (agentapi.UsageReport, error) {
	var r agentapi.UsageReport
	err := c.get("/usage", &r)
	return r, err
}

// Agents lists the roster. This is the call that makes every other one
// possible: there is no implicit "the agent" any more, so a caller that wants
// to reach all of them has to ask which exist.
func (c *Client) Agents() ([]agentapi.Status, error) {
	var out []agentapi.Status
	err := c.get("/agents", &out)
	return out, err
}

// EventsSince fetches every event of one agent after id, using the poll form
// rather than SSE because this is a one-shot read, not a subscription.
func (c *Client) EventsSince(agentID string, id int) ([]agentapi.Event, int, error) {
	var r agentapi.EventsPage
	err := c.get("/agents/"+agentID+"/events?poll=1&since="+strconv.Itoa(id), &r)
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

// SendMessage queues a user turn for one agent. The idempotency key lets a
// retried or double-clicked send collapse instead of queueing it twice.
func (c *Client) SendMessage(agentID, text, idempotencyKey string) error {
	hdr := map[string]string{}
	if idempotencyKey != "" {
		hdr["Idempotency-Key"] = idempotencyKey
	}
	return c.post("/agents/"+agentID+"/messages", map[string]string{"text": text}, hdr)
}

// Pending is every agent currently waiting on a person: the team's raised
// hands. Machine-wide, because a person answers the team rather than polling
// each specialist to find out which one has its hand up.
func (c *Client) Pending() ([]agentapi.Raised, error) {
	var out []agentapi.Raised
	err := c.get("/pending", &out)
	return out, err
}

// Resolve answers a pending approval or question.
//
// No agent argument: the id names the agent that raised it, so there is no way
// to deliver an answer to the wrong one, and no way for a worker's approval to
// be routed through the boss.
func (c *Client) Resolve(approvalID string, body map[string]any) error {
	return c.post("/approvals/"+approvalID, body, nil)
}

// Interrupt stops one agent's turn and revokes its outstanding consent grants.
func (c *Client) Interrupt(agentID string) error {
	return c.post("/agents/"+agentID+"/interrupt", map[string]string{}, nil)
}
