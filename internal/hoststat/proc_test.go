package hoststat

import "testing"

// A real stat line: fields 14 and 15 after comm are utime and stime.
const statLine = `4242 (firecracker) S 1 4242 4242 0 -1 4194560 1337 0 2 0 950 610 0 0 20 0 5 0 88123 1234567 890 18446744073709551615 1 1 0 0 0 0 0 0 0 0 0 0 17 3 0 0 0 0 0`

func TestParseStatJiffies(t *testing.T) {
	got, err := parseStatJiffies(statLine)
	if err != nil {
		t.Fatalf("parseStatJiffies: %v", err)
	}
	if want := uint64(950 + 610); got != want {
		t.Errorf("jiffies = %d, want %d", got, want)
	}
}

// A comm containing spaces and parens is the classic /proc/{pid}/stat trap:
// splitting on the FIRST ')' or on whitespace alone shifts every later field.
func TestParseStatJiffiesUglyComm(t *testing.T) {
	line := `7 (we ird) (name) S 1 7 7 0 -1 0 0 0 0 0 11 22 0 0 20 0 1 0 5 0 0`
	got, err := parseStatJiffies(line)
	if err != nil {
		t.Fatalf("parseStatJiffies: %v", err)
	}
	if want := uint64(33); got != want {
		t.Errorf("jiffies = %d, want %d", got, want)
	}
}

func TestParseStatJiffiesMalformed(t *testing.T) {
	for _, in := range []string{"", "no parens here", "4242 (fc) S 1 2 3"} {
		if _, err := parseStatJiffies(in); err == nil {
			t.Errorf("parseStatJiffies(%q) = nil error, want one", in)
		}
	}
}

func TestParseRSSKiB(t *testing.T) {
	status := "Name:\tfirecracker\nVmPeak:\t 4300000 kB\nVmRSS:\t  262144 kB\nThreads:\t4\n"
	got, err := parseRSSKiB(status)
	if err != nil {
		t.Fatalf("parseRSSKiB: %v", err)
	}
	if got != 262144 {
		t.Errorf("rss = %d KiB, want 262144", got)
	}
}

func TestParseRSSKiBMissing(t *testing.T) {
	if _, err := parseRSSKiB("Name:\tfirecracker\n"); err == nil {
		t.Error("parseRSSKiB with no VmRSS = nil error, want one")
	}
}
