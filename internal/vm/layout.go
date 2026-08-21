package vm

import "path/filepath"

// Layout derives every host path the control plane touches. It is the single
// choke point that a future jailer migration would rewrite.
type Layout struct{ Base string }

// RunDir is the ephemeral per-VM directory, wiped by the startup sweep.
func (l Layout) RunDir(id string) string { return filepath.Join(l.Base, "run", id) }

// Workspace is the persisted 5 GiB disk: overlay upper layer plus user data.
func (l Layout) Workspace(id string) string {
	return filepath.Join(l.Base, "workspaces", id+".ext4")
}

// Sock is the firecracker API socket. The file survives process exit, so it is
// never a liveness test.
func (l Layout) Sock(id string) string { return filepath.Join(l.RunDir(id), "firecracker.sock") }

// Config is the generated firecracker --config-file.
func (l Layout) Config(id string) string { return filepath.Join(l.RunDir(id), "config.json") }

// Console is the size-capped serial output of the guest.
func (l Layout) Console(id string) string { return filepath.Join(l.RunDir(id), "console.log") }

// StateFile records pid/slot/tap for the crash-recovery sweep only.
func (l Layout) StateFile(id string) string { return filepath.Join(l.RunDir(id), "vm.json") }

// Rootfs is the shared, immutable, read-only image booted by every VM.
func (l Layout) Rootfs() string { return filepath.Join(l.Base, "images", "rootfs.ext4") }

// Kernel is the uncompressed ELF vmlinux (never a bzImage).
func (l Layout) Kernel() string { return filepath.Join(l.Base, "images", KernelName) }

// WorkspacesDir and RunRoot expose the two parent directories.
func (l Layout) WorkspacesDir() string { return filepath.Join(l.Base, "workspaces") }
func (l Layout) RunRoot() string       { return filepath.Join(l.Base, "run") }
