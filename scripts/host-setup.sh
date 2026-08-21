#!/usr/bin/env bash
# One-time, idempotent provisioning for the Firecracker VM host. Run as root.
set -euo pipefail

BASE="${CRACKED_BASE:-/var/lib/cracked}"
USER_NAME="${CRACKED_USER:-cracked}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
KERNEL_VERSION="${KERNEL_VERSION:-6.18}"
GUEST_CIDR="172.16.0.0/16"
VPC_CIDR="${VPC_CIDR:-10.0.0.0/8}"

[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }

# --- 1. Nested virtualisation gate --------------------------------------------
# m7i supports KVM as of the June 2026 nested-virt launch, but it is OPT-IN.
check_kvm() {
  local fail=0
  lsmod | grep -q kvm_intel || { echo "FAIL: kvm_intel module not loaded"; fail=1; }
  { [ -r /dev/kvm ] && [ -w /dev/kvm ]; } || { echo "FAIL: /dev/kvm not accessible"; fail=1; }
  if [ "$fail" -ne 0 ]; then
    cat <<'MSG'

Nested virtualisation is not enabled on this instance. Either launch with:
  aws ec2 run-instances --instance-type m7i.2xlarge \
      --cpu-options "NestedVirtualization=enabled" ...
or stop this instance and enable it in place:
  aws ec2 stop-instances --instance-ids <id>
  aws ec2 modify-instance-cpu-options --instance-id <id> \
      --core-count 4 --threads-per-core 2 --nested-virtualization enabled
  aws ec2 start-instances --instance-ids <id>
MSG
    exit 1
  fi
  echo "KVM OK"
}

# --- 2. Service user ----------------------------------------------------------
# Never run the control plane as root: KVM comes from group membership and tap
# creation from AmbientCapabilities=CAP_NET_ADMIN.
setup_user() {
  id -u "$USER_NAME" >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin "$USER_NAME"
  getent group kvm >/dev/null && usermod -aG kvm "$USER_NAME"
  echo "user $USER_NAME ready"
}

# --- 3. Directories -----------------------------------------------------------
setup_dirs() {
  mkdir -p "$BASE"/{images,workspaces,run}
  chown -R "$USER_NAME:$USER_NAME" "$BASE"
  chmod 0750 "$BASE" "$BASE/workspaces" "$BASE/run"
  chmod 0555 "$BASE/images"
  echo "directories ready under $BASE"
}

# --- 4. Binaries and guest kernel ---------------------------------------------
install_firecracker() {
  local arch tmp
  arch="$(uname -m)"
  tmp="$(mktemp -d)"
  curl -fsSL "https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-${arch}.tgz" \
    | tar -C "$tmp" -xz
  install -m 0755 "$tmp/release-${FC_VERSION}-${arch}/firecracker-${FC_VERSION}-${arch}" /usr/local/bin/firecracker
  rm -rf "$tmp"
  firecracker --version | head -1
}

# Ubuntu's /boot/vmlinuz is a bzImage; no released firecracker boots one on
# x86_64. Pull an uncompressed ELF vmlinux from the CI bucket instead.
install_kernel() {
  local s3 arch prefix key out
  s3="https://s3.amazonaws.com/spec.ccfc.min"
  arch="$(uname -m)"
  out="$BASE/images/vmlinux-${KERNEL_VERSION}"
  [ -f "$out" ] && { echo "kernel already present: $out"; return; }
  prefix=$(curl -fsSL "$s3?list-type=2&prefix=firecracker-ci/&delimiter=/" \
    | grep -oP "(?<=<Prefix>)firecracker-ci/[0-9]{8}-[^/]+/(?=</Prefix>)" | sort | tail -1)
  key=$(curl -fsSL "$s3?list-type=2&prefix=${prefix}${arch}/vmlinux-${KERNEL_VERSION}" \
    | grep -oP "(?<=<Key>)(${prefix}${arch}/vmlinux-[0-9]+\.[0-9]+(\.[0-9]{1,3})?)(?=</Key>)" \
    | sort -V | tail -1)
  [ -n "$key" ] || { echo "FAIL: no vmlinux-${KERNEL_VERSION} in CI bucket"; exit 1; }
  curl -fsSL -o "$out" "$s3/$key"
  chmod 0444 "$out"
  echo "installed $out from $key"
}

