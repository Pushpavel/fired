# Playground

Working notes from trying fired-artifacts one use case at a time: what we
did, what the system did, and what we might change. Not product docs; the
design is in [docs/artifacts.md](../docs/artifacts.md).

Setup: `make dev` (fired, fake driver) and `make dev-artifacts` (console at
http://127.0.0.1:8444/ui/, token `dev`).

Each use case gets its own file, `NN-name.md`, written as we go:

- **Tried:** what we did, in the console or with curl
- **Saw:** what happened, including versions, runs, events and anything surprising
- **Questions:** what we didn't understand or the docs don't say
- **Could change:** ideas, each marked as a service, console or docs change

## Where we left off

**Paused after 01 (2026-10-10)** to build the cluster work in
[design-cluster.md](design-cluster.md). That's built (2026-10-10):

- fired on Postgres, any host answering for the cluster
  ([docs/cluster.md](../docs/cluster.md));
- per-user GB-hour and memory limits by pool;
- fired-artifacts charging runs to the user's `artifacts` pool;
- the Helm chart, Skaffold and CD in [.k8s/](../.k8s/README.md).

**Next: resume with 02, editing over time.** The `hello-page` artifact from
01 is still in the dev service (fired-artifacts keeps its own SQLite; fired's
dev state moved to Postgres, so 01's runner sandbox and template are gone:
recreate the `base` template before job use cases).

## Use cases

| # | Use case | Covers | Status |
|---|---|---|---|
| 01 | A static page | create + first publish, the default spec (index.html, no artifact.json), serving, preview origin | done: [01-static-page.md](01-static-page.md) |
| 02 | Editing over time | publish as a patch, labels, `base_version` conflicts (409), compare, restore, zip export | not started |
| 03 | Views | artifact.json views (md, csv, json, svg, react), strict spec validation, a view whose entry is deleted | not started |
| 04 | Uploading a build | blobs, `/blobs/missing` dedup, folder upload with `replace_dirs`, size and file limits | not started |
| 05 | A scheduled job | cron + timezone, `writes`, run versions, a run that changes nothing, the 12h activity window, expiry | not started |
| 06 | Job state | the `state` dir across runs, state on a failed run, state not copied on fork | not started |
| 07 | Triggers and backlogs | manual triggers with payloads, `on_busy` latest (superseded) vs queue (max, max_age, queue_full) | not started |
| 08 | Failures | non-zero exit, timeout (SIGTERM/SIGKILL), logs, retry behavior when fired is unavailable | not started |
| 09 | Approvals | `timeout` / `active_for` above the defaults, blocked jobs, grants, revoking | not started |
| 10 | Credentials | the broker, `secret_env` / `secret_files`, log redaction, a run that leaks a secret into its writes | not started |
| 11 | Forks and owners | fork to another quota key, schedules off after a fork, shared blobs | not started |
| 12 | Quotas and limits | storage, concurrent runs, run-minutes per day, min interval, the global cap | not started |
| 13 | Events and agent feedback | `/v1/events`, `run_now` + `notify` on publish, what an agent session would hear | not started |
| 14 | Cleanup | retention of run versions, `delete_after`, delete, blob GC | started: see 14-cleanup.md |

## Design notes

- [design-cluster.md](design-cluster.md): several fired hosts behind one
  API, a shared Postgres private to fired, per-user GB-hour limits by pool,
  Kubernetes deployment and CD. Built.

## Archive

Before starting, the artifacts from the first console walkthrough were
exported and deleted: [archive/2026-10-10-console-walkthrough](archive/2026-10-10-console-walkthrough/README.md).
