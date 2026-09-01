package chat

import (
	"net/http"
	"slices"
	"time"

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
}

// listAppConnections is every account this person has connected.
//
// Strictly more than the Apps screen shows: an agent can connect any app the
// provider supports, not only the six offered here, so somebody may hold
// accounts this build would never list. Those still have to be visible and
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
	writeJSON(w, http.StatusOK, projectConnections(held))
}

// projectConnections turns the provider's records into the rows the app renders.
func projectConnections(held []composio.Connection) []Connection {
	out := make([]Connection, 0, len(held))
	for _, conn := range held {
		out = append(out, Connection{
			ID: conn.ID, Slug: conn.Toolkit, Name: labelFor(conn.Toolkit), Status: conn.Status,
		})
	}
	return out
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
// A slug this build does not offer is refused here rather than passed through.
// The provider supports over a thousand apps and would happily mint a link for
// any of them, but this build has tested six -- and an arbitrary slug from a
// client is not a thing to hand onward.
func (s *Server) connectApp(w http.ResponseWriter, r *http.Request, user string) {
	if s.composio == nil {
		fail(w, http.StatusBadGateway, "connecting apps is not available here")
		return
	}
	slug := r.PathValue("slug")
	if !slices.Contains(featured, slug) {
		fail(w, http.StatusBadRequest, "that app is not one this version can connect")
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
