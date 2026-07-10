# The gputex lock protocol

This file is the contract. gputex is one client of it; any program may
speak the protocol directly (nevla's stdlib `gpu` module does). A change
here is a breaking change for every client, not an implementation detail.

## Directory

All state lives under one directory:

- `$GPUTEX_DIR` if set (tests, sandboxes),
- else `~/.gputex`.

Files inside, per GPU id (`default` unless a job says otherwise):

- `<gpu>.lock` — the lock file. Contents are meaningless; only the flock
  on it matters.
- `<gpu>.holders/<pid>.json` — one registry file per holding process.

## The lock

A single `flock(2)` on `<gpu>.lock` used as a reader/writer lock:

- Exclusive jobs (training) take `LOCK_EX`. One at a time, and only once
  every shared holder has released.
- Shared, lowest-priority holders (`gputex run --low`; ComfyUI,
  llama-swap) take `LOCK_SH`. Many coexist; all block while an exclusive
  holder has the card, and an exclusive acquirer may preempt them
  (SIGTERM, then SIGKILL after ~10s).
- Non-blocking probes add `LOCK_NB` and treat `EWOULDBLOCK` as "busy".

flock releases when the holding process exits, however it exits. A crash
never strands the card. Clients must keep the locking fd open for the
whole hold; closing it releases.

## The holder registry

The kernel won't say who holds an flock, so holders self-report: on
acquire, write `<gpu>.holders/<pid>.json`; on release, remove it. The
registry is advisory (status, preemption targeting) — the flock is the
lock. Readers must liveness-check entries (`kill(pid, 0)`) and prune the
dead: a crashed holder's flock auto-released, but its file lingers.

Schema (all fields strings unless noted):

```json
{
  "label": "tinyllama run 3",
  "framework": "pytorch",
  "pid": 12345,
  "host": "trig",
  "started": "2026-07-10T11:58:00-06:00",
  "cmd": "python train.py",
  "preemptible": false
}
```

- `label` — required; human-readable job name.
- `framework` — optional; the stack the job runs on (`pytorch`,
  `lmkit-go`, `nevla`). Surfaced by the metrics exporter.
- `pid` (int), `host` — required; who to signal, and a guard against
  signalling across hosts. Preemption refuses when `host` isn't ours.
- `started` — RFC 3339.
- `cmd` — the command line, for status display.
- `preemptible` (bool) — true for shared `--low` holders; marks the entry
  evictable by an exclusive acquirer.

Unknown fields must be preserved-on-read/ignored, never an error.

## The managed environment

`$GPUTEX_ENV_FILE` if set, else `/etc/gputex/env`: `KEY=VALUE` lines,
`#` comments and blanks ignored. Every client must inject these into the
job's environment at acquire time, existing values winning. This is how
the metrics contract holds: a job cannot take the card without also
getting `MLFLOW_TRACKING_URI`.

## Known clients

- gputex (this repo) — CLI wrapper, preemption, status.
- nevla stdlib `gpu` module — in-language acquire for `.nv` programs (the language formerly named rikki).
