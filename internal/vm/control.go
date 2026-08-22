package vm

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"cracked/internal/fc"
	"cracked/internal/hostnet"
)

// Pause freezes a running VM via PATCH /vm.
func (r *Registry) Pause(v *VM) error {
	if s := r.Snapshot(v); s != StateRunning {
		return fmt.Errorf("%w: cannot pause from %s", ErrState, s)
	}
	if err := fc.New(r.dirs.Sock(v.ID)).SetVMState("Paused"); err != nil {
		return err
	}
	return r.SetState(v, StatePaused)
}

// Resume unfreezes a paused VM via PATCH /vm.
func (r *Registry) Resume(v *VM) error {
	if s := r.Snapshot(v); s != StatePaused {
		return fmt.Errorf("%w: cannot resume from %s", ErrState, s)
	}
	if err := fc.New(r.dirs.Sock(v.ID)).SetVMState("Resumed"); err != nil {
		return err
	}
	return r.SetState(v, StateRunning)
}

// Refresh reconciles our record with firecracker's own view of the VM.
func (r *Registry) Refresh(v *VM) {
	if s := r.Snapshot(v); s != StateRunning && s != StatePaused {
		return
	}
	_, _ = r.Inspect(v)
}

// Inspect reconciles our record like Refresh but hands back firecracker's
// reply, so a caller that wants to display it does not pay a second round
// trip. The API allows only 10 concurrent connections, so halving the calls
// on a polled page is worth the extra method.
func (r *Registry) Inspect(v *VM) (*fc.InstanceInfo, error) {
	info, err := fc.New(r.dirs.Sock(v.ID)).Describe()
	if err != nil {
		return nil, err
	}
	want := map[string]State{"Running": StateRunning, "Paused": StatePaused}
	if to, ok := want[info.State]; ok {
		_ = r.SetState(v, to)
	}
	return info, nil
}

// View renders a VM for the API, including the URLs a client needs.
func (r *Registry) View(v *VM) View {
	state := r.Snapshot(v)
	return View{
		ID: v.ID, State: state, Slot: v.Slot, VCPUs: VCPUs, MemMiB: MemMiB,
		Tap: v.Tap, GuestIP: v.GuestIP, MAC: v.MAC, PID: v.PID,
		WorkspacePath: r.dirs.Workspace(v.ID), WorkspaceNew: v.workspaceIsNew,
		VNCURL:    fmt.Sprintf("/vms/%s/vnc/vnc.html?path=vms/%s/vnc/websockify", v.ID, v.ID),
		AgentURL:  fmt.Sprintf("/vms/%s/agent/", v.ID),
		CreatedAt: v.CreatedAt,
	}
}

// Sweep reclaims state left by an unclean control-plane exit: orphaned
// firecracker processes, stray taps, and stale run directories. Workspaces are
// never touched, so VM data survives a crash.
func (r *Registry) Sweep() {
	sweepRunDirs(r.dirs)
	sweepTaps()
	if err := os.RemoveAll(r.dirs.RunRoot()); err != nil {
		log.Printf("sweep: remove run root: %v", err)
	}
	if err := os.MkdirAll(r.dirs.RunRoot(), 0750); err != nil {
		log.Printf("sweep: recreate run root: %v", err)
	}
}

// sweepRunDirs kills any firecracker still running from a previous process.
func sweepRunDirs(l Layout) {
	entries, err := filepath.Glob(filepath.Join(l.RunRoot(), "*", "vm.json"))
	if err != nil {
		return
	}
	for _, path := range entries {
		var sf stateFile
		b, err := os.ReadFile(path)
		if err != nil || json.Unmarshal(b, &sf) != nil {
			continue
		}
		killIfOurs(sf.PID, sf.ID)
	}
}

// killIfOurs SIGKILLs a pid only when its cmdline still names firecracker and
// this VM id, which guards against pid reuse.
func killIfOurs(pid int, id string) {
	if pid <= 0 {
		return
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return
	}
	args := strings.ReplaceAll(string(b), "\x00", " ")
	if !strings.Contains(args, "firecracker") || !strings.Contains(args, id) {
		return
	}
	log.Printf("sweep: killing orphaned firecracker pid %d (vm %s)", pid, id)
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}

// sweepTaps deletes every tap this server owns, reclaiming devices whose state
// file was never written.
func sweepTaps() {
	for i := 0; i < MaxVMs; i++ {
		_ = hostnet.DeleteTap(hostnet.TapName(i))
	}
}
