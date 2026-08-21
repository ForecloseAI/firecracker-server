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
COPY files/agent.py     /usr/local/bin/agent.py
COPY files/resolvconf.service files/xvfb.service files/x11vnc.service \
     files/novnc.service files/agent.service /etc/systemd/system/
RUN chmod 0755 /sbin/overlay-init /usr/local/bin/agent.py

# Masking the network managers is load-bearing: the guest IP comes from the
# kernel ip= boot arg and any of them would clobber it. ctrl-alt-del.target is
# deliberately left alone -- masking it makes SendCtrlAltDel a silent no-op.
RUN systemctl set-default multi-user.target \
 && systemctl enable resolvconf.service xvfb.service x11vnc.service \
      novnc.service agent.service \
 && systemctl mask systemd-networkd.service systemd-networkd-wait-online.service \
      systemd-resolved.service NetworkManager.service snapd.service \
      apparmor.service systemd-timesyncd.service systemd-random-seed.service \
      e2scrub_reap.service

# Docker bakes a machine-id into the image; every VM would otherwise share one
# identity. Emptying it makes systemd mint a unique id on first boot, which
# lands in that VM's overlay upper and persists per-VM.
RUN truncate -s 0 /etc/machine-id
