#!/usr/bin/env bash
# uninstall-gate.sh — fully revert install-gate.sh. Run as root on the GPU box.
# Removes the persistent device-perm config and restores /dev/nvidia* to 0666.
# Leaves the `agent` user and `gpu` group in place by default (harmless; pass
# --purge-user / --purge-group to remove them too).
set -uo pipefail
[[ $EUID -eq 0 ]] || { echo "run as root (sudo)" >&2; exit 1; }

say() { printf '  %s\n' "$*"; }
rm -f /etc/modprobe.d/nvidia-gate.conf && say "removed modprobe.d/nvidia-gate.conf"
rm -f /etc/udev/rules.d/70-nvidia-gpu-gate.rules && say "removed udev rule"
udevadm control --reload-rules >/dev/null 2>&1 || true

# Restore current-boot perms to the stock world-open default.
shopt -s nullglob
for d in /dev/nvidia[0-9]* /dev/nvidiactl /dev/nvidia-uvm /dev/nvidia-uvm-tools /dev/nvidia-modeset; do
  [[ -c $d ]] || continue
  chown root:root "$d"; chmod 0666 "$d"
done
say "restored /dev/nvidia* to root:root 0666 (current boot)"

# gputex: drop setgid; leave the binary (it still works as a plain mutex).
[[ -e /usr/local/bin/gputex ]] && { chmod 0755 /usr/local/bin/gputex; chown root:root /usr/local/bin/gputex; say "gputex setgid removed (still usable as a mutex)"; }

for a in "$@"; do
  case "$a" in
    --purge-user)  userdel -r agent 2>/dev/null && say "removed user agent"; rm -f /etc/sudoers.d/agent ;;
    --purge-group) groupdel gpu 2>/dev/null && say "removed group gpu" ;;
  esac
done

echo "gate reverted. (NVreg param is gone; a reboot returns to stock perms.)"
