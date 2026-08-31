package chat

import (
	"encoding/json"
	"io"
	"log"
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

// maxAttachmentV1 is the most this will copy out of a guest.
//
// Set ABOVE the guest's own send limit on purpose, so it stays a backstop and
// never a cap: io.Copy through a LimitReader stops at the limit and reports no
// error, so a limit set AT the guest's would hand the person a truncated file
// and call it a success. The guest's refusal is the real limit.
const maxAttachmentV1 = 20<<20 + 1<<20

// getAttachment proxies a file an agent sent out of the guest.
//
// Proxied rather than linked, for the reason getShot is: it is the person's own
// file, so it stays behind the same session check as the rest of /v1.
//
// The guest's own Content-Type and Content-Disposition are forwarded rather than
// recomputed here. The guest is what decided whether a file may render inline,
// and this is the hop that actually faces a browser, so dropping that decision
// at this end would make the care taken at the other end worth nothing.
func (s *Server) getAttachment(w http.ResponseWriter, r *http.Request, user string) {
	cl, err := guestOf(s, user)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	resp, err := cl.Attachment(r.PathValue("id"), r.PathValue("name"))
	if err != nil {
		fail(w, http.StatusNotFound, "no such attachment")
		return
	}
	defer resp.Body.Close()
	forwardAttachmentHeaders(w, resp)
	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxAttachmentV1)); err != nil {
		// The status and headers are already sent, so the person gets a short
		// file whatever happens now. The log is the only place it can be seen.
		log.Printf("chat attachment %s: %v", r.PathValue("name"), err)
	}
}

// forwardAttachmentHeaders passes on what the guest decided about this file.
func forwardAttachmentHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, h := range []string{"Content-Type", "Content-Disposition", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	// Set here as well as on the guest: this is the response a browser sees, and
	// without it one is free to sniff an octet-stream back into HTML.
	w.Header().Set("X-Content-Type-Options", "nosniff")
}
