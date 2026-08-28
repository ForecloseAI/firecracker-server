package vm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
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

// Creating -> Stopping is legal, so a delete can land on a machine that is still
// booting -- and shutdown returns at once because no process exists yet. The
// purge therefore takes effect immediately, which is what makes the recreate
// race in Create worth guarding against.
func TestDeleteDuringCreationPurgesAtOnce(t *testing.T) {
	if !canTransition(StateCreating, StateStopping) {
		t.Skip("a delete can no longer land on a booting machine")
	}
	r, path := workspaceIn(t, "alice1")
	v, err := r.Allocate("alice1") // as Create does, before it boots anything
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- r.Delete(v, true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("delete during creation = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("delete blocked on a machine with no process")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the workspace survived: %v", err)
	}
	if _, err := r.Get("alice1"); err == nil {
		t.Error("the deleted machine is still registered")
	}
}

// A workspace this Create made must not outlive a delete that raced it. Nothing
// follows stopping, so a failed transition to running is that race, and the disk
// has to go with it -- otherwise the machine someone just erased comes back with
// a fresh workspace their next request adopts.
func TestCleanupPurgesAWorkspaceThisCallCreated(t *testing.T) {
	r, path := workspaceIn(t, "alice1")
	v, err := r.Allocate("alice1")
	if err != nil {
		t.Fatal(err)
	}
	v.workspaceIsNew = true
	if err := r.cleanup(v, v.workspaceIsNew); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("a workspace created by a raced boot was left behind")
	}
}

// The mirror: a workspace that was already there is a returning person's data,
// and a boot that fails for its own reasons must not take it.
func TestCleanupKeepsAPreexistingWorkspace(t *testing.T) {
	r, path := workspaceIn(t, "alice1")
	v, err := r.Allocate("alice1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.cleanup(v, v.workspaceIsNew); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a failed boot destroyed existing data: %v", err)
	}
}

// A rollback that arrives after the id was reclaimed must not reach into the
// machine that holds it now. cleanup addresses the tap, run directory and disk
// by id, so a late one would take a live machine's resources -- including, since
// a raced creation now purges, its workspace.
func TestCleanupSkipsWhenTheIdBelongsToANewerMachine(t *testing.T) {
	r, path := workspaceIn(t, "alice1")
	stale, err := r.Allocate("alice1")
	if err != nil {
		t.Fatal(err)
	}
	stale.workspaceIsNew = true
	r.Release(stale) // as a delete racing this creation would
	fresh, err := r.Allocate("alice1")
	if err != nil {
		t.Fatal(err)
	}
	// The conflict is the point of the companion test; here it is the resources
	// surviving that matters.
	if err := r.cleanup(stale, stale.workspaceIsNew); !errors.Is(err, ErrState) {
		t.Fatalf("cleanup = %v, want a conflict", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("a stale rollback destroyed the current machine's disk: %v", err)
	}
	if got, err := r.Get("alice1"); err != nil || got != fresh {
		t.Errorf("a stale rollback deregistered the current machine: %v", err)
	}
}

// A creation that rolls back must not leave its process behind.
//
// cleanup is the only step every exit path shares, and it released the named
// resources without ever signalling the process. shutdown covers the delete
// path, but a creation rolling back has already spawned by the time
// waitForAgent times out or a raced delete steals its state -- and the process
// would outlive every record of it, holding a disk nothing can reach.
func TestCleanupReapsAProcessLeftByARollback(t *testing.T) {
	r, _ := workspaceIn(t, "alice1")
	v, err := r.Allocate("alice1")
	if err != nil {
		t.Fatal(err)
	}
	// A real child that ignores everything short of SIGKILL, standing in for a
	// firecracker that spawned and then had its creation rolled back.
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	v.cmd, v.PID = cmd, cmd.Process.Pid
	v.done = make(chan struct{})
	go func() { _ = cmd.Wait(); close(v.done) }()
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

	if err := r.cleanup(v, true); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	select {
	case <-v.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the rolled back creation's process is still running")
	}
}

// A VM that never spawned must not have a signal sent anywhere. Kill(-0, ...)
// would reach the control plane's own process group.
func TestCleanupDoesNotSignalWhenNothingSpawned(t *testing.T) {
	r, _ := workspaceIn(t, "alice1")
	v, err := r.Allocate("alice1")
	if err != nil {
		t.Fatal(err)
	}
	if v.PID != 0 || v.cmd != nil {
		t.Fatal("a freshly allocated VM already has a process")
	}
	if err := r.cleanup(v, true); err != nil { // must not signal, must not hang
		t.Fatalf("cleanup: %v", err)
	}
}

// A skipped cleanup must not report a purge that did not happen.
//
// The machine now holding this id was never touched and its workspace is intact,
// so nil here would answer 204 and tell someone their data was erased. Reachable
// without two deletes racing: firecracker exits on its own just before a delete,
// watchProcess cleans up with purge=false and releases the id, a first-use
// request recreates the machine, and the delete's own cleanup lands here.
func TestASkippedPurgeIsReportedAsAConflict(t *testing.T) {
	r, path := workspaceIn(t, "alice1")
	stale, err := r.Allocate("alice1")
	if err != nil {
		t.Fatal(err)
	}
	r.Release(stale)
	if _, err := r.Allocate("alice1"); err != nil { // the replacement
		t.Fatal(err)
	}
	err = r.cleanup(stale, true)
	if !errors.Is(err, ErrState) {
		t.Fatalf("a skipped purge returned %v, want a conflict", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the replacement's workspace was removed: %v", err)
	}
}

// A skipped cleanup that was not asked to purge promised nothing, so it stays
// success -- the Create rollback paths ignore the result and must keep working.
func TestASkippedStopIsNotAnError(t *testing.T) {
	r, _ := workspaceIn(t, "alice1")
	stale, err := r.Allocate("alice1")
	if err != nil {
		t.Fatal(err)
	}
	r.Release(stale)
	if _, err := r.Allocate("alice1"); err != nil {
		t.Fatal(err)
	}
	if err := r.cleanup(stale, false); err != nil {
		t.Errorf("a skipped stop returned %v, want success", err)
	}
}
