package chat

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
	"time"

	"cracked/internal/agentapi"
	"cracked/internal/composio"
)

// listApps is one page of the Apps screen: what this build offers, and which of
// them this person has connected.
//
// Deliberately not guestOf. Rendering a list must not boot a five gigabyte
// microVM -- that is the absurdity deleteAccount already calls out, and it is
// the whole reason this state lives in Postgres rather than on the guest.
func (s *Server) listApps(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil {
		// No provider configured is an empty shelf, not a failure: the screen
		// should render its empty state rather than an error nobody can act on.
		writeJSON(w, http.StatusOK, AppPage{Items: []App{}})
		return
	}
	kits, offset, ok := s.appsQuery(w, r)
	if !ok {
		return
	}
	held, ok := s.heldApps(w, r, user)
	if !ok {
		return
	}
	page, next := pageOf(kits, offset, limitOf(r))
	writeJSON(w, http.StatusOK, AppPage{Items: projectApps(page, held), NextCursor: next})
}

// appsQuery is the catalogue this request asked for and where its page starts,
// or an answer already written.
func (s *Server) appsQuery(w http.ResponseWriter,
	r *http.Request) ([]composio.Toolkit, int, bool) {
	q := r.URL.Query()
	offset, ok := offsetOf(q.Get("cursor"))
	if !ok {
		fail(w, http.StatusBadRequest, "that is not a page of the app list")
		return nil, 0, false
	}
	kits, err := s.catalog.toolkits(r.Context())
	if err != nil {
		// 502 and never an empty page. The client renders an empty list as "no
		// apps are offered here", so a provider having a bad minute would tell
		// somebody their apps had disappeared.
		fail(w, http.StatusBadGateway, "could not read the list of apps")
		return nil, 0, false
	}
	return matching(kits, q.Get("q"), q.Get("category")), offset, true
}

// limitOf is how many rows this request asked for, defaulted and clamped -- a
// client asking for everything gets a page rather than the catalogue.
func limitOf(r *http.Request) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n < 1 {
		return appsPageDefault
	}
	return min(n, appsPageMax)
}

// offersApp reports whether this build can act on an app, or writes the refusal.
//
// A slug this build does not offer is refused here rather than passed through.
// The provider carries over a thousand apps and would happily mint a link for
// any of them, but only the ones whose OAuth it runs itself lead to a page a
// person can finish -- and an arbitrary slug from a client is not a thing to
// hand onward in any case.
func (s *Server) offersApp(w http.ResponseWriter, r *http.Request, slug string) bool {
	held, err := s.catalog.offers(r.Context(), slug)
	if err != nil {
		// Never the refusal below. Telling somebody we do not carry their app
		// because the provider had a bad minute is a wrong answer, not a slow one.
		fail(w, http.StatusBadGateway, "could not check which apps are available")
		return false
	}
	if !held {
		fail(w, http.StatusBadRequest, "that app is not one this version offers")
		return false
	}
	return true
}

// heldApps is this person's connected accounts, or an answer already written --
// the shape decode uses. Every route here needs the same list, and the failure
// it reports is the same one.
//
// 502 and never 401: the client signs the person out of the whole product on any
// 401, and a provider having a bad minute is not a reason to end their session.
//
// It also tells noticeApps what it saw. Every caller wants that and none wants
// to remember it, and a route that forgot would be a person connecting an app
// and waiting an hour for their agents to know.
func (s *Server) heldApps(w http.ResponseWriter, r *http.Request, user string) ([]composio.Connection, bool) {
	held, err := s.composio.Connections(r.Context(), user)
	if err != nil {
		fail(w, http.StatusBadGateway, "could not check your connected accounts")
		return nil, false
	}
	s.noticeApps(user, held)
	return held, true
}

// Connection is one account this person has connected, as the app lists it.
type Connection struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	// Name is derived from the slug rather than fetched. This list can hold apps
	// the catalogue knows nothing about, so there is no copy to look up.
	Name   string `json:"name"`
	Status string `json:"status"`
	// Policy is what this person allows agents to do in this app without being
	// asked, by capability. Sparse: only what they changed, so the screen falls
	// back to its own default for anything absent.
	Policy map[string]string `json:"policy,omitempty"`
}

