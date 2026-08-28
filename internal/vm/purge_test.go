package vm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// workspaceIn creates a registry over a temp dir with one workspace file on
// disk, standing in for a person's machine that is not currently running.
func workspaceIn(t *testing.T, id string) (*Registry, string) {
	t.Helper()
	base := t.TempDir()
	r := NewRegistry(base, "tester")
	if err := os.MkdirAll(filepath.Join(base, "workspaces"), 0o750); err != nil {
		t.Fatal(err)
	}
	path := r.Layout().Workspace(id)
	if err := os.WriteFile(path, []byte("their data"), 0o640); err != nil {
		t.Fatal(err)
	}
	return r, path
}

// The case the whole feature rests on: the registry holds only running VMs and
// the startup sweep empties it, so a person's machine is usually stopped and
// their workspace is a file nothing owns. Deleting it must still work.
func TestPurgeWorkspaceRemovesAStoppedMachine(t *testing.T) {
	r, path := workspaceIn(t, "alice1")
	if err := r.PurgeWorkspace("alice1"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the workspace survived: %v", err)
	}
}

// Deleting data that is already gone is what a retry looks like, and a retry
// must not report failure.
func TestPurgeWorkspaceIsIdempotent(t *testing.T) {
	r, _ := workspaceIn(t, "alice1")
	if err := r.PurgeWorkspace("alice1"); err != nil {
		t.Fatal(err)
	}
	if err := r.PurgeWorkspace("alice1"); err != nil {
		t.Errorf("second purge = %v, want success", err)
	}
	if err := r.PurgeWorkspace("never-existed"); err != nil {
		t.Errorf("purge of an unknown id = %v, want success", err)
	}
}

// A machine that booted between the lookup and the purge must not have its disk
// removed underneath it -- that would corrupt a live VM rather than tidy a dead
// one. Delete is the path for a running machine.
func TestPurgeWorkspaceRefusesARunningMachine(t *testing.T) {
	r, path := workspaceIn(t, "alice1")
	if _, err := r.Allocate("alice1"); err != nil {
		t.Fatal(err)
	}
	err := r.PurgeWorkspace("alice1")
	if !errors.Is(err, ErrState) {
		t.Fatalf("purge of a live machine = %v, want ErrState", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the live machine's workspace was removed anyway: %v", err)
	}
}

// A purge that could not unlink the file must say so. Reporting success would
// tell someone their data was erased while it sits on disk waiting to come back
// on their next sign-in.
func TestPurgeWorkspaceReportsAFailedRemove(t *testing.T) {
	base := t.TempDir()
	r := NewRegistry(base, "tester")
	dir := filepath.Join(base, "workspaces")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	// A directory where the workspace file should be: os.Remove refuses to
	// unlink a non-empty one, which is the failure this has to surface.
	inner := filepath.Join(r.Layout().Workspace("alice1"), "busy")
	if err := os.MkdirAll(inner, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := r.PurgeWorkspace("alice1"); err == nil {
		t.Fatal("a failed purge reported success")
	}
}
