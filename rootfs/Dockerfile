FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv udev dbus dbus-x11 ca-certificates curl wget gnupg sudo \
      xvfb x11vnc novnc websockify openbox x11-utils x11-xserver-utils xdg-utils \
      scrot \
      fonts-liberation fonts-dejavu-core fonts-noto-cjk fonts-noto-color-emoji \
      libnss3 libxss1 libasound2t64 \
      python3 python3-venv python3-pip e2fsprogs iproute2 iputils-ping procps \
 && rm -rf /var/lib/apt/lists/*

# Chrome stable; apt resolves the .deb's own dependencies.
RUN wget -q -O /tmp/chrome.deb \
      https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb \
 && apt-get update && apt-get install -y /tmp/chrome.deb \
 && rm -f /tmp/chrome.deb && rm -rf /var/lib/apt/lists/*

# Node 26. Ubuntu 24.04's apt `nodejs` is 18 and its engines field rules it out.
# The only thing in this image that needs Node is chrome-devtools-mcp, which asks
# for `^20.19.0 || ^22.12.0 || >=23`; the old pin to 22 was the Agent SDK's, and
# the Agent SDK is gone. 26 is Current until October 2026 and becomes LTS then,
# so taking the LTS of today (24) would only mean bumping again in six weeks.
RUN curl -fsSL https://deb.nodesource.com/setup_26.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && node --version

# ubuntu:24.04 already owns uid 1000, so useradd fails without removing it first.
#
# The agent gets passwordless root. It was told it has a computer and is asked to
# install what it needs, and without this it cannot: apt, pip and systemctl all
# refuse, and the failure reads to the model as the task being impossible rather
# than as a missing privilege. The blast radius is one guest -- a microVM already
# firewalled off from IMDS, the VPC, the host and every other VM, whose whole disk
# is a per-user overlay that `DELETE ?purge=true` throws away.
#
# uploads/ is where files sent from the app land, inside the shared workspace so
# every agent can reach one the person sent to any of them.
RUN userdel -r ubuntu 2>/dev/null || true \
 && useradd -m -u 1000 -s /bin/bash agent \
 && usermod -aG sudo agent \
 && printf 'agent ALL=(ALL) NOPASSWD:ALL\n' > /etc/sudoers.d/agent \
 && chmod 0440 /etc/sudoers.d/agent \
 && visudo -cf /etc/sudoers.d/agent \
 && mkdir -p /home/agent/Downloads /home/agent/workspace/uploads /home/agent/.config \
 && chown -R agent:agent /home/agent

# sshd, so the host can open a real terminal on a guest rather than driving it
# one 30-second command at a time through /debug/exec. Reachable ONLY from the
# host that runs the VM -- each tap is a point-to-point /30 and the firewall has
# no DNAT at all -- so this opens nothing to the internet.
#
# Its own layer, deliberately: appending to the apt line at the top would
# invalidate Chrome, Node, the npm ci and the agentd copy on every iteration.
# /run/sshd is created at runtime by the unit's RuntimeDirectory=, so without
# the mkdir the `sshd -t` guards below fail with "Missing privilege separation
# directory" for a reason that has nothing to do with the config.
RUN apt-get update && apt-get install -y --no-install-recommends openssh-server \
 && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /run/sshd

# overlay-init mounts /dev/vdb on /overlay and pivots with the old root at /rom.
# Both must EXIST in the image: mount(8) does not create its mountpoint, and the
# root is read-only at that stage so the script cannot mkdir them itself. Without
# these the overlay silently fails and the VM boots read-only but "healthy".
RUN mkdir -p /overlay /rom

COPY files/overlay-init /sbin/overlay-init
COPY files/resolvconf.service files/xvfb.service files/x11vnc.service \
     files/novnc.service files/openbox.service files/chrome.service \
     files/agentd.service /etc/systemd/system/
COPY files/sshd_config.d/10-cracked.conf /etc/ssh/sshd_config.d/
COPY files/ssh.service.d/10-cracked.conf /etc/systemd/system/ssh.service.d/
RUN chmod 0755 /sbin/overlay-init

# The one Node package left in the image. agentd drives Chrome through it as a
# stdio subprocess; nothing else here runs Node at all.
#
# chrome-devtools-mcp runs on puppeteer-core, which downloads no browser, so the
# image keeps exactly one Chrome (the .deb above). This env var is the guard: if
# a future version switches to full puppeteer it fails here instead of silently
# adding a ~170MB second browser.
ENV PUPPETEER_SKIP_DOWNLOAD=true
COPY files/mcp/package.json files/mcp/package-lock.json /opt/agent/
# The test -f is the guard that matters now: agentd spawns this entrypoint by
# absolute path, so a missing or moved file would give every VM a researcher
# with no browser tools and no complaint from anywhere. It is checked here, in
# seconds, rather than after the multi-GB ext4 pipeline in build-rootfs.sh.
RUN cd /opt/agent \
 && npm ci --omit=dev --no-audit --no-fund \
 && test -f /opt/agent/node_modules/chrome-devtools-mcp/build/src/bin/chrome-devtools-mcp.js \
 && npm cache clean --force \
 && chown -R agent:agent /opt/agent

# Document tooling, for the pdf/xlsx/docx/pptx skills below. This is the single
# most expensive layer in the image, so it sits after everything cheap.
#
# LibreOffice as three component packages and NOT the `libreoffice` metapackage,
# which drags in the whole desktop suite and a JRE for no gain here. Only two
# things need it: converting a legacy .doc/.ppt/.xls that Python cannot open,
# and rendering a document to PDF so it can be looked at. pandoc handles
# markdown in and out; poppler-utils is for pdftoppm, which turns a PDF page
# into an image the agent can actually see.
RUN apt-get update && apt-get install -y --no-install-recommends \
      libreoffice-writer libreoffice-calc libreoffice-impress \
      pandoc poppler-utils \
 && rm -rf /var/lib/apt/lists/* \
 && soffice --version \
 && pandoc --version | head -1 \
 && pdftoppm -v 2>&1 | head -1

# A venv, not `pip install` into the system Python. Ubuntu 24.04 marks its
# python3 externally-managed (PEP 668), so a plain pip refuses, and
# --break-system-packages would put these libraries in the path apt itself
# reaches for. The venv also means the agent's own later `pip install` lands
# somewhere sane without sudo.
#
# The final import line is the guard, and it is the point of the layer: a
# missing library otherwise boots a VM whose skills confidently instruct
# something that fails, and which passes every health check while doing it.
# Import names differ from package names -- python-docx is `docx`, python-pptx
# is `pptx`, Pillow is `PIL` -- so this checks what the skills actually write.
RUN python3 -m venv /opt/agent/venv \
 && /opt/agent/venv/bin/pip install --no-cache-dir --upgrade pip \
 && /opt/agent/venv/bin/pip install --no-cache-dir \
      pypdf pdfplumber openpyxl python-docx python-pptx pandas Pillow defusedxml lxml \
 && /opt/agent/venv/bin/python -c \
      "import pypdf, pdfplumber, openpyxl, docx, pptx, pandas, PIL, defusedxml, lxml" \
 && chown -R agent:agent /opt/agent/venv

# Built-in skills: procedures every agent can follow, listed in its prompt by
# name and description and read off disk only when a job matches.
#
# They ship HERE rather than embedded in agentd or seeded onto the overlay,
# because this image is opened read-only by every VM at once and the host page
# cache holds one copy for all of them. Seeding instead would pay for the same
# files per agent, per start, and leave existing VMs on a stale copy forever.
# Keeping them out of an agent's own writable tree is also what stops one agent
# rewriting what every other agent is told to do; an agent's own skills live
# beside its memory instead.
#
# Note this lands AFTER the chown above, so these stay root-owned: an ordinary
# write by the agent user is refused by the kernel whatever the daemon's path
# checks do. It is NOT a security boundary -- the agent has passwordless sudo
# and / is an overlayfs with a writable upper, so `sudo tee` here succeeds and
# shadows the image file (verified on a live guest). Like the destructive
# denylist, this stops an honest mistake, not a determined one; the containment
# is that the overlay belongs to one disposable VM.
#
# After the npm layer on purpose: editing a skill must not reinstall node_modules.
COPY files/skills /opt/agent/skills
# Named rather than counted. A count says only that the number changed, which is
# what it said the first time this ran and the number was wrong; naming each one
# says WHICH is missing, and it does not need editing when a skill is added.
RUN for s in skill-creator pdf xlsx docx pptx doc-coauthoring; do \
      test -f "/opt/agent/skills/$s/SKILL.md" || { echo "missing built-in skill: $s"; exit 1; }; \
    done

# agentd is compiled on the host by scripts/build-rootfs.sh, because its Go
# source lives OUTSIDE this build context. Running it here is the guard: it
# proves the file is present, executable, the right architecture and not
# truncated. Same class of check as the test -f above, and it exists for the same
# reason -- a silently missing artifact boots a VM that passes every health check
# and does nothing. `-h` exits 0, so this needs no special flag.
COPY files/agentd /usr/local/bin/agentd
RUN chmod 0755 /usr/local/bin/agentd \
 && /usr/local/bin/agentd -h

# Masking the network managers is load-bearing: the guest IP comes from the
# kernel ip= boot arg and any of them would clobber it. ctrl-alt-del.target is
# deliberately left alone -- masking it makes SendCtrlAltDel a silent no-op.
RUN systemctl set-default multi-user.target \
 && systemctl enable resolvconf.service xvfb.service x11vnc.service \
      novnc.service openbox.service chrome.service agentd.service \
 && systemctl mask systemd-networkd.service systemd-networkd-wait-online.service \
      systemd-resolved.service NetworkManager.service snapd.service \
      apparmor.service systemd-timesyncd.service systemd-random-seed.service \
      e2scrub_reap.service

# Which ssh unit 24.04's postinst enables is not safe to assume -- it ships
# socket activation -- so choose one and then ASSERT the choice. Everything
# below is a hypothesis that fails here in seconds rather than on a booted VM
# that passes every health check and refuses every connection.
#
# `sshd -T` reports the EFFECTIVE config, so it catches a missing Include, a
# mis-sorted drop-in filename and a typo'd keyword in one check. It must run
# BEFORE the rm: sshd refuses to parse anything once there are no host keys.
#
# The host keys go for the same reason /etc/machine-id is truncated below --
# every VM would otherwise share one identity. ssh.service.d/10-cracked.conf
# mints a set per VM on first boot.
RUN systemctl disable ssh.socket \
 && systemctl enable ssh.service \
 && test -L /etc/systemd/system/multi-user.target.wants/ssh.service \
 && test ! -e /etc/systemd/system/sockets.target.wants/ssh.socket \
 && test ! -e /etc/ssh/sshd_not_to_be_run \
 && [ "$(grep -c '^ExecStartPre=' /usr/lib/systemd/system/ssh.service)" = 1 ] \
 && sshd -T | grep -q '^passwordauthentication no$' \
 && sshd -T | grep -q '^allowusers agent$' \
 && sshd -T | grep -q '^usedns no$' \
 && sshd -T | grep -q '^x11forwarding no$' \
 && rm -f /etc/ssh/ssh_host_* \
 && test ! -e /etc/ssh/ssh_host_ed25519_key

# TODO(security): the API key lives in this image, so it is shared by every VM
# built from it and readable by anything in the guest. The agent browses
# untrusted pages with it present, so a prompt injection can exfiltrate it, and
# rotating it means rebuilding the rootfs. Before any public release, replace
# this with a host token-broker: the agent calls a proxy on its own gateway IP
# (slot N is 172.16.(4N+1), so derive it from the default route) and the host
# injects the real key, keeping the guest free of credentials.
ARG ANTHROPIC_API_KEY=""
RUN printf 'ANTHROPIC_API_KEY=%s\n' "$ANTHROPIC_API_KEY" > /etc/cracked-agent.env \
 && chmod 0640 /etc/cracked-agent.env \
 && chown root:agent /etc/cracked-agent.env

# The operator's public key for reaching a guest over ssh. Unlike the API key
# above this is safe to bake: the PRIVATE half never enters a guest, it stays on
# the EC2 host, which is already fully privileged over every VM through the
# control plane. `docker history` exposing this build arg exposes a public key.
#
# Warn rather than fail when it is empty: build-rootfs.sh is meant to run on a
# builder box, where the host's key file does not exist, and a hard guard here
# would break every off-host build. `ssh-keygen -l` beats `test -s` -- it
# rejects a truncated or garbage value AND prints the fingerprint into the build
# log, which is what you compare against on the host afterwards.
ARG SSH_PUBKEY=""
RUN if [ -n "$SSH_PUBKEY" ]; then \
      install -d -m 0700 -o agent -g agent /home/agent/.ssh \
   && printf '%s\n' "$SSH_PUBKEY" > /home/agent/.ssh/authorized_keys \
   && chmod 0600 /home/agent/.ssh/authorized_keys \
   && chown agent:agent /home/agent/.ssh/authorized_keys \
   && ssh-keygen -l -f /home/agent/.ssh/authorized_keys ; \
    else \
      echo "WARN: SSH_PUBKEY is unset; no one will be able to ssh into a guest." ; \
    fi

# Docker bakes a machine-id into the image; every VM would otherwise share one
# identity. Emptying it makes systemd mint a unique id on first boot, which
# lands in that VM's overlay upper and persists per-VM.
RUN truncate -s 0 /etc/machine-id
