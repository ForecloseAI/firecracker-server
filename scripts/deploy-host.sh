#!/usr/bin/env bash
# Deploy cracked ON the VM host: rebuild the guest image if its recipe moved,
# install the host binaries that actually changed, and bring the machines back.
#
# Runs on the box, from ~/cracked, after scripts/deploy.sh has rsynced the tree
# there. A separate file rather than a command string over ssh on purpose: a
# multi-step deploy quoted through bash-inside-ssh-inside-bash gets mangled by
# the LOCAL shell before it ever arrives.
#
# Every stage decides for itself whether it has work to do, so a re-run after a
# failure is cheap and safe. Nothing here is a guess: sha256 says whether a
# binary moved, and a recipe fingerprint says whether the image did.
set -euo pipefail

BASE="${CRACKED_BASE:-/var/lib/cracked}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"
STAMP="$BASE/images/rootfs.stamp"
IMAGE_BUILDING="$BASE/images/rootfs.building"
PENDING_FLEET="$BASE/images/rootfs.pending-vms"
DEPLOYED="$BASE/deployed.txt"
DROPIN=/etc/systemd/system/cracked.service.d/override.conf
COMMIT="${CRACKED_COMMIT:-unknown}"
ASSUME_YES="${ASSUME_YES:-0}"
STAGES="test image binaries vms verify"
CHANGED=""
TOK=""
LIVE_IDS=""
LIVE_IDS_KNOWN=0
IMAGE_CHANGED=0
IMAGE_HASH=""
RESTARTED_CRACKED=0
EXPLICIT_STAGES=0
NEED_AUTO_VMS=0
VMS_RAN=0

# usage prints how to run this.
usage() {
  cat <<'EOF'
usage: scripts/deploy-host.sh [-y] [--only "stage stage"]

stages: test image binaries vms verify   (default: all, in that order)
  -y   answer every prompt yes (for an unattended run)
EOF
}

# parse_args reads the flags.
parse_args() {
  while [ $# -gt 0 ]; do
    case "$1" in
      -y|--yes)  ASSUME_YES=1 ;;
      --only)    STAGES="$2"; EXPLICIT_STAGES=1; shift ;;
      -h|--help) usage; exit 0 ;;
      *) echo "unknown flag: $1"; usage; exit 1 ;;
    esac
    shift
  done
}

# say prints a stage banner so a fifteen-minute run stays readable.
say() { printf '\n== %s ==\n' "$*"; }

# confirm asks before something that cannot be undone, unless -y was given.
confirm() {
  [ "$ASSUME_YES" = "1" ] && return 0
  local answer
  read -r -p "$1 [y/N] " answer </dev/tty
  [ "$answer" = "y" ] || [ "$answer" = "Y" ]
}

# same reports whether two files hold identical bytes. This is the check that
# proves a deploy landed; everything else is inference.
same() {
  # sudo on the existence test, not just the hashes: the services run as user
  # cracked, and /proc/<pid>/exe is not stat-able across users. As ubuntu the
  # bare [ -e ] returns false and this reports a good deploy as a failed one.
  sudo test -e "$2" || return 1
  [ "$(sudo sha256sum "$1" | cut -d' ' -f1)" = "$(sudo sha256sum "$2" | cut -d' ' -f1)" ]
}

# token reads the API bearer out of the systemd drop-in, where it is the only
# copy on the box.
token() {
  sudo sed -n 's/^Environment=CRACKED_TOKEN=//p' "$DROPIN" 2>/dev/null | tr -d '"'
}

# api calls the control plane, which listens on loopback only.
api() {
  local method="$1" path="$2"; shift 2
  curl -fsS -X "$method" -H "Authorization: Bearer $TOK" \
    "http://127.0.0.1:8080$path" "$@"
}

# stage_test runs the gate here rather than on the laptop: TestParseSchedule
# fails on macOS because APFS is case-insensitive, so "asia/kolkata" resolves
# and the expected error never fires. Linux is the authoritative run.
stage_test() {
  say "tests"
  make -C "$HERE" test vet
}

# recipe_hash fingerprints everything that goes INTO the guest image, so a
# deploy touching only host code can skip a fifteen-minute rebuild. The build
# script is part of the recipe: changing how the image is made changes it.
recipe_hash() {
  # Relative paths: sha256sum prints the path beside the hash, so absolute ones
  # would refingerprint the whole image just because the tree moved. Paths stay
  # IN, rather than being stripped, so renaming a skill still counts as a change.
  ( cd "$HERE" &&
    { find rootfs -type f ! -name agentd ! -name .DS_Store -print0 | sort -z | xargs -0 sha256sum
      sha256sum rootfs/files/agentd scripts/build-rootfs.sh
    } | sha256sum | cut -d' ' -f1 )
}

