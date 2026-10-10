# Design: several fired hosts, per-user limits, Kubernetes

Status: agreed direction, nothing built yet (2026-10-10). Came out of use
case 01, when publishing from sandboxes raised the question of several fired
hosts.

## Decisions

- **Shared Postgres for fired hosts, private to fired.** All fired hosts
  use one Postgres. Only fired code touches it; its schema is an internal
  detail versioned with fired. Single-host and dev setups keep SQLite
  (`store.Store` becomes an interface with both implementations).
- **The API is the only contract.** The backend and fired-artifacts never
  read fired's tables. Any fired host answers for the whole cluster:
  - lists and gets come from the database (responses gain a `host`
    field);
  - per-sandbox calls (exec, files, terminal, ports, lifecycle) are
    forwarded to the owning host, streaming included;
  - creates do placement (pool, capacity, template ready) and admission,
    then forward to the chosen host;
  - `/v1/events` is cluster-wide (Postgres `LISTEN/NOTIFY` between
    hosts).

  A load balancer (a Kubernetes Service) in front of all hosts gives every
  client one URL. `from_sandbox.fired_url` stays as an optional override.
- **One owner per sandbox.** The database describes sandboxes; VM disks,
  snapshots and memory are local files on the owning host, and only that
  host changes a sandbox's lifecycle.
  - Hosts heartbeat in a `hosts` table.
  - A dead host's sandboxes show as `unavailable` (503 on use), not
    deleted.
- **Hot path stays off the database.**
  - Exec, file and port calls on a running sandbox never touch Postgres.
  - `last_active` stays in memory and is flushed periodically.
  - Only lifecycle transitions write through.
- **Templates:** the template record is shared; the built rootfs and base
  snapshot are per node, so a per-node "template X ready on host A" table
  feeds placement.

## Per-user limits

- **Split pools:** `chat` (agent sandboxes) and `artifacts` (job runners),
  each with its own budget per user, so one can't starve the other. For
  isolation between pools across all users, give each pool its own hosts
  (or reserved capacity).
- **Metric:** memory × time (GB-hours), like Lambda GB-seconds. 2 GB × 2 h
  = 4 GB-h, comparable to 100 MB × 48 h = 4.8 GB-h. Per user and pool:
  - a concurrent memory cap (protects now);
  - a GB-hours budget per day or month (protects the total).

  Suspended sandboxes don't count, for now.
- **Admission is a database transaction on every start** (create, resume,
  wake): lock the user's pool row, sum running memory, allow or refuse in
  the same transaction, so concurrent starts on different hosts can't both
  slip under the cap. GB-hours come from `started_at` × `memory_mb` per
  running sandbox, with no heartbeats per sandbox.
- **Fail closed:** no database, no new starts or resumes. Running
  sandboxes are never killed because the database is unreachable.
- **Where limits live:**
  - The backend sets limits per user and pool (plans) and reads usage,
    through fired's API (like `PUT /v1/quotas/{key}` in fired-artifacts).
  - Sandboxes carry an opaque `quota_key` (+ pool) label: the backend sets
    it on agent sandboxes, fired-artifacts on runners.
  - On a refusal, a job run stays pending and backs off for that user
    (distinct from "host full", which retries soon).
- **fired-artifacts keeps:** storage per quota key, and its job rules (one
  run at a time per artifact, max run length and schedule window with
  grants, queue and payload sizes). Its per-key concurrent runs,
  run-minutes per day and global run cap move to fired's admission and
  host capacity.

## Mixed fired versions

- **Schema: expand/contract migrations.**
  - A release only adds (tables, nullable columns); new code tolerates old
    rows.
  - Migrations run once before rollout (`fired migrate`, or an advisory
    lock), never racing from every host.
  - Drops and renames come only in a later release.
  - A `schema_version` table plus a minimum compatible code version: a
    host refuses to start against a schema newer than its window.
