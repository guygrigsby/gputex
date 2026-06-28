# Agent GPU compliance via chokepoints (design, 2026-06-27)

## Problem

Many agents (Claude Code, Codex, others) run training on trig's GPU. They are
supposed to acquire the card through `gputex` (the per-GPU mutex) and, soon, to
report run metrics to MLflow. Many comply. Some do not: they grab the card raw
(colliding with held jobs) or train untracked. A central overseer can only
*detect* a defector after the fact, and a defector that skips gputex also skips
telling anyone it did. Surveillance loses this game.

## Thesis: enforce at chokepoints you own, do not police from the center

There is no new "task master" daemon. Compliance is made structural: the only
path that reaches the resource is the compliant path. Three gates, each one Guy
already owns:

1. **Identity** boundary. Agents run as a dedicated `agent` user, not as Guy.
2. **GPU access** gate. The card is reachable only through `gputex` (OS device
   permissions, not convention).
3. **Metrics** gate. The only training libraries (`lmkit`, `lmkit-go`) report to
   MLflow natively, so training *is* reporting.

A defector cannot opt out of a gate it must pass through. The fleet registry
(gyr) is left as an optional observability/escalation surface, not the enforcer.

## Scope split (two agents working in parallel)

- **This repo (gputex) + trig host: components 1 and 2.** Identity, device-perm
  gate, setgid gputex, and the `MLFLOW_TRACKING_URI` env injection gputex hands
  to its children (the launcher owns the env; the library owns the reporting).
- **lmkit / lmkit-go + bee: component 3** (separate agent). MLflow tracking
  server on bee; native run/param/metric reporting built into both libraries.
  Contract from this side: every gputex child gets `MLFLOW_TRACKING_URI` in its
  environment, pointing at bee. lmkit reads it with zero per-run config.

## Decisions

### Identity: one `agent` user, not Guy (component 1)

Agents ssh to trig as a shared `agent` account, distinct from `guygrigsby`. This
is the keystone: it gives the device gate real teeth (below) and contains the
blast radius of agents doing system-level work, which today runs as Guy's own
uid. Guy keeps god-mode by being a member of the `gpu` group; `agent` is not.

`agent` gets sudo by explicit allowlist only (default deny). The allowlist starts
empty and grows per demonstrated need. This is the one open item that needs Guy's
input: enumerate what `agent` legitimately must run as root.

### GPU access: device-permission gate (component 2)

Current state on trig: `/dev/nvidia*` are `crw-rw-rw-` (0666), world-open. That
is the leak.

The gate:

- A system group `gpu`. `guygrigsby` is a member (god-mode); `agent` is not.
- The nvidia driver creates its device nodes owned `root:gpu`, mode `0660`, via
  module params in `/etc/modprobe.d/nvidia-gate.conf`:
  `options nvidia NVreg_DeviceFileUID=0 NVreg_DeviceFileGID=<gpu gid>
  NVreg_DeviceFileMode=0660`. This is authoritative: `nvidia-modprobe` (setuid
  root) recreates nodes on demand, and these params make even those creations use
  our group and mode. A udev rule (`70-nvidia-gpu-gate.rules`,
  `KERNEL=="nvidia*", GROUP="gpu", MODE="0660"`) is the belt for nodes created
  outside that path. Targets `nvidia*` only; trig also has an AMD GPU.
- `gputex` is installed setgid `gpu` (mode `2755`, owner `root:gpu`). It is a Go
  binary, so setgid is honored (Linux ignores setgid on scripts). When `agent`
  runs `gputex run ... -- <cmd>`, gputex executes with egid `gpu`; the child
  inherits that egid across exec and can open `/dev/nvidia*`. A bare
  `python train.py` with no gputex prefix runs with egid `agent`, cannot open the
  devices, and CUDA fails hard.

Result: the mutex stops being a convention. `agent` cannot reach the card except
through gputex, and cannot `chmod` the devices back (it neither owns them nor is
root). This is exactly what the separate-user model buys over running as Guy.

**Ceiling.** A *malicious* process running as `agent` that gains root could undo
this; the gate stops accidental and lazy defection, which is the actual problem,
not a hostile insider. Recorded, not papered over.

### Metrics: library-native (component 3, other agent)

gputex exports `MLFLOW_TRACKING_URI` (pointing at bee) into every child. lmkit and
lmkit-go report runs/params/metrics natively, so any training launched through the
GPU gate is tracked by construction. lmkit-go has no python MLflow client; it
posts to MLflow's REST tracking API directly.

**Residual leak, deferred.** The device gate forces gputex, but gputex forces GPU
*access*, not *lmkit*. A raw torch script run through gputex gets the card without
reporting. Today lmkit/lmkit-go are the only training entry points, so this is
theoretical; if that changes, the backstop is a gputex-injected `sitecustomize.py`
calling `mlflow.autolog()` to catch strays. Not built now (YAGNI).

### Fleet (gyr): observability/escalation only, deferred

Optional and last. gputex can emit a fleet heartbeat at run start/done
(`meta={gpu, label, mlflow_run, agent}`) to the registry, giving a live board and
a place to escalate if device perms are ever tampered. Ships independently after
the gates work. Not part of the compliance guarantee.

## Durability

- Module params + udev rule survive reboot and driver reload. A one-shot
  `chmod` does not, so the persistent config is the real fix; the immediate
  `chgrp`/`chmod` is only to apply the gate without unloading the in-use driver.
- gputex mutex stays flock-based (auto-releases on holder death).
- MLflow server on bee runs under `systemd --user` + linger (component 3).

## Acceptance (components 1 and 2)

Proven on trig, GPU idle:

1. `sudo -u agent cat /dev/nvidia0` -> Permission denied (gate closed).
2. `sudo -u agent gputex run probe --gpu 0 -- cat /dev/nvidia0` -> succeeds
   (gate open through gputex).
3. `sudo -u agent gputex run probe --gpu 0 -- env | grep MLFLOW_TRACKING_URI` ->
   set (contract for component 3).
4. As `guygrigsby` (in `gpu`): raw device access still works (god-mode intact).
5. Perms config is persistent: modprobe.d + udev present; re-deriving on reboot
   yields `root:gpu 0660` (verified by re-running the apply check, full reboot
   verification noted where a live reboot is deferred).

## Decision record

New trust boundary (a uid split and an OS-level resource gate). Earns an ADR in
the gputex repo recording: enforcement-by-chokepoint over central policing; the
separate `agent` user as the keystone; driver-param device perms as authoritative
over udev; setgid Go binary as the gate; the malicious-root ceiling. Per the
trust-boundary rule, a fresh-context adversarial pass on the gate before it is
relied on.
