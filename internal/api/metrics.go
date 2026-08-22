package api

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"cracked/internal/agent"
	"cracked/internal/vm"
)

// vmMetric is one per-VM metric family and how to read it off a row.
type vmMetric struct {
	name, help, typ string
	value           func(vmStats) float64
}

// vmMetrics is every per-VM family, each labelled by vm id.
var vmMetrics = []vmMetric{
	{"cracked_vm_up", "1 when the VM is running.", "gauge",
		func(r vmStats) float64 { return boolVal(r.State == vm.StateRunning) }},
	{"cracked_vm_uptime_seconds", "Seconds since the VM was created.", "gauge",
		func(r vmStats) float64 { return r.UptimeSeconds }},
	// A counter, not the sampled percentage: the scrape interval and the
	// dashboard's poll interval are different clocks, so let rate() do it.
	{"cracked_vm_cpu_seconds_total", "Cumulative CPU seconds used by the firecracker process.", "counter",
		func(r vmStats) float64 { return r.CPUSeconds }},
	{"cracked_vm_rss_bytes", "Resident memory of the firecracker process.", "gauge",
		func(r vmStats) float64 { return float64(r.RSSBytes) }},
	{"cracked_vm_workspace_bytes", "Disk actually allocated to the VM's workspace image.", "gauge",
		func(r vmStats) float64 { return float64(r.WorkspaceBytes) }},
	{"cracked_vm_agent_up", "1 when the guest agent answered its health check.", "gauge",
		func(r vmStats) float64 { return boolVal(r.Agent.Reachable) }},
	{"cracked_vm_agent_cost_usd_total", "Lifetime agent spend for this workspace, in USD.", "counter",
		func(r vmStats) float64 { return r.Usage.CostUSD }},
	{"cracked_vm_agent_turns_total", "Lifetime completed agent turns for this workspace.", "counter",
		func(r vmStats) float64 { return float64(r.Usage.Turns) }},
}

// tokenKinds splits the token counter by billing category.
var tokenKinds = []struct {
	kind  string
	value func(agent.Totals) int64
}{
	{"input", func(t agent.Totals) int64 { return t.InputTokens }},
	{"output", func(t agent.Totals) int64 { return t.OutputTokens }},
	{"cache_read", func(t agent.Totals) int64 { return t.CacheReadTokens }},
	{"cache_creation", func(t agent.Totals) int64 { return t.CacheCreationTokens }},
}

// handleMetrics renders one collect() snapshot as Prometheus text. Written by
// hand to keep the zero-dependency promise; the format is stable and trivial.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	f := s.collect()
	var b strings.Builder
	writeHostMetrics(&b, f)
	for _, m := range vmMetrics {
		writeVMFamily(&b, m, f.VMs)
	}
	writeTokenFamily(&b, f.VMs)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	if _, err := io.WriteString(w, b.String()); err != nil {
		log.Printf("write metrics: %v", err)
	}
}

// writeHostMetrics emits the machine-wide families, which carry no labels.
func writeHostMetrics(b *strings.Builder, f fleet) {
	scalar(b, "cracked_host_load1", "1-minute load average.", f.Host.Load1)
	scalar(b, "cracked_host_cpus", "Host CPU count.", float64(f.Host.CPUs))
	scalar(b, "cracked_host_mem_total_bytes", "Total host memory.", float64(f.Host.MemTotalBytes))
	scalar(b, "cracked_host_mem_available_bytes", "Host memory available without swapping.", float64(f.Host.MemAvailableBytes))
	scalar(b, "cracked_host_disk_total_bytes", "Size of the filesystem holding VM state.", float64(f.Host.DiskTotalBytes))
	scalar(b, "cracked_host_disk_free_bytes", "Free space on the filesystem holding VM state.", float64(f.Host.DiskFreeBytes))
	scalar(b, "cracked_slots_total", "Total VM slots on this host.", float64(f.Capacity.SlotsTotal))
	scalar(b, "cracked_slots_used", "VM slots currently in use.", float64(f.Capacity.SlotsUsed))
}

// scalar emits one unlabelled gauge family.
func scalar(b *strings.Builder, name, help string, v float64) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, v)
}

// writeVMFamily emits HELP and TYPE once, then one sample per VM. Label values
// need no escaping: ValidID already limits ids to [a-z0-9-].
func writeVMFamily(b *strings.Builder, m vmMetric, rows []vmStats) {
	fmt.Fprintf(b, "# HELP %s %s\n# TYPE %s %s\n", m.name, m.help, m.name, m.typ)
	for _, r := range rows {
		fmt.Fprintf(b, "%s{vm=%q} %g\n", m.name, r.ID, m.value(r))
	}
}

// writeTokenFamily emits the token counter, split by vm and billing category.
func writeTokenFamily(b *strings.Builder, rows []vmStats) {
	const name = "cracked_vm_agent_tokens_total"
	fmt.Fprintf(b, "# HELP %s Lifetime tokens billed for this workspace.\n# TYPE %s counter\n", name, name)
	for _, r := range rows {
		for _, k := range tokenKinds {
			fmt.Fprintf(b, "%s{vm=%q,kind=%q} %d\n", name, r.ID, k.kind, k.value(r.Usage))
		}
	}
}

// boolVal renders a flag as the 1/0 Prometheus expects.
func boolVal(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
