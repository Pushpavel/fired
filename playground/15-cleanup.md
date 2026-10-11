# 15. Cleanup

## Tried

2026-10-10: deleted all 13 artifacts from the first walkthrough with
`DELETE /v1/artifacts/{id}`, after exporting them (see the archive).

2026-10-11 (after 14), real Firecracker runners, `POST /v1/gc` (new, see
Fixed) to run GC instead of waiting for `gc_interval`:

1. **Delete:** `u15-full` with three versions, a 2 MB file of its own (so
   chunked), a 1 MB file shared with `u15-keep` (another key), a job that
   ran, a document. Counted its rows, runner and storage before and after,
   then GC inside the grace period, then with the blobs aged 3h (files'
   mtimes, then `chunked.touched_at`).
2. **`delete_after`:** artifacts with `2s`, `1ms` and `-1s`, then GC.
3. **Run records:** a finished run aged to 31 days, then GC.
4. **Delete during a run:** a job sleeping 20s then writing the db,
   deleted 5s in.

## Saw

- (2026-10-10) Each delete returned 204; all 6 runner sandboxes were gone
  within seconds; the blob store kept 66 blobs until GC.
- **Delete:** versions, files, the db, runs, the job and the runner went
  at once (`versions 3 → 0`, `files 12 → 0`, runners `1 → 0`), and the key's
  `storage_used` went from 3,000,142 to 0. Its events stay (8, until
  retention). The blob store didn't shrink: the grace period.
- **GC:** within the grace, it removed only 104 old unreferenced blobs
  (22 kB) from earlier use cases. With the blobs' mtimes aged, 35 more
  (3.7 kB), but not the 2 MB file: a chunked file's grace is
  `chunked.touched_at`, not the chunks' mtimes. With that aged too, 11
  blobs (10 MB) went, and the blob store was down to 1 MB. `u15-keep`
  still served the shared file and every example still served.
- **`delete_after`:** `2s` and `1ms` were deleted by GC, `-1s` kept. But
  reads count as activity at most once a minute, so a `delete_after`
  under a minute can delete an artifact someone is viewing.
- **Run records:** the 31-day-old run was pruned, the recent one kept.
- **Delete during a run (a bug):** the delete returned 204 and removed
  everything, then the run ended (its runner was gone: "follow: 404
  sandbox not found") and wrote its record back: an orphan run of a
  deleted artifact, kept 30 days with its logs. Worse, a run of
  `u10-slow` (deleted in 10 while it waited on the slow broker) had put
  itself back to `pending` after the delete, and the scheduler logged
  "dispatch run … artifact u10-slow not found" every tick since.
- Created artifacts answered with nanosecond timestamps
  (`…45.388046901Z`); reads have milliseconds.

## Fixed

- **`POST /v1/gc`** runs GC now and says what it removed (artifacts
  deleted, runs and events pruned, blobs removed, bytes freed); Go: `GC`.
- **A run of a deleted artifact leaves no record:** finishing or putting a
  run back to pending checks its artifact in the same transaction (and
  drops the record if it's gone), the scheduler drops a pending run whose
  artifact is gone, and GC removes rows of artifacts that no longer exist.
  After deploying: the stuck run was gone, no orphans, no more dispatch
  errors. Tested (a run of an artifact deleted mid-run).
- **`delete_after` is at least a minute** (0 the default, negative never),
  on the artifact and in `limits.delete_after`.
- **The service's clock is in milliseconds**, as stored, so every answer
  matches a later read.
- Docs: what a delete removes at once and what waits, events' retention,
  the GC endpoint.

## Questions

- A delete while fired is unreachable can't remove the runner sandbox; it
  goes by its fired TTL (`runner.ttl.delete_after`, 48h).
- A schedule running every minute keeps its artifact active (and makes
  43,000 run records a month, kept 30 days); the 12h window bounds it
  unless approved for longer.
