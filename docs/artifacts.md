# Artifacts

`fired-artifacts` is a separate service that stores **artifacts**: versioned
sets of files that agents publish, that people view and share, and that can
carry a **job** which runs on a schedule in a fired sandbox and keeps the
artifact's data fresh in its **db**, a small document store its page reads. A page, a deck, a dashboard that refreshes every
morning and an automation with no output at all are the same thing here:
files, plus `artifact.json` saying how to view them and what to run.

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
| serving each artifact's view (its entry and folder), raw files, zip export | the domain viewers use, who may see what, rendering single-file views (copy, download, …) |
| scheduler, `on_busy` queues, per-quota-key and global limits | approving grants (longer schedules, longer runs), quotas per plan |
| runner sandboxes, each artifact's db, logs, events | credentials for each run, MCP tools, notifications to sessions, who owns an artifact |
| retention, `delete_after`, garbage collection | turning sandbox folders, MCP resources and tool outputs into files to publish |

It is a separate binary because its data has a different life from
sandboxes: artifacts are kept and shared, so run it next to your backend on
durable storage. Its records (artifacts, versions, db documents, runs) are in
Postgres, normally fired's database in a schema of its own (`artifacts`),
and file contents in a blob store on disk. Runs happen on whichever fired
host it points at.

## Concepts

- **Artifact**: an ID, a quota key, opaque `labels` and `meta` (anything
  your backend wants to keep with it; not exported, forked or shown to
  the job), and a current version. Its `title` and `description` are the
  current version's, from `artifact.json`. It is created together with its
  first version, so it always has files.
- **Version**: an immutable file tree, what the author made. Versions are
  numbered 1, 2, 3… and never change. Each has an `origin`:
  - `edit`: a publish
  - `restore`: a copy of an older version
  - `fork`: the first version of a fork
- **Job and db**: an artifact has at most one job. It can't change the
  artifact (versions are what the author made); what it keeps or produces
  goes in the artifact's **db**, JSON documents its page reads. The owner
  writes the db too (from the page, or your backend); everyone else the
  artifact is shared with only reads it. See The db.
- **File tree**: paths (relative, slash-separated) to blobs. Each blob is
  stored once, named by the SHA-256 of its content and zstd-compressed, so
  unchanged files cost nothing across versions, forks and artifacts. Large
  files are stored as chunks, so a changed large file costs only its
  changed chunks (see Publishing).
- **`artifact.json`**: an ordinary file in the tree, so it is versioned,
  exported and forked with everything else. Every version has one: when a
  publish doesn't bring one, the current one is kept, and a new artifact
  gets an empty one (`{}`, plus a title if the create sets one). It can't
  be deleted. Nothing in it is inferred from the files.

## artifact.json

```json
{
  "title": "Weekly sales",
  "description": "Refreshed every weekday morning",
  "view": {"type": "page", "entry": "dist/index.html"},
  "job": {
    "run": "python refresh.py",
    "schedule": "0 9 * * 1-5",
    "timezone": "Asia/Kolkata",
    "timeout": "10m"
  }
}
```

Decoding is strict: an unknown field (say `"schedul"`) fails the publish
instead of being ignored.

