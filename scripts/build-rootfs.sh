#!/usr/bin/env bash
# Build the shared, immutable, read-only rootfs image from the Dockerfile.
# Run this on a builder box if you can: installing Docker on the VM host injects
# rules into FORWARD (the CRACKED_FWD chain defends against that, but not
# having Docker there at all is cleaner).
set -euo pipefail

BASE="${CRACKED_BASE:-/var/lib/cracked}"
OUT="$BASE/images/rootfs.ext4"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
trap 'sudo rm -rf "$WORK"' EXIT

sudo mkdir -p "$BASE/images"

# TODO(security): baking the key into the image means every VM shares it and
# anything in the guest can read it. Replace with a host token-broker before any
# public release. See the TODO in rootfs/Dockerfile.
if [ -z "${ANTHROPIC_API_KEY:-}" ]; then
  echo "WARN: ANTHROPIC_API_KEY is unset; the agent will not be able to run."
  echo "      Re-run as: ANTHROPIC_API_KEY=sk-ant-... $0"
fi
# agentd is Go source OUTSIDE the rootfs/ build context, so it cannot be compiled
# by a build stage in the Dockerfile: it has to be built here and dropped in.
#
# Rebuilding is the default and is unconditional, because go's build cache makes
# a no-op rebuild near-instant and anything conditional is how a stale binary
# ships. AGENTD_PREBUILT=1 opts out for the deployment model this repo actually
# documents -- "build on a laptop and ship only the binary; the EC2 box needs no
# Go toolchain of its own" -- where the builder has no usable toolchain. It is
# opt-IN on purpose: silently falling back to whatever file happens to be lying
# in the context is the failure this guard exists to prevent.
if [ "${AGENTD_PREBUILT:-0}" = "1" ]; then
  [ -x "$HERE/rootfs/files/agentd" ] || {
    echo "FATAL: AGENTD_PREBUILT=1 but $HERE/rootfs/files/agentd is missing or not executable"
    exit 1; }
  echo "using prebuilt agentd: $(ls -lh "$HERE/rootfs/files/agentd" | awk '{print $5}')"
else
  command -v go >/dev/null || {
    echo "FATAL: no go toolchain here. Build agentd elsewhere, copy it to"
    echo "       rootfs/files/agentd, and re-run with AGENTD_PREBUILT=1"
    exit 1; }
  make -C "$HERE" build-agentd
fi

docker build -t cracked-rootfs \
  --build-arg "ANTHROPIC_API_KEY=${ANTHROPIC_API_KEY:-}" \
  "$HERE/rootfs"
CID="$(docker create cracked-rootfs)"
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true; sudo rm -rf "$WORK"' EXIT

# --same-owner is REQUIRED: without it ownership collapses to the invoking user
# and SUID bits drop, which breaks systemd's expectations for /var in the guest.
docker export "$CID" | sudo tar -C "$WORK" -xpf - --same-owner
docker rm "$CID" >/dev/null

# The docker daemon creates /.dockerenv inside every container, and `docker
# export` faithfully carries it into the rootfs. systemd-detect-virt then reports
# "docker" INSIDE THE MICROVM and systemd runs in container mode, which is wrong
# in two load-bearing ways:
#
#   - it reads /proc/1/cmdline ("/sbin/init") instead of the kernel command line,
#     so every ConditionKernelCommandLine silently evaluates false;
#   - it installs no ctrl-alt-del handler, so firecracker's SendCtrlAltDel is
#     accepted with a 204 and then ignored. Every teardown therefore degrades to
#     SIGTERM and SIGKILL with no filesystem sync, and ANY guest write since the
#     last flush is lost -- agent memory and Chrome's profile LevelDB alike.
#
# bootArgs() already omits i8042.nopnp specifically to keep SendCtrlAltDel
# working; this is the other half of that promise.
sudo rm -f "$WORK/.dockerenv"

SIZE_MB=$(( $(sudo du -sm "$WORK" | cut -f1) + 768 ))
sudo rm -f "$OUT"
sudo truncate -s "${SIZE_MB}M" "$OUT"

# mkfs -d populates from a directory tree: no loop device, no mount, no
# privileged container. This is what makes the build work anywhere Docker does.
sudo mkfs.ext4 -L rootfs -d "$WORK" -F "$OUT"
sudo e2fsck -fp "$OUT" || true
sudo resize2fs -M "$OUT"   # shrink to minimum: smaller image, more page cache headroom

# Enforce immutability at the filesystem layer. Every VM opens this file
# read-only and concurrently; the host page cache then serves them all.
sudo chmod 0444 "$OUT"

echo "built $OUT ($(sudo du -h "$OUT" | cut -f1))"
