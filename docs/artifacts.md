# Artifacts

`fired-artifacts` is a separate service that stores **artifacts**: versioned
sets of files that agents publish, that people view and share, and that can
carry **jobs** which run on a schedule in fired sandboxes and write their
results back as new versions. A page, a deck, a dashboard that refreshes
every morning and an automation with no output at all are the same thing
here: files, plus `artifact.json` saying how to view them and what to run.

Like fired, it knows nothing about users. Your backend groups artifacts under
an opaque **quota key** (typically a user), decides who may see or change
what, and answers a **credentials request** before each run. The service
stores, serves, schedules, enforces limits and runs.

```
agent ─ MCP tools ─▶ your backend ──────────────▶ fired-artifacts ──────────────▶ fired
                     users, sharing, approvals     versions, blobs, serving        runner sandboxes
                     credential broker ◀────────── scheduler, limits, runs
                     resolves sources (sandbox
                     folders, MCP resources,
                     tool outputs) into files
```

| fired-artifacts | your backend |
|---|---|
| content-addressed blob store, versions, `artifact.json` validation | users, sharing, who may fork or edit |
| serving files, zip export | preview domain, auth for viewers, rendering views |
| scheduler, `on_busy` queues, per-quota-key and global limits | approving grants (longer schedules, longer runs), quotas per plan |
| runner sandboxes, run versions, state, logs, events | credentials for each run, MCP tools, notifications to sessions |
| retention, `delete_after`, garbage collection | turning sandbox folders, MCP resources and tool outputs into files to publish |

It is a separate binary because its data has a different life from
sandboxes: artifacts are kept and shared, so run it next to your backend on
durable storage. Runs happen on whichever fired host it points at.

## Concepts

- **Artifact**: an ID, a quota key, opaque `labels` and `meta` (title,
  description, anything your backend wants), and a current version. It is
  created together with its first version, so it always has files.
- **Version**: an immutable file tree. Versions are numbered 1, 2, 3… and
  never change. Each has an `origin`:
  - `edit`: a publish
  - `run`: a job run's output
  - `restore`: a copy of an older version
  - `fork`: the first version of a fork
- **File tree**: paths (relative, slash-separated) to blobs. Each blob is
  stored once, named by the SHA-256 of its content and zstd-compressed, so
  unchanged files cost nothing across versions, forks and artifacts.
- **`artifact.json`**: an ordinary file in the tree, so it is versioned,
  exported and forked with everything else. Without it, an `index.html` at
  the root is viewed as html and there are no jobs.

## artifact.json

```json
{
  "views": [
    {"type": "react", "entry": "app/App.jsx"},
    {"type": "csv", "entry": "data/sales.csv"}
  ],
  "jobs": {
    "refresh": {
      "run": "python refresh.py",
      "writes": ["data"],
      "state": ".state",
      "schedule": "0 9 * * 1-5",
      "timezone": "Asia/Kolkata",
      "timeout": "10m"
    },
    "on-push": {
      "run": "python handle.py",
      "on_busy": "queue",
      "queue": {"max": 100, "max_age": "24h"}
    }
  }
}
```

Decoding is strict: an unknown field (say `"schedul"`) fails the publish
instead of being ignored.

**Views** tell your frontend how to present a file. `type` is one of `html`,
`react`, `md`, `csv`, `json`, `text`, `image`, `svg`, `pdf`, and `entry` must
be a file in the version. The service serves raw files; rendering (compiling
a JSX entry, showing a CSV as a table) is the viewer's job.

**Jobs**:

| Field | Meaning |
|---|---|
| `run` | Command, run with `/bin/sh -c` in the artifact's directory. Required. |
| `writes` | Files or directories the job may change. A successful run's changes under these paths become a new version; everything else is read-only to the job. A job can't write `artifact.json`. |
| `state` | A directory carried from one run to the next (cursors, caches). It is not part of the files, not served, and not copied by a fork. It must not overlap `writes` or contain published files. |
| `schedule` | 5-field cron (`minute hour day month weekday`) or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`. Empty means the job only runs when triggered. |
| `timezone` | IANA zone for the schedule (default UTC). |
| `timeout` | Longest run. Default and maximum without approval: `limits.default_max_run` (30m). |
| `active_for` | How long the schedule stays on after a publish. Default and maximum without approval: `limits.default_active_for` (12h). |
| `on_busy` | `latest` (default): at most one pending run, a new trigger replaces it. `queue`: every trigger waits its turn. |
| `queue` | `{max, max_age}` for `queue` (defaults: 100, 24h). |
| `template` | fired template for the runner sandbox (default `runner.template`). |

## Publishing

An artifact is created with its first version, and each later publish adds
one. Both are one call:

```
POST /v1/artifacts                {"id": "weekly-report", "quota_key": "u1", "meta": {...},
                                   "publish": {"files": [...], ...}}          → v1
