package agentd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cracked/internal/agentapi"
)

// maxUpload is the largest file the app may send, matching what it enforces on
// its side. The guest writes to a 5 GiB overlay it shares with Chrome's profile
// and everything the agents produce, so this cannot be open-ended.
const maxUpload = 20 << 20

// uploadsDir is the one folder files from the app land in.
//
// Inside the shared workspace, not an agent's private state: the person sends a
// file to whoever they happen to be talking to, and then asks someone else about
// it. Every agent already reaches the workspace, so this needs no new permission.
func uploadsDir(workspace string) string { return filepath.Join(workspace, "uploads") }

// handleUpload stores a file from the app and reports where it landed.
func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+(1<<20))
	file, header, err := r.FormFile("file")
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "no file in the request", "file")
		return
	}
	defer file.Close()
	// The person's clock, not the guest's: an upload sent at 11pm belongs in
	// today's folder as they would name it, the same date a task folder gets.
	saved, err := saveUpload(s.sup.workspace, header.Filename, file, personNow(s.sup.stateDir))
	if err != nil {
		fail(w, http.StatusInternalServerError, "write_failed", err.Error(), "file")
		return
	}
	reply(w, http.StatusCreated, saved)
}

// saveUpload writes one upload to the uploads folder under a safe, dated name.
func saveUpload(workspace, name string, body io.Reader, now time.Time) (agentapi.File, error) {
	dir := uploadsDir(workspace)
	if err := os.MkdirAll(dir, 0o770); err != nil {
		return agentapi.File{}, err
	}
	clean := now.Format("2006-01-02") + "-" + safeName(name)
	f, err := os.OpenFile(filepath.Join(dir, clean), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o660)
	if err != nil {
		return agentapi.File{}, err
	}
	defer f.Close()
	n, err := io.Copy(f, io.LimitReader(body, maxUpload+1))
	if err != nil {
		return agentapi.File{}, err
	}
	if n > maxUpload {
		os.Remove(f.Name())
		return agentapi.File{}, fmt.Errorf("the file is larger than %d MB", maxUpload>>20)
	}
	return agentapi.File{Name: clean, Path: f.Name(), Size: n}, nil
}

// safeName reduces a client-supplied filename to something that cannot escape
// the uploads folder or confuse a shell.
//
// The path is handed to agents who will pass it to Bash, so a name is stripped
// to its last element and then to a known alphabet rather than merely checked:
// rejecting what looks dangerous means guessing at every way it could be written.
func safeName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return trimmedName(strings.Trim(b.String(), ".-"))
}

// trimmedName bounds the length and never returns empty.
func trimmedName(name string) string {
	if name == "" {
		return "file"
	}
	if len(name) > 120 {
		return name[:120]
	}
	return name
}

// handleAttachment serves a file an agent sent the person: the mirror of
// handleUpload, and the way anything the agents produce actually leaves here.
//
// Modelled on handleShot. The name is stripped to its last element, so the only
// files reachable are the ones an agent put in its own outbox; there is no path
// in this request that could point anywhere else.
func (s *Server) handleAttachment(w http.ResponseWriter, r *http.Request) {
	name := filepath.Base(r.PathValue("name"))
	path := filepath.Join(outboxDir(s.sup.dirFor(r.PathValue("id"))), name)
	if _, err := os.Stat(path); err != nil {
		fail(w, http.StatusNotFound, "not_found", "no such attachment", "attachment")
		return
	}
	setAttachmentHeaders(w, name)
	http.ServeFile(w, r, path)
}

// setAttachmentHeaders decides whether a browser may render this or must save it.
//
// Only images are served as themselves. The AGENT chooses these filenames, so a
// web client that let one name an .html and then served it from its own origin
// would be running that file's script; nosniff closes the same hole a second
// time, by stopping a browser guessing its way past the octet-stream.
//
// ServeFile only sniffs a type when none is set, so this wins.
func setAttachmentHeaders(w http.ResponseWriter, name string) {
	mimeType, isImage := attachmentMIME(name)
	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Deliberately not "immutable", and not a year. A number is never reused
	// while the agent lives, but DELETE ?purge=true takes its outbox with it and
	// a new agent minted with the same id starts again at 0001 -- so a name can
	// come to mean different bytes. An hour matches the shot route and is all a
	// conversation needs.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	if !isImage {
		// The name the person reads, not the one on disk: a browser saves what
		// this says, and "0001-report.pdf" is our sequence leaking into their
		// downloads folder. The URL keeps the number; the filename does not.
		w.Header().Set("Content-Disposition", `attachment; filename="`+readableName(name)+`"`)
	}
}
