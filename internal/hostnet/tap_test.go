package hostnet

import (
	"fmt"
	"net/netip"
	"testing"
)

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

// TestGuestIsHostPlusOneWithinTheSame24 pins the arithmetic scripts/vm-ssh.sh
// depends on from the other side of a language boundary.
//
// That helper finds running VMs by reading live tap addresses and incrementing
// the last octet, which is only safe because 4*slot+1 is always 1 mod 4: the
// host octet is therefore at most 253 and the increment can never carry into
// the next /24. MaxVMs makes that unreachable in production today; this makes
// it unreachable for good, so a future change to SlotAddrs fails here rather
// than turning vm-ssh into a tool that connects to the wrong machine.
func TestGuestIsHostPlusOneWithinTheSame24(t *testing.T) {
	for _, slot := range []int{0, 1, 4, 62, 63, 64, 127, 128, 255, 256, 1000} {
		host, guest, _ := SlotAddrs(slot)
		var ha, hb, hc, hd, ga, gb, gc, gd int
		if _, err := fmt.Sscanf(host, "%d.%d.%d.%d", &ha, &hb, &hc, &hd); err != nil {
			t.Fatalf("slot %d: unparseable host %q", slot, host)
		}
		if _, err := fmt.Sscanf(guest, "%d.%d.%d.%d", &ga, &gb, &gc, &gd); err != nil {
			t.Fatalf("slot %d: unparseable guest %q", slot, guest)
		}
		if ha != ga || hb != gb || hc != gc {
			t.Errorf("slot %d: %s and %s are in different /24s", slot, host, guest)
		}
		if gd != hd+1 {
			t.Errorf("slot %d: guest %s is not host %s plus one", slot, guest, host)
		}
		if hd%4 != 1 {
			t.Errorf("slot %d: host octet %d is not 1 mod 4, so vm-ssh's filter drops it",
				slot, hd)
		}
		if hd > 253 {
			t.Errorf("slot %d: host octet %d would carry on increment", slot, hd)
		}
	}
}

// SlotOf must undo SlotAddrs exactly, because the model broker uses it to say
// which machine made a call. Host tap addresses and anything off the /30 grid
// are not guests and must be refused rather than rounded to a neighbour.
func TestSlotOfInvertsSlotAddrs(t *testing.T) {
	for slot := 0; slot <= 1000; slot++ {
		host, guest, _ := SlotAddrs(slot)
		if got, ok := SlotOf(netip.MustParseAddr(guest)); !ok || got != slot {
			t.Fatalf("SlotOf(%s) = %d, %v; want %d", guest, got, ok, slot)
		}
		if _, ok := SlotOf(netip.MustParseAddr(host)); ok {
			t.Fatalf("SlotOf(%s) accepted a host address", host)
		}
	}
	for _, bad := range []string{"172.16.0.3", "172.16.0.0", "10.0.0.2", "172.17.0.2", "::1"} {
		if _, ok := SlotOf(netip.MustParseAddr(bad)); ok {
			t.Errorf("SlotOf(%q) accepted a non-guest", bad)
		}
	}
}