**`title` and `description`** name the artifact (up to 200 and 2000
bytes). They're versioned like everything else here: renaming makes a
version, and a restore or fork brings the old name along. A zip of a
version is therefore complete: published again elsewhere, it comes back
with its name, view and job (not its db, which isn't versioned).

**The view** is what viewers see: `{"type", "entry"}`, one entry file and
its type. The type says who renders it:

| `type` | `entry` | Rendered by |
|---|---|---|
| `page` | `.html`, `.htm` | the page itself: the agent's HTML, scripts and styles |
| `markdown` | `.md`, `.markdown` | your frontend |
| `csv` | `.csv` | your frontend |
| `json` | `.json` | your frontend |
| `code` | any text file | your frontend (as source, language from the extension) |
| `svg` | `.svg` | your frontend |
| `image` | `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp` | your frontend |

A **page** is like Claude's artifacts: a page plus supporting files. The
agent builds everything shown (a dashboard fetches its CSV, a React app
loads React), and sees exactly what viewers see. Every **other type** is
one file whose UX the platform owns: your frontend renders Markdown, a
table or source the same way for every artifact, with its own copy,
download and so on, and improves it in one place. (Structured types such
as slides, where the frontend owns a richer UX, are planned.)

Viewers reach the entry and the other files of its folder at their paths:
a page's other pages (`about.html`, `docs/setup.html`), scripts, styles
and data; a Markdown file's images. Plus `/.fired/`, which is the
service's: the db and its client (see The db). Nothing else outside that
folder is reachable, so a build
in `dist/` can sit next to its sources; there are no index files (link to
`docs/index.html`, not `docs/`), and `artifact.json` isn't served.

The view is only what `artifact.json` says: without `"view"`, there is
nothing to view, even with an `index.html`. The entry must be a file of
its type in the version; files coming and going never change the view,
and an entry that goes away fails the publish.

**The job** (one per artifact):

| Field | Meaning |
|---|---|
| `run` | Command, run with `/bin/sh -c` in the artifact's directory. Required. |
| `schedule` | 5-field cron (`minute hour day month weekday`) or `@hourly`, `@daily`, `@weekly`, `@monthly`, `@yearly`. Empty means the job only runs when triggered. |
| `timezone` | IANA zone for the schedule (default UTC). |
| `timeout` | Longest run. Default and maximum without approval: `limits.default_max_run` (30m). |
| `active_for` | How long the schedule stays on after a publish. Default and maximum without approval: `limits.default_active_for` (12h). |
| `on_busy` | `latest` (default): at most one pending run, a new trigger replaces it. `queue`: every trigger waits its turn. |
| `queue` | `{max, max_age}` for `queue` (defaults: 100, 24h). |
| `template` | fired template for the runner sandbox (default `runner.template`). |

The job runs against the version's files and can't change them: its
changes to files are discarded when it ends. What it keeps (a cursor, a
count) and what it produces (results the page shows) go in the db, which
it reaches at `$FIRED_DB_URL` with `$FIRED_DB_TOKEN`.

## Publishing

An artifact is created with its first version, and each later publish adds
one. Both are one call:

```
POST /v1/artifacts                {"id": "weekly-report", "quota_key": "u1",
                                   "publish": {"title": "Weekly report", "files": [...], ...}}   → v1
POST /v1/artifacts/{id}/versions  {"base_version": 7, "files": [...], ...}   → v8
```

`id` is optional (one is generated). Your backend can choose it; an ID
that's taken fails with `409 exists`. Large files go up as blobs first, so
the publish refers to them by hash:

```
POST /v1/blobs/missing  {"hashes": ["<sha256>", ...]}   → {"missing": [...]}
PUT  /v1/blobs/<sha256>  (raw bytes, only the missing ones)
```

**Large files are chunked.** A file of 1 MiB or more is stored as
content-defined chunks (FastCDC, `fastcdc-v1.0.0`: 256 KiB min, 1 MiB
average, 4 MiB max), each chunk a blob. An edit or an append changes one
or two chunks; the rest are shared with earlier versions and with any
file, artifact or user that has them. A file is still named by the SHA-256
of its whole content everywhere (publish, ETags, `from_sandbox`), and
storage counts the distinct chunks a quota key references. To send only
the changed chunks of a large file:

```
POST /v1/blobs/missing        {"hashes": ["<file>", "<chunk>", ...]}
PUT  /v1/blobs/<chunk>        (each missing chunk's bytes)
PUT  /v1/blobs/<file>/chunks  {"chunks": [{"hash", "size"}, ...]}   (in order)
```

The service checks every chunk is uploaded and that, in order, they hash
to `<file>` (`422 missing_blob` / `hash_mismatch` otherwise). Cut chunks
with `pkg/artifacts.SplitChunks` (the Go client's `Upload` and
`UploadContent` do all of this), or chunks won't match stored ones; any
cut is accepted, it just shares less. A whole large file `PUT` as one blob
is chunked by the service, so it shares storage the same way, only the
upload isn't smaller.

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
  "spec": {"view": {"type": "page", "entry": "app/index.html"}},
  "label": "add weekly chart",
  "run_now": true,
  "notify": "session-8f2c"
}
```

- **`base_version` must be the current version,** or the publish fails with
  `409 conflict`. Read the latest, merge, publish again. It is required
  (`400` when left out), except in a create, where it's left out.
- **A publish that changes nothing makes no version.** When the result has
  exactly the current version's files (same paths, hashes and modes), the
  answer is `200` with `"unchanged": true` and the current version, and no
  `artifact.version` event is sent. Its `label` and `labels` are dropped;
  `run_now` and `notify` still apply, to the current version.
- **A version always has at least one file**, and `artifact.json` counts (a
  job-only artifact is fine). A create or `replace` without files fails
  with `400`, and so does deleting `artifact.json`. Files that leave it out
  (`replace`, a folder mirrored at the root) keep the current one.
- **Files are a patch:** listed files are added or replaced, the rest are
  kept, `delete` removes one. Later entries win for the same path.
  `replace: true` starts from an empty tree instead.
- **`replace_dirs` mirrors folders:** they are emptied before `files` apply,
  so files that disappeared from a build folder disappear from the artifact.
- **Each file op sets exactly one of** `hash` (an uploaded blob), `content`,
  `content_base64`, `delete` or `from_sandbox` (below). Inline content is
  capped by `limits.max_inline_bytes`. `mode` is 0644 (default) or 0755.
- **`spec` writes `artifact.json`,** replacing it. Passing both `spec` and an
  `artifact.json` file is an error. Without either, the previous
  `artifact.json` carries over and is validated again against the new files
  (a view whose entry was deleted fails the publish).
- **`title` and `description` change just those fields** of `artifact.json`
  and keep the rest; they win over `spec` and over an `artifact.json` in the
  files. An agent publishing an unzipped export sets `title` to name its
  copy and keeps the zip's view and job. A publish with only `title` is a
  rename.
- **All or nothing:** any invalid path, missing blob, invalid spec or limit
  breach means no version is created.
- **Limits:** `limits.max_files_per_version` (`artifact.json` isn't
  counted), `limits.max_version_bytes`, the request body (twice
  `limits.max_inline_bytes` plus 1 MiB; over it is `413 too_large`), and
  the quota key's `storage_bytes`, counted over the distinct blobs and
  chunks its versions reference plus its db documents. `GET /quotas/{key}`
  shows that (`storage_used`, with `db_bytes`) next to `storage_logical`,
  every file of every version in full: the difference is what sharing
  saves.
- **`run_now: true`** triggers the job right after the publish (its runs
  say `trigger: publish`), so an agent learns quickly whether it works. A
  blocked job isn't triggered; its status in the response (`job`) says why.
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
  or `current` follows the current version). It gets the artifact's view:
  - `/` is the entry, as it is, with `X-Artifact-View: <type>` and
    `X-Artifact-View-Name: <file name>`. For a `page`, frame it and you're
    done; for other types, render it (and "copy source" is this response).
  - Other paths are files of the entry's folder; anything else is a 404
    (`no_view` when there's no view). 404s name only the URL path.
  - `?download` on any of them makes it an attachment with its file name.
  - `/.fired/db.js` and `/.fired/db/…` are the db, for the page. Add
    `X-Artifact-Owner: true` when the viewer owns the artifact: the owner
    writes the db and reads `_private/`; everyone else only reads the rest.
    A view pinned to a version (`X-Artifact-Version: <n>`) is read-only
    for everyone, the owner included: the db isn't versioned, so an old
    page mustn't write an old shape of data into it.

  - `/.fired/job` is the job's status, for anyone; the owner runs it with
    `POST /.fired/job/runs`.

  Responses are the same as the file API (ETag, `X-Artifact-Version`,
  cache headers: a pinned version is cacheable forever); only GET and HEAD
  are allowed, except the owner's db writes and job runs.

  **Your viewing domain, as a checklist** (tried in playground 11 with a
  stand-in proxy):
  - **Its own registrable domain**, not your app's (`artifacts.example`
    next to `example.com`), and **one origin per artifact**
    (`<id>.artifacts.example`; a pinned version gets its own,
    `<id>--v3.…`): pages are user-generated code, and an origin is what
    keeps one from reading another's data.
  - **Strip** `X-Artifact`, `X-Artifact-Version`, `X-Artifact-Owner`,
    `X-Fired-Token` and `Authorization` from what the browser sends, then
    set them yourself. The service trusts them: a forwarded
    `X-Artifact-Owner` from a viewer would make them the owner. Don't
    forward your cookies either; the service doesn't use them.
  - **Decide the owner** from your own session (a cookie on the viewing
    domain), per request.
  - **Add `Content-Security-Policy: frame-ancestors`** for the origins
    allowed to frame artifacts (your app), so another site can't frame one
    and click through it.
  - **Never add CORS headers** to these responses. Owner writes go to the
    page's own origin as JSON (`415` otherwise), so a sibling artifact's
    page (same site, so it gets your session cookie too) can't write
    another's db: its JSON requests need a preflight nobody approves, and
    forms, beacons and `no-cors` requests aren't JSON. Tried: all refused.
  - **Render single-file views** (`X-Artifact-View` other than `page`)
    in your frontend; their other files (a Markdown file's images) come
    from the same origin.
  - **Turn 404s into your own page**: they're JSON
    (`not_found`, `no_view`).

- **Files:** `GET /v1/artifacts/{id}/files/{path}?version=N` serves any one
  file, current version by default: the owner's raw access, not the view.
  There are no index files; `?download` makes it an attachment. Responses carry `ETag` (the blob hash) and
  `X-Artifact-Version`. With `?version=` they are cacheable forever; without
  it, `no-cache`. Reads are rate limited per artifact (`limits.serve_rps`).
  Serve artifact files to viewers from a separate registrable domain, since
  they are user-generated HTML and JS.
- **Restore:** `POST /v1/artifacts/{id}/restore {"version": 3,
  "base_version": 9}` creates v10 with v3's files. History stays linear, and
  the restore itself can be undone the same way. Both fields are required,
  and a restore to the files the current version already has is
  `"unchanged": true`, like a publish.
- **Fork:** `POST /v1/artifacts/{id}/fork {"quota_key": "u2", "version": 4}`
  creates a new artifact whose v1 has v4's files (blobs are shared, nothing
  is copied) and, if v4 is the current version (the default), a copy of
  the source's db except `_private/`, which was the source owner's. A fork
  of an older version starts with an empty db: the db is the current
  version's data, which an older page may not understand.
  `"include_private": true` copies `_private/` too, for an owner
  duplicating their own artifact (see What your backend offers). `"title"`
  renames the fork in its `artifact.json`. Which versions someone may fork
  is your backend's call (say, the latest of an artifact shared with them,
  any of their own); the fork's storage, documents included, counts
  against its new quota key.
  The job comes along, but its schedule is off until the new owner
  publishes or a grant turns it on.
- **Export:** `GET /v1/artifacts/{id}/versions/{v}/export` is a zip of the
  version's files, `artifact.json` (title, description, view, job)
  included; the db isn't versioned, so it isn't in it. Published again, it's the same artifact; `meta`, labels and
  the quota key aren't in it, since they belong to whoever owns it.
- **Compare:** there's no compare endpoint; `GET …/versions/{n}` lists
  every file with its hash, so two versions diff by path and hash. The
  console does this.

## The job

### When a job may run

Every job has a status:

| `state` | Meaning |
|---|---|
| `scheduled` | Has a schedule, and its window is open. |
| `manual` | No schedule; runs when triggered. |
| `expired` | Has a schedule, but the window has closed. Publishing re-opens it. |
| `blocked` | Its `timeout` exceeds the allowed max run; nothing runs until approved. |

- **Windows:** each publish (or restore) that makes a version opens the
  schedule's window for `active_for`, capped at
  `limits.default_active_for` (a publish that changes nothing doesn't).
  Like a sandbox's idle timer, publishing is activity. A forgotten automation stops on its own
  instead of running indefinitely.
- **Longer windows and runs need a grant.** A job asking for more than the
  defaults is listed with `needs_approval`. A longer `active_for` still runs
  within the default window meanwhile; a longer `timeout` blocks the job.
  When the owner approves, your backend sets the grant:

  ```
  PUT /v1/artifacts/{id}/job/grant  {"active_until": "2026-11-10T00:00:00Z", "max_run": "2h"}
  ```

  The grant is replaced as a whole (`{}` revokes it); an `active_until` in
  the past is no grant. It's a budget for the artifact, not an approval of
  one command: the job can change and stay approved as long as it fits
  (a `timeout` above `max_run` blocks it again). The service puts no upper
  bound on it: your backend decides what an owner may approve.

### Triggers and `on_busy`

A trigger is a schedule firing (`trigger: schedule`), `run_now` on publish
(`publish`), `POST /v1/artifacts/{id}/job/runs {"payload": {...}}`
(`manual`), or the owner's page (`page`, see The db). A blocked job refuses
triggers (`409 needs_approval`); one whose schedule window has closed
still runs when triggered. Each creates a
**pending** run. Whether it starts right away depends on the limits below;
until then, `on_busy` decides what happens:

- **`latest`:** a new trigger replaces the pending run (`superseded`). Right
  for refreshes and polling: after a backlog, one run with the newest
  trigger is enough.
- **`queue`:** triggers wait in order. `max` counts the waiting runs (not
  the one running); when it's reached, a new trigger is refused with
  `429 queue_full` (or recorded as `dropped` if it was a schedule). Runs
  waiting longer than `max_age` are dropped as `expired` (checked every
  scheduler tick).

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
`min_interval` (and `limits.min_interval`) are skipped. A job's `next_run`
already accounts for that (it's the next slot that will fire), and is left
out when that's after the window closes.

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

1. **Gets credentials:** POSTs `{artifact, quota_key, labels, version,
   run_id}` to `broker.url`, and gets `{secret_env, secret_files}` back. A
   refusal fails the run with the broker's error code; a broker that can't
   be reached or answers `502`/`503`/`504` makes the run wait, like fired
   (below).
2. **Sets up the sandbox:**
   - empties `runner.workdir` (`/artifact`) and unpacks the current
     version's files into it;
   - writes the trigger to `/tmp/fired-artifacts/trigger.json` as
     `{type, run_id, at, payload}`.
3. **Starts the job** as a background process. Environment:
   - `ARTIFACT_ID`, `ARTIFACT_VERSION`, `ARTIFACT_RUN_ID`,
     `TRIGGER`, `TRIGGER_FILE`;
   - `FIRED_DB_URL` (`<runner.db_url>/v1/artifacts/<id>/db`) and, as a
     secret, `FIRED_DB_TOKEN`, valid for this artifact's db while the run
     runs (when `runner.db_url` is set);
   - the credentials as fired `secret_env` / `secret_files`, so they are
     never stored and are redacted from output. `secret_files` are keyed
     by file name (`{"key.pem": "…"}`, no paths): the job finds them in
     `$SECRET_DIR` (mode 0600).
4. **Follows its output** until it exits or hits its timeout. A timed-out
   run gets `SIGTERM`, then `SIGKILL`.
5. **Records the outcome**, with `db_writes`, the documents it wrote or
   deleted. Versions don't move, so edits published during the run, or by
   an agent working on the page meanwhile, never conflict with it. Its db
   writes land as it makes them, so a run that fails halfway leaves what
   it wrote: write in a batch for all or nothing.

A db write by a job that contains one of the run's credential values (or
its base64, URL or JSON encoding, or any line of 16+ characters of a
multi-line one such as a key file) is refused (`422 secret`): artifacts get
shared, so a run must never publish the credentials it ran with.

Run statuses:
- `pending`, `running`;
- `succeeded`, `failed` (with `reason`, e.g. `exit status 3`), `timed_out`;
- `dropped`, `superseded`.

If fired can't be reached or is out of capacity (a connection error, `502`,
`503`, `504`) before the job starts, the run goes back to pending with
`reason: "waiting: …"`, and runs wait 15s before trying again. If fired
becomes unavailable while the job runs, the runner keeps following the same
process (a restarting fired snapshots its sandboxes, processes included)
until the run's timeout. When a run ends, whatever its job left in its
process group is killed. `GET /v1/artifacts/{id}/runs/{run}/logs` returns the tail of
stdout and stderr (`runner.log_bytes` each, with credential values
redacted).

### Events

`GET /v1/events` (SSE, `?artifact=` to filter):

| Event | When |
|---|---|
| `artifact.created`, `artifact.deleted` | |
| `artifact.version` | Every new version, with `origin`. Not sent for an unchanged publish or restore. |
| `db.changed` | Documents were written or deleted (`paths`). |
| `run.started` | A run left `pending`. |
| `run.finished` | With `status`, `reason`, and `notify` if the publish asked for it. |
| `run.dropped` | With `reason`: `queue_full`, `expired`, `job_removed`, `needs_approval`. |

Monitors in your backend subscribe to an artifact's events and forward
what matters to sessions.

## The db

Every artifact has a db: JSON objects ("documents") at slash-separated
paths, like Firestore. A path with an even number of segments is a
document (`stats/today`, `tasks/t1/notes/n1`), an odd one a collection
(`tasks`, `tasks/t1/notes`): the documents directly in it. Segments are 1
to 128 of `A-Z a-z 0-9 _ . ~ @ + : -`, up to 16 deep. The db isn't
versioned: it's the artifact's live data.

**Who may do what** is fixed, not configured (artifacts are personal
apps: their owner shares what they made, others look):

| Caller | Reads | Writes |
|---|---|---|
| The API token (your backend, agents) | everything | everything |
| The owner, through a view with `X-Artifact-Owner: true` | everything | everything |
| The job, while it runs, with `$FIRED_DB_TOKEN` | its artifact's db | its artifact's db |
| Anyone else your backend lets view the artifact | all but `_private/` | nothing (`403 read_only`) |

`_private/` is the owner's and the job's: the job's working state and raw
data go there, and what it publishes for viewers goes anywhere else. To a
viewer, `_private/` doesn't exist. Shared data comes only from the owner
and their job, so a page can trust what it reads.

**The API** is the same under `/v1/artifacts/{id}/db/` (API token, or a
job's token) and under `/.fired/db/` in a view (the viewer's access):

| | |
|---|---|
| `GET …/db/{doc}` | `{path, data, rev, created_at, updated_at}`; `404 no_document` |
| `GET …/db/{collection}` | `{docs, next}`: `?where=field op value` (repeatable; `==` `!=` `<` `<=` `>` `>=`, a dotted field for nested keys, the value JSON or a bare string), `?order=field` or `-field` (missing values last), `?limit=` (100, at most 1000), `?after=` (the `next` of the previous page) |
| `GET …/db/` | every document, by path (a viewer's without `_private/`) |
| `GET …?watch` | the same as server-sent events: a `snapshot` now and after every change (a document is `null` while it doesn't exist) |
| `PUT …/db/{doc}` | sets it to the body, a JSON object |
| `PATCH …/db/{doc}` | merges the body into it (JSON merge patch: `null` removes a key), creating it |
| `DELETE …/db/{doc\|collection}` | deletes the document, or the collection and everything under it; `DELETE …/db` empties the whole db, `_private/` included (the owner's "reset data") |
| `POST …/db` | `{"writes": [{"op": "set"\|"update"\|"delete", "path", "data", "rev"}]}`, applied in order, all or none |

Every write bumps the document's `rev`. `If-Match: <rev>` (or a write's
`rev`) makes it conditional, `-1` meaning "only if it doesn't exist";
otherwise it fails with `412 rev_mismatch`. Range filters compare values
of the filter's type only (`n>2` skips `"n": "10"`).

Limits: `limits.max_doc_bytes` (1 MiB) per document, `limits.max_docs`
(10,000) per artifact, `limits.max_batch_writes` (500) per batch, and the
quota key's `storage_bytes`, which counts documents too.

**From a page,** `/.fired/db.js` is a client:

```js
import { db } from "/.fired/db.js";
const today = await db.doc("stats/today").get();          // {id, path, data, rev, …} or null
db.collection("tasks").where("done", "==", false).orderBy("due").onSnapshot(tasks => render(tasks));
if (db.canWrite) await db.doc("tasks/t1").update({ done: true });
await db.collection("tasks").add({ title: "New" });        // a new id
```

`db.canWrite` is true for the owner. A page's writes are JSON requests
(`415` otherwise), so another site's form can't post to it.

The same module runs the job from the owner's page (a "Refresh now"
button, a form whose input the job handles):

```js
import { job } from "/.fired/db.js";
const st = await job.status();             // the job's status (next_run, …), or null: anyone
if (job.canRun) await job.run({ q: "x" }); // the owner: a run with this payload, trigger "page"
```

That's `GET /.fired/job` and `POST /.fired/job/runs {"payload"}` (owner
only, `403 read_only` otherwise), the same as the API's trigger: the
payload is in `$TRIGGER_FILE`, `on_busy` applies, a blocked job refuses.

**From the job,** with any HTTP client:

```sh
curl -H "Authorization: Bearer $FIRED_DB_TOKEN" -X PUT -H 'Content-Type: application/json' \
  -d '{"runs": 3}' "$FIRED_DB_URL/_private/state"
