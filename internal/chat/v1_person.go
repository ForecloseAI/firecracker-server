package chat

import (
	"encoding/json"
	"io"
	"log"
	"mime"
	"net/http"
	"strconv"

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
	setAttachmentHeaders(w, r.PathValue("name"), resp)
	if _, err := io.Copy(w, io.LimitReader(resp.Body, maxAttachmentV1)); err != nil {
		// The status and headers are already sent, so the person gets a short
		// file whatever happens now. The log is the only place it can be seen.
		// %q, because the name is a path segment a client chose and a raw one
		// could put a newline in the log and forge a line after it.
		log.Printf("chat attachment %q: %v", r.PathValue("name"), err)
	}
}

// setAttachmentHeaders decides how this file may be served, HERE, at the gateway.
//
// Recomputed rather than taken from the guest, and that is the whole point. A
// guest is the person's own machine and they have root on it, so a daemon they
// patched can answer any Content-Type it likes. This response comes back on the
// same origin as the operator console and its __Host-sess cookie, so served as
// text/html, guest-authored script would run with that authority -- and nosniff
// does nothing about an explicitly declared type, only about a sniffed one. The
// VNC gateway is kept off this origin for exactly the same reason.
//
// The name is not trusted either: a patched guest answers 200 to any name it
// likes, so the filename is formatted by mime rather than concatenated into the
// header.
func setAttachmentHeaders(w http.ResponseWriter, name string, resp *http.Response) {
	mimeType, isImage := agentapi.AttachmentMIME(name)
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if !isImage {
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment",
			map[string]string{"filename": agentapi.ReadableName(name)}))
	}
	forwardLength(w, resp)
}

// forwardLength passes on the guest's Content-Length when it is a plausible one.
//
// Worth having: without it the response is chunked, so the client gets no total
// size -- no progress on a 20 MB file, and no way to notice the truncation the
// copy error can only write to a log. Worth checking: it is the one number here
// that still comes from the guest.
func forwardLength(w http.ResponseWriter, resp *http.Response) {
	n, err := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	if err != nil || n < 0 || n > maxAttachmentV1 {
		return
	}
	w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
}