// listAppConnections is every account this person has connected.
//
// Strictly more than the Apps screen shows: an agent connects any app the
// provider supports, not only the ones whose OAuth it runs itself, so somebody
// may hold accounts this build would never list. Those still have to be visible
// and disconnectable.
//
// Whole rather than paged, deliberately. It is bounded by how many apps one
// person connected rather than by what the provider offers, and it is the list
// the client needs entire to render its connected and needs-attention sections
// against a catalogue it only holds a page of.
func (s *Server) listAppConnections(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil {
		writeJSON(w, http.StatusOK, []Connection{})
		return
	}
	held, ok := s.heldApps(w, r, user)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, projectConnections(held, s.storedPolicy(r.Context(), user)))
}

// projectConnections turns the provider's records into the rows the app renders.
func projectConnections(held []composio.Connection, policy map[string]map[string]string) []Connection {
	out := make([]Connection, 0, len(held))
	for _, conn := range held {
		out = append(out, Connection{
			ID: conn.ID, Slug: conn.Toolkit, Name: labelFor(conn.Toolkit), Status: conn.Status,
			Policy: policy[conn.Toolkit],
		})
	}
	return out
}

// storedPolicy is what this person has chosen, or nothing.
//
// Never an error: the screen falls back to its own default for any capability it
// is not told about, so a row that cannot be read costs them their settings on
// that screen rather than the screen itself. Nil store is the ordinary shape of a
// deployment with no integration provider, and of every test that does not care.
func (s *Server) storedPolicy(ctx context.Context, user string) map[string]map[string]string {
	if s.apps == nil {
		return nil
	}
	held, err := s.apps.Get(ctx, user)
	if err != nil {
		log.Printf("chat: could not read app policy for %s: %v", user, err)
		return nil
	}
	return held.Policy
}

// policyReq is one row of the permissions screen being set.
type policyReq struct {
	Capability string `json:"capability"`
	Policy     string `json:"policy"`
}

// settable are the answers a person may give. read is not among the capabilities
// below because the screen shows it as always allowed with no control: a
// connected app can already read, and a switch that only ever said yes would be
// a promise we could not keep.
var (
	settable = map[string]bool{
		agentapi.ActionAsk: true, agentapi.ActionAuto: true, agentapi.ActionNever: true}
	governable = map[string]bool{composio.CapWrite: true, composio.CapDelete: true}
)

// setAppPolicy records what this person lets one app do without being asked.
//
// Stored rather than derived, unlike the actions pushed from it: this is their answer and
// nobody else's. Note it is a PREFERENCE and not a boundary against them -- this
// service reaches Postgres with the caller's own token so row-level security can
// isolate, which means they can write this row directly. That is sound for a
// person restraining their own agents, and it is why anything the guest must be
// able to trust is validated where it is pushed rather than where it is stored.
func (s *Server) setAppPolicy(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil || s.apps == nil {
		fail(w, http.StatusBadGateway, "connecting apps is not available here")
		return
	}
	// Two refusals, not one condition: collapsing them with || short-circuits the
	// decode, so an app we do not offer answered 200 with an empty body and did
	// nothing at all.
	slug := r.PathValue("slug")
	if !s.offersApp(w, r, slug) {
		return
	}
	var req policyReq
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req) != nil {
		fail(w, http.StatusBadRequest, "that is not a permission this version can set")
		return
	}
	if !governable[req.Capability] || !settable[req.Policy] {
		fail(w, http.StatusBadRequest, "that is not a permission this version can set")
		return
	}
	stored, err := s.apps.Get(r.Context(), user)
	if err != nil {
		fail(w, http.StatusBadGateway, "could not read your permissions")
		return
	}
	if err := s.savePolicy(r.Context(), user, stored, slug, req); err != nil {
		fail(w, http.StatusBadGateway, "could not save that permission")
		return
	}
	s.settledConnection(w, r, user, slug)
}