# mount_ro mounts the current image read-only, clearing a mount left behind by
# a run that died between mounting and unmounting -- without this, one failed
# deploy wedges every deploy after it.
mount_ro() {
  local image="${1:-$BASE/images/rootfs.ext4}"
  sudo mkdir -p /mnt/rootfs-ro
  sudo umount /mnt/rootfs-ro 2>/dev/null || true
  sudo mount -o ro,loop "$image" /mnt/rootfs-ro
}

# recover_key reads the baked API key out of the CURRENT image so a rebuild
# never needs it handed over. build-rootfs.sh only WARNS when it is missing --
# it will happily ship an image whose every agent has an empty key.
recover_key() {
  mount_ro "${1:-$BASE/images/rootfs.ext4}"
  ANTHROPIC_API_KEY="$(sudo sed -n 's/^ANTHROPIC_API_KEY=//p' /mnt/rootfs-ro/etc/cracked-agent.env)"
  sudo umount /mnt/rootfs-ro
  [ -n "$ANTHROPIC_API_KEY" ] || { echo "FATAL: no API key in the current image"; exit 1; }
  export ANTHROPIC_API_KEY
  echo "recovered the API key from the running image (${#ANTHROPIC_API_KEY} chars)"
}

# check_space refuses to start a build that cannot finish, and says what NOT to
# delete: this host still runs the CLASSIC docker builder, so dangling images
# ARE the layer cache. `docker image prune` to make room turns a ten-minute
# incremental rebuild into a full one -- `docker system df` reports "Build Cache
# 0B" while the cache is sitting in those images, which is what makes it a trap.
check_space() {
  local need free
  need=$(( $(sudo du -sm "$BASE/images/rootfs.ext4" | cut -f1) * 2 ))
  free="$(df -Pm "$BASE" | awk 'NR==2{print $4}')"
  [ "$free" -gt "$need" ] || {
    echo "FATAL: ${free}MB free, the backup plus the build needs about ${need}MB."
    echo "       Delete an old rootfs.ext4.bak or an unused workspace -- do NOT"
    echo "       run 'docker image prune', it destroys the build cache."
    exit 1; }
}

# build_image backs the current image up, then rebuilds. The backup is not
# paranoia: build-rootfs.sh rm -f's the output before mkfs, so a build that
# dies halfway otherwise leaves no image at all.
build_image() {
  check_space
  # The build script removes its output before recreating it. Preserve the
  # original backup across retries rather than replacing it with a partial
  # image left by the previous failed attempt.
  if ! sudo test -e "$IMAGE_BUILDING"; then
    sudo cp "$BASE/images/rootfs.ext4" "$BASE/images/rootfs.ext4.bak"
    printf '%s\n' "$COMMIT" | sudo tee "$IMAGE_BUILDING" >/dev/null
    recover_key
  else
    echo "retrying image build; preserving the existing known-good backup"
    recover_key "$BASE/images/rootfs.ext4.bak"
  fi
  # As ubuntu, never `sudo -E`: the script self-sudoes, and sudo resets HOME
  # and PATH, losing both the guest pubkey and the Go toolchain.
  SSH_PUBKEY="$(cat "$HOME/.ssh/cracked_guest.pub")" "$HERE/scripts/build-rootfs.sh"
}

# verify_image proves the new image carries the binary we just built. Better
# than grepping for a hand-picked string: a discriminator derived from an old
# diff silently passes against a stale image, and a hash cannot.
verify_image() {
  local want have keylen
  want="$(sha256sum "$HERE/rootfs/files/agentd" | cut -d' ' -f1)"
  mount_ro
  have="$(sudo sha256sum /mnt/rootfs-ro/usr/local/bin/agentd | cut -d' ' -f1)"
  keylen="$(sudo sed -n 's/^ANTHROPIC_API_KEY=//p' /mnt/rootfs-ro/etc/cracked-agent.env | wc -c)"
  sudo umount /mnt/rootfs-ro
  [ "$want" = "$have" ] || { echo "FATAL: the image's agentd is not the one just built"; exit 1; }
  [ "$keylen" -gt 20 ] || { echo "FATAL: the image shipped with no API key"; exit 1; }
  echo "image ok: agentd ${want:0:12}, key present"
}

# stage_image rebuilds the guest image only when its recipe moved. Nothing is
# taken down here on purpose: running VMs hold the old inode after rm, so a
# failed build costs no downtime at all.
stage_image() {
  say "guest image"
  make -C "$HERE" build-agentd
  local want; want="$(recipe_hash)"
  if [ "$want" = "$(sudo cat "$STAMP" 2>/dev/null || true)" ]; then
    echo "recipe unchanged; keeping the current image"
    return
  fi
  build_image
  verify_image
  sudo rm -f "$IMAGE_BUILDING"
  # Do not stamp yet. A VM that is not successfully recreated still uses the
  # old, unlinked image, and the next deploy must retry the whole fleet.
  IMAGE_HASH="$want"
  IMAGE_CHANGED=1
  echo "previous image kept at $BASE/images/rootfs.ext4.bak"
}

