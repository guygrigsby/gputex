# gputex

A dead-simple single-GPU advisory mutex. Wrap any GPU job so two never stack on one card —
which panics Macs (Metal) and OOMs an 8GB CUDA box.

```
gputex run    [--gpu ID] [--wait S | --queue | --preempt] "<label>" -- <cmd...>
gputex status [--gpu ID]
```

`run` takes an exclusive `flock` on `~/.gputex/<gpu>.lock`, records the holder, runs your command,
and releases on exit — **including crashes**: the OS drops the lock when the holding process dies, so
there is never a stale lock to clean up. A second `run` while the card is busy exits **75** and prints
who holds it.

On a busy card you have two ways to wait instead of failing:

- `--wait S` polls for up to S seconds, then gives up with exit 75.
- `--queue` blocks in the kernel until it's your turn (waits forever, FIFO-ish). The wait queue is the
  kernel's, so a queued job that's killed or Ctrl-C'd is simply dropped from it — same auto-release
  story as a running holder, no stale queue state.

Or take the card by force:

- `--preempt` signals the current holder (`SIGTERM`, then `SIGKILL` after a ~10s grace) and grabs the
  lock once it dies. Only works for a holder on the same host — a remote PID isn't ours to signal, so
  it refuses rather than guess.

Or run at the bottom of the pile:

- `--low` marks a **shared, lowest-priority holder** (e.g. an always-on ComfyUI and llama-swap sharing a
  training card). Normal jobs take an exclusive lock (`LOCK_EX`); `--low` jobs take a shared one
  (`LOCK_SH`), so **several `--low` holders coexist on the card at once**. They all yield to a normal job:
  any normal acquire evicts *every* low holder (SIGTERM, then SIGKILL after a ~10s grace) and takes the
  card exclusively — no `--preempt` needed. Two normal jobs still queue/refuse against each other, so the
  light services give the card up on demand without a trainer ever killing a trainer. A `--low` job blocks
  while a normal holder has the card, so it simply waits out training and resumes after.

## Examples

```sh
gputex run "kohya-train" -- bash 03_train.sh
gputex run --queue "kohya-train" -- bash 03_train.sh             # wait for the card instead of exiting 75
tmux new-session "gputex run 'comfyui' -- bash start-comfy.sh"   # daemon holds the lock for its life
ssh rig 'gputex run "comfyui" -- bash start-comfy.sh'            # coordinate the rig card remotely
gputex status                                                    # FREE / BUSY + holder
```

One lock per physical GPU per host (default id `default`; `--gpu gpu1` reserved for multi-GPU).
Exit codes: `0` ok, `75` GPU busy, `1` error, `2` usage.

## Install

Single static binary, no runtime deps:

```sh
go build -o ~/.local/bin/gputex .
# or cross-compile and scp to each GPU host:
GOOS=darwin GOARCH=arm64 go build -o gputex-darwin-arm64 .
GOOS=linux  GOARCH=amd64 go build -o gputex-linux-amd64 .
```

Design: [`docs/specs/2026-06-03-gputex-design.md`](docs/specs/2026-06-03-gputex-design.md).

MIT.
