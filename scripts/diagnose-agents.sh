#!/usr/bin/env bash
# Find out why agents are not answering. Run it ON the EC2 host.
#
#   scripts/diagnose-agents.sh              # every running VM
#   scripts/diagnose-agents.sh alice        # one machine
#   scripts/diagnose-agents.sh --no-spend   # skip the one billable probe
#
# "The agent does not answer" is the same sentence for eight different faults,
# and the fleet table cannot tell them apart: a VM whose guest lost DNS, whose
# API key was rotated, or whose credit ran out looks exactly like a healthy one.
# Every column stays green because every column is measured on the host, and the
# thing that broke is three layers below it.
#
# So this walks the chain a message actually takes, from the outside in, and
# stops being interesting at the first layer that fails:
#
#   caddy -> cracked-chat -> cracked -> tap -> agentd -> api.anthropic.com
#
# Read-only. It starts nothing, stops nothing and changes no state. The one
# exception is the live model probe, which spends about one input token per VM
# to prove the key can still buy a completion -- credit exhaustion and a revoked
# key are indistinguishable from a network fault until something tries. Pass
# --no-spend to skip it and lose that distinction.
set -uo pipefail

SPEND=1
WANT=""
for arg in "$@"; do
  case "$arg" in
    --no-spend) SPEND=0 ;;
    # Quits at the first non-comment line rather than at a line number, so
    # editing the header above cannot make --help print the code under it.
    -h|--help) sed -n '2,${/^#/!q; s/^# \{0,1\}//p;}' "$0"; exit 0 ;;
    -*) echo "unknown flag: $arg" >&2; exit 2 ;;
    *) WANT="$arg" ;;
  esac
done

# The model a probe asks for. Guests take it from the profile, so this has to
# track internal/agentd/profiles/*.md -- a model retired upstream fails every
# turn while leaving every health check green, which is exactly the fault this
# script exists to name.
PROBE_MODEL="${CRACKED_PROBE_MODEL:-claude-sonnet-5}"

