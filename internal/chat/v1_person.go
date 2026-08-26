package chat

import (
	"encoding/json"
	"io"
	"net/http"

	"cracked/internal/agentapi"
)

// maxUploadV1 is the largest file the app may send. The client enforces the same
// number, so anything over it is a client that skipped the check.
const maxUploadV1 = 20 << 20

// getProfile reports what the machine knows about the person, and whether
// onboarding has happened.
func (s *Server) getProfile(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	p, err := cl.Person()
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// putProfile saves what onboarding collected.
func (s *Server) putProfile(w http.ResponseWriter, r *http.Request, user string) {
	var p agentapi.Person
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10)).Decode(&p) != nil {
		fail(w, http.StatusBadRequest, "could not read the profile")
		return
	}
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if err := cl.SetPerson(p); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadFile streams a file from the app into the person's machine.
//
// Streamed straight through rather than read into memory first: this process
// shares a host with five VMs, and buffering every upload would take memory from
// them for no gain.
func (s *Server) uploadFile(w http.ResponseWriter, r *http.Request, user string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadV1+(1<<20))
	file, header, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "no file in the request")
		return
	}
	defer file.Close()
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	saved, err := cl.Upload(header.Filename, file)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

// getShot proxies a handoff screenshot out of the guest.
//
// Proxied rather than linked: the picture is of the person's own screen, so it
// must stay behind the same session check as the rest of /v1 instead of becoming
// a URL that works for anyone holding it.
func (s *Server) getShot(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	body, err := cl.Shot(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusNotFound, "no such screenshot")
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	io.Copy(w, io.LimitReader(body, 8<<20))
}
