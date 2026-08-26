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

# overlay-init mounts /dev/vdb on /overlay and pivots with the old root at /rom.
# Both must EXIST in the image: mount(8) does not create its mountpoint, and the
# root is read-only at that stage so the script cannot mkdir them itself. Without
# these the overlay silently fails and the VM boots read-only but "healthy".
RUN mkdir -p /overlay /rom

COPY files/overlay-init /sbin/overlay-init
COPY files/resolvconf.service files/xvfb.service files/x11vnc.service \
     files/novnc.service files/openbox.service files/chrome.service \
     files/agentd.service /etc/systemd/system/
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

# Docker bakes a machine-id into the image; every VM would otherwise share one
# identity. Emptying it makes systemd mint a unique id on first boot, which
# lands in that VM's overlay upper and persists per-VM.
RUN truncate -s 0 /etc/machine-id