FAIL=0; WARN=0
pass() { printf '  \033[32mok\033[0m    %s\n' "$*"; }
warn() { printf '  \033[33mwarn\033[0m  %s\n' "$*"; WARN=$((WARN + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m  %s\n' "$*"; FAIL=$((FAIL + 1)); }
note() { printf '        %s\n' "$*"; }
head_() { printf '\n\033[1m%s\033[0m\n' "$*"; }

# ---------------------------------------------------------------- host services

head_ "host services"
for unit in cracked cracked-chat caddy; do
  state="$(systemctl is-active "$unit" 2>/dev/null || true)"
  if [ "$state" = "active" ]; then
    pass "$unit is $state"
  else
    bad "$unit is ${state:-missing}"
    note "systemctl status $unit | tail -30"
  fi
done

# The token is read the way the README tells an operator to read it, so this
# needs no argument and no copy of the secret anywhere.
TOKEN="${CRACKED_TOKEN:-}"
if [ -z "$TOKEN" ]; then
  TOKEN="$(sudo systemctl show cracked -p Environment --value 2>/dev/null \
    | tr ' ' '\n' | sed -n 's/^CRACKED_TOKEN=//p' | head -1)"
fi
[ -n "$TOKEN" ] || { bad "no CRACKED_TOKEN; set it or run with sudo"; exit 1; }

CTL="http://127.0.0.1:8080"
api() { curl -sS --max-time 10 -H "Authorization: Bearer $TOKEN" "$@"; }

# ------------------------------------------------------------------ control plane

head_ "control plane"
if [ "$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' "$CTL/healthz" 2>/dev/null)" = "200" ]; then
  pass "$CTL/healthz answers"
else
  bad "$CTL/healthz does not answer"
  note "journalctl -u cracked -n 50 --no-pager"
  exit 1
fi

VMS="$(api "$CTL/vms" 2>/dev/null)"
if [ -z "$VMS" ]; then
  bad "GET /vms returned nothing -- the token is probably wrong"
  exit 1
fi

# jq is installed by host-setup.sh, so its absence is itself worth reporting.
command -v jq >/dev/null || { bad "jq is not installed; host-setup.sh installs it"; exit 1; }

RUNNING="$(printf '%s' "$VMS" | jq -r '(.vms // .) | .[]? | select(.state=="running") | .id')"
ALL="$(printf '%s' "$VMS" | jq -r '(.vms // .) | .[]? | "\(.id) \(.state)"')"
if [ -z "$ALL" ]; then
  warn "no VMs exist at all -- nothing has been booted since the last restart"
  note "the registry is in memory, so this is normal after 'systemctl restart cracked'"
else
  # A here-string and not a pipe: `... | while` runs the body in a subshell, so
  # every pass/warn it counted would be discarded at the done and the verdict
  # would under-report.
  while read -r id state; do
    [ "$state" = "running" ] && pass "vm $id is $state" || warn "vm $id is $state"
  done <<<"$ALL"
fi
[ -n "$WANT" ] && RUNNING="$WANT"
[ -n "$RUNNING" ] || { head_ "verdict"; echo "  no running VM to inspect."; exit 0; }

# ---------------------------------------------------------------------- host NAT

head_ "host egress rules"
# Without MASQUERADE a guest resolves nothing and every turn dies on dial. The
# rules are saved by netfilter-persistent, so the way this goes missing is a
# reboot after a host-setup.sh run that could not save them.
if sudo iptables-nft -t nat -S POSTROUTING 2>/dev/null | grep -q MASQUERADE; then
  pass "nat POSTROUTING has a MASQUERADE rule"
else
  bad "no MASQUERADE rule -- guests have no route off the box"
  note "sudo scripts/host-setup.sh   # re-applies and saves the rules"
fi
if sudo iptables-nft -S CRACKED_FWD >/dev/null 2>&1; then
  pass "CRACKED_FWD chain is present"
else
  bad "CRACKED_FWD chain is missing -- guest forwarding is not configured"
fi
[ "$(cat /proc/sys/net/ipv4/ip_forward 2>/dev/null)" = "1" ] \
  && pass "ip_forward is on" || bad "ip_forward is off"

# ------------------------------------------------------------------- per machine

# guest_ip asks the control plane where a VM is. Deliberately not derived from
# the tap the way vm-ssh does it: vm-ssh has to work without a token, and this
# script already holds one -- and a tap that is up says nothing about which id
# is behind it after a slot has been recycled.
guest_ip() {
  api "$CTL/vms/$1" 2>/dev/null | jq -r '.guest_ip // .ip // empty'
}

# inguest runs one command inside a VM over the operator exec route. It does not
# need the ssh key, and it works when sshd itself is what is broken.
inguest() {
  local ip="$1" cmd="$2"
  curl -sS --max-time 45 -X POST "http://$ip:8080/debug/exec" \
    -H 'Content-Type: application/json' \
    --data "$(jq -nc --arg c "$cmd" '{cmd:$c}')" 2>/dev/null
}

for id in $RUNNING; do
  head_ "vm $id"
  IP="$(guest_ip "$id")"
  if [ -z "$IP" ]; then
    bad "no guest ip -- the control plane does not know where this VM is"
    continue
  fi
  note "guest $IP"

  # --- the daemon itself
  # The status code and not curl's exit status: curl exits 0 on a 500, so a
  # daemon answering every request with an error would read as healthy here.
  HEALTH="$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' \
    "http://$IP:8080/health" 2>/dev/null)"
  if [ "$HEALTH" = "200" ]; then
    pass "agentd answers /health"
  else
    bad "agentd does not answer on $IP:8080"
    note "the guest booted but the daemon is down or crash-looping"
    note "sudo journalctl -u cracked -n 50 | grep $id   # and the console log:"
    note "sudo tail -50 /var/lib/cracked/run/$id/console.log"
    continue
  fi

  # --- what the agents say they are doing
  ROSTER="$(curl -sS --max-time 10 "http://$IP:8080/agents" 2>/dev/null)"
  if [ -n "$ROSTER" ]; then
    STATES="$(printf '%s' "$ROSTER" | jq -r '(.agents // .) | .[]? | "\(.id) \(.state)"')"
    while read -r aid astate; do
      [ -n "$aid" ] || continue
      case "$astate" in
        waiting) warn "agent $aid is WAITING on a person, not thinking" ;;
        working) note "agent $aid is working" ;;
        *)       note "agent $aid is $astate" ;;
      esac
    done <<<"$STATES"
  fi

  # A raised hand nobody can see is the quietest version of this fault: the
  # agent is blocked on an approval the app never rendered, and it stays blocked
  # for thirty minutes looking exactly like a hang.
  PENDING="$(curl -sS --max-time 10 "http://$IP:8080/pending" 2>/dev/null \
    | jq -r '(.raised // .) | length' 2>/dev/null)"
  if [ -n "$PENDING" ] && [ "$PENDING" != "0" ] && [ "$PENDING" != "null" ]; then
    warn "$PENDING unanswered approval/question card(s) are blocking agents here"
    note "curl -s http://$IP:8080/pending | jq   # answer or interrupt to clear"
  fi

  # --- the credential
  KEY_LEN="$(inguest "$IP" 'sed -n "s/^ANTHROPIC_API_KEY=//p" /etc/cracked-agent.env | tr -d "\n" | wc -c' \
    | jq -r '.stdout // ""' | tr -d '[:space:]')"
  if [ -z "$KEY_LEN" ] || [ "$KEY_LEN" = "0" ]; then
    bad "no ANTHROPIC_API_KEY baked into this image -- every turn fails"
    note "rebuild: ANTHROPIC_API_KEY=sk-ant-... scripts/build-rootfs.sh, then recreate VMs"
    continue
  fi
  pass "an API key is present ($KEY_LEN chars)"

  # --- DNS, then TLS, then auth, then billing. In that order, because each one
  #     only means anything if the one before it passed.
  DNS="$(inguest "$IP" 'getent hosts api.anthropic.com || echo NORESOLVE' | jq -r '.stdout // ""')"
  if printf '%s' "$DNS" | grep -q NORESOLVE; then
    bad "the guest cannot resolve api.anthropic.com"
    note "resolvconf.service writes /etc/resolv.conf at boot; check it started:"
    note "curl -sX POST http://$IP:8080/debug/exec -d '{\"cmd\":\"systemctl status resolvconf\"}'"
    continue
  fi
  pass "api.anthropic.com resolves in the guest"

  AUTH="$(inguest "$IP" 'set -a; . /etc/cracked-agent.env; set +a
    curl -sS --max-time 20 -o /dev/null -w "%{http_code}" \
      https://api.anthropic.com/v1/models \
      -H "x-api-key: $ANTHROPIC_API_KEY" -H "anthropic-version: 2023-06-01"' \
    | jq -r '.stdout // ""' | tr -d '[:space:]')"
  case "$AUTH" in
    200) pass "the guest reached the API and the key authenticates" ;;
    401|403)
      bad "the API rejected the key ($AUTH) -- it was revoked or rotated"
      note "rebuild the rootfs with a current key, then delete and recreate every VM"
      continue ;;
    000|"")
      bad "the guest could not reach api.anthropic.com at all (TLS/egress)"
      note "DNS worked, so this is the MASQUERADE/FORWARD path or an egress block"
      continue ;;
    *) bad "the API answered $AUTH to a plain models call"; continue ;;
  esac

  [ "$SPEND" = "1" ] || { note "skipping the billable probe (--no-spend)"; continue; }

  # The decisive one. Auth passing proves the key exists; only a completion
  # proves it can still buy one, and "credit exhausted" is otherwise invisible
  # from every surface this system has.
  PROBE="$(inguest "$IP" "set -a; . /etc/cracked-agent.env; set +a
    curl -sS --max-time 30 -w '\n%{http_code}' https://api.anthropic.com/v1/messages \
      -H \"x-api-key: \$ANTHROPIC_API_KEY\" -H 'anthropic-version: 2023-06-01' \
      -H 'content-type: application/json' \
      -d '{\"model\":\"$PROBE_MODEL\",\"max_tokens\":1,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}'" \
    | jq -r '.stdout // ""')"
  CODE="$(printf '%s' "$PROBE" | tail -1 | tr -d '[:space:]')"
  BODY="$(printf '%s' "$PROBE" | sed '$d')"
  case "$CODE" in
    200) pass "a real completion came back -- the model path is healthy" ;;
    400)
      bad "the API refused the request for $PROBE_MODEL"
      note "$(printf '%s' "$BODY" | jq -r '.error.message // .' 2>/dev/null | head -2)"
      note "if it names the model, the profiles point at one that no longer exists" ;;
    429)
      bad "rate limited or out of credit"
      note "$(printf '%s' "$BODY" | jq -r '.error.message // .' 2>/dev/null | head -2)" ;;
    *)
      bad "the completion probe answered $CODE"
      note "$(printf '%s' "$BODY" | jq -r '.error.message // .' 2>/dev/null | head -2)" ;;
  esac

  # --- what the daemon itself last complained about
  ERRS="$(inguest "$IP" 'journalctl -u agentd -n 200 --no-pager 2>/dev/null | grep -iE "error|refused|timeout|WARNING" | tail -5' \
    | jq -r '.stdout // ""')"
  [ -n "$(printf '%s' "$ERRS" | tr -d '[:space:]')" ] && {
    warn "recent agentd complaints:"; printf '%s\n' "$ERRS" | sed 's/^/        /'
  }
done

head_ "verdict"
if [ "$FAIL" -gt 0 ]; then
  echo "  $FAIL failing check(s) above -- the first FAIL is the one to fix."
  exit 1
fi
if [ "$WARN" -gt 0 ]; then
  echo "  nothing is broken, but $WARN thing(s) want a look."
  echo "  an agent in 'waiting' with a pending card is blocked on a person, not stuck."
  exit 0
fi
echo "  the whole chain is healthy: the guest can reach the API, the key works,"
echo "  and a completion came back. If agents still look silent to the app, the"
echo "  fault is above the control plane -- read the chat journal, which logs"
echo "  every /v1 request and response:"
echo "      journalctl -u cracked-chat -n 200 --no-pager"