- **New enum values** (states) are only written once all hosts understand
  them; old code treats unknown states as "busy, leave alone".
- **API skew between hosts:** forwarding sends v2 requests to v1 owners,
  and fired decodes strictly (unknown fields are 400). New request fields
  are only used once all hosts are upgraded, or forwarding checks the
  peer's version (recorded in `hosts`).

## Kubernetes

The fired hosts are bare-metal workstations with KVM, joined to the
cluster as a dedicated node pool.

- **fired is a DaemonSet:**
  - `nodeSelector` plus a toleration for the pool's taint
    (`fired=true:NoSchedule`);
  - `privileged: true` (the jailer needs it, as with docker today);
  - `/dev/kvm`;
  - `hostPath` volumes for the tiers: the hot tier is a host tmpfs with
    exec allowed, so suspended sandboxes survive pod replacement; cold,
    templates and state on disk.
- **No special networking.** VMs have no network interface: egress goes
  through fired's proxy, ingress through fired to exposed ports, both over
  vsock. A normal pod network works.
- **Memory:**
  - VM memory is charged to the fired pod: no pod memory limit below what
    VMs can use (an OOM kill takes every VM);
  - `capacity.max_running_memory_mb` plus snapshots and disks stay under
    the node's allocatable memory;
  - a high `priorityClassName` keeps it out of eviction.
- **Rolling upgrades** (`maxUnavailable: 1`):
  - on SIGTERM, fired snapshots every running VM to the hot tier, and the
    new pod resumes them on demand;
  - `terminationGracePeriodSeconds` must cover snapshotting a full node;
  - a resume continues from the exact next instruction (full memory and
    CPU state), so a running script picks up mid-statement. What can
    differ: wall-clock jumps, and connections the remote side timed out;
  - a Firecracker version change can't restore snapshots: those sandboxes
    cold-boot (files kept, processes lost).
- **Node reboots wipe the hot tmpfs:** drain deliberately
  (`shutdown_action: archive` first), no autoscaler scale-down on this
  pool.
- **Security:**
  - fired pods are privileged and run untrusted code (in VMs) in the same
    cluster as the backend;
  - keep the pool dedicated (taint), production egress blocks private
    ranges (`allow_private_cidrs: []`), and a NetworkPolicy limits fired
    pods to Postgres, their peers and the internet;
  - minimal node credentials on that pool.
- **Entry points:**
  - a Service in front of the DaemonSet is the cluster-wide API;
  - pods find each other by pod IP (recorded in `hosts`) or a headless
    Service;
  - previews go through a wildcard Ingress to the Service, which forwards
    to the owner.

## CD

One environment, deploy on every merge to `main`:

- **Files:**
  - `.k8s/skaffold.yaml` builds the image(s) and deploys the Helm chart;
  - `.k8s/fired/` is the chart: namespace, fired DaemonSet + Service,
    fired-artifacts Deployment, Postgres, NetworkPolicy;
  - `values.example.yaml` is documented; the real `values.yaml` is
    gitignored;
  - `.k8s/deploy.sh` runs the same thing by hand.
- **`.github/workflows/deploy.yaml`** on push to `main`:
  1. checkout, buildx, registry login;
  2. write `values.yaml` from the `VALUES_YAML` repository variable;
  3. install pinned Helm and Skaffold;
  4. kubeconfig from a secret, then `skaffold run`.

  Registry, namespace and runner labels are repository variables.
- **Image build time:** the image compiles the guest kernel (10–20 min
  cold), so CI needs a buildx cache (e.g. in the registry).
- **Runner:** it must reach the cluster, likely a self-hosted runner.

## Open

- Where per-user quota state lives is settled by the shared database
  (inside fired). Other backend requirements may still change it.
- Moving archived sandboxes between hosts would need the cold tier on
  shared object storage. Not needed now.
- fired-artifacts on the same Postgres (own schema) would allow replicas,
  but its in-memory counters (running runs, read rate limits) would have to
  move into the database first.