# install_binaries installs only what moved. An agentd-only deploy leaves both
# host binaries byte-identical, and then there is nothing to restart and no
# reason to take the control plane down.
install_binaries() {
  make -C "$HERE" build
  for b in cracked cracked-chat; do
    if same "$HERE/bin/$b" "/usr/local/bin/$b"; then
      local pid
      pid="$(systemctl show -p MainPID --value "$b" 2>/dev/null || true)"
      if systemctl is-active --quiet "$b" && [ -n "$pid" ] && [ "$pid" != "0" ] && \
          same "$HERE/bin/$b" "/proc/$pid/exe"; then
        echo "$b unchanged and already running"
      else
        echo "$b is installed but needs a restart"
        CHANGED="$CHANGED $b"
      fi
    else
      sudo install -m 0755 "$HERE/bin/$b" "/usr/local/bin/$b"
      CHANGED="$CHANGED $b"
    fi
  done
}

# restart_changed restarts only the services whose binary moved. Restarting
# cracked kills every running VM -- workspaces survive, the machines do not --
# so it asks first, and the vms stage brings them back.
restart_changed() {
  [ -n "$CHANGED" ] || { echo "nothing to restart"; return; }
  # systemd serves a cached unit definition until told otherwise, so a changed
  # drop-in is not picked up by a restart alone -- and it warns on every
  # systemctl call until this is run.
  sudo systemctl daemon-reload
  for b in $CHANGED; do
    if [ "$b" = "cracked" ]; then
      confirm "restart cracked? it kills the $(pgrep -c firecracker || true) running VMs" || exit 1
      ensure_pending_fleet
    fi
    sudo systemctl restart "$b"
    [ "$b" = "cracked" ] && RESTARTED_CRACKED=1
    echo "restarted $b"
  done
  if [ "$RESTARTED_CRACKED" = "1" ] && ! stage_selected vms; then
    echo "cracked restarted; automatically adding the vms stage"
    NEED_AUTO_VMS=1
  fi
}

# stage_binaries is the host half: build, install what changed, restart that.
stage_binaries() {
  say "host binaries"
  install_binaries
  restart_changed
}

# vm_ids lists the machines to bring back. From the API while it still knows
# them, and from the workspaces on disk when it does not: the registry is in
# memory only, so a cracked restart forgets every id while the disks remain.
vm_ids() {
  [ "$LIVE_IDS_KNOWN" = "0" ] || { echo "$LIVE_IDS"; return; }
  local fleet ids
  if fleet="$(api GET /vms 2>/dev/null)" &&
      ids="$(printf '%s' "$fleet" | jq -r '.vms[].id' 2>/dev/null)"; then
    echo "$ids"
    return
  fi
  # Last resort, and it GUESSES: every workspace on disk, including machines
  # that were not running. sudo because /var/lib/cracked is cracked:cracked
  # and ubuntu cannot even traverse it -- without it this silently finds
  # nothing and the fleet stays down.
  echo "WARN: no record of what was running; guessing from the workspaces" >&2
  ids="$(sudo ls "$BASE/workspaces" 2>/dev/null | sed 's/\.ext4$//')"
  echo "$ids"
}

# ensure_pending_fleet persists the pre-deploy fleet before anything can stop
# or delete it. A retry loads this snapshot instead of trusting the restarted
# control plane's now-incomplete in-memory registry.
ensure_pending_fleet() {
  sudo test -e "$PENDING_FLEET" && return
  local ids; ids="$(vm_ids)"
  printf '%s\n' "$ids" | sudo tee "$PENDING_FLEET" >/dev/null
}

stage_selected() {
  case " $STAGES " in
    *" $1 "*) return 0 ;;
    *) return 1 ;;
  esac
}

# recreate rebuilds one machine on the current image. DELETE *without* ?purge
# keeps workspaces/<id>.ext4 and the re-POST reuses it, so agent memory, event
# log and open tasks all come back. workspace_new:false is the proof.
recreate() {
  local id="$1" had=0 out
  sudo test -e "$BASE/workspaces/$id.ext4" && had=1
  api DELETE "/vms/$id" >/dev/null 2>&1 || true
  out="$(api POST /vms -d "{\"id\":\"$id\"}")"
  echo "$out" | jq -c '{id, state, workspace_new}'
  # workspace_new:false is THE proof the agent's memory, event log and open
  # tasks came back. Unattended there is no one reading the line above, so the
  # one case that means data loss has to stop the run.
  if [ "$had" = "1" ] && [ "$(echo "$out" | jq -r '.workspace_new')" != "false" ]; then
    echo "FATAL: $id came back on a NEW workspace; its data did not survive"
    exit 1
  fi
}

