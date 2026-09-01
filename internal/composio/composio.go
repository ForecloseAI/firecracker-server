// Package composio opens tool-router sessions against the app-integration
// provider.
//
// It is deliberately tiny and lives on the HOST. The project API key is
// authority over every user's connected accounts, so it must never reach a
// guest: what a guest gets is one session URL, scoped by the provider to the
// person whose machine it is.
package composio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultBase is the provider's REST root. A var, not a const, so a test can
// point at an httptest server without a build tag.
var DefaultBase = "https://backend.composio.dev/api/v3.1"

// Client talks to the provider's REST API on behalf of this project.
type Client struct {
	key  string
	base string
	http *http.Client
}

// New builds a client, or nil when no key is configured -- which is how the
// whole feature stays off by default. Every caller must tolerate a nil client.
func New(key, base string) *Client {
	if key == "" {
		return nil
	}
	if base == "" {
		base = DefaultBase
	}
	return &Client{key: key, base: base, http: &http.Client{Timeout: 15 * time.Second}}
}

// Session is one person's tool-router session: the MCP endpoint an agent dials.
type Session struct {
	ID  string
	URL string
}

// sessionReq is the body of POST /tool_router/session.
//
// Every field here is a security decision rather than a default worth
// inheriting, which is why the struct spells them all out instead of sending a
// bare user_id. Connection REMOVAL in particular is off: disconnecting an app
// is a person's decision, and a prompt injection that could do it would be able
// to cut an agent off from the work it was asked to do.
type sessionReq struct {
	UserID            string     `json:"user_id"`
	ManageConnections manageConn `json:"manage_connections"`
	Search            enable     `json:"search"`
	Execute           execConf   `json:"execute"`
	Workbench         enable     `json:"workbench"`
}

// manageConn configures the meta-tool that mints connect links.
//
// The links it returns EXPIRE IN TEN MINUTES, which is shorter than the half
// hour a connect card waits. An agent whose person comes back late has to mint
// a fresh one rather than report a failure; the connected-apps skill says so.
type manageConn struct {
	Enable       bool   `json:"enable"`
	CallbackURL  string `json:"callback_url,omitempty"`
	WaitForConns bool   `json:"enable_wait_for_connections"`
	AllowRemoval bool   `json:"enable_connection_removal"`
}

// enable is the shape of the provider's simple on/off blocks.
type enable struct {
	Enable bool `json:"enable"`
}

// execConf switches the execute meta-tool on.
//
// Verified against the live API: enable_multi_execute is not a choice between
// batched and single execution -- it is the ONLY execution path. With it false
// the session advertises search, schemas and connections and nothing that can
// act, so an agent can find a tool, connect the app, and then never use it.
type execConf struct {
	MultiExecute bool `json:"enable_multi_execute"`
}

// sessionResp is what the provider answers with.
type sessionResp struct {
	SessionID string `json:"session_id"`
	MCP       struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	} `json:"mcp"`
}

// NewSession opens a tool-router session scoped to one person.
//
// userID is the Supabase user id WITH its hyphens -- not the machine id, which
// is the same UUID with them stripped. The two are both hex strings of similar
// length, so a mix-up would be silent: it would isolate a person from their own
// connections and report nothing.
func (c *Client) NewSession(ctx context.Context, userID, callback string) (Session, error) {
	body := sessionReq{
		UserID: userID,
		ManageConnections: manageConn{
			Enable: true, CallbackURL: callback, WaitForConns: true, AllowRemoval: false,
		},
		Search:  enable{Enable: true},
		Execute: execConf{MultiExecute: true},
		// Explicitly off, and it has to be said out loud: OMITTING this block
		// turns the workbench ON and brings COMPOSIO_REMOTE_BASH_TOOL with it.
		// The guest has its own shell and its own workspace, and a second remote
		// one is attack surface bought for nothing.
		Workbench: enable{Enable: false},
	}
	var out sessionResp
	if err := c.send(ctx, http.MethodPost, "/tool_router/session", body, &out); err != nil {
		return Session{}, err
	}
	if out.MCP.URL == "" {
		return Session{}, fmt.Errorf("composio: session %q came back with no mcp url", out.SessionID)
	}
	return Session{ID: out.SessionID, URL: out.MCP.URL}, nil
}

// send makes one request and decodes the reply into out, or discards it when
// out is nil -- which a DELETE needs, since it answers with no body at all.
func (c *Client) send(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("x-api-key", c.key)
	return c.do(req, out)
}

// do runs a request, turning a non-2xx into an error that names the status.
//
// The body is read into the error but the KEY never is: this client is the only
// holder of a credential with authority over every user, and an error string
// travels into logs the operator console renders.
func (c *Client) do(req *http.Request, out any) error {
	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("composio %s: %w", req.URL.Path, err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snip, _ := io.ReadAll(io.LimitReader(res.Body, 2<<10))
		return fmt.Errorf("composio %s: %s: %s", req.URL.Path, res.Status, bytes.TrimSpace(snip))
	}
	if out == nil {
		return nil // a 204 has no body to read, and decoding one is an EOF
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}

// Connection is one app account a person has connected.
type Connection struct {
	ID      string
	Toolkit string
	// Status is the provider's own word for it -- ACTIVE, EXPIRED, INITIATED.
	// Surfaced rather than reduced to a boolean, because "expired" and "never
	// finished" need different things said to the person.
	Status string
}

// connectionsResp is one page of GET /connected_accounts.
type connectionsResp struct {
	Items []struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Toolkit struct {
			Slug string `json:"slug"`
		} `json:"toolkit"`
	} `json:"items"`
	NextCursor string `json:"next_cursor"`
}

// connectionPages bounds the walk. Nobody connects two thousand accounts; a
// cursor that never empties is a provider bug, and looping on it forever inside
// an account deletion would be worse than stopping short.
const connectionPages = 20

// Connections lists every app account this person has connected.
//
// user_idS, plural, and that letter matters: the singular form is not rejected,
// it is IGNORED -- it comes back with other people's accounts in the list. A
// caller that deleted what that returned would revoke strangers' grants.
func (c *Client) Connections(ctx context.Context, userID string) ([]Connection, error) {
	var out []Connection
	cursor := ""
	for range connectionPages {
		page, err := c.connectionPage(ctx, userID, cursor)
		if err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			out = append(out, Connection{ID: it.ID, Toolkit: it.Toolkit.Slug, Status: it.Status})
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return out, nil
}

// connectionPage fetches one page of a person's connected accounts.
func (c *Client) connectionPage(ctx context.Context, userID, cursor string) (connectionsResp, error) {
	q := url.Values{"user_ids": {userID}, "limit": {"100"}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	var page connectionsResp
	err := c.send(ctx, http.MethodGet, "/connected_accounts?"+q.Encode(), nil, &page)
	return page, err
}

// Disconnect revokes one connected account.
//
// The person's authorisation at the provider is theirs, not ours, so erasing
// their machine has to erase this too -- otherwise they are told their data is
// gone while a live Google grant keeps their inbox reachable.
func (c *Client) Disconnect(ctx context.Context, id string) error {
	return c.send(ctx, http.MethodDelete, "/connected_accounts/"+url.PathEscape(id), nil, nil)
}
