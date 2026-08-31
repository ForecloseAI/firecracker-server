package agentd

import (
	"net/http"
	"os"
	"path/filepath"

	"cracked/internal/agentapi"
)

// personBodyCap bounds a profile upload. Onboarding sends three short answers;
// anything near this is a client bug.
const personBodyCap = 32 << 10

// handleGetPerson reports what this machine knows about the person.
//
// Onboarded is derived from the file existing rather than stored as a flag: one
// fact, in one place, so the two can never disagree about whether onboarding
// happened.
func (s *Server) handleGetPerson(w http.ResponseWriter, r *http.Request) {
	body := ReadPerson(s.sup.stateDir)
	reply(w, http.StatusOK, agentapi.Person{Notes: body, Onboarded: body != ""})
}

// handlePutPerson replaces the profile with what onboarding collected.
func (s *Server) handlePutPerson(w http.ResponseWriter, r *http.Request) {
	var p agentapi.Person
	if !decode(w, r, personBodyCap, &p) {
		return
	}
	// A body carrying only a zone changes only the zone: WritePerson replaces the
	// file, so treating the settings screen's zone-only save like an onboarding
	// save would answer "I moved to Berlin" by forgetting who they are.
	if !onlyZone(p) {
		if err := WritePerson(s.sup.stateDir, p); err != nil {
			fail(w, http.StatusInternalServerError, "write_failed", err.Error(), "person")
			return
		}
	}
	// The zone is machine state rather than something an agent reads about the
	// person, so it is stored outside the rendered profile. This is its only
	// writer, and going through the supervisor is what moves the guest's own TZ
	// and any existing clock schedules with it.
	s.sup.RememberZone(p.TZ)
	// Every running agent composed its prompt at start, so none of them can see
	// this yet. Evicting is what makes onboarding take effect now rather than
	// whenever an agent happens to be recycled.
	s.sup.EvictIdle()
	w.WriteHeader(http.StatusNoContent)
}

// onlyZone reports whether this profile says nothing except where the person is.
// A GET does not hand back the name and work, so the settings screen has
// nothing else it could send.
func onlyZone(p agentapi.Person) bool {
	return p.TZ != "" && p.Name == "" && p.Work == "" && p.Notes == ""
}

// handleShot serves a handoff screenshot.
//
// The name is taken apart rather than trusted: it arrives from a client, and
// joining it onto a path unchecked is how "../../conversation.json" becomes a
// download. Base strips any path, and the extension check keeps this to images.
func (s *Server) handleShot(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	if filepath.Ext(name) != ".png" {
		fail(w, http.StatusBadRequest, "bad_request", "not an image", "shot")
		return
	}
	path := filepath.Join(shotsDir(s.sup.dirFor(r.PathValue("id"))), name)
	if _, err := os.Stat(path); err != nil {
		fail(w, http.StatusNotFound, "not_found", "no such screenshot", "shot")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	http.ServeFile(w, r, path)
}
