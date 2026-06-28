#!/usr/bin/env bash
# verify-gate.sh — prove the GPU compliance gate. Run as root on the GPU box.
# Each check prints PASS/FAIL; exits non-zero if any check fails.
set -uo pipefail
[[ $EUID -eq 0 ]] || { echo "run as root (sudo)" >&2; exit 1; }

AGENT_USER="${AGENT_USER:-agent}"
GPU="${GPU:-0}"             # the gputex --gpu id / the /dev/nvidiaN to probe
DEV="/dev/nvidia${GPU}"
fails=0
ok()  { printf 'PASS  %s\n' "$*"; }
no()  { printf 'FAIL  %s\n' "$*"; fails=$((fails+1)); }

# open the device read-only and immediately close it (no read) — tests the OS
# permission gate, not device semantics.
probe='exec 3<'"$DEV"'; exec 3<&-'

echo "== gate closed for agent (no gputex) =="
out="$(sudo -u "$AGENT_USER" bash -c "$probe" 2>&1)"
if [[ $? -ne 0 ]]; then ok "agent cannot open $DEV directly  ($out)"; else no "agent opened $DEV WITHOUT gputex — gate leaks"; fi

echo "== gate open for agent THROUGH gputex =="
if sudo -u "$AGENT_USER" gputex run gate-probe --gpu "$GPU" -- bash -c "$probe" >/dev/null 2>&1; then
  ok "agent can open $DEV via gputex"
else
  no "agent could NOT open $DEV via gputex — gate is too tight (setgid? group?)"
fi

echo "== metrics-contract env reaches the job =="
mlflow="$(sudo -u "$AGENT_USER" gputex run env-probe --gpu "$GPU" -- bash -c 'echo "${MLFLOW_TRACKING_URI:-UNSET}"' 2>/dev/null | tail -1)"
if [[ "$mlflow" != "UNSET" && -n "$mlflow" ]]; then
  ok "MLFLOW_TRACKING_URI injected: $mlflow"
else
  printf 'WARN  MLFLOW_TRACKING_URI not set yet (component 3 fills /etc/gputex/env)\n'
fi

echo "== god-mode intact =="
if id -nG guygrigsby 2>/dev/null | tr ' ' '\n' | grep -qx gpu; then ok "guygrigsby is in gpu group"; else no "guygrigsby missing from gpu group"; fi

echo "== device perms persistent config present =="
[[ -f /etc/modprobe.d/nvidia-gate.conf ]] && ok "modprobe.d/nvidia-gate.conf present" || no "modprobe.d config missing"
[[ -f /etc/udev/rules.d/70-nvidia-gpu-gate.rules ]] && ok "udev rule present" || no "udev rule missing"

echo
ls -l /dev/nvidia* 2>/dev/null | grep -v nvidia-caps
echo
[[ $fails -eq 0 ]] && echo "ALL GATE CHECKS PASSED" || { echo "$fails CHECK(S) FAILED"; exit 1; }