# Verify the two configs the whole boot path depends on. Without OVERLAY_FS
# overlay-init dies; without IP_PNP the ip= boot arg is silently ignored and the
# VM comes up with no address. Neither is guaranteed by the CI test kernel.
verify_kernel_config() {
  local img="$BASE/images/vmlinux-${KERNEL_VERSION}" tool
  tool="$(command -v extract-ikconfig || echo /usr/src/linux/scripts/extract-ikconfig)"
  if [ ! -x "$tool" ]; then
    echo "WARN: extract-ikconfig not found; verify manually:"
    echo "      extract-ikconfig $img | grep -E 'OVERLAY_FS|IP_PNP'"
    return
  fi
  local cfg; cfg="$("$tool" "$img" 2>/dev/null || true)"
  for opt in CONFIG_OVERLAY_FS CONFIG_IP_PNP; do
    if ! grep -q "^${opt}=y" <<<"$cfg"; then
      echo "WARN: $opt not set to y in $img -- boot will fail; rebuild the kernel with it on"
    else
      echo "$opt=y OK"
    fi
  done
}

# --- 5. Sysctl and swap -------------------------------------------------------
# No swap: firecracker guidance is no-swap/no-overcommit, so overcommitting
# would mean the OOM killer reaping live VMs.
setup_sysctl() {
  cat > /etc/sysctl.d/99-cracked.conf <<'SYSCTL'
net.ipv4.ip_forward=1
SYSCTL
  sysctl -q -p /etc/sysctl.d/99-cracked.conf
  swapoff -a || true
  sed -i.bak '/[[:space:]]swap[[:space:]]/s/^\([^#]\)/#\1/' /etc/fstab || true
  echo "ip_forward on, swap off"
}

# --- 6. Firewall --------------------------------------------------------------
setup_firewall() {
  local dev; dev="$(ip -j route list default | jq -r '.[0].dev')"
  echo "host egress interface: $dev"
  iptables-nft -t nat -C POSTROUTING -s "$GUEST_CIDR" -o "$dev" -j MASQUERADE 2>/dev/null \
    || iptables-nft -t nat -A POSTROUTING -s "$GUEST_CIDR" -o "$dev" -j MASQUERADE

  # Own chain so Docker's FORWARD rules cannot reorder ours.
  iptables-nft -N CRACKED_FWD 2>/dev/null || iptables-nft -F CRACKED_FWD
  iptables-nft -C FORWARD -i tap+ -j CRACKED_FWD 2>/dev/null \
    || iptables-nft -I FORWARD 1 -i tap+ -j CRACKED_FWD
  iptables-nft -C FORWARD -o tap+ -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null \
    || iptables-nft -I FORWARD 2 -o tap+ -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

  # CRITICAL: firecracker does zero traffic filtering. A compromised guest must
  # not reach IMDS and steal the instance role credentials.
  iptables-nft -A CRACKED_FWD -d 169.254.0.0/16 -j DROP   # link-local, incl. IMDS
  iptables-nft -A CRACKED_FWD -d "$GUEST_CIDR"  -j DROP   # VM-to-VM isolation
  iptables-nft -A CRACKED_FWD -d "$VPC_CIDR"    -j DROP   # VPC internals
  iptables-nft -A CRACKED_FWD -j ACCEPT

  # FORWARD does not cover traffic addressed to the host itself: without this a
  # guest reaches sshd and the control plane on 172.16.0.1 and the ENI address.
  iptables-nft -C INPUT -i tap+ -m conntrack --ctstate NEW -j DROP 2>/dev/null \
    || iptables-nft -I INPUT 1 -i tap+ -m conntrack --ctstate NEW -j DROP

  command -v netfilter-persistent >/dev/null && netfilter-persistent save || \
    echo "WARN: netfilter-persistent not installed; rules will not survive reboot"
}

# --- 7. Limits ----------------------------------------------------------------
setup_limits() {
  mkdir -p /etc/systemd/system.conf.d
  cat > /etc/systemd/system.conf.d/99-cracked.conf <<'LIMITS'
[Manager]
DefaultLimitNOFILE=65536
LIMITS
  echo "nofile limit raised"
}

check_kvm
setup_user
setup_dirs
install_firecracker
install_kernel
verify_kernel_config
setup_sysctl
setup_firewall
setup_limits

cat <<MSG

Host setup complete.

Remaining steps:
  1. Build the guest image:   scripts/build-rootfs.sh
  2. Install the unit:        install -m0644 deploy/cracked.service /etc/systemd/system/
  3. Set the token:           systemctl edit cracked  (Environment=CRACKED_TOKEN=...)
  4. Start:                   systemctl enable --now cracked

Before building anything on top, run the benchmark gate: boot one VM and check
guest steal time (vmstat 1) and 4k randread on /dev/vdb. AWS steers
latency-sensitive workloads to bare metal; if steal exceeds ~15% at capacity,
move to m7i.metal-24xl. Only MaxVMs and the instance type change.
MSG
