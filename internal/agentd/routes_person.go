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
	if err := WritePerson(s.sup.stateDir, p); err != nil {
		fail(w, http.StatusInternalServerError, "write_failed", err.Error(), "person")
		return
	}
	// The zone is not part of the rendered profile -- it is machine state, not
	// something an agent reads about the person -- so it is stored separately.
	// Going through the supervisor rather than the plain function is what moves
	// any existing clock schedules onto the new zone.
	s.sup.RememberZone(p.TZ, "")
	// Every running agent composed its prompt at start, so none of them can see
	// this yet. Evicting is what makes onboarding take effect now rather than
	// whenever an agent happens to be recycled.
	s.sup.EvictIdle()
	w.WriteHeader(http.StatusNoContent)
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
