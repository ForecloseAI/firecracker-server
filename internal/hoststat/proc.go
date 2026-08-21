// Package hoststat reads utilisation off /proc and the filesystem. It has no
// HTTP or VM knowledge; parsing is split from I/O so the parsers stay testable
// on a non-Linux dev machine.
package hoststat

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// UserHZ is the kernel's clock tick. Reading it properly needs sysconf, which
// needs cgo, and this control plane only ever runs on Linux/amd64 where it is 100.
const UserHZ = 100

// ProcCPUSeconds returns a process's cumulative user+system CPU time.
func ProcCPUSeconds(pid int) (float64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	jiffies, err := parseStatJiffies(string(b))
	return float64(jiffies) / UserHZ, err
}

// parseStatJiffies pulls utime+stime out of /proc/{pid}/stat. It splits after
// the LAST ')' because comm is unquoted and may itself contain spaces and parens.
func parseStatJiffies(s string) (uint64, error) {
	end := strings.LastIndex(s, ")")
	if end < 0 {
		return 0, fmt.Errorf("malformed stat line")
	}
	// After comm the next field is state, which is field 3, so field N is at N-3.
	f := strings.Fields(s[end+1:])
	if len(f) < 13 {
		return 0, fmt.Errorf("stat has %d fields after comm, want >= 13", len(f))
	}
	utime, err := strconv.ParseUint(f[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(f[12], 10, 64)
	return utime + stime, err
}

// ProcRSSBytes returns a process's resident set size.
func ProcRSSBytes(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	kib, err := parseRSSKiB(string(b))
	return kib * 1024, err
}

// parseRSSKiB finds the VmRSS line in /proc/{pid}/status.
func parseRSSKiB(s string) (int64, error) {
	v, ok := fieldAfter(s, "VmRSS:")
	if !ok {
		return 0, fmt.Errorf("no VmRSS line")
	}
	return strconv.ParseInt(v, 10, 64)
}

// fieldAfter returns the first whitespace-separated value on the line starting
// with prefix. Shared by the VmRSS and meminfo parsers, which use one format.
func fieldAfter(s, prefix string) (string, bool) {
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		if f := strings.Fields(strings.TrimPrefix(line, prefix)); len(f) > 0 {
			return f[0], true
		}
	}
	return "", false
}

// AllocatedBytes reports the disk a file actually consumes. Workspaces are
// sparse 5 GiB images, so only the allocated blocks are real cost; Size is the cap.
func AllocatedBytes(path string) (int64, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		return 0, err
	}
	// st_blocks is always in 512-byte units, regardless of the filesystem's
	// own block size. This is fixed by POSIX, not by st_blksize.
	return st.Blocks * 512, nil
}
