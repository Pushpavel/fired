# 05. A scheduled job

Artifacts `u05-*` (quota keys `u05`, `u05w`, `u05d`), real Firecracker
runners, fired-artifacts on Postgres. Deleted afterwards.

## Tried

1. **Validation:** a cron of `61 * * * *`, timezone `Mars/Base`,
   `@every 5m`; a daily `30 9 * * *` in `Asia/Kolkata`.
2. **Run versions** (`u05-clock`, three every-minute jobs): `tick` writes
   the time under `data/`; `same` writes the same file every run; `sneaky`
   writes under its `writes` and also edits `index.html` and
   `artifact.json`. Then `retain_runs: 2`.
3. **Window and min interval** (`u05-win`): every-minute schedule,
   `active_for: 3m`, the key's `min_interval: 2m`; job status every 30s.
4. **Data, edits during runs, events** (`u05-data`): `grow` (every minute)
   writes a 5 MB CSV, then appends a row per run; `slow` (manual) sleeps 15s
   then writes, with an edit published while it runs; `/v1/events`
   recorded throughout.
5. **Downtime:** stopped fired-artifacts for ~2.5 minutes, twice.

## Saw

- Validation is clear: minute out of range, unknown time zone, "want 5
  fields … or a macro like @daily". 09:30 IST shows as 04:00 UTC next day.
- Each `tick` run made a version (origin `run`, labelled `run tick`);
  `same` made one on its first run and none after (`result_version` 0);
  `sneaky`'s `index.html` and `artifact.json` edits were dropped, only
  `sneak/` landed. Jobs of one artifact run one at a time; warm runs take
  30–600 ms.
- `retain_runs: 2` kept the newest two run versions plus the current one;
  the edit version stayed.
- The window: the job fired inside `active_for`, turned `expired` right
  after it ended, and fired nothing more.
- A publish during the 15s run was kept: the run committed on top of it
  (v4 has both the edit and the run's output).
- Chunking pays off here: six versions of the 5 MB CSV take 5.3 MB stored
  against 30 MB logical.
- Events: `run.started`, `artifact.version` (with `origin: run`, `job`,
  `run_id`) and `run.finished` per run, in order, with the mid-run edit's
  `artifact.version` between them.
- Missed slots while down fire once.

## Fixed

- **`next_run` lied under a minimum interval:** with `min_interval: 2m`
  and an every-minute schedule it said 08:09, but 08:09 was skipped
  (silently, at debug level). It's now the next slot that will fire.
- **`next_run` was shown past the window:** the IST job showed tomorrow's
  slot though its window closed tonight. It's left out now.
- **An every-minute schedule fired every other minute** (found while
  fixing the above): the scheduler reaches a slot a few seconds late, and
  measuring the interval from that time pushed the next slot past it.
  Intervals now allow for that (the scheduler interval + 1s)…
- **…and a catch-up after downtime no longer fires twice:** it fired at
  08:24:59 and the 08:25 slot fired a second later, despite a 1m minimum.
  Intervals count from when the firing happened, so the next slot waits.
- Docs: changes outside `writes` aren't "read-only", they're discarded.

## Decided: one job, and runs record outputs, not versions (2026-10-11)

Every changing run made a version. With an every-minute job that meant an
agent editing the page got `409 conflict` on every publish (a run had
moved the current version), the history was mostly runs (`u05-clock` hit
v16 in minutes), and restoring an edit rewound the data too.

- **One job per artifact:** `artifact.json` has `"job": {…}`, not a
  `"jobs"` map. The API lost the names: `GET /artifacts/{id}/job`,
  `PUT …/job/grant`, `POST …/job/runs`; runs have no `job`; `run_now` is
  `true`, and its runs say `trigger: publish`.
- **The job owns its `writes`.** A run that changes them records an
  **output** (`outputs`/`output_files`; `retain_outputs`, 24, replaces
  `retain_runs`). Versions are only edits, restores and forks.
- **What everyone sees** (viewers, files API, export, the job's next run,
  forks) is the version's files with the current output over the job's
  paths. Before the first output, or after `DELETE /artifacts/{id}/data`,
  the version's own files there show (the seed).
- **Publishing into the job's paths** while it has an output stores the
  files but the output still shows; the response lists them in
  `shadowed`. (Rather than a 400: an agent that pulls the artifact into a
  sandbox and mirrors it back carries the job's data with it.)
- `GET /artifacts/{id}/data`: the outputs, newest first, with files.
  `artifact.data` events replace `artifact.version` for runs.
- Console: one Job tab with the job's Data (outputs, reset), runs show
  `output N`, versions no longer have runs.

Tried live (real VMs): an every-minute job ran 3 times (version stayed 1,
output 3); an "agent" that read v1 before those runs published on v1 →
v2, with `data/now.txt` shadowed and the job's file still showing; reset
brought the agent's seed back. The clock and table examples render their
job's data from output 1 with one version each.

**Revised the same day: the job works in `.output/`.** The overlay above
(job-owned `writes` paths shown over the version's, seeds, `shadowed`,
reset) was a file-based imitation of a data store, with rules that only
existed because source and data shared paths. Looked at how Claude's live
artifacts do it: the page is versioned source, and data lives outside it
(a `db` the page reads and Claude writes, connectors pulled at view
time), never regenerated files. A data store is for later; for now:

- The job has one fixed folder, **`.output/`**: its results and its
  memory (counters, caches) together, so `writes` and `state` are gone and
  the job config is `run` plus scheduling. Each run starts with the latest
  output there; what a successful run leaves is the next output. It feels
  like the source produces the output.
- `.output/` is never in a version: a publish skips files there and lists
  them in `skipped` (an exported zip or a sandbox folder carrying the
  output publishes cleanly). Viewers get it at `/.output/…` from any page;
  a view can point at it (a markdown view of `.output/inbox.md`).
- Everything in it is public and kept in each output's history; a
  credential in it still fails the run.
- `GET/DELETE /artifacts/{id}/outputs`, `artifact.output` events; export
  and fork carry the current output.

Tried in Chromium with real VMs: the clock and table pages read
`/.output/…`, the inbox's markdown view shows `.output/inbox.md` after a
run, the failing job records no output; each has one version.

## Questions

- A job's edits outside `writes` vanish without a word. An agent writing
  a job could waste time on that; the run could note "changes outside
  writes were discarded" in its logs, but detecting it means diffing the
  whole tree after every run. Left for now.
- `run_now` runs show `trigger: manual`; `publish` would say where they
  came from.
- Tier definitions lived in SQLite and are gone after the move to
  Postgres; dev runs with no `default` tier (only the service-wide 1m
  minimum interval and no storage limit) until one is defined again.
