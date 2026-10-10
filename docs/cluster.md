# Running several hosts

fired keeps its state in Postgres. Several fired hosts can share one
database and act as one service: **any host answers any request**, so
clients (your backend, fired-artifacts) need one URL, e.g. a load balancer
or a Kubernetes Service in front of every host.

```
backend ─┐                ┌─ fired host a (pool chat) ── its VMs
         ├─ one URL (LB) ─┤
artifacts┘                └─ fired host b (pool artifacts) ── its VMs
                    both ──── Postgres (fired's own; nobody else reads it)
```

The database is fired's private state. Other services use the API only, so
the schema can change with fired releases without touching them.

## What lives where

- **One owner per sandbox.** A sandbox runs on one host: its disk, memory
  snapshot and archive are files on that host. The database records which
  host (`host` in the API), and only that host changes the sandbox.
- **Requests are forwarded.** A request about a sandbox that lives elsewhere
  (exec, files, terminal WebSocket, ports, previews, lifecycle) is proxied
  to its host unchanged, streaming included. Lists and quota reads come from
  the database.
- **Events are cluster-wide.** Hosts relay their events through Postgres
  `LISTEN/NOTIFY`, so `GET /v1/events` on any host shows every host's
  events (each with `host`). Notifications sent while a host is
  disconnected are lost; re-read state after reconnecting.
- **Templates are built on every host.** The template record is shared, but
  the built root filesystem and base snapshot are files on each host.
  Creating a template builds it where the request landed and records it for
  the others, which build it within seconds (and hosts that join later
  build it on start). Placement only uses hosts where it's `ready`.

## Placement and pools

Each host runs some pools (`cluster.pools`, e.g. `["chat"]`; `"*"` is any).
A new sandbox's `pool` (default `cluster.default_pool`) picks the hosts it
may run on; among those with its template ready, it goes to the one with
the most free memory (`capacity.max_running_memory_mb` minus what's
running). If none runs the pool: `503 no_host`.

Use separate pools, ideally on separate hosts, for workloads that must not
starve each other: e.g. `chat` for agent sandboxes and `artifacts` for job
runners.

## Per-user limits

A sandbox created with a `quota_key` (typically a user) is charged to that
key in its pool. Two limits per key and pool:

- `max_memory_mb`: memory of the key's running sandboxes at once.
- `gb_hours_per_day`: memory × running time per UTC day, in GB-hours
  (2 GB for 2 h = 4, 100 MB for 48 h = 4.7).

Defaults per pool are in the config (`quotas`); override them per key with
`PUT /v1/quotas/{key}/{pool}` (your backend, per plan).

- **Admission** happens on every start (create, resume, or a wake by an
  exec): in one database transaction, the key's quota row is locked, its
  running memory and today's usage are summed across all hosts, and the
  sandbox is marked running. Two hosts starting sandboxes for the same key
  at once can't both slip under a limit. Over a limit is
  `429 quota_exceeded`.
- **Fail closed:** if the database is unreachable, starts are refused
  (`503 admission_unavailable`). Running sandboxes keep running.
- **Usage** is settled into a per-day table when a sandbox stops, and every
  minute while it runs (so it lands on the right day). Suspended and
  archived sandboxes don't count.

## Hosts coming and going

- Each host heartbeats every `cluster.heartbeat` (5s); one silent for
  `cluster.host_timeout` (30s) is **down**. A clean shutdown marks it down
  at once. `GET /v1/hosts` lists hosts with `alive`.
- A down host's sandboxes are listed with `host_down: true` (their last
  recorded state), other calls about them return `503 host_unavailable`,
  and their memory stops counting against their users' limits. They come
  back when the host does.
- `cluster.host_id` names a host and must stay the same across restarts
  (its sandboxes are recorded under it). On Kubernetes, use the node name.
- `cluster.advertise_url` is how other hosts reach this one; hosts must be
  able to reach each other and the database.

## Upgrades and versions

Hosts of different versions share the database during a rolling upgrade, so
schema changes follow expand/contract:

- A release only **adds** to the schema (tables, columns with defaults).
  Older hosts ignore what they don't know.
- Removing or renaming something waits for a later release, once no running
  version uses it. That migration raises the minimum code version, and older
  hosts refuse to start against the database instead of misreading it.
- Migrations run on startup under a Postgres advisory lock (one host at a
  time), or ahead of a rollout with `fired migrate`.
- New request fields and endpoints should be used once every host runs the
  new version: forwarding may send a request to an older host, which rejects
  unknown fields.

`GET /v1/hosts` shows each host's `version` and `schema`.
