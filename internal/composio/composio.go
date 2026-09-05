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
	"log"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
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
	// Bounded well ABOVE a full page, so this is never what stops a real answer.
	// At ~2.3 KB per unfiltered tool row, a 1 MiB ceiling died at roughly 450 --
	// under the 500 a page asks for, so a toolkit that reached it failed as
	// "unexpected EOF" having decoded NOTHING. Silent, total, and never cached,
	// so it repeats on the short clock forever.
	//
	// Costs no memory to raise: this streams into structs that keep a handful of
	// fields, so what is retained is tens of KB whatever the body is.
	return json.NewDecoder(io.LimitReader(res.Body, bodyCap)).Decode(out)
}

// bodyCap bounds a response we will decode: a guard against a provider having a
// very bad day, not a size any real answer approaches.
//
// The largest is one page of unfiltered tool rows, and the NEAR MISS is the
// whole reason this is not 1 MiB: outlook, the biggest of the six this project
// drove by hand, reconstructs its 305 tools to roughly a megabyte on its own.
// Every page is now bounded at toolPage rows rather than however many an app
// happens to have -- about 1.2 MB at that measured rate -- so the headroom here
// is a factor of six against a page that can no longer grow. A ceiling a real
// answer can reach is one that will be reached.
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
			out = append(out, Connection{
				ID: it.ID, Toolkit: slugOf(it.Toolkit.Slug), Status: it.Status})
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

// slugOf is how every identifier from the provider enters this package.
//
// Lowercase, because a slug is compared for EQUALITY in several places that have
// nothing to do with each other: a person's stored permissions are keyed by it,
// the capability walk checks each row's parent against it, and the browse screen
// counts apps per category by it. Those comparisons were written at different
// times against different endpoints, and the provider does not promise the three
// agree on casing -- connectionFor in the chat package already hedges with
// EqualFold, which is the same worry solved one call site at a time.
//
// Solving it here instead means the rule is "identifiers are lowercase" and
// every consumer can use ==. The provider's own slugs are lowercase today, so
// this normally changes nothing; it is the day one endpoint starts shouting that
// it earns itself, and that day would otherwise turn a standing refusal into a
// card somebody can say yes to.
func slugOf(s string) string { return strings.ToLower(s) }

// Category is one of the provider's own groupings for an app -- "productivity",
// "crm", "developer-tools".
//
// Theirs and not ours, for the reason the copy below is theirs: a taxonomy over
// a thousand apps is something somebody has to keep up, and they are already
// keeping one up. The id is what filters; the name is what a person reads.
type Category struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Toolkit is one app as the provider's catalogue describes it.
//
// Fetched rather than written down here: a name, a logo and a blurb are copy
// that goes stale, and the only thing this project should be choosing is WHICH
// apps to put in front of somebody first.
type Toolkit struct {
	Slug        string
	Name        string
	Logo        string
	Description string
	Categories  []Category
	// NoAuth marks an app that needs no account at all. There is nothing for a
	// person to authorise, so a Connect button on one is a button that does
	// nothing they can see.
	NoAuth bool
	// ManagedAuth says the provider holds credentials for this app, which is the
	// only kind this project can put somebody through: createAuthConfig below
	// brings none of its own. Without it the sign-in page either never mints or
	// mints against a config with no credentials behind it.
	ManagedAuth bool
	// Deprecated marks an app on its way out. Offered to nobody new: a person who
	// connects one today is being handed a thing scheduled to stop working.
	Deprecated bool
}

// Connectable reports whether this project can put a person through this app's
// sign-in and end up with a working account.
//
// Three ways to fail and they fail differently, which is why this is one
// predicate rather than a filter written out at each call site: nothing to
// authorise, nothing for us to authorise WITH, or something being withdrawn.
// All three end the same way -- a Connect button that cannot work -- and the
// screen should not offer any of them.
func (t Toolkit) Connectable() bool {
	return !t.NoAuth && t.ManagedAuth && !t.Deprecated
}

