package vm

import (
	"os"
	"time"

	"cracked/internal/hoststat"
)

// Stats is a View plus what the VM is actually consuming on the host, as
// opposed to what its slot reserves. Zero values mean "not measurable right
// now" (no pid yet, or the process just exited), never "idle".
type Stats struct {
	View
	UptimeSeconds     float64 `json:"uptime_seconds"`
	CPUSeconds        float64 `json:"cpu_seconds_total"`
	CPUPercent        float64 `json:"cpu_percent"`
	RSSBytes          int64   `json:"rss_bytes"`
	WorkspaceBytes    int64   `json:"workspace_bytes"`
	WorkspaceCapBytes int64   `json:"workspace_cap_bytes"`
}

// Stats renders a VM together with its live host resource usage. Every probe
// degrades to zero on failure so one dead VM cannot fail a fleet-wide poll.
func (r *Registry) Stats(v *VM) Stats {
	s := Stats{View: r.View(v), UptimeSeconds: time.Since(v.CreatedAt).Seconds()}
	s.WorkspaceBytes, s.WorkspaceCapBytes = workspaceUsage(r.dirs.Workspace(v.ID))
	if v.PID <= 0 {
		return s
	}
	if rss, err := hoststat.ProcRSSBytes(v.PID); err == nil {
		s.RSSBytes = rss
	}
	if cpu, err := hoststat.ProcCPUSeconds(v.PID); err == nil {
		s.CPUSeconds, s.CPUPercent = cpu, r.cpuPercent(v, cpu)
	}
	return s
}

// cpuPercent turns cumulative CPU seconds into a rate against the previous
// sample. It is percent of ONE core, so a fully busy 2-vCPU VM reads ~200%,
// matching top. The first sample after boot has no predecessor and reports 0.
func (r *Registry) cpuPercent(v *VM, now float64) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	prev, at := v.lastCPUSeconds, v.lastSampleAt
	sampledAt := time.Now()
	v.lastCPUSeconds, v.lastSampleAt = now, sampledAt
	elapsed := sampledAt.Sub(at).Seconds()
	if at.IsZero() || elapsed <= 0 {
		return 0
	}
	return (now - prev) / elapsed * 100
}

// workspaceUsage reports blocks actually allocated and the image's nominal
// size. The workspace is a sparse 5 GiB file, so only the former is real cost.
func workspaceUsage(path string) (used, size int64) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, 0
	}
	used, err = hoststat.AllocatedBytes(path)
	if err != nil {
		return 0, fi.Size()
	}
	return used, fi.Size()
}
