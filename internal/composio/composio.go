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
	"slices"
	"strconv"
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
	// Bounded well ABOVE what any caller's own row guard allows, so that guard is
	// the one that fires. At ~2.3 KB per unfiltered tool row, a 1 MiB ceiling died
	// at roughly 450 rows -- under Capabilities' readOnlyPage of 500, so the
	// toolkit that outgrew the call would fail as "unexpected EOF" having decoded
	// NOTHING, rather than as the named "this needs paging" error written for
	// exactly that case. Silent, total, and never cached, so it re-fans-out on the
	// short clock forever.
	//
	// Costs no memory to raise: this streams into structs that keep a handful of
	// fields, so what is retained is tens of KB whatever the body is.
	return json.NewDecoder(io.LimitReader(res.Body, bodyCap)).Decode(out)
}

// bodyCap bounds a response we will decode: a guard against a provider having a
// very bad day, not a size any real answer approaches.
//
// The largest today is one toolkit's unfiltered tool list, and the NEAR MISS is
// the whole reason this is not 1 MiB: outlook, the biggest of the six at 305
// tools, reconstructs to roughly a megabyte -- close enough that it cannot be
// said from here which side of that line it falls on, and it is the fastest
// growing of the six. A ceiling a real answer can reach is one that will be
// reached.
const bodyCap = 8 << 20

// Connection is one app account a person has connected.
type Connection struct {
	ID      string
	Toolkit string
	// Status is the provider's own word for it -- ACTIVE, EXPIRED, INITIATED.
	// Surfaced rather than reduced to a boolean, because "expired" and "never
	// finished" need different things said to the person.
	Status string
}

// StatusActive is the provider's word for a connection that works. Written down
// here rather than at the call site: this package is where the vocabulary is
// documented, so it should be the one holding the constant too.
const StatusActive = "ACTIVE"

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

// Toolkit is one app as the provider's catalogue describes it.
//
// Fetched rather than written down here: a name, a logo and a blurb are copy
// that goes stale, and the only thing this project should be choosing is WHICH
// apps to offer.
type Toolkit struct {
	Slug        string
	Name        string
	Logo        string
	Description string
}

// toolkitResp is the shape of GET /toolkits/{slug}.
type toolkitResp struct {
	Slug string `json:"slug"`
	Name string `json:"name"`
	Meta struct {
		Logo        string `json:"logo"`
		Description string `json:"description"`
	} `json:"meta"`
}

// Toolkit fetches one app's public metadata.
func (c *Client) Toolkit(ctx context.Context, slug string) (Toolkit, error) {
	var out toolkitResp
	if err := c.send(ctx, http.MethodGet, "/toolkits/"+url.PathEscape(slug), nil, &out); err != nil {
		return Toolkit{}, err
	}
	return Toolkit{Slug: out.Slug, Name: out.Name,
		Logo: out.Meta.Logo, Description: out.Meta.Description}, nil
}

// readOnlyTag is the provider's annotation for a tool that only reads.
//
// Measured against the live catalogue on 2026-09-02: 910 of 910 tools across the
// featured six carry an effect hint, none carries readOnlyHint alongside
// destructiveHint or createHint, and it classifies correctly every slug whose
// NAME lies -- GMAIL_SEND_DRAFT is destructive, GOOGLECALENDAR_CALENDAR_LIST_INSERT
// creates, MICROSOFT_TEAMS_CREATE_OR_GET_ONLINE_MEETING creates. Which is why
// nothing downstream parses a tool name.
const readOnlyTag = "readOnlyHint"

// readOnlyPage is asked for in one page. Outlook is the largest of the featured
// six at 117 read-only tools, so this is four times the biggest real answer --
// deliberately, because ReadOnly refuses a full page rather than paging.
const readOnlyPage = 500

// toolsResp is one page of GET /tools. Tags come back so the filter can be
// checked rather than trusted -- see ReadOnly.
type toolsResp struct {
	Items []struct {
		Slug string   `json:"slug"`
		Tags []string `json:"tags"`
	} `json:"items"`
}

