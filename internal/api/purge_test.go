package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"cracked/internal/vm"
)

// stoppedWorkspace stands a server up with one workspace on disk and no VM in
// the registry -- a person's machine after the control plane restarted, which
// the startup sweep makes the ordinary state rather than a rare one.
func stoppedWorkspace(t *testing.T, id string) (http.Handler, string) {
	t.Helper()
	base := t.TempDir()
	s := New(vm.NewRegistry(base, "cracked"), "s3cret")
	if err := os.MkdirAll(filepath.Join(base, "workspaces"), 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "workspaces", id+".ext4")
	if err := os.WriteFile(path, []byte("their data"), 0o640); err != nil {
		t.Fatal(err)
	}
	return s.Routes(), path
}

// del issues an authenticated delete and returns the recorder.
func del(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("DELETE", path, nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// Purging a machine that is not running has to erase it. Answering 404 here --
// which is what a registry lookup alone does, since the registry holds only live
// VMs -- would make "delete my data" a no-op for most people and hand the data
// straight back on their next sign-in.
func TestPurgeWorksWhenTheMachineIsNotRunning(t *testing.T) {
	h, path := stoppedWorkspace(t, "alice1")
	w := del(h, "/vms/alice1?purge=true")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body)
	}
	var got struct {
		ID     string `json:"id"`
		Purged bool   `json:"purged"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "alice1" || !got.Purged {
		t.Errorf("body = %+v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the workspace survived the purge: %v", err)
	}
}

// A plain stop of a machine that is not running is still a 404. Only a purge
// reaches past the registry, because only a purge has something left to do.
func TestDeleteWithoutPurgeStill404sWhenNotRunning(t *testing.T) {
	h, path := stoppedWorkspace(t, "alice1")
	if w := del(h, "/vms/alice1"); w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a non-purge delete removed the workspace: %v", err)
	}
}

// The id reaches a filesystem path, so a traversal must be refused rather than
// resolved. Without the shape check this deletes files outside the workspaces
// directory entirely.
func TestPurgeRefusesAnIdThatIsNotAnId(t *testing.T) {
	base := t.TempDir()
	s := New(vm.NewRegistry(base, "cracked"), "s3cret")
	h := s.Routes()
	victim := filepath.Join(base, "images", "rootfs.ext4")
	if err := os.MkdirAll(filepath.Dir(victim), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("shared image"), 0o640); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"..%2f..%2fimages%2frootfs", "Alice1", "a_b"} {
		w := del(h, "/vms/"+id+"?purge=true")
		if w.Code != http.StatusBadRequest && w.Code != http.StatusNotFound {
			t.Errorf("id %q gave %d, want a refusal", id, w.Code)
		}
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("the shared image was removed: %v", err)
	}
}

// Purging is what a retry looks like when the first attempt's response was lost.
func TestPurgeOfAlreadyDeletedDataSucceeds(t *testing.T) {
	h, _ := stoppedWorkspace(t, "alice1")
	if w := del(h, "/vms/alice1?purge=true"); w.Code != http.StatusOK {
		t.Fatalf("first purge = %d", w.Code)
	}
	if w := del(h, "/vms/alice1?purge=true"); w.Code != http.StatusOK {
		t.Errorf("second purge = %d, want 200", w.Code)
	}
}
