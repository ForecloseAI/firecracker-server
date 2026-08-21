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

docker build -t cracked-rootfs "$HERE/rootfs"
CID="$(docker create cracked-rootfs)"
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true; sudo rm -rf "$WORK"' EXIT

# --same-owner is REQUIRED: without it ownership collapses to the invoking user
# and SUID bits drop, which breaks systemd's expectations for /var in the guest.
docker export "$CID" | sudo tar -C "$WORK" -xpf - --same-owner
docker rm "$CID" >/dev/null

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
