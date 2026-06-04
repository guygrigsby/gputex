# gputex

A dead-simple single-GPU advisory mutex. Wrap any GPU job so two never stack on one card —
which panics Macs (Metal) and OOMs an 8GB CUDA box.

```
gputex run    [--gpu ID] [--wait S] "<label>" -- <cmd...>
gputex status [--gpu ID]
```

`run` takes an exclusive `flock` on `~/.gputex/<gpu>.lock`, records the holder, runs your command,
and releases on exit — **including crashes**: the OS drops the lock when the holding process dies, so
there is never a stale lock to clean up. A second `run` while the card is busy exits **75** and prints
who holds it. `--wait S` blocks up to S seconds first.

## Examples

```sh
gputex run "kohya-train" -- bash 03_train.sh
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
