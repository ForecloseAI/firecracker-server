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