// toolkitResp is the shape of GET /toolkits/{slug}, and of one row of the list
// and multi endpoints, which answer with the same object.
type toolkitResp struct {
	Slug   string `json:"slug"`
	Name   string `json:"name"`
	NoAuth bool   `json:"no_auth"`
	// ManagedSchemes is empty for an app whose credentials a project has to bring
	// itself. Note it is NOT auth_schemes, which lists every method the app
	// supports including the ones the provider holds nothing for.
	ManagedSchemes []string `json:"composio_managed_auth_schemes"`
	// Deprecated is an object rather than a boolean, and it is ABSENT rather than
	// false for a healthy app -- so what marks one is the field being there at
	// all. Decoded into a pointer for exactly that: a struct would be
	// indistinguishable from an app the provider said nothing about.
	Deprecated *struct {
		Version string `json:"version"`
	} `json:"deprecated"`
	Meta struct {
		Logo        string     `json:"logo"`
		Description string     `json:"description"`
		Categories  []Category `json:"categories"`
	} `json:"meta"`
}

// toolkit is one row in this package's own vocabulary.
func (r toolkitResp) toolkit() Toolkit {
	groups := make([]Category, 0, len(r.Meta.Categories))
	for _, group := range r.Meta.Categories {
		groups = append(groups, Category{ID: slugOf(group.ID), Name: group.Name})
	}
	return Toolkit{
		Slug: slugOf(r.Slug), Name: r.Name, Logo: r.Meta.Logo,
		Description: r.Meta.Description, Categories: groups, NoAuth: r.NoAuth,
		ManagedAuth: len(r.ManagedSchemes) > 0, Deprecated: r.Deprecated != nil,
	}
}

// toolkitsResp is one page of GET /toolkits.
type toolkitsResp struct {
	Items      []toolkitResp `json:"items"`
	NextCursor string        `json:"next_cursor"`
}

// toolkitPage is how many apps are asked for at once. The provider caps limit at
// a thousand and the catalogue is a few thousand rows, so the whole thing is a
// handful of requests -- which is the point: the per-slug fetch this sits beside
// costs one request per app, and that was affordable only while there were six.
const toolkitPage = 500

// toolkitPages bounds the walk, for the reason connectionPages does: a cursor
// that never empties is a provider bug, and a loop that trusts it spends a
// request a second forever with nobody watching.
//
// Deliberately far above the catalogue's real size. A short answer costs
// somebody an app they cannot find and looks exactly like an app the provider
// does not carry, so this ceiling exists to stop a runaway and should never be
// the thing that ends a normal walk.
const toolkitPages = 20

