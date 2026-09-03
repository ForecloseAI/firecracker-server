package chat

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"cracked/internal/agentapi"
	"cracked/internal/composio"
)

// listApps is the Apps screen: what this build offers, and which of them this
// person has connected.
//
// Deliberately not guestOf. Rendering a list must not boot a five gigabyte
// microVM -- that is the absurdity deleteAccount already calls out, and it is
// the whole reason this state lives in Postgres rather than on the guest.
func (s *Server) listApps(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil {
		// No provider configured is an empty shelf, not a failure: the screen
		// should render its empty state rather than an error nobody can act on.
		writeJSON(w, http.StatusOK, []App{})
		return
	}
	held, ok := s.heldApps(w, r, user)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, projectApps(s.catalog.toolkits(r.Context()), held))
}

// heldApps is this person's connected accounts, or an answer already written --
// the shape decode uses. Every route here needs the same list, and the failure
// it reports is the same one.
//
// 502 and never 401: the client signs the person out of the whole product on any
// 401, and a provider having a bad minute is not a reason to end their session.
func (s *Server) heldApps(w http.ResponseWriter, r *http.Request, user string) ([]composio.Connection, bool) {
	held, err := s.composio.Connections(r.Context(), user)
	if err != nil {
		fail(w, http.StatusBadGateway, "could not check your connected accounts")
		return nil, false
	}
	// The only place this service learns that somebody connected an app
	// somewhere else -- an agent's own connect card never reaches here, and the
	// page a person lands on afterwards is deliberately anonymous. Hung off the
	// read every one of these routes already does, so it costs nothing.
	s.noteApps(machineFor(user), appsMark(held))
	return held, true
}

// Connection is one account this person has connected, as the app lists it.
type Connection struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	// Name is derived from the slug rather than fetched. This list can hold apps
	// the featured catalogue knows nothing about, so there is no copy to look up.
	Name   string `json:"name"`
	Status string `json:"status"`
	// Policy is what this person allows agents to do in this app without being
	// asked, by capability. Sparse: only what they changed, so the screen falls
	// back to its own default for anything absent.
	Policy map[string]string `json:"policy,omitempty"`
}

