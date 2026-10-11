# Sandbox lifecycle

A sandbox is never simply "on" or "off". When it goes idle, fired moves it
down a ladder of cheaper states, and any request brings it back. This page
covers the states, the three timers that drive them, and what counts as
activity.

## States

```
            POST /sandboxes
                  │
                  ▼
             ┌──────────┐
             │ creating │  clone the template snapshot, or boot from scratch
             └────┬─────┘
                  ▼
   ┌────────▶┌──────────┐   idle_suspend: no activity for N, and nothing in progress
   │         │ running  │ ──────────────────────────────────────────────┐
   │         └──────────┘   (also: memory budget full → oldest idle      │
   │              ▲          sandbox is suspended early)                 ▼
   │              │                                              ┌────────────┐
   │              │     resume: memory snapshot restored,        │ suspended  │  memory + disk on the
   │              ├────────────── processes intact ──────────────│ (hot tier) │  hot tier (tmpfs)
   │              │                                              └─────┬──────┘
   │              │                                                    │ archive_after: N since
   │              │                                                    │ it was suspended
   │              │     resume: unpack, then restore,            ┌─────▼──────┐
   │              └────────────── processes intact ──────────────│  archived  │  zstd archive on the
   │                                                             │ (cold tier)│  cold disk
   │   any exec / file / process / shell / preview request       └────────────┘
   └── (or POST /resume) wakes it from either state

   ┌──────────┐  VM exited without a snapshot: disk kept, the next use boots from scratch
   │ stopped  │  (processes lost). archive_after applies here too.
   └──────────┘

   delete_after: no activity for N, in ANY state ──▶ deleted
   (disk, snapshot, archive and logs are all removed)
```

| State | Where it lives | What waking it costs |
|---|---|---|
| `running` | guest RAM and the hot tier | nothing, it's live |
| `suspended` | memory snapshot and disk on the hot tier | a snapshot restore; processes keep running |
| `archived` | one compressed file on the cold tier | unpack, then a snapshot restore; processes keep running |
| `stopped` | disk on the hot tier, no memory snapshot | a full boot; processes are lost |
| `error` | whatever survived | can't resume; the sandbox's `error` field says why |

The transitional states `creating`, `suspending`, `archiving`, `resuming`
and `deleting` are short-lived. A sandbox in one of them isn't acted on by
the timers.

## Timers

Each sandbox has three timers. Set them per sandbox in `ttl` when creating
it, or change them later with `PATCH /sandboxes/{id}`; the change applies
right away. An unset or `0` value uses the daemon default from
`defaults.ttl` in the config, and `-1` turns that timer off.

| Timer | Default in `deploy/config.yaml` | Measured from | Acts on |
|---|---|---|---|
| `idle_suspend` | `5m` | last activity | `running`, only when no command, shell or file operation is in progress |
| `archive_after` | `1h` | the moment the sandbox became `suspended` or `stopped` | `suspended`, `stopped` |
| `delete_after` | `168h` (7 days) | last activity | every state, including `running` |

`delete_after` is an inactivity timeout, not a maximum lifetime. A sandbox
that keeps getting used is never deleted. If you need "delete N hours after
creation no matter what", enforce it in your backend with
`DELETE /sandboxes/{id}`.

A background reaper checks the timers every `reaper_interval` (default
`15s`), so a transition can happen up to one interval after its deadline.

### Example

With `idle_suspend: 5s`, `archive_after: 5s`, `delete_after: 40s` and the
default 15s reaper interval, a sandbox that nobody touches after creation
goes:

```
t+1s   running
t+16s  suspending → suspended    idle for 5s, picked up at the next check
t+31s  archiving  → archived     5s after it was suspended
t+46s  deleted                   40s after its last activity
```

## What counts as activity

These reset the "last activity" clock and wake a sandbox that isn't running:

- `POST /exec`, and an open `GET /exec/stream` (shell) WebSocket
- file calls: read, write, list, move, delete, archive upload and download
- process calls: list, output, signal
- port preview and proxy traffic, counted at most once a second. Sending
  `X-Fired-No-Wake: 1` gets `503 not_running` instead of waking the sandbox.
- `POST /resume`

These do **not** count:

- reading the sandbox's status (`GET /sandboxes`, `GET /sandboxes/{id}`),
  events or metrics
- a background process running on its own. A dev server that nobody is
  using still gets suspended; its process is frozen with the VM and picks
  up where it left off on wake.
- an open preview WebSocket with no traffic on it

While a command, shell or file operation is in progress, the sandbox counts
as busy and `idle_suspend` won't act on it.

## Other ways a sandbox changes state

- **Memory pressure.** `capacity.max_running_memory_mb` caps the memory of
  all running sandboxes. When a create or resume would exceed it and
  `capacity.evict_idle` is on, fired suspends the least recently active
  sandbox that has nothing in progress. Otherwise the request fails with
  `503 capacity`.
- **Hot storage pressure.** Suspended sandboxes keep their memory snapshot
  in the hot directory. When a start, a suspend or the reaper would leave
  less than `capacity.min_free_hot_mb` free there, fired archives the least
  recently active suspended or stopped sandboxes to the cold tier first
  (`503 no_space` if that isn't enough).
- **Explicit calls.** `POST /suspend`, `/archive` and `/resume` do the
  transition immediately. They are idempotent.
- **Daemon shutdown.** On SIGTERM, `shutdown_action` decides what happens to
  running sandboxes: `suspend` (the default) snapshots them to the hot tier,
  `archive` also compresses them to the cold tier, and `none` leaves them.
  On startup fired reconciles: anything whose data is gone, for example a
  hot tier lost in a host reboot, becomes `error`.

## Caveat: long commands and `delete_after`

The `delete_after` check doesn't look at work in progress. A single command
that runs longer than `delete_after` can be deleted mid-run, because its
activity is recorded when it starts. With the defaults this can't happen:
the maximum command timeout (`exec.max_timeout`, 2h) is far shorter than
7 days. Keep `delete_after` longer than your longest command.
