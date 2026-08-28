package vm

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"syscall"
	"time"

	"cracked/internal/hostnet"
	"cracked/internal/util"
)

// Tunables for the create path.
const (
	WorkspaceSize = "5G"
	consoleCap    = 4 << 20
	agentTimeout  = 60 * time.Second
)

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidID reports whether id is an acceptable VM identifier.
func ValidID(id string) bool { return idRe.MatchString(id) }

// Create allocates a slot and boots a microVM, rolling back on any failure.
func (r *Registry) Create(id string) (*VM, error) {
	v, err := r.Allocate(id)
	if err != nil {
		return nil, err
	}
	if err := r.boot(v); err != nil {
		// A workspace this call created is rolled back with it. The purge error
		// is already logged and there is nothing to tell the caller: the boot
		// failure is the answer they get.
		_ = r.cleanup(v, v.workspaceIsNew)
		return nil, err
	}
	if err := r.SetState(v, StateRunning); err != nil {
		r.cleanup(v, false)
		return nil, err
	}
	return v, nil
}

// boot runs every step from directory creation to the guest agent answering.
func (r *Registry) boot(v *VM) error {
	l := r.dirs
	if err := os.MkdirAll(l.RunDir(v.ID), 0750); err != nil {
		return err
	}
	if err := prepareWorkspace(v, l); err != nil {
		return err
	}
	if err := hostnet.CreateTap(v.Tap, v.HostIP, r.user); err != nil {
		return fmt.Errorf("tap: %w", err)
	}
	if err := writeConfigJSON(v, l); err != nil {
		return err
	}
	if err := r.spawn(v); err != nil {
		return err
	}
	return waitForAgent(v)
}

// prepareWorkspace creates the 5 GiB disk, or repairs an existing one. The
// fsck is mandatory: any prior SIGKILL leaves the filesystem dirty.
func prepareWorkspace(v *VM, l Layout) error {
	path := l.Workspace(v.ID)
	if _, err := os.Stat(path); err == nil {
		return fsckWorkspace(path)
	}
	if err := os.MkdirAll(l.WorkspacesDir(), 0750); err != nil {
		return err
	}
	if err := exec.Command("truncate", "-s", WorkspaceSize, path).Run(); err != nil {
		return fmt.Errorf("truncate workspace: %w", err)
	}
	if out, err := exec.Command("mkfs.ext4", "-L", "workspace", "-F", path).CombinedOutput(); err != nil {
		os.Remove(path)
		return fmt.Errorf("mkfs workspace: %w: %s", err, out)
	}
	v.workspaceIsNew = true
	return nil
}

// fsckWorkspace repairs a reused disk. e2fsck exit codes 0 and 1 mean clean or
// successfully repaired; 4 and above are real failures.
func fsckWorkspace(path string) error {
	err := exec.Command("e2fsck", "-p", "-f", path).Run()
	var ee *exec.ExitError
	if err == nil {
		return nil
	}
	if ok := asExitError(err, &ee); ok && ee.ExitCode() < 4 {
		return nil
	}
	return fmt.Errorf("e2fsck %s: %w", path, err)
}

// asExitError unwraps err into an *exec.ExitError if it is one.
func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// spawn removes any stale socket, starts firecracker, and begins watching it.
func (r *Registry) spawn(v *VM) error {
	l := r.dirs
	os.Remove(l.Sock(v.ID))
	console, err := util.NewCappedWriter(l.Console(v.ID), consoleCap)
	if err != nil {
		return err
	}
	cmd := exec.Command("firecracker", "--id", v.ID,
		"--api-sock", l.Sock(v.ID), "--config-file", l.Config(v.ID))
	cmd.Stdout, cmd.Stderr = console, console
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		console.Close()
		return fmt.Errorf("start firecracker: %w", err)
	}
	v.cmd, v.PID, v.console = cmd, cmd.Process.Pid, console
	writeStateFile(v, l)
	go r.watchProcess(v)
	return nil
}

// stateFile is the sweep's only input after an unclean control-plane exit.
type stateFile struct {
	ID        string    `json:"id"`
	Slot      int       `json:"slot"`
	PID       int       `json:"pid"`
	Tap       string    `json:"tap"`
	CreatedAt time.Time `json:"created_at"`
}

// writeStateFile records the pid and tap so a restart can reap this process.
func writeStateFile(v *VM, l Layout) {
	b, err := json.Marshal(stateFile{v.ID, v.Slot, v.PID, v.Tap, v.CreatedAt})
	if err == nil {
		err = os.WriteFile(l.StateFile(v.ID), b, 0640)
	}
	if err != nil {
		log.Printf("vm %s: write state file: %v", v.ID, err)
	}
}

// watchProcess reaps the firecracker child. An exit we did not ask for marks
// the VM failed and reclaims its slot, so a crash never strands capacity.
func (r *Registry) watchProcess(v *VM) {
	err := v.cmd.Wait()
	close(v.done)
	if v.stopping.Load() {
		return
	}
	log.Printf("vm %s: firecracker exited unexpectedly: %v", v.ID, err)
	if serr := r.SetState(v, StateFailed); serr != nil {
		log.Printf("vm %s: %v", v.ID, serr)
	}
	r.cleanup(v, false)
}

// waitForAgent polls the guest agent port until it answers, aborting early if
// the VM dies first.
func waitForAgent(v *VM) error {
	addr := net.JoinHostPort(v.GuestIP, strconv.Itoa(AgentPort))
	deadline := time.Now().Add(agentTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-v.done:
			return fmt.Errorf("vm %s exited during boot", v.ID)
		case <-time.After(250 * time.Millisecond):
		}
		if c, err := net.DialTimeout("tcp", addr, time.Second); err == nil {
			c.Close()
			return nil
		}
	}
	return fmt.Errorf("agent on %s did not come up within %s", addr, agentTimeout)
}