// savePolicy merges one answer into the person's row and makes it take effect.
func (s *Server) savePolicy(ctx context.Context, user string, held agentapi.Apps,
	slug string, req policyReq) error {
	if held.Policy == nil {
		held.Policy = map[string]map[string]string{}
	}
	if held.Policy[slug] == nil {
		held.Policy[slug] = map[string]string{}
	}
	held.Policy[slug][req.Capability] = req.Policy
	if err := s.apps.Put(ctx, user, held); err != nil {
		return err
	}
	// Otherwise the machine keeps the policy it was pushed until its claim runs
	// out, which is up to an hour -- a person would change a setting and watch it
	// do nothing.
	//
	// staleApps, not forgetApps, for the reason staleApps exists: an agent can be
	// holding an approval on this very app while somebody flips its switch, and
	// dropping the ticket would 404 the retry at the broker. That the permissions
	// screen is "not mid-call" was an assumption about how people use the app,
	// not an invariant.
	s.staleApps(machineFor(user))
	return nil
}

// settledConnection answers with this app's whole row, which is what the screen
// replaces its own with. A partial answer erases what it does not mention.
func (s *Server) settledConnection(w http.ResponseWriter, r *http.Request, user, slug string) {
	held, ok := s.heldApps(w, r, user)
	if !ok {
		return
	}
	conn := connectionFor(held, slug)
	writeJSON(w, http.StatusOK, Connection{ID: conn.ID, Slug: slug, Name: labelFor(slug),
		Status: conn.Status, Policy: s.storedPolicy(r.Context(), user)[slug]})
}

// ConnectLink is where a person signs in to authorise an app.
type ConnectLink struct {
	RedirectURL string `json:"redirectUrl"`
	// ExpiresAt is the provider's own deadline, about ten minutes out. Sent so
	// the screen can ask for a fresh link rather than show a dead button to
	// somebody who put their phone down and came back.
	ExpiresAt time.Time `json:"expiresAt"`
}

// connectApp mints the page a person authorises one app on. offersApp says why
// an unknown slug is refused here rather than handed to the provider.
func (s *Server) connectApp(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil {
		fail(w, http.StatusBadGateway, "connecting apps is not available here")
		return
	}
	slug := r.PathValue("slug")
	if !s.offersApp(w, r, slug) {
		return
	}
	link, err := s.composio.Link(r.Context(), user, slug, s.cfg.ComposioCallback)
	if err != nil {
		fail(w, http.StatusBadGateway, "could not start connecting that app")
		return
	}
	writeJSON(w, http.StatusCreated, ConnectLink{RedirectURL: link.URL, ExpiresAt: link.ExpiresAt})
}

// disconnectApp hands one account's authorisation back.
//
// The id is resolved against THIS person's connections first. Ids are the only
// thing standing between one person's accounts and another's, and a delete
// straight through would let anybody holding an id revoke a stranger's Gmail.
//
// An id that is not theirs answers 404, the same as one already gone. Not 403:
// the client treats a 404 on delete as success -- "the row is where they wanted
// it either way" -- and confirming that an id exists but belongs to somebody
// else is itself an answer worth not giving.
func (s *Server) disconnectApp(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil {
		fail(w, http.StatusBadGateway, "connecting apps is not available here")
		return
	}
	held, ok := s.heldApps(w, r, user)
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !owns(held, id) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := s.composio.Disconnect(r.Context(), id); err != nil {
		fail(w, http.StatusBadGateway, "could not disconnect that account")
		return
	}
	// noticeApps read the connections BEFORE this, so it saw the app still
	// present. Without this the machine keeps that app's actions until its answer
	// goes stale on its own -- resolved against a grant that no longer exists.
	s.staleApps(machineFor(user))
	w.WriteHeader(http.StatusNoContent)
}

// owns reports whether a connection id is one of this person's.
//
// The empty id cannot arrive through the route -- the mux answers 404 before the
// handler runs, checked by removing this and watching it still 404 -- but the
// guard belongs to the function rather than the route: a provider record with no
// id would otherwise match a caller who has none either.
func owns(held []composio.Connection, id string) bool {
	if id == "" {
		return false
	}
	return slices.ContainsFunc(held, func(c composio.Connection) bool { return c.ID == id })
}
