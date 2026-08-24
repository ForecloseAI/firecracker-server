# cracked

Firecracker microVM control plane for a single EC2 host. Boots agent sandboxes
(Ubuntu + Chrome + desktop + Python) on demand, tracks capacity, and proxies
VNC and agent traffic to each VM.

Go 1.27, stdlib only, zero dependencies.

## Layout

| Path | Purpose |
|---|---|
| `cmd/cracked` | entrypoint: sweep, serve, drain |
| `internal/vm` | lifecycle, slot allocator, firecracker config, teardown |
| `internal/fc` | firecracker API client over its unix socket |
| `internal/hostnet` | TAP devices and slot→IP/MAC derivation |
| `internal/api` | HTTP routes, bearer auth, WebSocket-capable proxy |
| `rootfs/` | guest image: Dockerfile, overlay-init, systemd units, agent |
| `scripts/` | `host-setup.sh` (once per host), `build-rootfs.sh` |
| `deploy/` | systemd unit for the control plane |

## Setup

The EC2 instance **must** have nested virtualization enabled — it is opt-in and
off by default:

```sh
aws ec2 run-instances --instance-type m7i.2xlarge \
    --cpu-options "NestedVirtualization=enabled" ...
# or, on an existing stopped instance:
aws ec2 modify-instance-cpu-options --instance-id <id> \
    --core-count 4 --threads-per-core 2 --nested-virtualization enabled
```

Then, on the host:

```sh
sudo scripts/host-setup.sh          # KVM gate, user, firewall, firecracker, kernel
scripts/build-rootfs.sh             # Docker -> shared read-only rootfs.ext4
make install                        # build and install /usr/local/bin/cracked
sudo install -m0644 deploy/cracked.service /etc/systemd/system/
sudo systemctl edit cracked         # add Environment=CRACKED_TOKEN=<secret>
sudo systemctl enable --now cracked
```

## API

All routes except `GET /healthz` need the token, via `Authorization: Bearer`,
`?token=`, or the `cracked_token` cookie.

```sh
export H=http://localhost:8080 T=<token>
A() { curl -sS -H "Authorization: Bearer $T" "$@"; }

A -X POST $H/vms -d '{"id":"alice"}'      # 201, blocks until the agent answers
A $H/vms                                   # list
A $H/vms/alice                             # one VM, state read live off firecracker
A -X POST $H/vms/alice/pause               # freeze
A -X POST $H/vms/alice/resume              # unfreeze
A -X DELETE "$H/vms/alice"                 # stop, KEEP the workspace
A -X DELETE "$H/vms/alice?purge=true"      # stop and delete the workspace
A $H/capacity                              # slot/vcpu/memory accounting
A $H/stats                                 # what the host is ACTUALLY using
A $H/vms/alice/stats                       # one VM in depth
A $H/metrics                               # the same numbers, for Prometheus
```

Capacity exhaustion returns **503** with `Retry-After: 30` and
`{"error":"capacity_exhausted","resource":"slots"}`.

VNC (open in a browser):

```
http://<host>:8080/vms/alice/vnc/vnc.html?path=vms/alice/vnc/websockify&token=<T>
```

The agent and the human share display `:0`, so takeover is instant.

## Dashboard

```
http://<host>:8080/dashboard?token=<T>
```

A single page, no build step. The token is taken out of the URL, kept in
`sessionStorage`, and sent as a header — it deliberately never becomes a
cookie, because a cookie here would be scoped to `/` and the untrusted guest
serves same-origin content under `/vms/{id}/agent/`.

The fleet table shows, per VM: uptime, state, CPU, resident memory, disk, agent
state, turns, tokens and cost, with pause/resume/stop/purge and links to VNC.
Click a VM to peek inside: firecracker's own view, the serial console tail, the
agent's recent activity, and a shell into the guest.

Three columns are easy to misread:

- **cpu** is percent of *one* core, like `top`, so a fully busy 2-vCPU VM
  reads ~200%.
- **rss** is the firecracker process's resident memory, which tracks the guest
  pages actually touched. It grows toward the 4096 MiB assigned and does not
  shrink when the guest frees memory.
- **disk** is blocks actually allocated to the sparse workspace image, not its
  5 GiB cap. This is the real cost on the host.

Cost and token counts are lifetime totals **for the workspace**, not for this
boot: the guest's event log lives on the overlay and survives `DELETE` without
`?purge=true`. The host aggregates it by polling
`/session/events?poll=1&since=N` incrementally, so nothing is counted twice.
`?purge=true` resets both.

Destructive buttons need two clicks rather than opening a native dialog, so the
page stays drivable by browser automation.

## Metrics

`GET /metrics` renders the same snapshot in Prometheus text format, behind the
same token:

```yaml
scrape_configs:
  - job_name: cracked
    scrape_interval: 30s
    authorization: { credentials: <CRACKED_TOKEN> }
    static_configs: [{ targets: ["<host>:8080"] }]
```

Every VM series is labelled `vm="<id>"`. CPU is exported as
`cracked_vm_cpu_seconds_total`, a counter, so utilisation is
`rate(cracked_vm_cpu_seconds_total[5m]) * 100`. Keep the interval at 15s or
slower: a scrape fans out to every guest, though a 2s per-guest timeout bounds
the worst case.

## Chat UI

Talk to an agent from a browser at `https://chat.usetypeo.com/chat?id=<vm>`.
Text only. Any running VM is reachable by id; an unknown id shows the list of
VMs that are running instead of spinning.

