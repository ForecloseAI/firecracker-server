package vm

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The first sample has no predecessor, so a lifetime average must not leak out
// as if it were a current rate.
func TestCPUPercentFirstSampleIsZero(t *testing.T) {
	r := NewRegistry(t.TempDir(), "cracked")
	v := newVM("probe", 0)
	if got := r.cpuPercent(v, 42); got != 0 {
		t.Errorf("first sample = %v%%, want 0", got)
	}
	if v.lastCPUSeconds != 42 {
		t.Errorf("sample not cached: %v", v.lastCPUSeconds)
	}
}

// Two vCPUs fully busy burn 2 CPU-seconds per wall second, which is 200% of
// one core. The rate must be against the previous sample, not against uptime.
func TestCPUPercentUsesDelta(t *testing.T) {
	r := NewRegistry(t.TempDir(), "cracked")
	v := newVM("probe", 0)
	v.lastCPUSeconds, v.lastSampleAt = 100, time.Now().Add(-2*time.Second)
	got := r.cpuPercent(v, 104)
	if got < 190 || got > 210 {
		t.Errorf("cpu = %v%%, want ~200", got)
	}
}

// A sparse image must report allocated blocks, not its nominal size, or every
// fresh VM would look like it had already consumed its whole 5 GiB.
func TestWorkspaceUsageSparse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe.ext4")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(5 << 30); err != nil {
		t.Fatal(err)
	}
	f.Close()
	used, size := workspaceUsage(path)
	if size != 5<<30 {
		t.Errorf("size = %d, want %d", size, int64(5)<<30)
	}
	if used >= size {
		t.Errorf("used = %d, want well under the %d cap for a sparse file", used, size)
	}
}

// A VM deleted with ?purge=true has no workspace at all; that is zero, not a
// failure that should propagate.
func TestWorkspaceUsageMissing(t *testing.T) {
	used, size := workspaceUsage(filepath.Join(t.TempDir(), "gone.ext4"))
	if used != 0 || size != 0 {
		t.Errorf("missing workspace = %d/%d, want 0/0", used, size)
	}
}