// Toolkits is the provider's whole catalogue.
//
// Ordered by usage rather than alphabetically, so a screen showing the first few
// of anything shows apps people actually connect. That ordering is the
// provider's read of its own traffic, which is a better answer than a list
// written here would be a month later -- and it is the same argument that keeps
// the names and blurbs theirs.
//
// Deprecated apps are left out by the provider's own default. Asking for them
// and filtering here would mean carrying rows only to drop them.
func (c *Client) Toolkits(ctx context.Context) ([]Toolkit, error) {
	var out []Toolkit
	cursor := ""
	for range toolkitPages {
		q := url.Values{"sort_by": {"usage"}, "limit": {strconv.Itoa(toolkitPage)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page toolkitsResp
		if err := c.send(ctx, http.MethodGet, "/toolkits?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, it := range page.Items {
			out = append(out, it.toolkit())
		}
		if page.NextCursor == "" {
			return out, nil
		}
		cursor = page.NextCursor
	}
	// An error rather than the pages that did arrive: a truncated catalogue is a
	// screen quietly missing its tail, and the caller caches what it is given.
	return nil, fmt.Errorf("composio: the catalogue did not end within %d pages", toolkitPages)
}

// Categories is every grouping the catalogue uses.
//
// One request and no walk: it answers with the whole set, and there are dozens
// rather than thousands. Should that stop being true a screen loses some
// headings while every app remains reachable by search, which is not worth the
// paging Toolkits carries.
func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	var out struct {
		Items []Category `json:"items"`
	}
	if err := c.send(ctx, http.MethodGet, "/toolkits/categories", nil, &out); err != nil {
		return nil, err
	}
	for i, group := range out.Items {
		out.Items[i].ID = slugOf(group.ID)
	}
	return out.Items, nil
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

// toolPage is asked for in one page. Outlook, the largest of the six this
// project drove by hand, has 305 tools -- but the catalogue is open now and
// nothing says the next app somebody connects is that size.
const toolPage = 500

// toolPages bounds the walk, for the reason toolkitPages does: a cursor that
// never empties is a provider bug that would otherwise spend a request a second
// forever.
//
// Ten thousand tools for one app, which is far past anything real. It has to be,
// because stopping short here is not merely a short list: an app whose walk is
// abandoned contributes NOTHING, and an action the person switched off then goes
// missing from the pushed answer, where absence means ask -- so a standing
// refusal quietly becomes a card they can say yes to.
const toolPages = 20

// toolsResp is one page of GET /tools.
type toolsResp struct {
	Items      []toolRow `json:"items"`
	NextCursor string    `json:"next_cursor"`
	// Total is the provider's own count. Read to NOTICE a short answer rather
	// than to refuse one: a walk that ends early costs some actions a card they
	// did not need, while trusting a number we cannot verify would cost an app
	// its whole answer the day that number turns out to include rows we drop.
	Total int `json:"total_items"`
}

// toolRow is one action as the catalogue lists it. Tags are what it is
// classified from, and the rest is the little needed to know the row belongs
// here at all -- which is what makes an unfiltered page cheap to hold however
// large the body was.
type toolRow struct {
	Slug string   `json:"slug"`
	Tags []string `json:"tags"`
	// Toolkit is read so an ignored filter is DETECTED rather than defended
	// against. This API answers a parameter it does not recognise with
	// everything it has, and the day toolkit_slug is renamed the answer to
	// "what can gmail do" becomes the whole catalogue -- classified, cached
	// and pushed. Reading each row's own parent is what turns that from a
	// silent wrong answer into a named one.
	Toolkit struct {
		Slug string `json:"slug"`
	} `json:"toolkit"`
	// Deprecated tools are asked about rather than run unasked. The provider
	// includes them by default here, unlike in the toolkit catalogue, and a
	// withdrawn action that still classifies read-only is one nobody is
	// choosing to keep working.
	Deprecated bool `json:"is_deprecated"`
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
// UNFILTERED on purpose, and that is the safer shape rather than the lazier one.
// This API answers a parameter it does not recognise with EVERYTHING -- user_id
// vs user_ids and toolkit vs toolkit_slug have both already been met here -- so
// asking for tags=readOnlyHint and trusting the answer would call every tool
// read-only the day that filter is renamed. Reading each row's own tags removes
// the trap instead of defending against it, and costs one walk per app rather
// than one per tag. The predecessor that did filter had to re-check every row it
// got back for exactly this reason.
//
// Walked rather than read in one page, which the open catalogue made necessary.
// A single page refused anything at or over its own size, on the reasoning that
// a short answer is worse than none -- true while the six apps on offer topped
// out at 305 tools, and false now that anybody can connect anything. What it
// actually produces for a larger app is a permanent error: nothing cached, the
// app absent from every pushed answer, and its actions asking forever -- the
// ones somebody switched off included, which then raise a card they can say yes
// to. Paging costs a round trip per five hundred actions and removes the cliff.
func (c *Client) Capabilities(ctx context.Context, slug string) (map[string]string, error) {
	slug = slugOf(slug)
	held := map[string]string{}
	cursor, seen, total := "", 0, 0
	for range toolPages {
		q := url.Values{"toolkit_slug": {slug}, "limit": {strconv.Itoa(toolPage)}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var out toolsResp
		if err := c.send(ctx, http.MethodGet, "/tools?"+q.Encode(), nil, &out); err != nil {
			return nil, err
		}
		seen += len(out.Items)
		// The FIRST count that arrives, not the last. The provider is not
		// obliged to repeat it on every page, and an omitted one decodes as
		// zero -- which would silently satisfy the check below and lose the only
		// signal that an app's actions came back short.
		if total == 0 {
			total = out.Total
		}
		if err := collect(held, out.Items, slug); err != nil {
			return nil, err
		}
		if out.NextCursor == "" {
			if total > seen {
				// Said out loud rather than refused. What it costs is some of
				// this app's actions asking when they need not; refusing would
				// cost the app its entire answer, and this number is one we
				// cannot check -- it may simply count rows we deliberately drop.
				log.Printf("composio: %s reports %d tools and the walk found %d",
					slug, total, seen)
			}
			return held, nil
		}
		cursor = out.NextCursor
	}
	return nil, fmt.Errorf("composio: %s did not finish within %d pages of tools", slug, toolPages)
}

// collect classifies one page of rows into the map being built.
func collect(held map[string]string, rows []toolRow, slug string) error {
	for _, it := range rows {
		if got := slugOf(it.Toolkit.Slug); got != "" && got != slug {
			// A row that NAMES another app, never one that names nothing. The
			// trap being caught is a filter coming back ignored, and the rows
			// that arrive then carry their real parents -- so an absent or
			// renamed field is a schema change and not the failure this is
			// looking for. Reading it as one would refuse every page forever:
			// nothing cached, every machine holding that app re-pushing on the
			// five-minute clock and rotating its ticket each time, fleet-wide.
			//
			// The whole answer, not just this row: a filter that came back
			// ignored means nothing in the page can be trusted to be about the
			// app that was asked for. Filtering down to the rows that match
			// would cache them as a complete answer for an app whose real tools
			// never arrived.
			return fmt.Errorf("composio: asked %s for its tools and got %s; "+
				"the toolkit filter is being ignored", slug, got)
		}
		if it.Deprecated {
			continue
		}
		held[it.Slug] = capabilityOf(it.Tags)
	}
	return nil
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
		// Managed and Status are read rather than assumed, because a config is
		// the one piece of provider state this project creates that OUTLIVES the
		// request that made it. A custom-typed config with no credentials behind
		// it, or one somebody disabled in the provider's console, mints links
		// that fail forever -- and the lookup below would keep finding it.
		Managed bool   `json:"is_composio_managed"`
		Status  string `json:"status"`
	} `json:"items"`
}

// authConfigDisabled is the provider's word for a config somebody switched off.
const authConfigDisabled = "DISABLED"

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
	held, err := c.findAuthConfig(ctx, slug)
	if err != nil || held != "" {
		return held, err
	}
	return c.createAuthConfig(ctx, slug)
}

// authConfigPage is how many configs the lookup reads. More than one, because
// the filters are asked for and then CHECKED here: a project that has both a
// managed and a custom config for an app must not have its answer decided by
// which the provider happened to sort first.
const authConfigPage = 20

// findAuthConfig is this project's usable managed config for one app, or the
// empty string when it has none yet.
//
// Every condition is re-checked on the rows that come back. is_composio_managed
// is asked for as a filter AND read off each row for the reason toolkit_slug is:
// a filter this API does not recognise is ignored rather than refused, and the
// row it would then hand back is the one that mints dead links for the life of
// the project.
func (c *Client) findAuthConfig(ctx context.Context, slug string) (string, error) {
	var held authConfigsResp
	q := "/auth_configs?" + url.Values{
		"toolkit_slug":        {slug},
		"is_composio_managed": {"true"},
		"limit":               {strconv.Itoa(authConfigPage)},
	}.Encode()
	if err := c.send(ctx, http.MethodGet, q, nil, &held); err != nil {
		return "", err
	}
	for _, it := range held.Items {
		if it.Toolkit.Slug == slug && it.Managed && it.Status != authConfigDisabled {
			return it.ID, nil
		}
	}
	return "", nil
}

// createAuthConfig asks for a provider-managed config for one app.
//
// The type is SENT rather than left to default, though the default is the same
// thing today. It decides whether the provider brings credentials or expects
// ours, it is now reached for any app in the catalogue rather than six that were
// checked by hand, and what it produces sticks: findAuthConfig above will hand
// back whatever this made for as long as the project exists. A default worth
// depending on is a default worth writing down -- the same reason sessionReq
// spells out every field it could have inherited.
func (c *Client) createAuthConfig(ctx context.Context, slug string) (string, error) {
	body := map[string]any{
		"toolkit":     map[string]string{"slug": slug},
		"auth_config": map[string]any{"type": "use_composio_managed_auth"},
	}
	var made authConfigResp
	if err := c.send(ctx, http.MethodPost, "/auth_configs", body, &made); err != nil {
		return "", err
	}
	if made.AuthConfig.ID == "" {
		return "", fmt.Errorf("composio: an auth config for %s came back with no id", slug)
	}
	return made.AuthConfig.ID, nil
}