POST /v1/artifacts/{id}/versions  {"base_version": 7, "files": [...], ...}   → v8
```

`id` is optional (one is generated). Your backend can choose it; an ID
that's taken fails with `409 exists`. Large files go up as blobs first, so
the publish refers to them by hash:

```
POST /v1/blobs/missing  {"hashes": ["<sha256>", ...]}   → {"missing": [...]}
PUT  /v1/blobs/<sha256>  (raw bytes, only the missing ones)
```

To publish a sandbox folder, use a `from_sandbox` op (below): the service
pulls only the files it doesn't have, so republishing a 200-file build where
one file changed copies one file.

```json
{
  "base_version": 7,
  "replace_dirs": ["app"],
  "files": [
    {"path": "app/index.js", "hash": "9f2c…"},
    {"path": "README.md", "content": "# Weekly report\n"},
    {"path": "logo.png", "content_base64": "iVBOR…"},
    {"path": "run.sh", "content": "#!/bin/sh\n…", "mode": 493},
    {"path": "old.css", "delete": true}
  ],
  "spec": {"views": [{"type": "html", "entry": "app/index.html"}]},
  "label": "add weekly chart",
  "run_now": ["refresh"],
  "notify": "session-8f2c"
}
```

- **`base_version` must be the current version,** or the publish fails with
  `409 conflict`. Read the latest, merge, publish again. In a create, leave
  it out.
- **A version always has at least one file.** A create without files, or a
  publish that deletes every file, fails with `400`.
- **Files are a patch:** listed files are added or replaced, the rest are
  kept, `delete` removes one. Later entries win for the same path.
  `replace: true` starts from an empty tree instead.
- **`replace_dirs` mirrors folders:** they are emptied before `files` apply,
  so files that disappeared from a build folder disappear from the artifact.
- **Each file op sets exactly one of** `hash` (an uploaded blob), `content`,
  `content_base64`, `delete` or `from_sandbox` (below). Inline content is
  capped by `limits.max_inline_bytes`. `mode` is 0644 (default) or 0755.
- **`spec` writes `artifact.json`.** Passing both `spec` and an
  `artifact.json` file is an error. Without either, the previous
  `artifact.json` carries over and is validated again against the new files
  (a view whose entry was deleted fails the publish).
- **All or nothing:** any invalid path, missing blob, invalid spec or limit
  breach means no version is created.
- **Limits:** `limits.max_files_per_version`, `limits.max_version_bytes`, and
  the quota key's `storage_bytes`, counted over distinct blobs.
- **`run_now`** triggers those jobs right after the publish, so an agent
  learns quickly whether a job works. A blocked job isn't triggered; its
  status in the response says why.
- **`notify`** is an opaque target, e.g. an agent session. The first run of
  this version emits its `run.finished` event with `notify` set, so your
  backend can tell that session whether it succeeded.

The response is the new version plus the status of every job (see below),
e.g. `"state": "blocked", "needs_approval": ["timeout 2h exceeds the allowed
max run 30m0s"]`. The publish itself never waits for approval.

### Publishing from a sandbox

A `from_sandbox` op copies a folder (or one file) from a fired sandbox and
mirrors it at `path` (`""` or `"."` is the artifact's root): files under
`path` that the source doesn't have are removed, like `replace_dirs`.

```json
{"path": "site", "from_sandbox": {"id": "sb-…", "path": "/workspace/dist",
  "exclude": ["node_modules", ".git"], "fired_url": "http://10.0.1.6:8443"}}
```

1. The service calls fired's `GET /files/scan`. fired checks every file
   against the secret values passed to that sandbox's commands, and hashes
   the same bytes. Any match fails the publish with `422 secret_found`
   (the file and the secret's name, never its value).
2. It asks its own store which hashes are missing, and downloads only those
   (8 at a time).
3. Each download must hash to the scanned value. A file that changed after
   the scan fails the publish with `409 changed`; publish again. Files
   whose scanned content the store already has aren't downloaded, so what
   is published is exactly what was scanned.

`fired_url` names the fired host the sandbox is on (default: `fired.url`).
The service sends its fired token there, so all hosts share one token, and
your backend must only pass URLs it chose, never one from an agent. Modes
come from the sandbox (executable becomes 0755). A missing sandbox or path
is `404`; other fired errors are `502 fired_error`. The secret check only
knows secrets fired was given since it started, and only with redaction
on; like the run check, it catches accidents and is not a boundary.

Other sources (MCP resources, tool outputs, inline text) are resolved by
your backend's artifact tool into blobs and file ops, applying its own
ignore rules and secret checks; runs are scanned by the service itself (see
Runs).

## Serving, versions, forks

- **Viewing through your domain (header routing):** your backend owns the
  domain artifacts are viewed on and decides who may see what. It forwards
  a viewer's request unchanged (same path and query) with the API token
  and `X-Artifact: <id>`, plus optionally `X-Artifact-Version: <n>` (absent
  or `current` follows the current version). The path is a file of the
  artifact (`/` serves `index.html`), or `/.fired/view?type=…&entry=…` for
  the view renderer (md, csv, json, text, react). Responses are the same as
  the file API (ETag, `X-Artifact-Version`, cache headers); only GET and
  HEAD are allowed. Strip these headers from what the viewer's browser
  sends, and set your own security headers (such as `frame-ancestors`) at
  your edge.

- **Files:** `GET /v1/artifacts/{id}/files/{path}?version=N` serves one file,
  current version by default. A path that is empty or ends in `/` serves its
  `index.html`. Responses carry `ETag` (the blob hash) and
  `X-Artifact-Version`. With `?version=` they are cacheable forever; without
  it, `no-cache`. Reads are rate limited per artifact (`limits.serve_rps`).
  Serve artifact files to viewers from a separate registrable domain, since
  they are user-generated HTML and JS.
- **Restore:** `POST /v1/artifacts/{id}/restore {"version": 3,
  "base_version": 9}` creates v10 with v3's files. History stays linear, and
  the restore itself can be undone the same way.
- **Fork:** `POST /v1/artifacts/{id}/fork {"quota_key": "u2", "version": 4}`
  creates a new artifact whose v1 has v4's files (blobs are shared, nothing
  is copied). Its jobs come along in `artifact.json`, but schedules are off
  until the new owner publishes or a grant turns them on. Run state is not
  copied.
- **Export:** `GET /v1/artifacts/{id}/versions/{v}/export` is a zip of the
  version, `artifact.json` included.

## Jobs

### When a job may run

Every job has a status:

| `state` | Meaning |
|---|---|
| `scheduled` | Has a schedule, and its window is open. |
| `manual` | No schedule; runs when triggered. |
| `expired` | Has a schedule, but the window has closed. Publishing re-opens it. |
| `blocked` | Its `timeout` exceeds the allowed max run; nothing runs until approved. |

- **Windows:** each publish (or restore) opens the schedule's window for
  `active_for`, capped at `limits.default_active_for`. Like a sandbox's idle
  timer, publishing is activity. A forgotten automation stops on its own
  instead of running indefinitely.
- **Longer windows and runs need a grant.** A job asking for more than the
  defaults is listed with `needs_approval`. A longer `active_for` still runs
  within the default window meanwhile; a longer `timeout` blocks the job.
  When the owner approves, your backend sets the grant:

  ```
  PUT /v1/artifacts/{id}/jobs/{job}/grant  {"active_until": "2026-11-10T00:00:00Z", "max_run": "2h"}
  ```

  A zero grant revokes it.

### Triggers and `on_busy`

A trigger is a schedule firing, `run_now` on publish, or
`POST /v1/artifacts/{id}/jobs/{job}/runs {"payload": {...}}`. Each creates a
**pending** run. Whether it starts right away depends on the limits below;
until then, `on_busy` decides what happens:

- **`latest`:** a new trigger replaces the pending run (`superseded`). Right
  for refreshes and polling: after a backlog, one run with the newest
  trigger is enough.
- **`queue`:** triggers wait in order. When `max` is reached, a new trigger is
  refused with `429 queue_full` (or recorded as `dropped` if it was a
  schedule). Pending runs older than `max_age` are dropped as `expired`.

A schedule missed while the service was down fires once, not once per missed
slot.

### Limits

A pending run starts only when all of these allow it:

- one run at a time per artifact;
- `limits.max_concurrent_runs`, the runs this service drives at once;
- **fired admits the runner sandbox** against the quota key's compute
  limits in `runner.pool` (default `artifacts`): memory at once and
  GB-hours per day, counted across every fired host and separate from the
  user's other sandboxes. See fired's [cluster.md](cluster.md). A refusal
  (`429 quota_exceeded`) puts the run back to pending, and that key's runs
  wait `runner.quota_backoff` (1m) before trying again; a full host
  (`503`) is retried on the next tick.

Separately, schedule firings closer together than the quota key's
`min_interval` (and `limits.min_interval`) are skipped.

This service's per-key limits are storage and `min_interval`, from
**tiers**. Which tier a key is in is fired's single mapping
(`PUT /v1/quotas/{key} {"tier": "pro"}` on fired); this service reads it
(cached for a minute, and the last known tier is used if fired can't be
reached) and applies its own definition of that tier:

- `PUT /v1/tiers/{name}` with `{"limits": {"storage_bytes": 21474836480,
  "min_interval": "1m"}}`; `GET /v1/tiers`, `GET`/`DELETE /v1/tiers/{name}`.
- Resolution: the key's own override (`PUT /v1/quotas/{key}`, for
  exceptions), else its tier's value, else the `default` tier's, else no
  limit. Negative disables a limit.
- `GET /v1/quotas/{key}` shows the tier, the override, the effective
  limits and storage used.

Compute limits (memory, GB-hours) are fired's tiers.

### Runs

Each artifact has one **runner sandbox** in fired, labelled
`fired-artifacts.artifact=<id>`, created in `runner.pool` and charged to the
artifact's quota key. It is only a cache: if it's gone, broken, on a host
that's down, or made for another template, quota key or pool, a new one is
created. A run:

1. **Gets credentials:** POSTs `{artifact, quota_key, labels, version, job,
   run_id}` to `broker.url`, and gets `{secret_env, secret_files}` back. A
   refusal fails the run with the broker's error code.
2. **Sets up the sandbox:**
   - empties `runner.workdir` (`/artifact`) and unpacks the current version's
     files into it;
   - unpacks the job's saved state into `state`;
   - writes the trigger to `/tmp/fired-artifacts/trigger.json` as
     `{type, run_id, job, at, payload}`.
3. **Starts the job** as a background process. Environment:
   - `ARTIFACT_ID`, `ARTIFACT_VERSION`, `ARTIFACT_JOB`, `ARTIFACT_RUN_ID`,
     `TRIGGER`, `TRIGGER_FILE`;
   - the credentials as fired `secret_env` / `secret_files`, so they are
     never stored and are redacted from output.
4. **Follows its output** until it exits or hits its timeout. A timed-out
   run gets `SIGTERM`, then `SIGKILL`.
5. **On success, reads back `writes` and `state`.** If any written file
   contains a credential value (or its base64 or URL encoding), the result
   is rejected and the run fails: artifacts get shared, so a run must never
   publish the credentials it ran with.
6. **Commits a new version (`origin: run`),** applying the written paths on
   top of the *current* version, so an edit published during the run is
   kept. If nothing changed, no version is created and `result_version`
   stays 0. State is saved only for successful runs.

Run statuses:
- `pending`, `running`;
- `succeeded`, `failed` (with `reason`, e.g. `exit status 3`), `timed_out`;
- `dropped`, `superseded`.

If fired is out of capacity (`503`), the run goes back to pending and is
retried. `GET /v1/artifacts/{id}/runs/{run}/logs` returns the tail of
stdout and stderr (`runner.log_bytes` each, with credential values
redacted).

### Events

`GET /v1/events` (SSE, `?artifact=` to filter):

| Event | When |
|---|---|
| `artifact.created`, `artifact.deleted` | |
| `artifact.version` | Every new version, with `origin`; for runs, also `job` and `run_id`. |
| `run.started` | A run left `pending`. |
| `run.finished` | With `status`, `reason`, and `notify` if the publish asked for it. |
| `run.dropped` | With `reason`: `queue_full`, `expired`, `job_removed`, `needs_approval`. |

Monitors in your backend subscribe to an artifact's events and forward
what matters to sessions.

## Retention and garbage collection

- **Run-produced versions:** each artifact keeps its newest `retain_runs`
  (artifact setting, else `limits.retain_runs`, 24) besides the current
  version. Edit, restore and fork versions are always kept.
- **Inactive artifacts:** with `delete_after` set (on the artifact, else
  `limits.delete_after`; 0 means never, negative means never for that
  artifact), an artifact with no reads, publishes or runs for that long is
  deleted, along with its runner sandbox.
- **Run records:** finished runs and their logs are kept for 30 days.
- **Blobs:** blobs that no version, state or log references are deleted on
  the next GC (`gc_interval`). Blobs younger than 2 hours are kept, which
  covers uploads waiting for their publish.

## API

All endpoints are under `/v1`. They need `X-Fired-Token: <token>` or
`Authorization: Bearer <token>`. Errors look like fired's:
`{"error": {"code", "message"}}`. A Go client is in `pkg/artifacts`.

| | |
|---|---|
| Blobs | `POST /blobs/missing`, `PUT /blobs/{sha256}` |
| Artifacts | `POST /artifacts` (with `publish`, the first version), `GET /artifacts?quota_key=&label=k=v`, `GET/PATCH/DELETE /artifacts/{id}` |
| Versions | `POST /artifacts/{id}/versions` (publish), `GET /artifacts/{id}/versions`, `GET /artifacts/{id}/versions/{n\|current}` (with files), `GET …/versions/{n}/export` |
| History | `POST /artifacts/{id}/restore`, `POST /artifacts/{id}/fork` |
| Files | `GET /artifacts/{id}/files/{path}?version=` |
| Jobs | `GET /artifacts/{id}/jobs`, `PUT /artifacts/{id}/jobs/{job}/grant`, `POST /artifacts/{id}/jobs/{job}/runs` |
| Runs | `GET /artifacts/{id}/runs?job=&status=&limit=`, `GET /artifacts/{id}/runs/{run}`, `GET …/runs/{run}/logs` |
| Quotas | `GET/PUT /quotas/{key}` |
| Ops | `GET /events` (SSE), `GET /healthz` |

## Running it

```bash
make dev            # fired with the fake driver, in one terminal
curl -H "X-Fired-Token: dev" -d '{"name":"base"}' http://127.0.0.1:8443/v1/templates
make dev-artifacts  # fired-artifacts on :8444, token "dev", in another
```

`make dev-artifacts` turns on the console (`ui: true` or
`FIRED_ARTIFACTS_UI=1`) at http://127.0.0.1:8444/ui/. Sign in with the token.
It lists artifacts with their version history, edits and uploads files and
publishes them, shows views, compares and restores versions, forks, runs jobs
(with payloads), approves grants, shows runs with their logs, edits quotas, and
follows `/v1/events` live. The home page has examples that cover each
feature: views, scheduled jobs with state, a queue, approval and a failing job.

The console shows artifact files on their own origin (for the console only;
in production your backend serves them with header routing, above),
`<id>-<version>-<sig>.<preview_domain>` (version 0 follows the current
version), so pages run at `/` with working relative paths and can't reach the
console's session. Without `preview_domain` (`FIRED_ARTIFACTS_PREVIEW_DOMAIN`)
it uses nip.io for the IP you opened it on, as fired's console does. Preview
origins also serve `/.fired/view`, a reference viewer that renders `md`,
`csv`, `json`, `text` and `react` views in the browser. A `react` entry and
the files it imports by relative path are compiled with Babel, and
packages come from esm.sh. Your own viewer can do the same or something
better.

The production image contains the binary too:
`docker run --entrypoint fired-artifacts fired:latest serve`. Configure it
with `deploy/artifacts.yaml` (every key is documented there) and env
overrides. Point `data_dir` at durable storage, give `runner.egress` the
hosts your jobs need, and set `broker.url` to your backend's credentials
endpoint.

## Not built yet

These were discussed but are not settled enough to build:

- **View rendering in the service:** views are metadata, and the API serves
  files raw. Only the console's reference viewer renders them (see above).
- **Webhook triggers:** need a decision on how incoming requests are
  authenticated (a signing secret per job, and where it lives). The `queue`
  policy and trigger payloads are already in place for them.
- **A `batch` on_busy mode** that hands one run every payload collected
  since the last run.
- **Per-job egress allowlists and resources.** All runner sandboxes use
  `runner.*` today.
- **Concurrent jobs within one artifact:** runs are one at a time per
  artifact.
- **Cancelling** pending or running runs.
- **Storage:** blobs are loose files on local disk. Packing small blobs and
  an object-storage backend are the next steps for scale and durability.
