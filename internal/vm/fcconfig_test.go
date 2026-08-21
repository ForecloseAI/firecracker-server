package vm

import (
	"encoding/json"
	"strings"
	"testing"
)

// renderConfig builds the config for a slot-0 VM and returns it with its JSON.
func renderConfig(t *testing.T) (fcConfig, string) {
	t.Helper()
	cfg := buildConfig(newVM("alice", 0), Layout{Base: "/var/lib/cracked"})
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, string(b)
}

// TestConfigFieldNames pins the JSON contract: is_read_only is the block device
// field (read_only belongs to pmem), and top-level keys are hyphenated.
func TestConfigFieldNames(t *testing.T) {
	_, raw := renderConfig(t)
	if !strings.Contains(raw, `"is_read_only":true`) {
		t.Error("root drive must be marked is_read_only")
	}
	if strings.Contains(raw, `"read_only"`) {
		t.Error("read_only is the wrong field name for a block device")
	}
	for _, key := range []string{`"boot-source"`, `"machine-config"`, `"network-interfaces"`} {
		if !strings.Contains(raw, key) {
			t.Errorf("missing hyphenated top-level key %s", key)
		}
	}
}

// TestBootArgsOmitsTraps checks the args that silently break boot or shutdown.
// firecracker appends root= itself from the root drive; pci=off and the i8042
// mute flags break PCI and SendCtrlAltDel respectively.
func TestBootArgsOmitsTraps(t *testing.T) {
	cfg, _ := renderConfig(t)
	args := cfg.BootSource.BootArgs
	if hasToken(args, "root=") {
		t.Errorf("boot_args must not set root=; firecracker appends it: %s", args)
	}
	for _, banned := range []string{"pci=off", "i8042.dumbkbd", "i8042.nokbd"} {
		if strings.Contains(args, banned) {
			t.Errorf("boot_args must not contain %q: %s", banned, args)
		}
	}
}

// TestBootArgsRequired checks the args the boot path depends on. reboot=k is
// what makes firecracker exit; overlay_root=vdb selects the writable layer.
func TestBootArgsRequired(t *testing.T) {
	cfg, _ := renderConfig(t)
	args := cfg.BootSource.BootArgs
	for _, need := range []string{
		"console=ttyS0", "reboot=k", "panic=1",
		"init=/sbin/overlay-init", "overlay_root=vdb",
		"ip=172.16.0.2::172.16.0.1:255.255.255.252::eth0:off",
	} {
		if !strings.Contains(args, need) {
			t.Errorf("boot_args missing %q: %s", need, args)
		}
	}
}

// TestDrivesLayout checks the two-drive split: shared ro root, per-VM rw overlay.
func TestDrivesLayout(t *testing.T) {
	cfg, _ := renderConfig(t)
	if len(cfg.Drives) != 2 {
		t.Fatalf("expected exactly 2 drives, got %d", len(cfg.Drives))
	}
	root, work := cfg.Drives[0], cfg.Drives[1]
	if !root.IsRootDevice || !root.IsReadOnly {
		t.Error("drive 0 must be the read-only root device")
	}
	if root.PathOnHost != "/var/lib/cracked/images/rootfs.ext4" {
		t.Errorf("root drive should be the shared image, got %s", root.PathOnHost)
	}
	assertWorkspaceDrive(t, work)
}

// assertWorkspaceDrive checks the overlay upper is writable and durable.
func assertWorkspaceDrive(t *testing.T, work drive) {
	t.Helper()
	if work.IsRootDevice || work.IsReadOnly {
		t.Error("drive 1 (overlay upper) must be writable and not a root device")
	}
	// Writeback so guest fsync is honored: credentials must survive a SIGKILL.
	if work.CacheType != "Writeback" {
		t.Errorf("workspace cache_type = %q, want Writeback", work.CacheType)
	}
}

// hasToken reports whether args contains prefix as a whole space-delimited token.
func hasToken(args, prefix string) bool {
	for _, f := range strings.Fields(args) {
		if strings.HasPrefix(f, prefix) {
			return true
		}
	}
	return false
}
