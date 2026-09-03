# cracked

Firecracker microVM control plane for a single EC2 host. Boots agent sandboxes
(Ubuntu + Chrome + desktop + Python) on demand, tracks capacity, and proxies
VNC and agent traffic to each VM.

Go 1.27. The host binaries depend only on `golang-jwt/jwt` and `keyfunc` for
verifying Supabase access tokens; everything else is stdlib.

## Layout

| Path | Purpose |
|---|---|
| `cmd/cracked` | entrypoint: sweep, serve, drain |
| `internal/vm` | lifecycle, slot allocator, firecracker config, teardown |
| `internal/fc` | firecracker API client over its unix socket |
| `internal/hostnet` | TAP devices and slot→IP/MAC derivation |
| `internal/api` | HTTP routes, bearer auth, WebSocket-capable proxy |
| `rootfs/` | guest image: Dockerfile, overlay-init, systemd units, agentd, built-in skills |
| `scripts/` | `host-setup.sh` (once per host), `build-rootfs.sh`, `vm-ssh.sh` |
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
sudo scripts/host-setup.sh          # KVM gate, user, firewall, firecracker, kernel, Go
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

## A shell inside a VM

Guests run sshd, reachable **only from the host that runs them**. Each tap is a
point-to-point /30, the firewall has no DNAT or PREROUTING at all, and
guest-to-guest is dropped — so nothing off the box can reach port 22, and no
firewall change was needed to make this work.

`host-setup.sh` generates `~/.ssh/cracked_guest` for whoever runs it and installs
`vm-ssh`. The **public** half is baked into the image at build time:

```sh
SSH_PUBKEY="$(cat ~/.ssh/cracked_guest.pub)" ANTHROPIC_API_KEY=sk-ant-... scripts/build-rootfs.sh
```

Unlike the API key in the same image, this is safe to bake: the private half
never enters a guest, it stays on the host, which is already fully privileged
over every VM. Rotating it does mean rebuilding the rootfs.

```sh
vm-ssh                       # one VM running -> straight in
vm-ssh 1                     # by slot
vm-ssh 0 -- systemctl status ssh
vm-ssh --ip 0                # for scp and rsync
vm-ssh 0 -L 9222:127.0.0.1:9222 -N &   # Chrome DevTools, bound to loopback in the guest
```

You land as `agent`, which has passwordless sudo — SSH in **is** root on that
guest. From a laptop it is one hop through the tunnel you already use:

```sh
ssh -J ubuntu@localhost:2222 -i ~/.ssh/cracked_guest agent@172.16.0.2
```

A VM answers `POST /vms` as soon as *agentd* is up, and sshd binds a few seconds
after that, so `vm-ssh` waits for the port rather than reporting a machine that
is not quite ready as a broken one. That wait is only for a guest actively
refusing; a paused one goes silent instead and still fails fast.

Two things that will look like bugs and are not. Host identity is deliberately
not checked: slots are recycled, so `172.16.0.2` is a different workspace after
a recreate, and a real `known_hosts` would cry man-in-the-middle on every routine
recreate. And a **paused** VM's tap is still listed, so `vm-ssh` will reach it
and time out after five seconds rather than knowing it is frozen.

After a rootfs rebuild, **existing VMs must be deleted and recreated** before
they have sshd — `build-rootfs.sh` only unlinks the old image, and a running VM
holds the deleted inode.

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
                                     # SUPABASE_URL is in the unit file itself

sudo systemctl enable --now cracked-chat
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

## App API

The mobile and web client talks to `cracked-chat` under `/v1`, which is a
separate surface from the `/api/*` routes the built-in chat page uses. Point the
client at `https://chat.usetypeo.com/v1`.

Identity is Supabase. The app signs in against the project directly — email and
password, or Google — and sends the access token it gets back. This service never
sees a password, holds no user store and mints nothing: it verifies.

```sh
TOK=$(curl -s "$SUPABASE_URL/auth/v1/token?grant_type=password" \
        -H "apikey: $SUPABASE_PUBLISHABLE_KEY" -H 'Content-Type: application/json' \
        -d '{"email":"someone@example.com","password":"..."}' | jq -r .access_token)
curl -s https://chat.usetypeo.com/v1/agents -H "Authorization: Bearer $TOK"
```

There is no `/v1/auth/sign-in` or `/v1/auth/sign-out`. Signing out is something
the client does with Supabase; there is nothing here to call.

Tokens are verified against the project's **public** keys, fetched once at
startup from `$SUPABASE_URL/auth/v1/.well-known/jwks.json` and refreshed in the
background. Signature, expiry, issuer and audience are all checked locally, so a
request costs no round trip — and this service holds no secret that could mint a
token. Rotating a signing key in the Supabase dashboard needs no redeploy. Add a
tester by inviting them in Supabase; there is no list in this repo to edit.

