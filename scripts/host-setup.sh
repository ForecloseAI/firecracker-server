#!/usr/bin/env bash
# One-time, idempotent provisioning for the Firecracker VM host. Run as root.
set -euo pipefail

BASE="${CRACKED_BASE:-/var/lib/cracked}"
USER_NAME="${CRACKED_USER:-cracked}"
FC_VERSION="${FC_VERSION:-v1.16.1}"
KERNEL_VERSION="${KERNEL_VERSION:-6.18}"
GUEST_CIDR="172.16.0.0/16"
# The one host port a guest may reach: the connected-apps broker, which holds
# the provider credential so no guest has to. Must match CHAT_APPS_ADDR.
APPS_PORT="${APPS_PORT:-8092}"
# The Go toolchain is pinned to whatever go.mod asks for, so resolve the repo
# root rather than trusting the caller's cwd.
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

[ "$(id -u)" -eq 0 ] || { echo "must run as root"; exit 1; }

# --- 0. Dependencies ----------------------------------------------------------
# jq and netfilter-persistent are NOT on the stock Ubuntu server AMI, and this
# script needs both. iptables-persistent prompts interactively unless preseeded.
install_deps() {
  export DEBIAN_FRONTEND=noninteractive
  echo iptables-persistent iptables-persistent/autosave_v4 boolean false | debconf-set-selections
  echo iptables-persistent iptables-persistent/autosave_v6 boolean false | debconf-set-selections
  apt-get update -qq
  apt-get install -y -qq jq curl iptables iptables-persistent e2fsprogs iproute2 openssh-client
  echo "dependencies installed"
}

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
#
# systemd-journal is what lets the dashboard's service-log panel read the OTHER
# units' journals -- the chat service's especially, where every API request
# lands. Without it GET /logs returns journalctl's permissions notice instead of
# lines, which reads as an empty log rather than as a misconfigured host.
setup_user() {
  id -u "$USER_NAME" >/dev/null 2>&1 || useradd -r -s /usr/sbin/nologin "$USER_NAME"
  getent group kvm >/dev/null && usermod -aG kvm "$USER_NAME"
  getent group systemd-journal >/dev/null && usermod -aG systemd-journal "$USER_NAME"
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

# --- 3b. The operator's key for reaching a guest ------------------------------
# Generated for the HUMAN who runs this, not for root and not for $USER_NAME:
# that account is nologin, and a key owned by root would need sudo on every
# connection. Only the public half ever leaves this host -- it is baked into the
# guest image, where its private counterpart never appears.
setup_guest_key() {
  local u home key
  u="${CRACKED_OPERATOR:-${SUDO_USER:-ubuntu}}"
  # getent, NOT ~$u: bash expands tildes BEFORE parameters, so "~$u" stays a
  # literal string and ssh-keygen would create a directory named "~ubuntu" in
  # whatever this script's working directory happens to be.
  home="$(getent passwd "$u" | cut -d: -f6)"
  [ -n "$home" ] || { echo "WARN: no home for $u; skipping the guest key"; return; }
  key="$home/.ssh/cracked_guest"
  # Never rotate an existing key: the public half is baked into an image that
  # was already built, and replacing it here would lock the operator out of
  # every running VM with no error anywhere.
  if [ ! -f "$key" ]; then
    install -d -m 0700 -o "$u" -g "$u" "$home/.ssh"
    # runuser rather than generate-as-root-then-chown: ownership and the 0600
    # mode come out right by construction.
    runuser -u "$u" -- ssh-keygen -t ed25519 -N "" -C cracked-guest -f "$key"
  fi
  install -m 0755 "$REPO_DIR/scripts/vm-ssh.sh" /usr/local/bin/vm-ssh
  GUEST_PUBKEY="$(cat "$key.pub")"
  echo "guest key ready at $key, vm-ssh installed"
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

# --- 5. Go toolchain ----------------------------------------------------------
# Resolve the Go release to install: GO_VERSION if set, else the newest patch of
# the line go.mod asks for. go.mod carries a language version (`go 1.27`) while
# releases are tagged `go1.27.0`, so appending a bare .0 would pin the oldest
# patch and miss every security fix on that line.
go_version() {
  local line
  [ -n "${GO_VERSION:-}" ] && { echo "$GO_VERSION"; return; }
  line=$(grep -oE '^go [0-9]+\.[0-9]+' "$REPO_DIR/go.mod" | awk '{print $2}')
  [ -n "$line" ] || { echo "FAIL: no go directive in $REPO_DIR/go.mod" >&2; exit 1; }
  curl -fsSL 'https://go.dev/dl/?mode=json&include=all' \
    | grep -oE "\"go${line//./\\.}(\.[0-9]+)?\"" | tr -d '"' \
    | sed 's/^go//' | sort -V | tail -1
}

# Install that Go under /usr/local/go. Ubuntu's golang-go is deliberately not
# used: 24.04 ships 1.22 against a go.mod that needs far newer, and with
# GOTOOLCHAIN=auto a too-old Go tries to fetch the newer one and fails, so
# NOTHING in this module builds. /usr/local/bin precedes /usr/bin in sudo's
# secure_path, so the symlink wins over any packaged go still installed.
install_go() {
  local want have arch tmp
  want="$(go_version)"
  [ -n "$want" ] || { echo "FAIL: could not resolve a Go release"; exit 1; }
  have="$(/usr/local/go/bin/go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')"
  if [ "$have" != "$want" ]; then
    arch="$(dpkg --print-architecture)"
    tmp="$(mktemp -d)"
    curl -fsSL "https://go.dev/dl/go${want}.linux-${arch}.tar.gz" -o "$tmp/go.tgz"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "$tmp/go.tgz"
    rm -rf "$tmp"
    echo "installed go $want (was ${have:-none})"
  fi
  # Relink unconditionally, never only on a fresh unpack. An already-unpacked
  # /usr/local/go with no symlink is invisible -- apt's /usr/bin/go keeps
  # winning and nothing builds -- and that is exactly the state a version-gated
  # relink would refuse to repair. This host spent ten days in it.
  ln -sf /usr/local/go/bin/go /usr/local/bin/go
  ln -sf /usr/local/go/bin/gofmt /usr/local/bin/gofmt
  /usr/local/bin/go version
}

# --- 6. Sysctl and swap -------------------------------------------------------
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

# --- 7. Firewall --------------------------------------------------------------
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
  # Deny every private range rather than guessing one VPC CIDR. This covers
  # VM-to-VM (172.16/16), the VPC itself (commonly 172.31/16 on default VPCs,
  # NOT 10/8), and docker0 (172.17/16). Guests only ever need public internet,
  # so allow-by-exception is both simpler and safer than enumerating.
  iptables-nft -A CRACKED_FWD -d 10.0.0.0/8     -j DROP
  iptables-nft -A CRACKED_FWD -d 172.16.0.0/12  -j DROP
  iptables-nft -A CRACKED_FWD -d 192.168.0.0/16 -j DROP
  iptables-nft -A CRACKED_FWD -j ACCEPT

  # FORWARD does not cover traffic addressed to the host itself: without this a
  # guest reaches sshd and the control plane on 172.16.0.1 and the ENI address.
  iptables-nft -C INPUT -i tap+ -m conntrack --ctstate NEW -j DROP 2>/dev/null \
    || iptables-nft -I INPUT 1 -i tap+ -m conntrack --ctstate NEW -j DROP

  # The single exception, inserted ABOVE the drop above -- which is why it comes
  # after it here, since both go in at position 1.
  #
  # This is the connected-apps broker. A guest needs to reach its own app
  # integrations, and the provider's endpoint requires a key that is authority
  # over EVERY user's connected accounts; that key cannot live on a machine its
  # owner has root on. So the host holds it and the guest gets a ticket, and this
  # rule is what lets the guest hand that ticket in.
  #
  # Narrow on purpose: one TCP port, only from a tap, and only to an address on
  # the guest side -- so it opens the broker and not sshd, not the control plane
  # on 172.16.0.1, and nothing on the ENI.
  iptables-nft -C INPUT -i tap+ -p tcp -d "$GUEST_CIDR" --dport "$APPS_PORT" \
      -m conntrack --ctstate NEW -j ACCEPT 2>/dev/null \
    || iptables-nft -I INPUT 1 -i tap+ -p tcp -d "$GUEST_CIDR" --dport "$APPS_PORT" \
      -m conntrack --ctstate NEW -j ACCEPT

  # The broker binds every interface, because tap addresses come and go with the
  # VMs. The port is not in the security group, and the rule above is the only
  # way in -- but say it out loud on the host's own uplink too, so a security
  # group edited by somebody else cannot quietly expose it.
  iptables-nft -C INPUT -i "$dev" -p tcp --dport "$APPS_PORT" -j DROP 2>/dev/null \
    || iptables-nft -A INPUT -i "$dev" -p tcp --dport "$APPS_PORT" -j DROP

  command -v netfilter-persistent >/dev/null && netfilter-persistent save || \
    echo "WARN: netfilter-persistent not installed; rules will not survive reboot"
}

# --- 8. Limits ----------------------------------------------------------------
setup_limits() {
  mkdir -p /etc/systemd/system.conf.d
  cat > /etc/systemd/system.conf.d/99-cracked.conf <<'LIMITS'
[Manager]
DefaultLimitNOFILE=65536
LIMITS
  echo "nofile limit raised"
}

install_deps
check_kvm
setup_user
setup_dirs
setup_guest_key
install_firecracker
install_kernel
verify_kernel_config
install_go
setup_sysctl
setup_firewall
setup_limits

cat <<MSG

Host setup complete.

Remaining steps:
  1. Build the guest image. The ssh key was generated HERE and is consumed by
     the build, so pass it explicitly -- otherwise the image ships with no
     operator key and vm-ssh fails with a bare "Permission denied (publickey)":

       SSH_PUBKEY='${GUEST_PUBKEY:-<run setup_guest_key>}' scripts/build-rootfs.sh
  2. Install the unit:        install -m0644 deploy/cracked.service /etc/systemd/system/
  3. Set the token:           systemctl edit cracked  (Environment=CRACKED_TOKEN=...)
  4. Start:                   systemctl enable --now cracked

Before building anything on top, run the benchmark gate: boot one VM and check
guest steal time (vmstat 1) and 4k randread on /dev/vdb. AWS steers
latency-sensitive workloads to bare metal; if steal exceeds ~15% at capacity,
move to m7i.metal-24xl. Only MaxVMs and the instance type change.
MSG
