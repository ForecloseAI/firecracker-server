#!/usr/bin/env bash
# Ship this checkout to the cracked EC2 host and run the deploy there.
#
# This half only gets the tree onto the box; scripts/deploy-host.sh does the
# work. Run it from a laptop:
#
#   scripts/deploy.sh                 # full deploy, prompts before anything destructive
#   scripts/deploy.sh -y              # unattended
#   scripts/deploy.sh --only image    # just rebuild the guest image
#
# Env overrides: CRACKED_INSTANCE, CRACKED_REGION, CRACKED_KEY, CRACKED_PORT,
# CRACKED_SSH_TARGET (set it to ubuntu@<dns> to skip the tunnel and go direct).
set -euo pipefail

INSTANCE="${CRACKED_INSTANCE:-i-045c46b232672d3d6}"
REGION="${CRACKED_REGION:-ap-south-1}"
KEY="${CRACKED_KEY:-$HOME/.ssh/firecracker-instance-key.pem}"
PORT="${CRACKED_PORT:-2222}"
TARGET="${CRACKED_SSH_TARGET:-ubuntu@localhost}"
HERE="$(cd "$(dirname "$0")/.." && pwd)"

# Short on purpose. A ControlPath under any longer prefix blows the 104-byte
# sockaddr limit, and ssh reports that as a connection failure without ever
# mentioning the length.
# %C hashes the SSH destination (including user, host, and port). Without it a
# live multiplexing master can silently send a deploy intended for a different
# CRACKED_SSH_TARGET to the previous host.
SOCK=/tmp/cr-%C.sock
SSH_OPTS=(-i "$KEY" -o ControlMaster=auto -o ControlPath="$SOCK" -o ControlPersist=30m)
[ -n "${CRACKED_SSH_TARGET:-}" ] || SSH_OPTS+=(-p "$PORT")

# check_branch refuses a tree that is not what is under review.
#
# vet in preflight is deliberate, and it is vet only, not test: TestParseSchedule fails on macOS by design (APFS is
# case-insensitive, so "asia/kolkata" resolves and the expected error never
# fires). The host runs the real gate as the first deploy stage.
check_branch() {
  local branch; branch="$(git -C "$HERE" rev-parse --abbrev-ref HEAD)"
  if [ "$branch" != "main" ]; then
    [ "${ALLOW_BRANCH:-0}" = "1" ] || {
      echo "FATAL: on $branch, not main. ALLOW_BRANCH=1 to ship it anyway."; exit 1; }
    echo "WARN: shipping $branch, not main"
    return
  fi
  # A stale main is the easy mistake: the tree looks right and is missing the
  # last merge. Only checked on main -- a deliberate branch deploy is not stale.
  git -C "$HERE" fetch --quiet origin main || true
  [ "$(git -C "$HERE" rev-parse HEAD)" = "$(git -C "$HERE" rev-parse origin/main)" ] || {
    echo "FATAL: main is behind origin/main. git pull first."; exit 1; }
}

# preflight refuses to ship a tree that is not the one under review.
preflight() {
  check_branch
  [ -z "$(git -C "$HERE" status --porcelain --untracked-files=all)" ] || [ "${ALLOW_DIRTY:-0}" = "1" ] || {
    echo "FATAL: working tree is dirty. ALLOW_DIRTY=1 to ship it anyway."; exit 1; }
  # rsync does not understand .gitignore. Refuse ignored files too unless they
  # are one of the generated paths excluded by sync_repo below; otherwise a
  # local secret or stale source file could become part of the deploy.
  local ignored
  ignored="$(git -C "$HERE" ls-files --others --ignored --exclude-standard | while IFS= read -r path; do
    case "$path" in
      bin/*|rootfs/files/agentd|*/.DS_Store|.DS_Store) ;;
      *) printf '%s\n' "$path" ;;
    esac
  done)"
  [ -z "$ignored" ] || [ "${ALLOW_DIRTY:-0}" = "1" ] || {
    printf 'FATAL: ignored files would be shipped:\n%s\n' "$ignored"
    echo "Set ALLOW_DIRTY=1 to ship them intentionally."; exit 1; }
  make -C "$HERE" vet
  echo "shipping $(git -C "$HERE" rev-parse --short HEAD)"
}

# open_tunnel brings up the EC2 Instance Connect tunnel unless one is already
# listening. Port 22 is usually shut: the security group allows a single /32
# and it goes stale every time the ISP rotates the address. There is no SSM
# fallback -- the instance has no IAM profile.
open_tunnel() {
  [ -z "${CRACKED_SSH_TARGET:-}" ] || { echo "going direct to $TARGET"; return; }
  nc -z localhost "$PORT" 2>/dev/null && { echo "tunnel already up on $PORT"; return; }
  aws ec2-instance-connect open-tunnel --region "$REGION" \
    --instance-id "$INSTANCE" --local-port "$PORT" &
  for _ in $(seq 20); do
    nc -z localhost "$PORT" 2>/dev/null && { echo "tunnel up on $PORT"; return; }
    sleep 1
  done
  echo "FATAL: the tunnel never came up on $PORT"; exit 1
}

# sync_repo ships the WHOLE tree. ~/cracked on the box is an rsync target, not
# a git checkout, so sending only Go files leaves scripts/ and the Dockerfile
# stale and the next image gets built from an old recipe. --stats because macOS
# ships rsync 2.6.9 with no --info=stats2.
#
# --delete, and it is not optional. Without it a file deleted from the repo lives
# on in the build tree forever, invisible from a laptop. On 2026-09-03 that broke
# the deploy: apps_risk.go -- a classifier deleted before it was ever committed --
# had sat there for a month, and the day a new test declared a helper by the same
# name as its own, the host build stopped compiling. The next one might be a .go
# that compiles and shadows something real. The two things the HOST generates are
# excluded below, and rsync does not delete excluded files, so this only ever
# removes what the repo no longer has.
sync_repo() {
  # .DS_Store is excluded, not merely gitignored: rsync does not read
  # .gitignore, and one landing in rootfs/ both changes the image fingerprint
  # (Finder rewrites it on every folder open, so rebuilds would flap) and gets
  # COPY'd into the guest beside the skills.
  rsync -az --delete --stats --exclude .git --exclude bin/ --exclude rootfs/files/agentd \
    --exclude '.DS_Store' \
    -e "ssh ${SSH_OPTS[*]}" "$HERE"/ "$TARGET:cracked/"
}

# run_host runs the deploy on the box. -t so its prompts reach the terminal.
run_host() {
  local commit args=""
  commit="$(git -C "$HERE" rev-parse --short HEAD)"
  # %q per argument, not "$*": --only "image binaries" otherwise arrives as two
  # tokens and the second reads as an unknown flag.
  [ $# -eq 0 ] || args="$(printf ' %q' "$@")"
  ssh "${SSH_OPTS[@]}" -t "$TARGET" \
    "CRACKED_COMMIT=$commit bash cracked/scripts/deploy-host.sh$args"
}

preflight
open_tunnel
sync_repo
run_host "$@"