The consequence of local verification is that revocation is not visible here: a
token stays good until it expires. That is the standard trade and the reason
access tokens are short-lived.

**A user's machine id is derived from their Supabase user id**, not stored. A
UUID with its hyphens removed is exactly the 32 characters the control plane's
id shape allows, so `3f8a1c92-5e4b-…` owns machine `3f8a1c925e4b…`. That is why
there is no user table: a person who has never been seen before still resolves
to a machine, and `ensureMachine` boots it on first use. Note the ceiling — one
VM per user against `MaxVMs = 5` means the sixth concurrent user gets a capacity
error.

`DELETE /v1/account` erases the caller's machine and everything on it, then
answers 204. It is what the app's "delete everything and sign out" offers. The
Supabase account is untouched — this service can verify tokens and nothing else —
so the person can sign in again and gets a blank machine booted on demand.

```sh
curl -sX DELETE https://chat.usetypeo.com/v1/account -H "Authorization: Bearer $TOK"
```

Deleting purges the workspace, which is the only copy of their agents, threads
and files. It works whether or not the machine is running: the control plane
holds only live VMs in its registry and empties it on restart, so a stopped
machine is the ordinary case, and `DELETE /vms/{id}?purge=true` removes a stopped
machine's workspace rather than answering 404. A purge that cannot unlink the
file is an error, never a silent success — the person was promised something.

Tokens reach `/v1` two ways — `Authorization: Bearer` and `?token=` (for the
event stream, which cannot set headers). The `__Host-sess` cookie is deliberately
*not* one of them: it carries the operator token for the built-in page, and
honouring it here would let whoever runs the service act as any account. Every
`/v1` route answers **401** rather than redirecting, so a client never receives
an HTML login page where it expected JSON.

### The built-in web page

`/chat` and its `/api/*` routes are an operator tool, gated on `CRACKED_TOKEN`
the way the control plane's dashboard is. Open it once as
`https://chat.usetypeo.com/chat?token=<CRACKED_TOKEN>`; the token moves into the
`__Host-sess` cookie and drops out of the address bar. `/logout` clears it.

It has no user login because it has no users. Those `/api/*` handlers take the VM
id from the request body and never check it against the caller — under the old
hardcoded scheme that let any tester drive any other tester's machine. Behind the
fleet token it is just the operator looking at their own fleet.

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

## Skills

A skill is a procedure an agent can follow again: one `SKILL.md` with a name, a
description, and the steps. Only the **description** is loaded into the prompt.
The body sits on disk until a job matches and the agent reads it, which is what
makes many skills nearly free.

They live in two places, and the difference is the whole design:

| Path | Holds | Written by |
|---|---|---|
| `/opt/agent/skills/` | the built-ins | the rootfs image, read-only |
| `<agent>/skills/` | what that agent learned | the agent itself |

Built-ins ship in the **image**, not the overlay and not the binary. That image
is opened read-only by every VM at once and the host page cache holds one copy
for all of them, so a megabyte of skills costs a megabyte per host rather than
per agent. It also means built-ins are always exactly what the image shipped —
no seeding, and no VM stuck on a stale copy. Read-only is load-bearing too:
built-ins are shared, so a writable one would let any agent rewrite what every
other agent is told to do. `Write` and `Edit` refuse them by name, and
`resolveWrite()` in `tools_file.go` is where that split lives — `resolve()` is
the read path, and deliberately allows them.

That is a guard against an honest mistake, not a security boundary. The image
files are root-owned so an ordinary write is refused, but the agent has
passwordless sudo and `/` is an overlayfs with a writable upper, so a
determined `sudo tee` does land — in that one VM's overlay, which is thrown
away with it.

An agent's own skills are private to it and sit on the overlay, so they survive
`DELETE` and die on `?purge=true`, exactly like its memory. An agent's own skill
of the same name overrides a built-in, which is how a wrong one gets fixed on a
machine without a new image.