# stamp_image records that there are no remaining machines on the old image.
# Keeping this in the vms stage makes an interrupted recreation retryable.
stamp_image() {
  [ "$IMAGE_CHANGED" = "1" ] || return
  echo "$IMAGE_HASH" | sudo tee "$STAMP" >/dev/null
  echo "recorded image recipe after all machines were recreated"
}

# stage_vms brings every machine back. A VM keeps running its old image until
# it is recreated, so this is not optional after an image build.
stage_vms() {
  say "machines"
  VMS_RAN=1
  # Recreating costs every machine its uptime, so it needs a reason: a new image
  # to pick up, a cracked restart that already killed them, or an explicit
  # --only vms. Without this, a no-op deploy run with -y would cycle the fleet
  # for nothing.
  if [ "$IMAGE_CHANGED$RESTARTED_CRACKED$EXPLICIT_STAGES" = "000" ] && \
      ! sudo test -e "$PENDING_FLEET"; then
    echo "nothing changed for the guests; machines keep running"
    return
  fi
  ensure_pending_fleet
  local ids; ids="$(sudo cat "$PENDING_FLEET")"
  [ -n "$ids" ] || {
    echo "no machines to recreate"
    stamp_image
    sudo rm -f "$PENDING_FLEET"
    return
  }
  echo "machines: $(echo "$ids" | tr '\n' ' ')"
  # Only ask when there is something to lose. If cracked was just restarted
  # they are already down, and declining would leave them down.
  if [ "$(pgrep -c firecracker || true)" != "0" ]; then
    confirm "recreate these? each is briefly down; workspaces are kept" || exit 1
  fi
  for id in $ids; do recreate "$id"; done
  stamp_image
  sudo rm -f "$PENDING_FLEET"
}

# stage_verify proves what is running and records it, so the next deploy does
# not have to reverse-engineer what was live from grepping binaries.
stage_verify() {
  say "verify"
  local failed=0
  for service in cracked cracked-chat; do
    if systemctl is-active --quiet "$service"; then
      echo "$service: active"
    else
      echo "FATAL: $service is not active"
      failed=1
    fi
  done
  curl -fsS 127.0.0.1:8080/healthz >/dev/null && echo "healthz ok" || {
    echo "FATAL: healthz did not answer"; failed=1; }
  for b in cracked cracked-chat; do
    local pid; pid="$(systemctl show -p MainPID --value "$b" 2>/dev/null || true)"
    if [ -n "$pid" ] && [ "$pid" != "0" ] && same "$HERE/bin/$b" "/proc/$pid/exe"; then
      echo "$b: running the binary just built"
    else
      echo "FATAL: $b is NOT running the binary just built"
      failed=1
    fi
  done
  [ "$failed" = "0" ] || { echo "FATAL: deployment verification failed"; return 1; }
  echo "$COMMIT deployed $(date -u +%FT%TZ)" | sudo tee -a "$DEPLOYED"
}

# main runs the requested stages in order.
main() {
  parse_args "$@"
  # Checked up front: jq missing mid-run aborts BETWEEN a machine's DELETE and
  # its POST, which leaves that machine deleted and not brought back.
  command -v jq >/dev/null || { echo "FATAL: jq is not installed"; exit 1; }
  TOK="$(token)"
  [ -n "$TOK" ] || echo "WARN: no CRACKED_TOKEN in $DROPIN; the vms stage will fail"
  # Recorded now, while the control plane still knows. The registry is in memory
  # only, so restarting cracked erases it -- asking after the restart is asking
  # a process that has already forgotten.
  local fleet ids
  if sudo test -e "$PENDING_FLEET"; then
    LIVE_IDS="$(sudo cat "$PENDING_FLEET")"
    LIVE_IDS_KNOWN=1
    echo "resuming pending fleet snapshot"
  elif fleet="$(api GET /vms 2>/dev/null)" && \
      ids="$(printf '%s' "$fleet" | jq -r '.vms[].id' 2>/dev/null)"; then
    LIVE_IDS="$ids"
    LIVE_IDS_KNOWN=1
  else
    echo "WARN: could not snapshot the running fleet; workspace fallback may be used" >&2
  fi
  echo "running now: $(echo "$LIVE_IDS" | tr '\n' ' ')"
  for s in $STAGES; do
    if [ "$s" = "verify" ] && [ "$NEED_AUTO_VMS" = "1" ] && [ "$VMS_RAN" = "0" ]; then
      stage_vms
    fi
    "stage_$s"
  done
  if [ "$NEED_AUTO_VMS" = "1" ] && [ "$VMS_RAN" = "0" ]; then
    stage_vms
  fi
  say "done"
}

main "$@"
