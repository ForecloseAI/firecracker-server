package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"cracked/internal/agentapi"
)

// AppsStore is where the host remembers a person's app-integration session.
//
// Keyed on the SUPABASE USER ID and never on the machine id. The sub -> machine
// derivation is one-way, the table keys on auth.users.id, and keying on the
// person rather than the machine is the point: it is what lets someone be moved
// to a different VM, or given a second one, without losing anything.
//
// What it holds is a POINTER to state the provider owns, never the state itself.
// Losing a row costs one API call to mint a fresh session, never a person's
// connections -- which is what keeps this cheap to get wrong.
type AppsStore interface {
	Get(ctx context.Context, userID string) (agentapi.Apps, error)
	Put(ctx context.Context, userID string, a agentapi.Apps) error
	Delete(ctx context.Context, userID string) error
}

// appsTable is the PostgREST resource behind AppsStore.
const appsTable = "/rest/v1/app_sessions"

// pgApps keeps the session in the project's Postgres, reached through PostgREST
// with the CALLER'S OWN access token.
//
// That is the whole security design: the publishable key identifies the project
// and is not a secret -- it already ships inside the app -- while the caller's
// token makes auth.uid() the person, so a row-level security policy does the
// isolation. This service still holds nothing that can mint a token or read
// somebody else's row, which is the property AGENTS.md asks for.
type pgApps struct {
	base        string
	publishable string
	http        *http.Client
}

// newPGApps builds the store, or nil when the project key is not configured.
func newPGApps(supabaseURL, publishable string) *pgApps {
	if supabaseURL == "" || publishable == "" {
		return nil
	}
	return &pgApps{base: strings.TrimSuffix(supabaseURL, "/"), publishable: publishable,
		http: &http.Client{Timeout: 10 * time.Second}}
}

// appRow is one row of app_sessions, as PostgREST renders it.
type appRow struct {
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	MCPURL    string `json:"mcp_url"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

// Get reads this person's session, or a zero value when they have none.
//
// A missing row is NOT an error. It means "mint one" -- never "this person does
// not exist" -- which is what keeps the machine derivation total: someone seen
// for the first time still resolves to a machine and still gets a session.
func (p *pgApps) Get(ctx context.Context, userID string) (agentapi.Apps, error) {
	q := appsTable + "?user_id=eq." + url.QueryEscape(userID) + "&select=session_id,mcp_url&limit=1"
	var rows []appRow
	if err := p.do(ctx, http.MethodGet, q, nil, &rows); err != nil {
		return agentapi.Apps{}, err
	}
	if len(rows) == 0 {
		return agentapi.Apps{}, nil
	}
	return agentapi.Apps{SessionURL: rows[0].MCPURL, SessionID: rows[0].SessionID}, nil
}

// Put records the session minted for this person, replacing any earlier one.
func (p *pgApps) Put(ctx context.Context, userID string, a agentapi.Apps) error {
	row := appRow{UserID: userID, SessionID: a.SessionID, MCPURL: a.SessionURL,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339)}
	return p.do(ctx, http.MethodPost, appsTable, []appRow{row}, nil)
}

// Delete forgets this person's session. Called when they erase their account,
// which purges the machine but leaves the Supabase user alone.
func (p *pgApps) Delete(ctx context.Context, userID string) error {
	return p.do(ctx, http.MethodDelete, appsTable+"?user_id=eq."+url.QueryEscape(userID), nil, nil)
}

// do runs one PostgREST call as the caller, decoding into out when given.
func (p *pgApps) do(ctx context.Context, method, path string, body, out any) error {
	req, err := p.request(ctx, method, path, body)
	if err != nil {
		return err
	}
	res, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("app store %s: %w", method, err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snip, _ := io.ReadAll(io.LimitReader(res.Body, 2<<10))
		return fmt.Errorf("app store %s: %s: %s", method, res.Status, bytes.TrimSpace(snip))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
}

// request builds the call, carrying the two credentials in their own slots.
//
// The project key goes in apikey and the PERSON'S token in Authorization, never
// the other way round: a key in the bearer slot silently drops auth.uid() to
// anonymous, and the policy would then hide every row rather than fail loudly.
func (p *pgApps) request(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(buf)
	}
	token := tokenFrom(ctx)
	if token == "" {
		return nil, fmt.Errorf("app store %s: no caller token on this request", method)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.base+path, rdr)
	if err != nil {
		return nil, err
	}
	p.setHeaders(req, token, body != nil)
	return req, nil
}

// setHeaders puts the two credentials and the upsert preference in place.
func (p *pgApps) setHeaders(req *http.Request, token string, hasBody bool) {
	req.Header.Set("apikey", p.publishable)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
		// One row per person, so a second mint replaces the first rather than
		// colliding on the primary key.
		req.Header.Set("Prefer", "resolution=merge-duplicates,return=minimal")
	}
}
