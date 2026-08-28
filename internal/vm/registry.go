package vm

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"cracked/internal/hostnet"
)

// Sentinel errors the API layer maps onto status codes.
var (
	ErrNotFound  = errors.New("vm not found")
	ErrDuplicate = errors.New("id already in use")
	ErrNoSlots   = errors.New("no free slot")
	ErrState     = errors.New("illegal state transition")
)

// Registry is the single owner of all VM state and the fixed slot table.
type Registry struct {
	mu    sync.Mutex
	slots [MaxVMs]*VM
	byID  map[string]*VM
	dirs  Layout
	user  string
}

// NewRegistry builds an empty registry rooted at base, running taps as user.
func NewRegistry(base, user string) *Registry {
	return &Registry{byID: map[string]*VM{}, dirs: Layout{Base: base}, user: user}
}

// Layout exposes the path helper so callers share one path derivation.
func (r *Registry) Layout() Layout { return r.dirs }

// Allocate claims a free slot for id and inserts it in the creating state.
// Everything but the slot index is derived, so nothing can drift.
func (r *Registry) Allocate(id string) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[id]; ok {
		return nil, fmt.Errorf("%w: %s", ErrDuplicate, id)
	}
	slot := r.freeSlot()
	if slot < 0 {
		return nil, fmt.Errorf("%w: %d/%d in use", ErrNoSlots, MaxVMs, MaxVMs)
	}
	v := newVM(id, slot)
	r.slots[slot], r.byID[id] = v, v
	return v, nil
}

// newVM builds a VM with all slot-derived addressing filled in.
func newVM(id string, slot int) *VM {
	host, guest, mac := hostnet.SlotAddrs(slot)
	return &VM{
		ID: id, Slot: slot, State: StateCreating, CreatedAt: time.Now().UTC(),
		Tap: hostnet.TapName(slot), HostIP: host, GuestIP: guest, MAC: mac,
		done: make(chan struct{}),
	}
}

// freeSlot returns the first unused slot index, or -1. Caller holds mu.
func (r *Registry) freeSlot() int {
	for i, v := range r.slots {
		if v == nil {
			return i
		}
	}
	return -1
}

// Release frees a VM's slot and id. Safe to call more than once.
func (r *Registry) Release(v *VM) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releaseLocked(v)
}

// releaseLocked is Release for callers already holding the mutex. Both checks
// compare the pointer, so releasing a VM whose id or slot has since been taken
// by another does not evict that one.
func (r *Registry) releaseLocked(v *VM) {
	if r.slots[v.Slot] == v {
		r.slots[v.Slot] = nil
	}
	if r.byID[v.ID] == v {
		delete(r.byID, v.ID)
	}
}

// PurgeWorkspace deletes the persisted disk of a VM that is not running.
//
// Delete covers the running case. This is the other one, and it is the common
// one: the registry only holds live VMs, and Sweep empties it on every restart,
// so most of the time a person's machine is stopped and their workspace is just
// a file on disk that nothing owns. Without this there is no way to honour
// "delete everything" for them.
//
// The absence check happens under the same lock that Allocate takes, so a VM
// that boots concurrently cannot have its disk pulled out from under it: either
// this runs first and the boot recreates an empty workspace, or the boot wins
// and this refuses.
func (r *Registry) PurgeWorkspace(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, live := r.byID[id]; live {
		return fmt.Errorf("%w: %s is running", ErrState, id)
	}
	if err := os.Remove(r.dirs.Workspace(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("purge workspace %s: %w", id, err)
	}
	return nil
}

// Get looks up a VM by id.
func (r *Registry) Get(id string) (*VM, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	return v, nil
}

// List returns every live VM, ordered by slot.
func (r *Registry) List() []*VM {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*VM, 0, MaxVMs)
	for _, v := range r.slots {
		if v != nil {
			out = append(out, v)
		}
	}
	return out
}

// SetState applies a state change, rejecting illegal transitions.
func (r *Registry) SetState(v *VM, to State) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v.State == to {
		return nil
	}
	if !canTransition(v.State, to) {
		return fmt.Errorf("%w: %s -> %s", ErrState, v.State, to)
	}
	v.State = to
	return nil
}

// Snapshot reads a VM's state under the lock.
func (r *Registry) Snapshot(v *VM) State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return v.State
}

// Capacity reports the host budget. vCPU and RAM are derived from slot usage,
// since sizing is fixed in v1.
type Capacity struct {
	SlotsTotal     int `json:"slots_total"`
	SlotsUsed      int `json:"slots_used"`
	SlotsFree      int `json:"slots_free"`
	VCPUsAllocated int `json:"vcpus_allocated"`
	MemTotalMiB    int `json:"mem_total_mib"`
	MemAllocated   int `json:"mem_allocated_mib"`
	MemFreeMiB     int `json:"mem_free_mib"`
}

// Capacity computes the current allocation report.
func (r *Registry) Capacity() Capacity {
	r.mu.Lock()
	defer r.mu.Unlock()
	used := 0
	for _, v := range r.slots {
		if v != nil {
			used++
		}
	}
	return Capacity{
		SlotsTotal: MaxVMs, SlotsUsed: used, SlotsFree: MaxVMs - used,
		VCPUsAllocated: used * VCPUs, MemTotalMiB: HostMemMiB,
		MemAllocated: used * MemMiB, MemFreeMiB: HostMemMiB - used*MemMiB,
	}
}
