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
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"cracked/internal/agentapi"
)

// timeout bounds a read of guest state. A wedged guest must never stall a
// dashboard poll, so this is short and failures degrade to "unreachable".
const timeout = 2 * time.Second

// writeTimeout bounds a call that CHANGES something. Sending a message resolves
// the agent through Supervisor.Get, which cold-starts it -- reading its
// conversation, seeding memory, building tools, possibly evicting another agent
// first. That runs well past 2s, and the failure would be the worst kind: the
// caller is told the send failed while the guest has already started the turn,
// so the person retries and the agent hears it twice.
const writeTimeout = 30 * time.Second

// Client talks to one guest agent.
type Client struct {
	base string
	http *http.Client
	slow *http.Client
}

// New builds a client for the agent listening on a guest's port.
func New(guestIP string, port int) *Client {
	return &Client{
		base: "http://" + net.JoinHostPort(guestIP, strconv.Itoa(port)),
		http: &http.Client{Timeout: timeout},
		slow: &http.Client{Timeout: writeTimeout},
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

// AgentTypes lists the profiles this machine can create agents from. It carries
// the display metadata -- title and description -- that a roster row needs and
// that Status does not have.
func (c *Client) AgentTypes() ([]agentapi.Profile, error) {
	var out []agentapi.Profile
	err := c.get("/agent-types", &out)
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
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return &StatusError{Code: resp.StatusCode, Path: path}
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

// StatusError is a guest refusal that the caller must be able to tell apart.
// A plain error would flatten "the agent is busy, retry in 5s" and "the guest is
// unreachable" into one 502, and a client cannot act on that.
type StatusError struct {
	Code int
	Path string
}

// Error renders the refusal.
func (e *StatusError) Error() string {
	return fmt.Sprintf("agent %s: %d", e.Path, e.Code)
}

// write posts a JSON body on the longer timeout and decodes the reply. Used for
// calls that change something, where losing the response loses the id or the
// reason it was refused.
func (c *Client) write(method, path string, body, out any) error {
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(method, c.base+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return c.decode(req, path, out)
}

// decode runs a write request and reads its reply, or reports the refusal.
func (c *Client) decode(req *http.Request, path string, out any) error {
	resp, err := c.slow.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		return &StatusError{Code: resp.StatusCode, Path: path}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
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

// Sent is what the guest reports back about a queued message. LastEventID is
// the id of the `user` event Send appended, which is the message's own id: the
// daemon reads it AFTER appending, so no second call is needed to learn it.
type Sent struct {
	MessageID   string `json:"message_id"`
	State       string `json:"session_state"`
	LastEventID int    `json:"last_event_id"`
}

// Post queues a user turn and reports what the guest recorded.
//
// No idempotency key: the header exists, but keying on the text collapses a
// deliberate repeat into silence, and a duplicate message is a far better
// failure than one that vanishes with no error.
func (c *Client) Post(agentID, text string) (Sent, error) {
	return c.PostMessage(agentID, Send{Text: text})
}

// PostMessage sends a turn with everything the client attached to it, which is
// a file the person picked and nothing else. Deliberately not a place to carry
// the timezone -- see agentapi.Person.TZ for why that is the only writer.
func (c *Client) PostMessage(agentID string, m Send) (Sent, error) {
	var out Sent
	err := c.write(http.MethodPost, "/agents/"+agentID+"/messages", m, &out)
	return out, err
}

// Send is what the daemon reads on POST /agents/{id}/messages.
type Send struct {
	Text string         `json:"text"`
	File *agentapi.File `json:"file,omitempty"`
}

// CreateAgent adds an agent of the given type to the roster. It does not start
// it -- an agent runs when it is first addressed.
//
// The name is passed explicitly because the daemon falls back to the TYPE KEY
// when it is empty, which is lowercase: the roster card would read "researcher"
// where the gallery card the person tapped said "Researcher".
func (c *Client) CreateAgent(typeKey, name string) (agentapi.Record, error) {
	var out agentapi.Record
	body := map[string]string{"type": typeKey, "name": name}
	err := c.write(http.MethodPost, "/agents", body, &out)
	return out, err
}

// DeleteAgent retires an agent, keeping its state so re-adding the same id gets
// its history back. Purging is deliberately not offered here: the app calls this
// "retire", and erasing a transcript should never be the quiet default.
func (c *Client) DeleteAgent(agentID string) error {
	return c.write(http.MethodDelete, "/agents/"+agentID, nil, nil)
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
// The write client is used so a refusal comes back TYPED: the guest answers 404
// when an interaction is already settled -- answered elsewhere, timed out, or
// revoked by an interrupt -- and a caller has to be able to tell that from an
// unreachable guest.
func (c *Client) Resolve(approvalID string, body map[string]any) error {
	return c.write(http.MethodPost, "/approvals/"+approvalID, body, nil)
}

// Interrupt stops one agent's turn and revokes its outstanding consent grants.
func (c *Client) Interrupt(agentID string) error {
	return c.post("/agents/"+agentID+"/interrupt", map[string]string{}, nil)
}

// Person reports what the machine knows about whoever it works for.
func (c *Client) Person() (agentapi.Person, error) {
	var p agentapi.Person
	return p, c.get("/person", &p)
}

// SetPerson replaces that profile with what onboarding collected.
func (c *Client) SetPerson(p agentapi.Person) error {
	return c.write(http.MethodPut, "/person", p, nil)
}

// Schedules lists the standing jobs on the machine, across every agent.
func (c *Client) Schedules() ([]agentapi.Schedule, error) {
	var out []agentapi.Schedule
	return out, c.get("/schedules", &out)
}

// DeleteSchedule cancels a standing job.
//
// There is deliberately no CreateSchedule here. Schedules are made by asking an
// agent, which raises an approval the person answers -- a path that already
// works end to end. A create route with no caller is a surface to keep working
// for nothing, and agentd's own POST /schedules stays available for when a
// client actually needs it.
func (c *Client) DeleteSchedule(id string) error {
	return c.write(http.MethodDelete, "/schedules/"+id, nil, nil)
}

// Upload streams a file into the guest's uploads folder and says where it landed.
//
// Streamed rather than buffered: 20 MB held in the gateway per upload is 20 MB
// the host cannot use for a VM, and this process runs beside five of them.
func (c *Client) Upload(name string, body io.Reader) (agentapi.File, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() { pw.CloseWithError(writePart(mw, name, body)) }()
	req, err := http.NewRequest(http.MethodPost, c.base+"/files", pr)
	if err != nil {
		return agentapi.File{}, err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	var out agentapi.File
	return out, c.decode(req, "/files", &out)
}

// writePart copies the body into one multipart field and closes the writer.
func writePart(mw *multipart.Writer, name string, body io.Reader) error {
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, body); err != nil {
		return err
	}
	return mw.Close()
}

// Shot fetches a handoff screenshot. The caller closes the body.
func (c *Client) Shot(agentID, name string) (io.ReadCloser, error) {
	resp, err := c.http.Get(c.base + "/agents/" + agentID + "/shots/" + url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode, Path: "/shots"}
	}
	return resp.Body, nil
}

// Attachment fetches a file an agent sent the person. The caller closes the body.
//
// On the SLOW client, unlike Shot, and that difference is the point: an
// http.Client's Timeout covers reading the response BODY and not just the
// headers, so the 2s one would cut a 20 MB attachment mid-stream and hand the
// person a truncated file. Handoff thumbnails are a few KB, which is why Shot
// has never met this.
//
// 30s is a wider budget and not immunity: it still bounds the whole transfer,
// so a stalled guest is still cut. What makes it enough is the hop -- a TAP
// device on the same host, where 20 MB is far inside it.
//
// Returns the whole response rather than just the body, because the guest is
// what decided this file's Content-Type and whether it may render inline, and
// the caller has to forward that decision rather than invent its own.
func (c *Client) Attachment(agentID, name string) (*http.Response, error) {
	resp, err := c.slow.Get(c.base + "/agents/" + url.PathEscape(agentID) + "/outbox/" + url.PathEscape(name))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode, Path: "/outbox"}
	}
	return resp, nil
}
