package vm

import (
	"encoding/json"
	"fmt"
	"os"

	"cracked/internal/hostnet"
)

// fcConfig is the firecracker --config-file document. Top-level keys are
// hyphenated; inner fields are snake_case, matching the API exactly.
type fcConfig struct {
	BootSource    bootSource `json:"boot-source"`
	Drives        []drive    `json:"drives"`
	MachineConfig machineCfg `json:"machine-config"`
	NetworkIfaces []netIface `json:"network-interfaces"`
}

type bootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args"`
}

// drive uses is_read_only (not read_only, which belongs to the pmem device).
type drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
	CacheType    string `json:"cache_type,omitempty"`
	IOEngine     string `json:"io_engine"`
}

type machineCfg struct {
	VCPUCount       int  `json:"vcpu_count"`
	MemSizeMiB      int  `json:"mem_size_mib"`
	TrackDirtyPages bool `json:"track_dirty_pages"`
}

type netIface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac"`
}

// bootArgs builds the kernel command line. Deliberately absent: root= (which
// firecracker appends itself from the root drive), pci=off, i8042.dumbkbd and
// i8042.nokbd (either breaks SendCtrlAltDel), and i8042.nopnp.
//
// i8042.nopnp is the subtle one. It is widely recommended as a boot-time saving
// and does not obviously touch the keyboard, but on this kernel it makes the
// controller probe fail outright:
//
//	i8042: PNP detection disabled
//	i8042 i8042: probe with driver i8042 failed with error -22
//
// The guest then has no input device, so SendCtrlAltDel is delivered to nothing
// and every teardown silently falls through to SIGTERM. That loses unflushed
// guest writes, which is how a persisted Chrome profile gets corrupted.
func bootArgs(v *VM) string {
	ip := fmt.Sprintf("ip=%s::%s:%s::eth0:off", v.GuestIP, v.HostIP, hostnet.Mask)
	return "console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux " +
		"random.trust_cpu=on " + ip + " init=/sbin/overlay-init overlay_root=vdb"
}

// buildConfig assembles the two-drive config: shared read-only root on vda,
// per-VM overlay upper plus workspace on vdb.
func buildConfig(v *VM, l Layout) fcConfig {
	return fcConfig{
		BootSource: bootSource{KernelImagePath: l.Kernel(), BootArgs: bootArgs(v)},
		Drives: []drive{
			{DriveID: "rootfs", PathOnHost: l.Rootfs(), IsRootDevice: true,
				IsReadOnly: true, IOEngine: "Sync"},
			{DriveID: "workspace", PathOnHost: l.Workspace(v.ID), IsRootDevice: false,
				IsReadOnly: false, CacheType: "Writeback", IOEngine: "Sync"},
		},
		MachineConfig: machineCfg{VCPUCount: VCPUs, MemSizeMiB: MemMiB},
		NetworkIfaces: []netIface{
			{IfaceID: "eth0", HostDevName: v.Tap, GuestMAC: v.MAC},
		},
	}
}

// writeConfigJSON renders the boot config to the VM's run directory.
func writeConfigJSON(v *VM, l Layout) error {
	b, err := json.MarshalIndent(buildConfig(v, l), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(l.Config(v.ID), b, 0640)
}