```sh
make install-chat
sudo install -m0644 deploy/cracked-chat.service /etc/systemd/system/
sudo install -m0644 deploy/Caddyfile /etc/caddy/Caddyfile && sudo systemctl reload caddy
make hashpw USER_NAME=alice | sudo tee -a /etc/cracked/chat-users
sudo systemctl edit cracked-chat     # Environment=CRACKED_TOKEN=<token>
sudo systemctl enable --now cracked-chat
sudo systemctl reload cracked-chat   # after adding a user; does not drop streams
```

Three origins, isolated by **hostname**:

| Origin | Serves | Holds |
|---|---|---|
| `chat.usetypeo.com` | the page and `/api/*` | the `__Host-sess` cookie |
| `vnc.usetypeo.com` | proxied noVNC, i.e. untrusted guest HTML | a 15-minute per-VM capability |
| `:8080` | the control plane | not exposed publicly |

The `__Host-` prefix requires an empty `Domain`, which is what structurally
stops the chat session from reaching the VNC origin. **Never set a cookie with
`Domain=.usetypeo.com` anywhere in this system.**

The browser never opens a socket to a VM; the chat service reaches guests
server-side. When a site needs a login, the agent asks via `ask_human` with
kind `handoff` and you take over the VM's own screen, so the credential never
touches this service.

## Storage model

- `images/rootfs.ext4` — one immutable image, opened **read-only by every VM at
  once**. The host page cache holds a single copy for all of them.
- `workspaces/{id}.ext4` — 5 GiB per VM, mounted as the **overlayfs upper
  layer**. Every guest write lands here: Chrome profile, Downloads,
  credentials, agent workspace. Survives `DELETE` unless `?purge=true`.
- `run/{id}/` — ephemeral; wiped by the startup sweep.

Delete a VM and recreate it with the same id to get its data back.

## Memory

Each agent keeps three separate things on the overlay, so all of them survive
`DELETE` and all of them die on `?purge=true`:

| Path | Holds |
|---|---|
| `~/agent-state/memory/` | durable facts, as Markdown concept files (OKF v0.1) |
| `~/agent-state/instructions.md` | standing role and behaviour, spliced into the system prompt |
| `~/agent-state/events.jsonl` | the event log |

`memory/index.md` and `memory/system/definition.md` are injected into the model's
context whenever a context window is created — at startup, and in principle after
compaction — each capped at 16k characters.

**Caveat on compaction, found while testing this:** `enableCompaction()` in
`session.ts` appears not to actually drive compaction on SDK 0.3.238. Across
`autoCompactWindow` values from 6,000 to 990,000 and ~46k of accumulated context,
compaction never fired and the SDK emitted no `autocompact_state` message, even
though `applyFlagSettings` accepted the call. That is pre-existing behaviour, not
caused by memory, but it means mid-session refresh is unverified — memory is
injected once at session start, and thereafter the agent reads the files on
demand. `test/compaction-canary.mjs` reproduces it. Everything deeper the agent reads on demand by
following links from the index. The doctrine in `definition.md` is deliberately
agent-editable: it is how the agent decides what is worth keeping.

Delivery is a `SessionStart` hook passed **inline** to `query()` as `settings`
(the flag tier), so the registration is compiled into the image rather than
sitting in a file the agent can rewrite. `buildOptions` pairs it with
`settingSources: []`.

That empty array is load-bearing, and not for the reason it looks. **Omitting
`settingSources` does not mean "load nothing" — it means load user *and* project
*and* local**, verified against the SDK's own `resolveSettings`. Project and
local resolve under `/home/agent/workspace`, which the agent writes with
auto-approved `Write`/`Edit`, so the default would let a prompt injection off a
web page register a `PreToolUse` command that runs on every later tool call,
persists on the overlay, and never shows up in `events.jsonl`. **Never put
`"project"` or `"local"` in that array.**

`src/memory/settings.ts` still holds a settings-file installer, kept as a tested
fallback; `install()` calls it only to clear entries an earlier build may have
written.

`test/canary.mjs` proves the whole path against the real API for a few cents,
without a rootfs build: `npm run build && node test/canary.mjs`.

The system prompt is composed as `BASE_IDENTITY` + `instructions.md` +
`BASE_LIMITS`. The limits go last on purpose: `instructions.md` is agent-writable,
so the safety rules have to be the final word.

Env overrides, all optional: `CRACKED_MEMORY=0` disables the subsystem and
removes its hook, `CRACKED_MEMORY_DIR`, `CRACKED_INSTRUCTIONS_FILE`,
`CRACKED_MEMORY_FILE_BUDGET`, `CRACKED_COMPACT_WINDOW`.

Did the hook actually fire? `cat ~/agent-state/memory/.last-hook` in the guest,
or look for a `memory` event in the log.

## Operational notes

- **Never `poweroff` inside a guest.** On x86 that stops the guest but leaves
  the firecracker process alive forever. Use `reboot` (the control plane's
  `SendCtrlAltDel` does this correctly).
- A control-plane restart **kills running VMs** but preserves every workspace.
  The startup sweep guarantees no orphan processes and no stray taps.
- Guests are firewalled off from IMDS (`169.254.0.0/16`), the VPC, each other,
  and the host itself. Verify after any firewall change:
  `curl -m 3 http://169.254.169.254/latest/meta-data/iam/` from inside a guest
  must time out.
- `MaxVMs` is 5 in `internal/vm/vm.go`. Raise to 6 only after confirming host
  memory headroom at steady state — swap is off, so overcommit means the OOM
  killer reaps a live VM.

## Development

Requires the Go 1.27 toolchain (`go.mod` pins it). Ubuntu's apt `golang` is
older than this — install from https://go.dev/dl/ on whatever box you build on.

```sh
make test    # unit tests
make vet
make build   # cross-compiles to bin/cracked for linux/amd64
```

`make build` cross-compiles, so you can build on a laptop and ship only the
binary to the host; the EC2 box needs no Go toolchain of its own.