// listAppConnections is every account this person has connected.
//
// Can still be more than any screen lists: an agent connects an app by asking
// the provider directly, and the catalogue this build browses is a filtered view
// of theirs. An account for something we would not offer today -- an app since
// withdrawn, one whose credentials moved -- still has to be visible and
// disconnectable.
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
	// decode, so a slug we refuse answered 200 with an empty body and did nothing
	// at all.
	//
	// The shape is all that is checked, and no catalogue lookup: this is a
	// preference, not a boundary, and a preference about an app somebody has not
	// connected is simply inert -- it resolves against no actions and reaches no
	// machine. Refusing one would mean a person could not set how an app should
	// behave until after they had already connected it, which is backwards, and
	// it would put the provider's availability in front of a setting screen.
	slug := r.PathValue("slug")
	if !validSlug(slug) {
		fail(w, http.StatusBadRequest, "that app is not one this version can set permissions for")
		return
	}
	var req policyReq
	if !decodePolicy(w, r, &req) {
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
	// A bound on the row rather than on the app, which is what dropping the
	// allowlist costs: nothing now stops a client setting a preference about a
	// slug nobody has heard of, and the row is read on every push. Far above
	// what anyone reaches, since it takes a settings screen tap per entry.
	if _, held := stored.Policy[slug]; !held && len(stored.Policy) >= policyAppCap {
		fail(w, http.StatusBadRequest, "that is more apps than this version keeps permissions for")
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
	// do nothing. forgetApps drops the claim so the next request re-pushes.
	s.forgetApps(machineFor(user))
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

// decodePolicy reads the body, answering the client itself when it cannot.
func decodePolicy(w http.ResponseWriter, r *http.Request, out *policyReq) bool {
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(out) != nil {
		fail(w, http.StatusBadRequest, "that is not a permission this version can set")
		return false
	}
	return true
}

// ConnectLink is where a person signs in to authorise an app.
type ConnectLink struct {
	RedirectURL string `json:"redirectUrl"`
	// ExpiresAt is the provider's own deadline, about ten minutes out. Sent so
	// the screen can ask for a fresh link rather than show a dead button to
	// somebody who put their phone down and came back.
	ExpiresAt time.Time `json:"expiresAt"`
}

// connectApp mints the page a person authorises one app on.
//
// The catalogue is the gate, and it is a real one rather than a formality. This
// is the only route that creates provider state which OUTLIVES the request: a
// link needs an auth config, and one is created for any app nobody has connected
// yet. That config is project-wide, it is what every later connection to that
// app is minted against, and nothing here deletes it -- so a slug straight from
// a client would let anybody signed in leave a config behind for every app in
// somebody else's catalogue, and count them against this project's plan.
//
// Passing the catalogue means the provider carries the app, it has credentials
// of its own for it, and it is not being withdrawn. Those are exactly the three
// things that decide whether the config this may create can ever work.
func (s *Server) connectApp(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil {
		fail(w, http.StatusBadGateway, "connecting apps is not available here")
		return
	}
	slug := r.PathValue("slug")
	if !validSlug(slug) || !s.offers(r.Context(), slug) {
		fail(w, http.StatusBadRequest, "that app is not one this version can connect")
		return
	}
	if !s.mayConnect(user) {
		// 429 rather than a refusal about the app: nothing is wrong with what
		// they asked for, and the client should show a "try again in a moment"
		// rather than "we do not have that app".
		fail(w, http.StatusTooManyRequests, "that is a lot of apps at once; try again in a minute")
		return
	}
	link, err := s.composio.Link(r.Context(), user, slug, s.cfg.ComposioCallback)
	if err != nil {
		fail(w, http.StatusBadGateway, "could not start connecting that app")
		return
	}
	writeJSON(w, http.StatusCreated, ConnectLink{RedirectURL: link.URL, ExpiresAt: link.ExpiresAt})
}

// offers reports whether this build will put somebody through an app's sign-in.
//
// During a catalogue outage it falls back to the featured apps, which is exactly
// the gate this route had before the catalogue was opened: six slugs written
// down here, every one of them already driven end to end and already holding a
// config in any deployment that has been used. So the fallback cannot create
// anything new and cannot fail in a way that sticks -- and a person who cannot
// reach the provider's catalogue can still connect the apps most of them came
// for.
func (s *Server) offers(ctx context.Context, slug string) bool {
	held := s.catalog.current(ctx)
	if held == nil {
		return slices.Contains(featured, slug)
	}
	_, ok := held.get(slug)
	return ok
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
	// The machine is holding an answer about an app this person no longer has.
	// Harmless in itself -- the calls fail at the provider either way -- but the
	// claim's mark now describes a set that is gone, and noteApps compares
	// against the list as it was read at the top of this handler. Dropping it
	// here is what stops the next read seeing "unchanged" and leaving a stale
	// answer standing for the rest of the hour.
	s.forgetApps(machineFor(user))
	w.WriteHeader(http.StatusNoContent)
}

// browseApps is the rest of the catalogue: every app this build can connect,
// grouped, searchable and paged.
//
// A second route rather than query parameters on the Apps screen, because it
// answers a different question. GET /v1/apps is "what are we putting in front of
// you", a short list whose whole shape is a JSON array; this is "what else is
// there", which needs headings, a page and a cursor. Folding them together would
// have turned an array into an object and broken every client that has the first
// one.
func (s *Server) browseApps(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil {
		writeJSON(w, http.StatusOK, Catalog{Apps: []App{}})
		return
	}
	held := s.catalog.current(r.Context())
	if held == nil {
		// 502 rather than an empty catalogue: an empty one reads as "there are no
		// other apps", which is a wrong answer a person would believe and act on.
		fail(w, http.StatusBadGateway, "could not read the list of apps just now")
		return
	}
	mine, ok := s.heldApps(w, r, user)
	if !ok {
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	group := r.URL.Query().Get("category")
	from := cursorOf(r)
	kits, next := held.browse(query, group, from, browsePage)
	out := Catalog{Groups: held.groups, Apps: projectApps(kits, mine), NextCursor: markCursor(next)}
	// Only on the screen as it first opens. Once somebody has searched or picked
	// a heading they are looking at one list, and previews of other headings
	// underneath it are furniture in the way of the answer.
	if query == "" && group == "" && from == 0 {
		out.Sections = held.preview(mine)
	}
	writeJSON(w, http.StatusOK, out)
}

// Catalog is the browse screen: the headings, a page of apps, and where the next
// page starts.
type Catalog struct {
	// Groups is every heading with something under it, so the screen can offer
	// them all without asking what each contains.
	Groups []Group `json:"categories,omitempty"`
	// Sections is a few apps under each of the leading headings -- the "here is
	// a taste of what there is" the screen opens on. Absent once somebody has
	// searched or chosen.
	Sections []Section `json:"sections,omitempty"`
	// Apps is this page of the full list, in the provider's usage order.
	Apps []App `json:"apps"`
	// NextCursor is empty when this was the last page.
	NextCursor string `json:"nextCursor,omitempty"`
}

// cursorOf reads where a page starts, treating anything unreadable as the
// beginning. A cursor is an offset into a list that is rebuilt every hour, so it
// is already approximate: somebody paging across a refresh may see a row twice
// or miss one, which is a better trade than holding a snapshot per reader.
func cursorOf(r *http.Request) int {
	from, err := strconv.Atoi(r.URL.Query().Get("cursor"))
	if err != nil || from < 0 {
		return 0
	}
	return from
}

// markCursor renders where the next page starts, or nothing when there is none.
func markCursor(from int) string {
	if from == 0 {
		return ""
	}
	return strconv.Itoa(from)
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