// ReadOnly is every tool in one app that the provider annotates as only reading.
//
// The tag is asked for as a filter AND checked on every row that comes back,
// which is not belt-and-braces so much as the whole safety of this call. This
// API ignores a query parameter it does not recognise rather than rejecting it
// -- user_id vs user_ids, toolkit vs toolkit_slug, both already met here -- so a
// renamed filter would answer with every tool in the toolkit, writes included,
// and a caller trusting the answer would let all of them run unasked. Checking
// the rows turns that from a catastrophe into a no-op. The test pins the query
// string as well, so the filter going stale is loud rather than silent.
func (c *Client) ReadOnly(ctx context.Context, slug string) ([]string, error) {
	var out toolsResp
	q := "/tools?" + url.Values{
		"toolkit_slug": {slug}, "tags": {readOnlyTag}, "limit": {strconv.Itoa(readOnlyPage)},
	}.Encode()
	if err := c.send(ctx, http.MethodGet, q, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Items) >= readOnlyPage {
		// Refused rather than paged. A full page means there may be more, and a
		// silently truncated set is one whose missing tools look like writes --
		// the caller would cache it for an hour and ask about ordinary reads. The
		// featured six top out at 117, so this firing means the catalogue grew
		// past what this call was designed for and wants paging, not a bigger
		// number.
		return nil, fmt.Errorf("composio: %s has %d or more read-only tools; this needs paging", slug, readOnlyPage)
	}
	got := make([]string, 0, len(out.Items))
	for _, it := range out.Items {
		if slices.Contains(it.Tags, readOnlyTag) {
			got = append(got, it.Slug)
		}
	}
	return got, nil
}

// The capabilities an action can belong to, most consequential first. These are
// the vocabulary a person sets a policy against, so they are named for what they
// do to somebody's account rather than for the provider's tags.
const (
	CapDelete = "del"
	CapWrite  = "write"
	CapRead   = "read"
)

// Capabilities is every action one app exposes, and what kind of thing it is.
//
// Unfiltered, unlike ReadOnly, and that is the safer shape rather than the lazier
// one. ReadOnly must ask for tags=readOnlyHint and then re-check every row it
// gets back, because this API answers a parameter it does not recognise with
// EVERYTHING -- a caller trusting a dropped filter would call all 910 tools
// read-only. Reading each row's own tags removes that trap instead of defending
// against it, and costs one request per app rather than one per tag.
func (c *Client) Capabilities(ctx context.Context, slug string) (map[string]string, error) {
	var out toolsResp
	q := "/tools?" + url.Values{
		"toolkit_slug": {slug}, "limit": {strconv.Itoa(readOnlyPage)},
	}.Encode()
	if err := c.send(ctx, http.MethodGet, q, nil, &out); err != nil {
		return nil, err
	}
	if len(out.Items) >= readOnlyPage {
		return nil, fmt.Errorf("composio: %s has %d or more tools; this needs paging", slug, readOnlyPage)
	}
	held := make(map[string]string, len(out.Items))
	for _, it := range out.Items {
		held[it.Slug] = capabilityOf(it.Tags)
	}
	return held, nil
}

// capabilityOf is what one action does, most consequential hint winning.
//
// A tool carries several: GMAIL_SEND_DRAFT is destructiveHint AND updateHint, and
// it is a delete. Nothing here reads the NAME -- the classifier that did was
// deleted from this project for calling GITHUB_CREATE_A_CHECK_SUITE read-only.
//
// openWorldHint is deliberately unused. It reads as "reaches other people" and is
// not: measured 2026-09-03, 753 of 910 tools carry it INCLUDING 334 of the 398
// read-only ones. It means the tool talks to a SaaS API, which all of them do.
// That is also why there is no draft-versus-send distinction here -- nothing the
// provider sends can carry it, and GMAIL_SEND_EMAIL and GMAIL_CREATE_EMAIL_DRAFT
// have byte-identical tags.
func capabilityOf(tags []string) string {
	switch {
	case slices.Contains(tags, "destructiveHint"):
		return CapDelete
	case slices.Contains(tags, "createHint"), slices.Contains(tags, "updateHint"):
		return CapWrite
	case slices.Contains(tags, readOnlyTag):
		return CapRead
	}
	// Unannotated is a write, so it asks. Measured zero on 2026-09-03 -- which is
	// a fact about that day and not a promise about the next tool they ship.
	return CapWrite
}

