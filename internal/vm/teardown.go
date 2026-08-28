package vm

import (
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"cracked/internal/fc"
	"cracked/internal/hostnet"
)

// Teardown ladder budgets.
const (
	// 20s, not 10: the guest must stop Chrome cleanly inside this window or its
	// profile LevelDB tears mid-write and saved logins are lost. Normal shutdown
	// is ~2s, so the extra budget only applies to VMs that are already hanging.
	gracefulWait = 20 * time.Second
	termWait     = 5 * time.Second
	killWait     = 2 * time.Second
)

// Delete stops a VM and reclaims its resources, optionally purging the
// persisted workspace.
func (r *Registry) Delete(v *VM, purge bool) error {
	prev, err := r.beginStop(v)
	if err != nil {
		return err
	}
	r.shutdown(v, prev)
	return r.cleanup(v, purge)
}

// beginStop marks the VM stopping so concurrent deletes conflict without
// holding the mutex across the shutdown ladder.
func (r *Registry) beginStop(v *VM) (State, error) {
	prev := r.Snapshot(v)
	if err := r.SetState(v, StateStopping); err != nil {
		return prev, err
	}
	v.stopping.Store(true)
	return prev, nil
}

// shutdown walks the ladder: resume if paused, Ctrl+Alt+Del, SIGTERM, SIGKILL.
func (r *Registry) shutdown(v *VM, prev State) {
	if v.cmd == nil {
		return
	}
	client := fc.New(r.dirs.Sock(v.ID))
	// A paused VM cannot process the injected keystroke, so resume it first or
	// every paused delete burns the full graceful timeout.
	if prev == StatePaused || r.wasPaused(client) {
		_ = client.SetVMState("Resumed")
	}
	if err := client.Action("SendCtrlAltDel"); err != nil {
		log.Printf("vm %s: ctrl-alt-del: %v", v.ID, err)
	}
	if r.waitExit(v, gracefulWait) {
		return
	}
	r.escalate(v)
}

// wasPaused asks firecracker directly, covering states set outside our record.
func (r *Registry) wasPaused(c *fc.Client) bool {
	info, err := c.Describe()
	return err == nil && info.State == "Paused"
}

// escalate sends SIGTERM then SIGKILL to the process group. This is the
// overwatcher the firecracker prod docs recommend, inlined.
func (r *Registry) escalate(v *VM) {
	_ = v.cmd.Process.Signal(syscall.SIGTERM)
	if r.waitExit(v, termWait) {
		return
	}
	_ = syscall.Kill(-v.PID, syscall.SIGKILL)
	if !r.waitExit(v, killWait) {
		log.Printf("vm %s: pid %d survived SIGKILL", v.ID, v.PID)
	}
}

// waitExit blocks until the process exits or the budget elapses.
func (r *Registry) waitExit(v *VM, d time.Duration) bool {
	select {
	case <-v.done:
		return true
	case <-time.After(d):
		return false
	}
}

// cleanup releases every host resource for a VM. It runs on all exit paths and
// tolerates each step already being done.
//
// Only the purge can fail the caller. The rest is best-effort and logged: a
// leaked tap or run directory is an operational annoyance, while a workspace
// that did not get removed is a person being told their data is gone when it is
// still on disk and will come back on their next sign-in.
func (r *Registry) cleanup(v *VM, purge bool) error {
	l := r.dirs
	if v.console != nil {
		_ = v.console.Close()
	}
	os.Remove(l.Sock(v.ID))
	if err := hostnet.DeleteTap(v.Tap); err != nil {
		log.Printf("vm %s: delete tap %s: %v", v.ID, v.Tap, err)
	}
	os.RemoveAll(l.RunDir(v.ID))
	var purgeErr error
	if purge {
		if err := os.Remove(l.Workspace(v.ID)); err != nil && !os.IsNotExist(err) {
			log.Printf("vm %s: purge workspace: %v", v.ID, err)
			purgeErr = fmt.Errorf("purge workspace %s: %w", v.ID, err)
		}
	}
	r.Release(v)
	return purgeErr
}

// DrainAll stops every VM within a total budget, for control-plane shutdown.
func (r *Registry) DrainAll(budget time.Duration) {
	vms := r.List()
	if len(vms) == 0 {
		return
	}
	done := make(chan struct{})
	go func() {
		for _, v := range vms {
			if err := r.Delete(v, false); err != nil {
				log.Printf("vm %s: drain: %v", v.ID, err)
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(budget):
		log.Printf("drain budget exceeded; killing remaining VMs")
		r.killRemaining()
	}
}

// killRemaining SIGKILLs any VM still alive after the drain budget.
func (r *Registry) killRemaining() {
	for _, v := range r.List() {
		if v.PID > 0 {
			_ = syscall.Kill(-v.PID, syscall.SIGKILL)
		}
		r.cleanup(v, false)
	}
}
