package chat

import (
	"net/http"

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
	held, err := s.composio.Connections(r.Context(), user)
	if err != nil {
		// 502 and never 401: the client signs the person out of the whole
		// product on any 401, and a provider having a bad minute is not a
		// reason to end their session.
		fail(w, http.StatusBadGateway, "could not check which apps you have connected")
		return
	}
	writeJSON(w, http.StatusOK, projectApps(s.catalog.toolkits(r.Context()), held))
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
	held, err := s.composio.Connections(r.Context(), user)
	if err != nil {
		fail(w, http.StatusBadGateway, "could not list your connected accounts")
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
