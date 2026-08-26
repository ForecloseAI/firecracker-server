package api

import (
	_ "embed"
	"io"
	"log"
	"net/http"
	"os"
	"sync"

	"cracked/internal/agent"
	"cracked/internal/agentapi"
	"cracked/internal/fc"
	"cracked/internal/hoststat"
	"cracked/internal/vm"
)

// consoleTailBytes is how much serial output the detail view shows. The whole
// log is capped at 4 MiB on disk, which is far more than anyone reads.
const consoleTailBytes = 8 << 10

//go:embed static/dashboard.html
var dashboardHTML []byte

// agentStatus is what a guest agent reported, or why it could not be reached.
type agentStatus struct {
	Reachable    bool   `json:"reachable"`
	Ready        bool   `json:"ready"`
	SessionState string `json:"session_state,omitempty"`
	Error        string `json:"error,omitempty"`
}

// vmStats is one dashboard row: host resource usage plus the guest agent's
// state and lifetime spend.
type vmStats struct {
	vm.Stats
	Agent agentStatus  `json:"agent"`
	Usage agent.Totals `json:"usage"`
	// Why this row's spend could not be read, when it could not be.
	UsageError string `json:"usage_error,omitempty"`
}

// fleet is the whole-host snapshot. /stats returns it and /metrics renders it.
type fleet struct {
	Host     hoststat.Host `json:"host"`
	Capacity vm.Capacity   `json:"capacity"`
	VMs      []vmStats     `json:"vms"`
	Totals   agent.Totals  `json:"totals"`
}

// vmDetail is the peek-inside payload: a fleet row plus everything only worth
// fetching one VM at a time.
type vmDetail struct {
	vmStats
	Firecracker *fc.InstanceInfo `json:"firecracker,omitempty"`
	ConsoleTail string           `json:"console_tail"`
	// Agents is the roster, not one agent's events. The peek-inside view used to
	// show the BOSS's log and nothing else, so a specialist doing the actual work
	// was invisible. The client picks an agent and reads its log through the
	// guest proxy, which no longer starts anything to serve a poll.
	Agents []agentapi.Status `json:"agents"`
}

// collect gathers one snapshot of the whole host. Every read surface renders
// from this, so /stats, /vms/{id}/stats and /metrics cannot drift apart.
func (s *Server) collect() fleet {
	vms := s.reg.List()
	f := fleet{
		Host: hoststat.Read(s.reg.Layout().Base), Capacity: s.reg.Capacity(),
		VMs: make([]vmStats, len(vms)),
	}
	var wg sync.WaitGroup
	for i, v := range vms {
		s.reg.Refresh(v)
		f.VMs[i] = vmStats{Stats: s.reg.Stats(v)}
		wg.Add(1)
		go func() { defer wg.Done(); s.probeGuest(&f.VMs[i], v.ID) }()
	}
	wg.Wait()
	f.Totals = sumUsage(f.VMs)
	return f
}

// probeGuest fills in a row's agent health and spend. Failure is recorded on
// the row and never returned: one unreachable guest must not fail the poll.
func (s *Server) probeGuest(row *vmStats, id string) {
	if row.State != vm.StateRunning {
		row.Agent.Error = "vm is " + string(row.State)
		row.Usage = s.usage.Snapshot(id)
		return
	}
	if h, err := agent.New(row.GuestIP, vm.AgentPort).Health(); err == nil {
		row.Agent = agentStatus{Reachable: true, Ready: h.Ready, SessionState: h.SessionState}
	} else {
		row.Agent.Error = err.Error()
	}
	// Update returns the last known totals even when the guest is unreachable,
	// so a blip shows stale spend rather than blanking it to zero. The error is
	// RECORDED rather than discarded: a guest whose usage cannot be read renders
	// a clean $0.00 otherwise, which is indistinguishable from a VM that has done
	// nothing -- and that is exactly how the whole go-agent rollout stayed
	// invisible for as long as it did.
	var err error
	row.Usage, err = s.usage.Update(id, row.GuestIP, vm.AgentPort)
	if err != nil {
		row.UsageError = err.Error()
	}
}

// roster is who lives on this machine, fetched on demand.
//
// This used to return the BOSS's event tail and nothing else, so a specialist
// doing the actual work was invisible in the peek-inside view. The client now
// picks an agent and reads its log through the guest proxy, which no longer
// starts an agent to serve a poll. Errors degrade to an empty list: an
// unreachable guest is already reported by the agent column.
func roster(v *vm.VM) []agentapi.Status {
	out, err := agent.New(v.GuestIP, vm.AgentPort).Agents()
	if err != nil {
		return nil
	}
	return out
}

// sumUsage adds every VM's spend into one fleet total.
func sumUsage(rows []vmStats) agent.Totals {
	var t agent.Totals
	for _, row := range rows {
		t.CostUSD += row.Usage.CostUSD
		t.InputTokens += row.Usage.InputTokens
		t.OutputTokens += row.Usage.OutputTokens
		t.CacheReadTokens += row.Usage.CacheReadTokens
		t.CacheCreationTokens += row.Usage.CacheCreationTokens
		t.Turns += row.Usage.Turns
	}
	return t
}

// handleStats reports host utilisation and one row per live VM.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.collect())
}

// handleVMStats reports one VM in depth: resource usage, firecracker's own
// view, the serial console tail, and the agent's recent activity.
func (s *Server) handleVMStats(w http.ResponseWriter, r *http.Request) {
	v, err := s.reg.Get(r.PathValue("id"))
	if err != nil {
		writeVMErr(w, err)
		return
	}
	// Inspect, not Refresh plus a second Describe: one firecracker round trip.
	// It returns nil on failure, and the field is omitempty, so a wedged or
	// gone socket simply drops out of the payload.
	info, _ := s.reg.Inspect(v)
	d := vmDetail{vmStats: vmStats{Stats: s.reg.Stats(v)}, Firecracker: info}
	s.probeGuest(&d.vmStats, v.ID)
	d.Agents = roster(v)
	d.ConsoleTail = tailFile(s.reg.Layout().Console(v.ID), consoleTailBytes)
	writeJSON(w, http.StatusOK, d)
}

// tailFile returns the last n bytes of a file, or "" if it cannot be read.
func tailFile(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	if fi.Size() > n {
		if _, err := f.Seek(-n, io.SeekEnd); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

// handleDashboard serves the operator UI. The page authenticates its own
// fetches with a header, so nothing here sets a cookie.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if _, err := w.Write(dashboardHTML); err != nil {
		log.Printf("write dashboard: %v", err)
	}
}
