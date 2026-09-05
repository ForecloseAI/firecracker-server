// Package hostnet manages per-VM TAP devices and derives their addressing.
// It shells out to iproute2, which is present on every EC2 AMI.
package hostnet

import (
	"fmt"
	"net/netip"
	"os/exec"
	"strings"
)

// Mask is the /30 netmask every VM subnet uses.
const Mask = "255.255.255.252"

// TapName is the deterministic device name for a slot.
func TapName(slot int) string { return fmt.Sprintf("tap%d", slot) }

// SlotAddrs derives the host IP, guest IP and MAC for a slot. Each VM gets its
// own /30 out of 172.16.0.0/16, so guests share no L2 domain. The MAC spends
// two bytes on the slot so it stays valid past slot 255.
func SlotAddrs(slot int) (hostIP, guestIP, mac string) {
	h, g := 4*slot+1, 4*slot+2
	hostIP = fmt.Sprintf("172.16.%d.%d", h/256, h%256)
	guestIP = fmt.Sprintf("172.16.%d.%d", g/256, g%256)
	mac = fmt.Sprintf("02:FC:00:00:%02X:%02X", slot/256, slot%256)
	return
}

// SlotOf is SlotAddrs run backwards for the guest address: which slot a guest
// at addr lives in. False for anything that is not guest-shaped -- a host tap
// address, a network or broadcast address, or anything outside 172.16/16 --
// rather than rounding to a neighbour, because the answer names a machine. It
// is what the host's guest broker uses to decide whether a caller is a guest.
func SlotOf(addr netip.Addr) (int, bool) {
	if !addr.Is4() {
		return 0, false
	}
	o := addr.As4()
	if o[0] != 172 || o[1] != 16 {
		return 0, false
	}
	n := int(o[2])*256 + int(o[3])
	if n%4 != 2 {
		return 0, false
	}
	return (n - 2) / 4, true
}

// CreateTap builds a fresh tap owned by user, reclaiming any stale device of
// the same name first. The `user` argument is what lets a non-root firecracker
// attach to it.
func CreateTap(name, hostIP, user string) error {
	_ = DeleteTap(name)
	if err := run("ip", "tuntap", "add", name, "mode", "tap", "user", user); err != nil {
		return err
	}
	if err := run("ip", "addr", "add", hostIP+"/30", "dev", name); err != nil {
		return err
	}
	return run("ip", "link", "set", name, "up")
}

// DeleteTap removes a tap device, ignoring the case where it is already gone.
func DeleteTap(name string) error {
	out, err := exec.Command("ip", "link", "del", name).CombinedOutput()
	if err != nil && strings.Contains(string(out), "Cannot find device") {
		return nil
	}
	return wrap(err, out)
}

// run executes an ip command and surfaces its stderr on failure.
func run(args ...string) error {
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	return wrap(err, out)
}

// wrap attaches command output to an error for a usable message.
func wrap(err error, out []byte) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
}
