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
	// i8042.nopnp makes the controller probe fail, leaving the guest with no
	// keyboard, so SendCtrlAltDel becomes a silent no-op and teardown always
	// degrades to SIGTERM.
	for _, banned := range []string{"pci=off", "i8042.dumbkbd", "i8042.nokbd", "i8042.nopnp"} {
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

// TestBootArgsDefaultsToNode guards the switchover's blast radius. A VM created
// the way every VM is created today must produce a command line with no trace of
// the agent flag, so the node path cannot regress while the go path is proven.
func TestBootArgsDefaultsToNode(t *testing.T) {
	for _, impl := range []string{"", AgentImplNode} {
		v := newVM("alice", 0)
		v.AgentImpl = impl
		args := bootArgs(v)
		if strings.Contains(args, "cracked.agent") {
			t.Errorf("impl %q must not appear on the command line: %s", impl, args)
		}
	}
}

// TestBootArgsCarriesGoFlag pins the one token agentd.service conditions on.
// ConditionKernelCommandLine matches /proc/cmdline literally, so a rename here
// silently boots node forever with no error anywhere.
func TestBootArgsCarriesGoFlag(t *testing.T) {
	v := newVM("alice", 0)
	v.AgentImpl = AgentImplGo
	args := bootArgs(v)
	if !hasToken(args, "cracked.agent=go") {
		t.Errorf("go VM must carry cracked.agent=go: %s", args)
	}
	// The flag rides after overlay_root, which init consumes; a trailing space or
	// a merge with the previous token would make the kernel see one bad arg.
	if !strings.Contains(args, "overlay_root=vdb cracked.agent=go") {
		t.Errorf("flag must be its own token after overlay_root: %s", args)
	}
}

// TestValidAgentImpl pins what the API will accept. Empty means node, so that a
// request body written before this field existed stays valid.
func TestValidAgentImpl(t *testing.T) {
	for _, ok := range []string{"", "node", "go"} {
		if !ValidAgentImpl(ok) {
			t.Errorf("%q should be accepted", ok)
		}
	}
	for _, bad := range []string{"rust", "Go", "node ", "python"} {
		if ValidAgentImpl(bad) {
			t.Errorf("%q should be rejected", bad)
		}
	}
}