// Link is a hosted page where a person authorises one app.
type Link struct {
	URL string
	// ExpiresAt is the provider's own deadline rather than an assumption. It is
	// about ten minutes out, which is shorter than a person left holding a card
	// may take -- so whoever shows this has to be able to ask for a fresh one.
	ExpiresAt time.Time
}

// linkReq is the body of POST /connected_accounts/link.
type linkReq struct {
	UserID       string `json:"user_id"`
	AuthConfigID string `json:"auth_config_id"`
	CallbackURL  string `json:"callback_url,omitempty"`
}

// linkResp is what it answers with.
type linkResp struct {
	RedirectURL string    `json:"redirect_url"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// Link mints a connect link for one person and one app.
//
// Two calls, not one: the link needs an auth config id, and the provider will
// not infer it from the toolkit.
func (c *Client) Link(ctx context.Context, userID, slug, callback string) (Link, error) {
	cfg, err := c.authConfig(ctx, slug)
	if err != nil {
		return Link{}, err
	}
	var out linkResp
	body := linkReq{UserID: userID, AuthConfigID: cfg, CallbackURL: callback}
	if err := c.send(ctx, http.MethodPost, "/connected_accounts/link", body, &out); err != nil {
		return Link{}, err
	}
	if out.RedirectURL == "" {
		return Link{}, fmt.Errorf("composio: a connect link came back with no url")
	}
	return Link{URL: out.RedirectURL, ExpiresAt: out.ExpiresAt}, nil
}

// authConfigsResp is one page of GET /auth_configs.
type authConfigsResp struct {
	Items []struct {
		ID      string `json:"id"`
		Toolkit struct {
			Slug string `json:"slug"`
		} `json:"toolkit"`
	} `json:"items"`
}

// authConfigResp is what creating one answers with.
type authConfigResp struct {
	AuthConfig struct {
		ID string `json:"id"`
	} `json:"auth_config"`
}

// authConfig is the provider-managed auth config for one app, created the first
// time anybody connects it.
//
// Note toolkit_slug, SINGULAR. The plural toolkit_slugs and the bare toolkit are
// both accepted and then IGNORED, answering with every config in the project --
// and the caller would mint a link against whichever came back first. That is
// the reverse of connected_accounts, where the plural user_ids is the live one
// and the singular is ignored. Neither is guessable; both are pinned by tests.
func (c *Client) authConfig(ctx context.Context, slug string) (string, error) {
	var held authConfigsResp
	q := "/auth_configs?" + url.Values{"toolkit_slug": {slug}, "limit": {"1"}}.Encode()
	if err := c.send(ctx, http.MethodGet, q, nil, &held); err != nil {
		return "", err
	}
	for _, it := range held.Items {
		if it.Toolkit.Slug == slug {
			return it.ID, nil
		}
	}
	return c.createAuthConfig(ctx, slug)
}

// createAuthConfig asks for a provider-managed config for one app. The body
// carries only the toolkit: everything else defaults to managed OAuth, which is
// the whole point of not bringing our own credentials yet.
func (c *Client) createAuthConfig(ctx context.Context, slug string) (string, error) {
	body := map[string]any{"toolkit": map[string]string{"slug": slug}}
	var made authConfigResp
	if err := c.send(ctx, http.MethodPost, "/auth_configs", body, &made); err != nil {
		return "", err
	}
	if made.AuthConfig.ID == "" {
		return "", fmt.Errorf("composio: an auth config for %s came back with no id", slug)
	}
	return made.AuthConfig.ID, nil
}
