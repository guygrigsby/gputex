# GPUtex — design

Date: 2026-06-03
Status: design for review (pre-implementation)
Repo: github.com/guygrigsby/gputex (standalone)

## Problem

Multiple uncoordinated processes — different Claude instances, human-launched jobs — start GPU
workloads on the same card and collide. This has cost real damage: a GPU panic that crashed the Mac
(stacking an mlx_vlm captioner on a router-resident model), and constant stop/restart juggling on
rig where ComfyUI and kohya training fight over the one 8GB CUDA card. Need a cross-process,
cross-instance mutex so only one GPU job holds a card at a time.

## Goals / non-goals

**Goal:** a simple, reliable *advisory mutex* per GPU, per host, that any process (Claude instance or
human) acquires before a GPU job, that auto-releases on exit/crash, with a CLI to run/check status,
plus an optional hook that enforces it.

**Non-goals:** GPU scheduling, fair queueing, multi-GPU bin-packing, remote orchestration. Just mutual
exclusion. A wait-queue can come later.

## Bounded context

A *GPU host* is a machine with GPU(s): the **Mac** (one Metal GPU; mlxd is meant to be the single
owner) and **rig** (one 8GB CUDA card shared by ComfyUI and kohya). Each host owns its own mutex(es).
A job that runs on rig — even when launched via SSH from the Mac's Claude instance — acquires the
**rig** lock. One lock per physical GPU; default is a single GPU per host.

## Core model

- **Lock** = an OS advisory file lock (flock semantics) on `~/.gputex/<gpu>.lock`. Held for the
  lifetime of the holding process, so it **auto-releases on exit, kill, or crash** — no stale-lock
  cleanup logic needed. This is the key property and the reason to use real flock, not a naive
  lockfile.
- **Holder sidecar** `~/.gputex/<gpu>.holder` (JSON: label, pid, host, started, cmd) for friendly
  `status` output and "GPU busy, held by X" messages. Cross-checked against PID liveness.
- `<gpu>` defaults to `default` (single GPU); `--gpu gpu1` reserved for future multi-GPU hosts.

## CLI

- `gputex run [--gpu ID] [--wait S] "<label>" -- <cmd...>`
  Acquire (non-blocking by default; `--wait S` blocks up to S seconds), write the holder record,
  exec the command, release on exit. If the card is busy: print the current holder and exit `75`.
- `gputex status [--gpu ID] [--json]`
  Is it locked, and by whom (label / pid / host / since)? Reports + clears a stale sidecar if the
  holder PID is dead.
- `gputex hold [--gpu ID] "<label>"` / `gputex release`
  Lower-level, for wrapping a session/daemon where you can't wrap a single child. `run` is preferred.
- Exit codes: `0` ok, `75` busy, `1` error.

## Lifetime patterns

- **One-shot job:** `gputex run "kohya-train" -- bash 03_train.sh`
- **Long-lived daemon (ComfyUI):** wrap the daemon so the lock is held for its whole life —
  `tmux new-session "gputex run 'comfyui' -- bash start-comfy.sh"`. Lock frees when ComfyUI stops.
- **Remote (rig from the Mac instance):** `ssh rig 'gputex run "comfyui" -- bash start-comfy.sh'`
  acquires the *rig* lock. The Mac instance coordinating rig work runs gputex on rig via the ssh cmd.

## Edge cases

- **Crash / host sleep** (rig went down mid-eval, exactly this session): flock auto-releases when the
  process dies, so there's no stale lock to clean. The sidecar may be stale; `status` checks PID
  liveness.
- **Race between instances:** flock is atomic — one wins, the other gets `75` + the holder info.
- **Advisory, not enforced by itself:** gputex can't tell whether a wrapped command actually uses the
  GPU. Correctness depends on wrapping GPU jobs; the hook below enforces the wrapping.

## Enforcement hook (phase 2, optional)

A `PreToolUse` Bash hook that detects likely GPU-launching commands by heuristic
(`python .*(torch|mlx_lm|mlx_vlm|diffusers)`, ComfyUI `main.py --listen`, `sdxl_train`,
`accelerate launch`, `mlxctl start`, etc.) and denies unless the command is wrapped in `gputex run`.
Mirrors the existing op-read hook. **Caveat:** pattern-matching "is this a GPU job" has false
positives/negatives — start permissive (warn + inject `gputex status` as context) before any hard
deny. Per-host: the Mac hook matches Mac patterns; rig jobs arrive as `ssh rig '...'`, so the hook
inspects the inner command and requires `gputex run` there.

## Language / implementation — DECIDED: Go

**Go single binary.** One static binary per arch, `scp` to `~/bin` on Mac (darwin/arm64) and rig
(linux/amd64) — no runtime, no python-version concerns (Guy hates distributing Python). Use
`golang.org/x/sys/unix.Flock` (or `syscall.Flock`) for the advisory lock; the fd is held open for the
child's lifetime so the lock auto-releases on exit/crash. Not a daemon+CLI service, so the rookery
scaffold doesn't apply — a plain single-package `main`. Keep it minimal (personal, 2 hosts): just
`run` + `status`, no config file, no daemon.

## Repo layout (github.com/guygrigsby/gputex)

```
gputex                 # the script/binary
README.md
docs/specs/2026-06-03-gputex-design.md   # this
```
Install: copy/symlink onto PATH on Mac + rig (or `go install`). Lock dir `~/.gputex/`.

## Deploy

Mac + rig (WSL). Once gputex exists, update the global CLAUDE.md GPU rule to point at
`gputex run` / `gputex status` as the concrete mechanism (it currently says "treat the GPU as a mutex
you must acquire... until [GPUtex] exists, check manually").

## Decisions
1. **Language: Go** (single static binary; Guy hates distributing Python). DECIDED.
2. **`run` default: non-blocking fail-fast (exit 75)** so an instance learns the card is busy and does
   other work; `--wait S` opts into blocking. Leaning — confirm.
3. **Ship binary + convention first; enforcement hook is a fast-follow.** Leaning — confirm.
