package hoststat

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// Host is the whole machine's real headroom, as opposed to the fixed budget
// vm.Capacity reports. Everything is bytes so callers never convert twice.
type Host struct {
	Load1             float64 `json:"load1"`
	Load5             float64 `json:"load5"`
	Load15            float64 `json:"load15"`
	CPUs              int     `json:"cpus"`
	MemTotalBytes     int64   `json:"mem_total_bytes"`
	MemAvailableBytes int64   `json:"mem_available_bytes"`
	DiskTotalBytes    int64   `json:"disk_total_bytes"`
	DiskFreeBytes     int64   `json:"disk_free_bytes"`
}

// Read gathers host utilisation, reporting disk for the filesystem holding
// basePath. Every field degrades to zero rather than failing the whole read.
func Read(basePath string) Host {
	h := Host{CPUs: runtime.NumCPU()}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		h.Load1, h.Load5, h.Load15 = parseLoadavg(string(b))
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		h.MemTotalBytes = parseMeminfoBytes(string(b), "MemTotal:")
		h.MemAvailableBytes = parseMeminfoBytes(string(b), "MemAvailable:")
	}
	h.DiskTotalBytes, h.DiskFreeBytes = diskBytes(basePath)
	return h
}

// parseLoadavg reads the three load figures from /proc/loadavg.
func parseLoadavg(s string) (l1, l5, l15 float64) {
	f := strings.Fields(s)
	if len(f) < 3 {
		return 0, 0, 0
	}
	l1, _ = strconv.ParseFloat(f[0], 64)
	l5, _ = strconv.ParseFloat(f[1], 64)
	l15, _ = strconv.ParseFloat(f[2], 64)
	return l1, l5, l15
}

// parseMeminfoBytes reads one KiB-denominated /proc/meminfo line as bytes.
func parseMeminfoBytes(s, key string) int64 {
	v, ok := fieldAfter(s, key)
	if !ok {
		return 0
	}
	kib, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return kib * 1024
}

// diskBytes reports the size and free space of the filesystem holding path.
func diskBytes(path string) (total, free int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	// Bsize is int64 on Linux and uint32 on Darwin; the cast keeps `make vet`
	// and `make test` compiling on the dev machine.
	bs := int64(uint64(st.Bsize))
	return int64(st.Blocks) * bs, int64(st.Bavail) * bs
}
