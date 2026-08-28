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
| `rootfs/` | guest image: Dockerfile, overlay-init, systemd units, agentd |
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

Port 8080 is deliberately **not** in the security group — the control plane is
guarded only by a bearer token, so reach it through an SSH local forward rather
than exposing it:

```sh
# The security group allows SSH from one /32, which goes stale whenever your
# address rotates. The Instance Connect Endpoint needs no SG change.
aws ec2-instance-connect open-tunnel --region ap-south-1 \
    --instance-id <instance-id> --local-port 2222 &

ssh -i ~/.ssh/<key>.pem -p 2222 -L 8080:localhost:8080 ubuntu@localhost

# read the token on the host, then open the URL below
sudo systemctl show cracked -p Environment --value | tr ' ' '\n' | grep ^CRACKED_TOKEN=
```

```
http://localhost:8080/dashboard?token=<T>
```

A single page, no build step. The token is taken out of the URL, kept in
`sessionStorage`, and sent as a header — it deliberately never becomes a
cookie, because a cookie here would be scoped to `/` and the untrusted guest
serves same-origin content under `/vms/{id}/agent/`.

The fleet table shows, per VM: uptime, state, CPU, resident memory, disk, agent
state, turns, tokens and cost, with pause/resume/stop/purge and links to VNC.
Click a VM to peek inside: firecracker's own view, the serial console tail, a
shell into the guest, **each agent's own event log**, and **the service
journals**.

Two panels are worth knowing about:

- **agent activity** has one button per agent. It is per agent, not per VM: a
  specialist doing the work has its own transcript, and the boss's log only
  records that it handed the job over. Reading one starts nothing — the guest
  serves the snapshot off disk.
- **service log** tails `cracked-chat`, `cracked` or `caddy`. The chat journal
  is where every `/v1` request and response lands, so it is the log to read when
  a client reports something the fleet table cannot explain. This needs the
  service user in the `systemd-journal` group, which `host-setup.sh` does; without
  it the panel shows a permissions notice rather than lines.

Three columns are easy to misread:

- **cpu** is percent of *one* core, like `top`, so a fully busy 2-vCPU VM
  reads ~200%.
- **rss** is the firecracker process's resident memory, which tracks the guest
  pages actually touched. It grows toward the 4096 MiB assigned and does not
  shrink when the guest frees memory.
- **disk** is blocks actually allocated to the sparse workspace image, not its
  5 GiB cap. This is the real cost on the host.

Cost and token counts are lifetime totals **for the workspace**, not for this
boot: the guest keeps its running total on the overlay, which survives `DELETE`
without `?purge=true`. The host reads it from the daemon's `GET /usage` and
applies the price table, so nothing is counted twice and rates can change
without rebuilding a guest image. `?purge=true` resets both.

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
sudo systemctl edit cracked-chat     # Environment=CRACKED_TOKEN=<token>
sudo systemctl enable --now cracked-chat
```

### Task feedback

When an agent closes a task the app offers to rate it, and `POST /v1/feedback`
appends the rating to a Google Sheet. The row is stamped server-side with the
signed-in email and the time, so a client cannot file feedback as someone else,
and nothing about it reaches the guest — the agent being rated must not be able
to read the rating.

Deploy `deploy/feedback-sheet.gs` as a web app bound to the sheet (the file's
header has the steps, including the "Who has access: Anyone" setting that a
Workspace account gets wrong by default), then:

```sh
sudo systemctl edit cracked-chat     # Environment=FEEDBACK_WEBHOOK_URL=<exec url>
```

The `/exec` URL is a bearer credential: anyone holding it can append rows, so it
belongs in the drop-in and never in the checked-in unit. Leaving it unset turns
the endpoint off — it answers 503 rather than quietly dropping what someone
took the trouble to type.

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

## App API

The mobile and web client talks to `cracked-chat` under `/v1`, which is a
separate surface from the `/api/*` routes the built-in chat page uses. Point the
client at `https://chat.usetypeo.com/v1`.

Logins are a hardcoded list in `internal/chat/users.go`. Add a tester by adding
a line and redeploying.

```sh
curl -sX POST https://chat.usetypeo.com/v1/auth/sign-in \
     -H 'Content-Type: application/json' \
     -d '{"email":"someone@example.com","password":"..."}'   # -> {userId,email,token}
curl -s https://chat.usetypeo.com/v1/agents -H "Authorization: Bearer $TOK"
curl -sX POST https://chat.usetypeo.com/v1/auth/sign-out -H "Authorization: Bearer $TOK"
```

Auth is deliberately trivial: a hardcoded list of email, password and token
compared with `==`, and one hardcoded machine that every login shares. There is
no user store, no hashing, no expiry and nothing to revoke — signing out just
drops the client's copy of a constant. All of it is scaffolding for a handful of
testers and is deleted when a real identity provider lands.

Sessions are reachable three ways — `Authorization: Bearer`, `?token=` (for the
event stream, which cannot set headers) and the `__Host-sess` cookie (for the
built-in page). Every `/v1` route answers **401** rather than redirecting, so a
client never receives an HTML login page where it expected JSON.

## Storage model

- `images/rootfs.ext4` — one immutable image, opened **read-only by every VM at
  once**. The host page cache holds a single copy for all of them.
- `workspaces/{id}.ext4` — 5 GiB per VM, mounted as the **overlayfs upper
  layer**. Every guest write lands here: Chrome profile, Downloads,
  credentials, agent workspace. Survives `DELETE` unless `?purge=true`.
- `run/{id}/` — ephemeral; wiped by the startup sweep.

Delete a VM and recreate it with the same id to get its data back.

## Memory

Every agent gets its own state directory on the overlay, so all of it survives
`DELETE` and all of it dies on `?purge=true`. `agentd.service` passes
`-state-dir /home/agent/agent-state/agentd`, and each agent lives under
`agents/<id>/` inside it:

| Path (relative to the agent's own directory) | Holds |
|---|---|
| `memory/` | durable facts, as Markdown concept files (OKF v0.1) |
| `instructions.md` | standing role and behaviour, spliced into the system prompt |
| `events.jsonl` | that agent's event log |
| `conversation.json` | the wire-format history it resumes from |

The tree is seeded from templates embedded in the binary (`internal/agentd/memtree`),
written with `O_EXCL` so the check and the write are one syscall: seeding runs on
every start and an agent's own edits always survive it.

`memory/index.md` and `memory/system/definition.md` are inlined into the system
prompt, each capped at 16 kB independently so one runaway file cannot crowd out
the other. Everything deeper the agent reads on demand by following links from
the index — the prompt carries a header naming the real paths so it knows where
to look. A tree that cannot be read at all injects nothing rather than a block
announcing its own absence, which would spend tokens saying only that there is
no memory.

Because the whole prompt is composed once when an agent starts, memory is
refreshed by eviction rather than mid-session: an agent evicted under the live
ceiling and later re-addressed comes back with a fresh view of its own files.
That also keeps the cached prefix stable, which is what makes context editing
free (see `internal/agentd/loop.go`).

The full prompt is `BaseIdentity` + browser guidance (browser profiles only) +
the profile's role + memory + `instructions.md` + `BaseLimits`. The limits go
last on purpose: `instructions.md` is agent-writable, so the safety rules have
to be the final word.

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
