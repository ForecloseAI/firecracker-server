package vm

import "testing"

// legalEdges lists every permitted state transition.
var legalEdges = []struct{ from, to State }{
	{StateCreating, StateRunning}, {StateCreating, StateFailed},
	{StateRunning, StatePaused}, {StatePaused, StateRunning},
	{StateRunning, StateStopping}, {StatePaused, StateStopping},
	{StateRunning, StateFailed}, {StateFailed, StateStopping},
}

// illegalEdges lists transitions that must be rejected with a conflict.
var illegalEdges = []struct{ from, to State }{
	{StateStopping, StateRunning}, {StateStopping, StatePaused},
	{StateFailed, StateRunning}, {StateCreating, StatePaused},
	{StatePaused, StatePaused},
}

// TestCanTransitionLegal pins the permitted state machine edges.
func TestCanTransitionLegal(t *testing.T) {
	for _, c := range legalEdges {
		if !canTransition(c.from, c.to) {
			t.Errorf("expected %s -> %s to be legal", c.from, c.to)
		}
	}
}

// TestCanTransitionIllegal pins the rejected state machine edges.
func TestCanTransitionIllegal(t *testing.T) {
	for _, c := range illegalEdges {
		if canTransition(c.from, c.to) {
			t.Errorf("expected %s -> %s to be illegal", c.from, c.to)
		}
	}
}

// TestValidID checks the id regex accepts and rejects the right shapes.
func TestValidID(t *testing.T) {
	good := []string{"alice", "vm-1", "a", "0", "a1234567890123456789012345678901"}
	bad := []string{"", "-lead", "Upper", "has_underscore", "has.dot",
		"a12345678901234567890123456789012", "trail "}
	for _, s := range good {
		if !ValidID(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	for _, s := range bad {
		if ValidID(s) {
			t.Errorf("expected %q to be invalid", s)
		}
	}
}

// TestAllocateExhaustion verifies slots run out at MaxVMs.
func TestAllocateExhaustion(t *testing.T) {
	r := NewRegistry(t.TempDir(), "cracked")
	fillSlots(t, r)
	if _, err := r.Allocate("overflow"); err == nil {
		t.Fatal("expected ErrNoSlots past capacity")
	}
	if _, err := r.Allocate(idFor(0)); err == nil {
		t.Fatal("expected ErrDuplicate for a live id")
	}
}

// TestReleaseRecyclesSlot verifies a freed slot becomes available again.
func TestReleaseRecyclesSlot(t *testing.T) {
	r := NewRegistry(t.TempDir(), "cracked")
	vms := fillSlots(t, r)
	r.Release(vms[2])
	if c := r.Capacity(); c.SlotsFree != 1 || c.MemAllocated != (MaxVMs-1)*MemMiB {
		t.Fatalf("unexpected capacity after release: %+v", c)
	}
	if _, err := r.Allocate("reused"); err != nil {
		t.Fatalf("expected released slot to be reusable: %v", err)
	}
}

// fillSlots allocates every slot and returns the resulting VMs.
func fillSlots(t *testing.T, r *Registry) []*VM {
	t.Helper()
	var vms []*VM
	for i := 0; i < MaxVMs; i++ {
		v, err := r.Allocate(idFor(i))
		if err != nil {
			t.Fatalf("allocate %d: %v", i, err)
		}
		vms = append(vms, v)
	}
	return vms
}

// idFor builds a deterministic test VM id.
func idFor(i int) string { return "vm-" + string(rune('a'+i)) }