The format is the [Agent Skills](https://agentskills.io) standard — `name` and
`description` front matter — so a skill written elsewhere drops in unchanged. A
skill with no description is **skipped and logged**, never silently ignored:
without a description nothing would ever trigger it, and that is indistinguishable
from a skill that simply never matched.

### What ships

| Skill | For |
|---|---|
| `skill-creator` | how to write a skill, and when something is worth saving as one |
| `pdf` | reading text and tables, splitting, merging, rendering a page |
| `xlsx` | reading and building spreadsheets, and keeping formulas as formulas |
| `docx` | reading and writing Word, including run-level edits that keep formatting |
| `pptx` | reading and building decks, and checking the layout by rendering it |
| `doc-coauthoring` | writing something long with the person rather than at them |

The four document skills need tooling in the image: LibreOffice (writer, calc
and impress only, not the metapackage), pandoc, poppler-utils, and a Python
venv at `/opt/agent/venv` holding pypdf, pdfplumber, openpyxl, python-docx,
python-pptx, pandas, Pillow, defusedxml and lxml.

A **venv** rather than `pip install`, because Ubuntu 24.04 marks its system
Python externally-managed and `--break-system-packages` would put these in the
path apt itself uses. `agentd.service` puts `/opt/agent/venv/bin` first on
`PATH`, and `bashTool` sets no `cmd.Env`, so every shell command an agent runs
gets that Python. Note `Environment=PATH=` **replaces** rather than prepends, so
the unit restates the whole default chain.

One thing the skills have to say out loud: `pandoc x.md -o x.pdf` **fails** here,
because pandoc writes PDFs through LaTeX and there is no LaTeX in the image. The
route that works is `pandoc x.md -o x.docx` then
`soffice --headless --convert-to pdf`.

### Learning without a deploy

An agent writes its own with `create_skill`, which every profile gets whether or
not it names it. The tool composes the front matter itself, so the model cannot
produce a file the loader will not read.

A prompt is composed once when an agent starts, so a skill written mid-conversation
is not in the index yet. At the end of that turn the agent asks the supervisor to
recycle it: `Recycle` cancels **one goroutine** inside a daemon that keeps running,
and the next message rebuilds the agent, reloading `conversation.json` from disk.
Nothing is deployed, no VM restarts, and the person sees an unchanged transcript
and one slightly slower reply.

`Recycle` refuses unless the agent is idle **and** its inbox is empty. Cancelling
the goroutine abandons that inbox, and a message is logged when it *arrives* — so
a message dropped here would sit in the person's transcript with no reply ever
coming. Refusing costs nothing, because the next ordinary start picks the skill
up anyway.

## Connected apps

An agent can work with the person's own apps — mail, calendar, chat, tasks, CRM,
whatever they use — through an integration provider, rather than by driving a
signed-in page in Chrome. It searches for a tool, and if the app is not
connected yet it hands the person a **Connect** card, waits, and then retries
the call it was already making — so the original request finishes without anyone
repeating it.

Off unless `COMPOSIO_API_KEY` is set on `cracked-chat`. With no key, no session is
minted, no guest-facing port opens, and an agent's tool surface is exactly what it
was before.

### Which apps

Every app the provider carries and can put somebody through, which is on the
order of a thousand. `GET /v1/apps` is the short list the Apps screen leads with
— `featured` in `internal/chat/apps_catalog.go`, the handful most people came
for, and the only apps written down anywhere in this repository.
`GET /v1/apps/catalog` is the rest: headings with counts, a taste of the leading
few, then `?q=`, `?category=` and `?cursor=` over the whole thing.

The catalogue is walked once an hour for the whole fleet and answered from
memory, so browsing costs no round trip per keystroke. Three kinds of app are
dropped from it, because all three end as a Connect button that cannot work:
one with nothing to authorise, one whose credentials a project has to bring
itself, and one being withdrawn.

Connecting is gated on that catalogue rather than on `featured`, and the gate is
load-bearing: minting a link creates an **auth config** for any app nobody has
connected yet, which is project-wide, permanent and counted against the plan.
Minting is bounded per person for the same reason. During a catalogue outage the
gate falls back to `featured`, which cannot create anything new — those six are
already driven end to end and already hold configs.

### Why there is a broker

The provider's session endpoint needs the **project** API key: the session URL on
its own answers `Unauthorized`. That key is authority over every user's connected
accounts, and a guest is a machine its owner has root on — so it cannot live
there. The host keeps it and hands each guest a ticket instead:

```
guest                      host                              provider
agentd ──▶ http://172.16.<4N+1>:8092/apps/<ticket>
                             │  ticket ─▶ which session
                             │  source address must match that guest
                             │  adds x-api-key
                             └──────────────▶ the person's own session
```

A ticket names no user, is pinned to one guest's address, and works only on a
port reachable from a tap. `host-setup.sh` opens exactly that: one TCP port, only
from `tap+`, only to an address on the guest side — and drops it on the uplink, so
a security group edited by somebody else cannot expose it. Tickets are re-minted
on every push and dropped when a machine is created or erased, because slots are
recycled and a route that outlived its machine is how one person's agent would end
up acting as another.

This is the token broker `rootfs/Dockerfile:207` sketches for the Anthropic key,
built for this case first.

### What is stored, and where

| | |
|---|---|
| the connections themselves | the provider's, keyed on the Supabase user id |
| which session belongs to whom | `app_sessions` in the project's Postgres, one row per person |
| the ticket a guest dials | in memory on the host, and `apps.json` on the guest |

The row is a pointer, never a record of truth: losing it costs one API call to
mint a fresh session, never somebody's connections. It is reached through
PostgREST with the **caller's own access token** plus the publishable key, so
row-level security — not a `WHERE` clause somebody remembered to write — is what
isolates one person from another. This service still holds no Supabase secret.

Before enabling the feature, run `deploy/supabase.sql` in the Supabase SQL
editor. It creates the `app_sessions` resource, enables row-level security, and
grants authenticated callers access only through the per-user policy. Apply it
before setting `COMPOSIO_API_KEY`; otherwise session provisioning remains
unavailable.

### Writes ask a person first

**A connected-app action runs unasked only if the provider annotates it
read-only.** Everything else raises a card and blocks until somebody answers:
unknown, newly shipped, renamed, and every action whose name says one thing and
does another. A refusal aborts the whole batch, reads included.

We keep no list of our own. Measured 2026-09-02: 910 of 910 tools across the
featured six carry an effect hint and 398 are `readOnlyHint`, and it is right
about the names that lie — `GMAIL_SEND_DRAFT` is destructive,
`GOOGLECALENDAR_CALENDAR_LIST_INSERT` creates despite the `LIST`,
`SLACK_FIND_CHANNELS` genuinely reads. Fetched on the host, cached an hour per
app, and pushed to each machine beside its session, so a tool shipped today is
understood without rebuilding a rootfs. Absent from it means ask, so an empty
set asks about everything: noisy, never permissive. One entry is ours —
`GMAIL_CREATE_PROMPT_POST` is tagged read-only and posts text to a third party,
and MCP's own rule is that annotations are untrusted hints.

The walk over one app's tools is paged. It used to refuse anything at or over a
single page, on the reasoning that a short answer is worse than none — true
while the six apps on offer topped out at 305 tools, and false now that anybody
can connect anything: refusing leaves the app absent from every pushed answer,
so its actions ask forever, the ones somebody switched off included.

Resolved over the apps a person has **connected**, in any status, plus the
featured few as a floor. An action in an app nobody has connected cannot run
whatever we say about it, and a thousand apps' worth would not fit; the floor is
there because an agent can mint its own connect link without this service ever
hearing about it, and a freshly connected app should not raise a card on every
read until the next push. Only the
`auto` and `never` answers are sent, because the guest already reads an unknown
slug as ask — the same policy in half the bytes, which is what keeps the push
under the 256 KiB the guest will accept (`appsBodyCap`, and exceeding it takes
the session down with the set rather than just the set). Both the fan-out and
the push are bounded well above anything real.

An app connected *after* a machine was pushed would otherwise raise a card on
every read until that machine came due, up to an hour later. Each machine's
claim remembers which apps its answer was about, so the next time this service
reads that person's connections for any reason — opening the Apps screen is the
usual one — it notices and pushes again. An agent's own Connect card never
touches this service and the page somebody lands on afterwards is deliberately
anonymous, so there is nowhere earlier to catch it.

**What this does not do.** It is not an exfiltration control. A guest has
unrestricted outbound internet by design (see the firewall notes above) and
`Bash` is gated only against its destructive denylist, so `curl` reaches anywhere
without touching this. Nor does it inspect arguments: an approved send with a
poisoned body is an approved send. What it buys is that loud, irreversible
actions stop and ask, and that every one is on the record.

## Operational notes

- **Never `poweroff` inside a guest.** On x86 that stops the guest but leaves
  the firecracker process alive forever. Use `reboot` (the control plane's
  `SendCtrlAltDel` does this correctly).
- A control-plane restart **kills running VMs** but preserves every workspace.
  The startup sweep guarantees no orphan processes and no stray taps.
- Guests are firewalled off from IMDS (`169.254.0.0/16`), the VPC, each other,
  and the host — with **one exception**: TCP 8092 on their own tap address, the
  connected-apps broker, and only when it is running. Verify after any firewall
  change, from inside a guest:
  `curl -m 3 http://169.254.169.254/latest/meta-data/iam/` must time out, and so
  must `curl -m 3 http://172.16.0.1:8080/healthz` — the control plane is still
  unreachable. `curl -m 3 http://<gateway>:8092/apps/anything` should answer
  **404**, which is the broker refusing a ticket it did not issue rather than
  nothing listening.
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

`make build` cross-compiles, so a laptop can build and ship only the binary.
The host builds too: `host-setup.sh` installs the Go release `go.mod` asks for
under `/usr/local/go` and links it into `/usr/local/bin`, ahead of Ubuntu's
packaged Go. Unpacking alone is not enough: an unlinked tree loses to
`/usr/bin/go` on PATH, so `go version` reports the old release while the right
toolchain sits unused on disk. Diagnose that with `which -a go`.
