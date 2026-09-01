package agentd

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// writeOutbox puts a file in the boss's outbox, as a send would have.
func writeOutbox(t *testing.T, s *Server, name, body string) {
	t.Helper()
	dir := outboxDir(s.sup.dirFor(BossID))
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

// An image is served as itself, so the app can draw it in the conversation.
func TestAttachmentRouteServesAnImageInline(t *testing.T) {
	s, _ := newTestServer(t)
	writeOutbox(t, s, "0001-screen.png", "pretend this is a png")
	rec := do(t, s, http.MethodGet, "/agents/boss/outbox/0001-screen.png", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "image/png" {
		t.Errorf("Content-Type is %q", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("an image was forced to download: %q", got)
	}
	if rec.Body.String() != "pretend this is a png" {
		t.Errorf("body is %q", rec.Body.String())
	}
}

// Everything else is handed over as a download. The AGENT names these files, so
// a web client that rendered an agent-named .html from its own origin would be
// running that file's script; nosniff stops a browser guessing its way back.
func TestAttachmentRouteForcesADownloadForAnythingElse(t *testing.T) {
	s, _ := newTestServer(t)
	writeOutbox(t, s, "0002-note.html", "<script>alert(1)</script>")
	rec := do(t, s, http.MethodGet, "/agents/boss/outbox/0002-note.html", "")
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type is %q", got)
	}
	// Named the way the person reads it: a browser saves what this says, and the
	// sequence prefix is ours, not something to leave in their downloads folder.
	if got := rec.Header().Get("Content-Disposition"); got != "attachment; filename=note.html" {
		t.Errorf("Content-Disposition is %q", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options is %q", got)
	}
}

// The name is stripped to its last element, so the only files this can reach are
// the ones an agent put in its own outbox -- not the ones a level above it.
func TestAttachmentRouteCannotReachOutsideTheOutbox(t *testing.T) {
	s, _ := newTestServer(t)
	dir := s.sup.dirFor(BossID)
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("private"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secret.txt", "0009-never-sent.pdf"} {
		if rec := do(t, s, http.MethodGet, "/agents/boss/outbox/"+name, ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", name, rec.Code)
		}
	}
}
