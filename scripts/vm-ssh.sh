#!/usr/bin/env bash
# Open a shell on a running microVM, from the host that runs them.
#
# Guest IPs are derived from live tap devices rather than from the control
# plane. That is not a shortcut: the registry is in memory, Sweep() deletes all
# of run/ on every control-plane restart, and what survives is 0750 owned by a
# nologin user. The taps are the only source that needs neither CRACKED_TOKEN
# nor sudo.
#
# Host identity is deliberately discarded. Slots are recycled, so 172.16.0.2 is
# a different workspace after a delete and recreate; with per-VM host keys a
# real known_hosts would throw the full man-in-the-middle banner on every
# routine recreate, which teaches an operator to ignore the one warning that
# would matter. There is no position from which to mount that attack anyway:
# each tap is a point-to-point /30, the host firewall has no DNAT and no
# PREROUTING at all, and guest-to-guest is dropped.
set -euo pipefail

KEY="${CRACKED_GUEST_KEY:-$HOME/.ssh/cracked_guest}"

# guests lists every live VM as "<tap> <guest-ip>".
#
# The host side of each /30 is 172.16.(4N+1) and its guest is the next address.
# 4*slot+1 is always 1 mod 4, so the host octet is at most 253 and incrementing
# it can never carry into the next /24 -- internal/hostnet/tap_test.go pins that
# contract. The % 4 == 1 test both proves it here and filters any interface
# that merely matches the name pattern.
guests() {
  ip -o -4 addr show | awk '$2 ~ /^tap[0-9]+$/ {
      split($4, a, "/"); split(a[1], o, ".")
      if (o[4] % 4 == 1) printf "%s %s.%s.%s.%d\n", $2, o[1], o[2], o[3], o[4] + 1 }'
}

# resolve turns an argument into a guest address, or picks the only VM running.
# It prints nothing and fails when the answer would be a guess.
resolve() {
  local want="${1:-}" all; all="$(guests)"
  if [ -z "$all" ]; then
    echo "vm-ssh: no microVMs are running" >&2; return 1
  fi
  case "$want" in
    "")        [ "$(wc -l <<<"$all")" -eq 1 ] && { awk '{print $2}' <<<"$all"; return; }
               echo "vm-ssh: several microVMs are running; name one:" >&2
               awk '{printf "  slot %s\t%s\n", substr($1,4), $2}' <<<"$all" >&2; return 1 ;;
    [0-9]*.*)  echo "$want" ;;                                   # a literal address
    [0-9]*)    awk -v t="tap$want" '$1 == t {print $2}' <<<"$all" ;;
    tap[0-9]*) awk -v t="$want"    '$1 == t {print $2}' <<<"$all" ;;
    *)         return 2 ;;                                       # not a target at all
  esac
}

# await waits for sshd to bind, but only while the guest is actively REFUSING.
#
# The control plane answers a create as soon as agentd is up, and sshd binds a
# few seconds after that -- so an ssh immediately after POST /vms lands on a
# closed port and reads as a broken image rather than as a machine that is not
# quite ready. A refusal is instant and cheap to retry; a paused or wedged VM
# does not refuse, it goes silent, and that case still fails fast on the
# ConnectTimeout below rather than being retried for half a minute.
await() {
  local ip="$1" i
  for i in $(seq 1 20); do
    if bash -c "exec 3<>/dev/tcp/$ip/22" 2>/dev/null; then return; fi
    [ "$i" = 1 ] && printf "vm-ssh: %s is not answering on 22 yet, waiting" "$ip" >&2
    printf "." >&2
    sleep 1
  done
  printf "\n" >&2
}

main() {
  local ip_only="" target=""
  [ "${1:-}" = "--ip" ] && { ip_only=1; shift; }

  # A first argument that does not name a VM is the remote command, so
  # `vm-ssh uptime` works when only one machine is running.
  if target="$(resolve "${1:-}")"; then
    [ $# -gt 0 ] && shift
  else
    [ $? -eq 2 ] || exit 1
    target="$(resolve "")" || exit 1
  fi
  [ -n "$target" ] || { echo "vm-ssh: no VM at ${1:-that slot}" >&2; exit 1; }
  [ -n "$ip_only" ] && { echo "$target"; return; }
  [ -r "$KEY" ] || { echo "vm-ssh: no key at $KEY; run host-setup.sh" >&2; exit 1; }
  await "$target"

  # ConnectTimeout and ServerAlive*: a PAUSED VM's tap is still listed and looks
  # exactly like a live one, so without these a paused or half-booted machine
  # hangs for two minutes rather than saying so in five seconds -- and a session
  # paused mid-use wedges the terminal instead of dropping.
  # IdentitiesOnly: the operator arrives here over ssh with an agent and other
  # keys loaded; offering all of them can hit MaxAuthTries, which reads as the
  # baked key being wrong.
  # SendEnv=-LC_*: macOS sends LC_CTYPE and it survives both hops. The guest has
  # no locales package, so everything inside it would print "Setting locale
  # failed" and read like a broken image.
  # ForwardAgent=no is the default, stated as intent: this guest browses
  # untrusted pages with an agent that has passwordless root.
  exec ssh -i "$KEY" -o IdentitiesOnly=yes \
    -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR \
    -o ConnectTimeout=5 -o ServerAliveInterval=15 -o ServerAliveCountMax=4 \
    -o ForwardAgent=no -o ForwardX11=no -o "SendEnv=-LC_*" \
    "agent@$target" "$@"
}

main "$@"
