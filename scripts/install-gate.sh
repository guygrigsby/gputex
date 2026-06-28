#!/usr/bin/env bash
# install-gate.sh — install the GPU compliance gate on a Linux NVIDIA host.
#
# Makes /dev/nvidia* reachable ONLY through gputex: a `gpu` group owns the device
# nodes (0660), gputex is installed setgid `gpu`, and the `agent` user is kept out
# of the group. A bare `python train.py` then cannot open the card; only
# `gputex run ... -- <cmd>` can. The display/compositor user and the human
# god-user are added to `gpu` so the GUI and god-mode keep working.
#
# Idempotent. Run as root on the GPU box. See
# docs/specs/2026-06-27-gpu-compliance-chokepoints-design.md
set -euo pipefail
[[ $EUID -eq 0 ]] || { echo "run as root (sudo)" >&2; exit 1; }

# Users that may touch the GPU directly (god-mode + display). NOT the agent user.
GPU_GROUP_USERS="${GPU_GROUP_USERS:-guygrigsby plasmalogin}"
AGENT_USER="${AGENT_USER:-agent}"        # gated; kept out of the gpu group
REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

say() { printf '  %s\n' "$*"; }

echo "== component 1: identity =="
getent group gpu >/dev/null || { groupadd --system gpu; say "created group gpu"; }
GPU_GID="$(getent group gpu | cut -d: -f3)"
say "gpu gid=${GPU_GID}"

if ! id "$AGENT_USER" >/dev/null 2>&1; then
  useradd --create-home --shell /bin/bash "$AGENT_USER"; say "created user $AGENT_USER"
fi
gpasswd -d "$AGENT_USER" gpu >/dev/null 2>&1 || true   # ensure agent is NOT in gpu
install -d -m 0700 -o "$AGENT_USER" -g "$AGENT_USER" "$(eval echo ~"$AGENT_USER")/.ssh"

for u in $GPU_GROUP_USERS; do
  id "$u" >/dev/null 2>&1 || { say "skip absent user $u"; continue; }
  id -nG "$u" | tr ' ' '\n' | grep -qx gpu || { usermod -aG gpu "$u"; say "added $u to gpu"; }
done

# scoped sudo for agent: default deny. Allowlist starts empty (see gyr-88y).
cat >/etc/sudoers.d/agent <<EOF
# agent: default-deny. Add specific NOPASSWD command allowlist entries below.
# (intentionally empty)
EOF
chmod 0440 /etc/sudoers.d/agent
visudo -cf /etc/sudoers.d/agent >/dev/null && say "sudoers.d/agent valid (deny-all)"

echo "== component 2: device-perm gate =="
# Authoritative: the nvidia driver creates its nodes root:gpu 0660. Covers
# nvidia-modprobe (setuid root), which recreates nodes on demand.
cat >/etc/modprobe.d/nvidia-gate.conf <<EOF
options nvidia NVreg_DeviceFileUID=0 NVreg_DeviceFileGID=${GPU_GID} NVreg_DeviceFileMode=0660
EOF
say "wrote /etc/modprobe.d/nvidia-gate.conf"

# Belt: udev for any node created outside the driver-param path. nvidia* only
# (this host also has an AMD GPU; we never touch amdgpu/dri nodes).
cat >/etc/udev/rules.d/70-nvidia-gpu-gate.rules <<'EOF'
KERNEL=="nvidia*", GROUP="gpu", MODE="0660"
EOF
udevadm control --reload-rules >/dev/null 2>&1 || true
say "wrote udev rule + reloaded"

# Apply now without unloading the in-use driver. Existing open fds (the running
# compositor) are unaffected; the persistent config above takes over on reboot /
# driver reload. nvidia-caps stay root-only (untouched).
shopt -s nullglob
for d in /dev/nvidia[0-9]* /dev/nvidiactl /dev/nvidia-uvm /dev/nvidia-uvm-tools /dev/nvidia-modeset; do
  [[ -c $d ]] || continue
  chgrp gpu "$d"; chmod 0660 "$d"
done
say "applied root:gpu 0660 to /dev/nvidia* (current boot)"

echo "== install gputex setgid gpu =="
command -v go >/dev/null || { echo "go not found; cannot build gputex" >&2; exit 1; }
( cd "$REPO_DIR" && go build -o /usr/local/bin/gputex . )
chown root:gpu /usr/local/bin/gputex
chmod 2755 /usr/local/bin/gputex
say "installed /usr/local/bin/gputex ($(ls -l /usr/local/bin/gputex | awk '{print $1, $3, $4}'))"

echo "== gputex managed env (metrics contract; component 3 fills the value) =="
install -d -m 0755 /etc/gputex
if [[ ! -f /etc/gputex/env ]]; then
  cat >/etc/gputex/env <<'EOF'
# gputex injects these KEY=VALUE lines into every wrapped GPU job's environment.
# Component 3 (lmkit/lmkit-go + bee) sets the MLflow tracking URI here, e.g.:
# MLFLOW_TRACKING_URI=http://bee:5000
EOF
  say "created /etc/gputex/env (empty; agent-3 sets MLFLOW_TRACKING_URI)"
else
  say "/etc/gputex/env already present (left as-is)"
fi
chmod 0644 /etc/gputex/env

echo
echo "gate installed. verify with: sudo $REPO_DIR/scripts/verify-gate.sh"