```

Jobs reach the service through fired's egress proxy at `runner.db_url`,
which is added to the runner sandboxes' allowlist. It can't be a loopback
address (sandboxes don't proxy those), and a private one (a cluster
Service) must be in fired's `egress.allow_private_cidrs`. Without
`runner.db_url`, jobs have no db.

## What your backend offers its users

The service doesn't know who anyone is; these are the user-facing actions
it's built for, and the calls behind them. "Owner" is whoever your backend
says owns the artifact.

| The user… | Who | Your backend calls |
|---|---|---|
| opens an artifact | anyone it's shared with | header routing with `X-Artifact` (add `X-Artifact-Owner: true` for the owner); `X-Artifact-Version` for a link to a past version, which is read-only |
| looks at its history | the owner | `GET …/versions`, `…/versions/{n}`, a pinned view per version |
| restores a version | the owner | `POST …/restore` (the db stays as it is) |
| **forks** an artifact shared with them | anyone it's shared with | `POST …/fork {"quota_key": <theirs>}`: the latest version only (they don't see the others), its db without `_private/`, the job off until they publish, no grant |
| **duplicates** their own artifact | the owner | `POST …/fork {"quota_key": <theirs>, "version": n}`: any version; for the current one, `"include_private": true` keeps the job's state, so the copy's job carries on where the original left off (an older version's copy starts with an empty db) |
| **resets its data** | the owner | `DELETE …/db`: every document, `_private/` included; the job starts from nothing on its next run. A collection or a document: `DELETE …/db/{path}` |
| runs its job now | the owner | `POST …/job/runs` (or the page's `job.run`) |
| approves a longer job | the owner | `PUT …/job/grant` |
| deletes it | the owner | `DELETE /v1/artifacts/{id}`: versions, db, runs and its runner sandbox |

Never set `include_private` for someone else's fork: `_private/` is where
a job keeps what its owner doesn't show (raw data, cursors, anything
derived from the owner's credentials).

## Retention and garbage collection

- **Versions** are always kept; the db is kept with its artifact.
- **Inactive artifacts:** with `delete_after` set (on the artifact, else
  `limits.delete_after`; 0 means never, negative means never for that
  artifact), an artifact with no reads, publishes or runs for that long is
  deleted, along with its runner sandbox.
- **Run records:** finished runs and their logs are kept for 30 days.
- **Blobs:** blobs that no version or log references are deleted on
  the next GC (`gc_interval`). Blobs younger than 2 hours are kept, which
  covers uploads waiting for their publish.

## API

All endpoints are under `/v1`. They need `X-Fired-Token: <token>` or
`Authorization: Bearer <token>`. Errors look like fired's:
`{"error": {"code", "message"}}`. A Go client is in `pkg/artifacts`.

`PATCH /artifacts/{id}` changes settings (`quota_key`, `labels`, `meta`,
`delete_after`) and never makes a version. The title and
description aren't settings: publish `{"base_version", "title"}` to rename. Omitted fields are
left alone; `labels` and `meta` are merged as a JSON merge patch (RFC 7386):
`{"labels": {"stage": "review", "draft": null}}` sets `stage`, removes
`draft` and keeps the other labels.

| | |
|---|---|
| Blobs | `POST /blobs/missing`, `PUT /blobs/{sha256}`, `PUT /blobs/{sha256}/chunks` |
| Artifacts | `POST /artifacts` (with `publish`, the first version), `GET /artifacts?quota_key=&label=k=v`, `GET/PATCH/DELETE /artifacts/{id}` |
| Versions | `POST /artifacts/{id}/versions` (publish), `GET /artifacts/{id}/versions`, `GET /artifacts/{id}/versions/{n\|current}` (with files), `GET …/versions/{n}/export` |
| History | `POST /artifacts/{id}/restore`, `POST /artifacts/{id}/fork` |
| Files | `GET /artifacts/{id}/files/{path}?version=` |
| Job | `GET /artifacts/{id}/job`, `PUT /artifacts/{id}/job/grant`, `POST /artifacts/{id}/job/runs` |
| Db | `GET/PUT/PATCH/DELETE /artifacts/{id}/db/{path}`, `POST /artifacts/{id}/db` (batch), `DELETE /artifacts/{id}/db` (see The db) |
| Runs | `GET /artifacts/{id}/runs?status=&limit=`, `GET /artifacts/{id}/runs/{run}`, `GET …/runs/{run}/logs` |
| Quotas | `GET/PUT /quotas/{key}` |
| Ops | `GET /events` (SSE), `GET /healthz` |

## Running it

```bash
make dev-vm         # fired with real VMs, detached (or `make dev`, the fake driver)
curl -H "X-Fired-Token: dev" -d '{"name":"base"}' http://127.0.0.1:8443/v1/templates
make dev-artifacts  # fired-artifacts on :8444, token "dev", detached; same Postgres
```

`make dev-artifacts` turns on the console (`ui: true` or
`FIRED_ARTIFACTS_UI=1`) at http://127.0.0.1:8444/ui/. Sign in with the token.
It lists artifacts with their version history, edits and uploads files and
publishes them, shows each artifact's view (as its owner or as a viewer), previews files, compares and restores versions, forks, browses and edits
its db, runs the job (with payloads), approves grants, shows runs with their logs, edits quotas, and
follows `/v1/events` live. The home page has examples that cover each
feature: a page, a markdown view, a scheduled job writing the db, a batch, a queue, a checklist only its owner ticks, approval and a failing job.

The console shows a `page` view on the artifact's own origin (for the
console only; in production your backend serves it with header routing,
above), `<id>-<version>-<sig>.<preview_domain>` (version 0 follows the
current version), exactly as viewers get it: the page at `/`, its folder's
files next to it, and no way to reach the console's session. Other view
types it renders itself, as a frontend would, with Copy source and
Download. Without
`preview_domain` (`FIRED_ARTIFACTS_PREVIEW_DOMAIN`) it uses nip.io for the
IP you opened it on, as fired's console does.

The console's renderer (for those views, and the Files tab's previews of
`md`, `csv`, `json`, text, images, `svg` and `jsx`/`tsx`, compiled with
Babel) runs in a sandboxed frame with no origin and gets files from the
console, which reads them with the files API; Files-tab previews include
unpublished edits. It's a reference for your frontend's renderers.

The production image contains the binary too:
`docker run --entrypoint fired-artifacts fired:latest serve`. Configure it
with `deploy/artifacts.yaml` (every key is documented there) and env
overrides. Set `database_url` (fired's database is fine; it only uses its
own schema), point `data_dir` at durable storage, give `runner.egress` the
hosts your jobs need, and set `broker.url` to your backend's credentials
endpoint.

## Not built yet

These were discussed but are not settled enough to build:

- **Webhook triggers:** need a decision on how incoming requests are
  authenticated (a signing secret per job, and where it lives). The `queue`
  policy and trigger payloads are already in place for them.
- **A `batch` on_busy mode** that hands one run every payload collected
  since the last run.
- **Per-artifact egress allowlists and resources for the job.** All runner sandboxes use
  `runner.*` today.
- **Cancelling** pending or running runs.
- **Storage:** blobs are loose files on local disk. Packing small blobs and
  an object-storage backend are the next steps for scale and durability.
