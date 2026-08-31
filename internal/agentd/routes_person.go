package agentd

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

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
	// The zone comes back too. It is machine state rather than something an agent
	// reads, so it is not in the rendered profile -- but the app has no other way
	// to learn it, and without it a reinstall or a second device shows "Not set"
	// beside a machine that is on a perfectly good clock.
	reply(w, http.StatusOK, agentapi.Person{
		Notes: body, Onboarded: body != "", TZ: readZoneFile(s.sup.stateDir)})
}

// handlePutPerson replaces the profile with what onboarding collected.
func (s *Server) handlePutPerson(w http.ResponseWriter, r *http.Request) {
	var p agentapi.Person
	if !decode(w, r, personBodyCap, &p) {
		return
	}
	// A zone we cannot resolve is refused rather than ignored. This call is the
	// only way a zone reaches the machine, so accepting a bad one with a 204 and
	// storing nothing would leave both sides believing the clock had moved.
	if p.TZ != "" {
		if _, err := time.LoadLocation(p.TZ); err != nil {
			fail(w, http.StatusBadRequest, "bad_request",
				strconv.Quote(p.TZ)+" is not a timezone such as Asia/Kolkata", "person")
			return
		}
	}
	// A body carrying only a zone changes only the zone, because WritePerson
	// replaces the file and the settings screen's zone-only save would otherwise
	// answer "I moved to Berlin" by forgetting who they are.
	//
	// Unless there is nothing to forget yet. Onboarded is derived from this file
	// existing, so skipping the write for someone who picked a country and
	// skipped the questions would leave them un-onboarded and asked again on
	// every launch. Nothing to protect means nothing to skip.
	if !onlyZone(p) || ReadPerson(s.sup.stateDir) == "" {
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
//
// A GET hands back the whole rendered profile in Notes and nothing in Name or
// Work, so a client cannot echo back what it read as the fields it came from.
// The settings screen therefore sends the zone alone, and this is what tells
// that apart from an onboarding body.
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
