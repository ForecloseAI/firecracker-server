package hostnet

import "testing"

// TestSlotAddrs checks the /30-per-VM arithmetic, including the 256 rollover.
func TestSlotAddrs(t *testing.T) {
	cases := []struct {
		slot             int
		host, guest, mac string
	}{
		{0, "172.16.0.1", "172.16.0.2", "02:FC:00:00:00:00"},
		{1, "172.16.0.5", "172.16.0.6", "02:FC:00:00:00:01"},
		{4, "172.16.0.17", "172.16.0.18", "02:FC:00:00:00:04"},
		{63, "172.16.0.253", "172.16.0.254", "02:FC:00:00:00:3F"},
		{64, "172.16.1.1", "172.16.1.2", "02:FC:00:00:00:40"},
		{1000, "172.16.15.161", "172.16.15.162", "02:FC:00:00:03:E8"},
	}
	for _, c := range cases {
		h, g, m := SlotAddrs(c.slot)
		if h != c.host || g != c.guest || m != c.mac {
			t.Errorf("slot %d = (%s,%s,%s), want (%s,%s,%s)",
				c.slot, h, g, m, c.host, c.guest, c.mac)
		}
	}
}

// TestSlotAddrsDistinct guards against two slots sharing an address or MAC.
func TestSlotAddrsDistinct(t *testing.T) {
	seen := map[string]int{}
	for i := 0; i < 128; i++ {
		h, g, m := SlotAddrs(i)
		for _, key := range []string{h, g, m} {
			if prev, dup := seen[key]; dup {
				t.Fatalf("slot %d reuses %q from slot %d", i, key, prev)
			}
			seen[key] = i
		}
	}
}
