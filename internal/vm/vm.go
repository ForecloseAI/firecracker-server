// Package vm owns microVM lifecycle and state. It has no HTTP knowledge.
package vm

import (
	"os/exec"
	"slices"
	"sync/atomic"
	"time"
)

// State is a VM's lifecycle position. Illegal transitions are rejected with 409.
type State string

const (
	StateCreating State = "creating"
	StateRunning  State = "running"
	StatePaused   State = "paused"
	StateStopping State = "stopping"
	StateFailed   State = "failed"
)

// Fixed per-VM sizing and host budget. Sizing is not request-configurable in v1.
const (
	MaxVMs     = 5
	VCPUs      = 2
	MemMiB     = 4096
	HostMemMiB = 28672
	KernelName = "vmlinux-6.18"

	AgentPort = 8080
	VNCPort   = 6080
)

// VM is one microVM occupying one slot. Mutable fields are guarded by the
// owning Registry's mutex; cmd/done are written once at spawn.
type VM struct {
	ID        string
	Slot      int
	State     State
	PID       int
	CreatedAt time.Time
	Tap       string
	HostIP    string
	GuestIP   string
	MAC       string

	cmd            *exec.Cmd
	done           chan struct{}
	stopping       atomic.Bool
	workspaceIsNew bool
	console        interface{ Close() error }

	// Previous CPU sample, so Stats can report a rate rather than a lifetime
	// average. Guarded by the Registry mutex, like State.
	lastCPUSeconds float64
	lastSampleAt   time.Time
}

// View is the JSON representation returned by the API.
type View struct {
	ID            string    `json:"id"`
	State         State     `json:"state"`
	Slot          int       `json:"slot"`
	VCPUs         int       `json:"vcpus"`
	MemMiB        int       `json:"mem_mib"`
	Tap           string    `json:"tap"`
	GuestIP       string    `json:"guest_ip"`
	MAC           string    `json:"mac"`
	PID           int       `json:"pid"`
	WorkspacePath string    `json:"workspace_path"`
	WorkspaceNew  bool      `json:"workspace_new"`
	VNCURL        string    `json:"vnc_url"`
	AgentURL      string    `json:"agent_url"`
	CreatedAt     time.Time `json:"created_at"`
}

// legalTransitions lists the states reachable from each state.
var legalTransitions = map[State][]State{
	StateCreating: {StateRunning, StateStopping, StateFailed},
	StateRunning:  {StatePaused, StateStopping, StateFailed},
	StatePaused:   {StateRunning, StateStopping, StateFailed},
	StateStopping: {},
	StateFailed:   {StateStopping},
}

// canTransition reports whether from -> to is a legal state change.
func canTransition(from, to State) bool {
	return slices.Contains(legalTransitions[from], to)
}
