package hoststat

import "testing"

func TestParseLoadavg(t *testing.T) {
	l1, l5, l15 := parseLoadavg("0.52 1.08 2.16 3/512 90210\n")
	if l1 != 0.52 || l5 != 1.08 || l15 != 2.16 {
		t.Errorf("load = %v %v %v, want 0.52 1.08 2.16", l1, l5, l15)
	}
}

func TestParseLoadavgShort(t *testing.T) {
	if l1, l5, l15 := parseLoadavg("0.52"); l1 != 0 || l5 != 0 || l15 != 0 {
		t.Errorf("short loadavg = %v %v %v, want zeros", l1, l5, l15)
	}
}

const meminfo = "MemTotal:       32946128 kB\nMemFree:         1048576 kB\nMemAvailable:   28123456 kB\n"

func TestParseMeminfoBytes(t *testing.T) {
	if got, want := parseMeminfoBytes(meminfo, "MemTotal:"), int64(32946128)*1024; got != want {
		t.Errorf("MemTotal = %d, want %d", got, want)
	}
	// MemFree is a prefix-collision risk for MemTotal-style scanning.
	if got, want := parseMeminfoBytes(meminfo, "MemAvailable:"), int64(28123456)*1024; got != want {
		t.Errorf("MemAvailable = %d, want %d", got, want)
	}
}

func TestParseMeminfoBytesMissing(t *testing.T) {
	if got := parseMeminfoBytes(meminfo, "Hugepagesize:"); got != 0 {
		t.Errorf("missing key = %d, want 0", got)
	}
}
