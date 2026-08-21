FROM ubuntu:24.04
ENV DEBIAN_FRONTEND=noninteractive

RUN apt-get update && apt-get install -y --no-install-recommends \
      systemd systemd-sysv udev dbus dbus-x11 ca-certificates curl wget gnupg \
      xvfb x11vnc novnc websockify openbox x11-utils x11-xserver-utils xdg-utils \
      fonts-liberation fonts-dejavu-core libnss3 libxss1 libasound2t64 \
      python3 python3-venv python3-pip e2fsprogs iproute2 iputils-ping \
 && rm -rf /var/lib/apt/lists/*

# Chrome stable; apt resolves the .deb's own dependencies.
RUN wget -q -O /tmp/chrome.deb \
      https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb \
 && apt-get update && apt-get install -y /tmp/chrome.deb \
 && rm -f /tmp/chrome.deb && rm -rf /var/lib/apt/lists/*

# Node 22. Ubuntu 24.04's apt `nodejs` is 18, which is too old for the Agent SDK.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
 && apt-get install -y --no-install-recommends nodejs \
 && rm -rf /var/lib/apt/lists/* \
 && node --version

# ubuntu:24.04 already owns uid 1000, so useradd fails without removing it first.
RUN userdel -r ubuntu 2>/dev/null || true \
 && useradd -m -u 1000 -s /bin/bash agent \
 && mkdir -p /home/agent/Downloads /home/agent/workspace /home/agent/.config \
 && chown -R agent:agent /home/agent

# overlay-init mounts /dev/vdb on /overlay and pivots with the old root at /rom.
# Both must EXIST in the image: mount(8) does not create its mountpoint, and the
# root is read-only at that stage so the script cannot mkdir them itself. Without
# these the overlay silently fails and the VM boots read-only but "healthy".
RUN mkdir -p /overlay /rom

COPY files/overlay-init /sbin/overlay-init
COPY files/resolvconf.service files/xvfb.service files/x11vnc.service \
     files/novnc.service files/openbox.service files/chrome.service \
     files/agent.service /etc/systemd/system/
RUN chmod 0755 /sbin/overlay-init

# chrome-devtools-mcp runs on puppeteer-core, which downloads no browser, so the
# image keeps exactly one Chrome (the .deb above). This env var is the guard: if
# a future version switches to full puppeteer it fails here instead of silently
# adding a ~170MB second browser. --omit=dev keeps devDependencies out too.
ENV PUPPETEER_SKIP_DOWNLOAD=true
COPY files/agent/package.json files/agent/tsconfig.json /opt/agent/
COPY files/agent/src /opt/agent/src
RUN cd /opt/agent \
 && npm install --omit=dev --no-audit --no-fund \
 && npm install --no-save --no-audit --no-fund typescript @types/node \
 && ./node_modules/.bin/tsc -p tsconfig.json \
 && rm -rf /opt/agent/node_modules/typescript /opt/agent/node_modules/@types \
 && npm cache clean --force \
 && chown -R agent:agent /opt/agent

# Masking the network managers is load-bearing: the guest IP comes from the
# kernel ip= boot arg and any of them would clobber it. ctrl-alt-del.target is
# deliberately left alone -- masking it makes SendCtrlAltDel a silent no-op.
RUN systemctl set-default multi-user.target \
 && systemctl enable resolvconf.service xvfb.service x11vnc.service \
      novnc.service openbox.service chrome.service agent.service \
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
