# 06. The db

Artifact `u06-report` (quota key `u06`, fork to `u06b`), real Firecracker
runners, the db as built at the end of 05. The job is `job.py` with the
console's `db.py` helper; the page watches `report/summary` and reads
`_private/state`. Deleted afterwards.

## Tried

1. **State and results:** the job counts its runs in `_private/state`
   (with the last five run IDs) and publishes `report/summary`, in one
   batch. Read as the API token, and through header routing as a viewer
   and as the owner (`X-Artifact-Owner: true`).
2. **Failing halfway:** a run that writes two documents one by one and
   exits 1; a run whose batch has an invalid third write; then a good run.
3. **Fork and delete:** forked to another quota key, ran the fork's job,
   compared quotas, deleted the fork.
4. **Live, in Chromium:** the page open in its own tab while the Data tab
   saves `report/summary`; a write from outside while the Data tab shows
   it; a save on a stale revision; `docker restart` of the service with
   the page open, then a write.
5. **Queries and limits:** nested fields (`addr.city==Pune`), numbers vs
   strings (`age==31`, `age=="31"`), `null`, arrays, `!=`, ranges with
   ordering, ordering by a nested field, a dotted document id (`v1.2`), a
   document over 1 MiB, a string instead of an object, a PUT on a
   collection, an unknown artifact.
6. **The job's token:** a job that echoes `$FIRED_DB_TOKEN`.

## Saw

- A viewer's root listing leaves `_private/` out, `_private/state` is a
  404 (`no_document`) and `_private` an empty collection, so to a viewer
  it doesn't exist; the owner reads it. Runs report `db_writes: 2`.
- **Halfway:** the failed run kept its two writes (`seen` went to 2 and
  `report/summary` says `partial`), as documented. **The failed batch**
  left nothing; the good run continued from `seen: 2`. A job's state moves
  even on failed runs unless it batches.
- **Fork:** got `report/summary` (rev reset to 1), not `_private/state`,
  so its job started over at `seen: 1` and overwrote the copied summary.
  The fork's quota key is charged for the copy (86 bytes). Deleting the
  fork deleted its documents.
- **Live:** the open page showed the Data tab's save at once; the Data tab
  picked up an outside write (when not being edited); a save on a stale
  revision was refused ("is at rev 6, not 5: read it again"); after the
  restart the page's `EventSource` reconnected and showed the next write
  within seconds.
- **Queries** match types exactly (`31` vs `"31"`), `!=` includes `null`
  but not missing fields, missing values sort last. Limits answer `413
  too_large`, `400` for a non-object or a collection path, `404` for an
  unknown artifact.
- The token shows as `[REDACTED:FIRED_DB_TOKEN]` in the run's logs; the
  URL is `http://127-0-0-1.nip.io:8444/v1/artifacts/u06-leak/db`.

## Fixed

- `where=addr.city == Pune` (spaces around the operator) was rejected as
  a bad field name; spaces are trimmed now.
- A single PUT of a non-object said `writes[0].data must be a JSON
  object`; errors name the document now ("the data of x/y must be …").
- Revision conflicts started with the path, which the console capitalises
  ("Report/summary is at rev 6"); they start with "document" now.
- `db.py` reported a failed batch as `POST : 400`; it says `db POST
  (batch)` now.

## Questions

- **Watches can miss a change under load.** The event bus drops events
  for a subscriber whose 256-event buffer is full, so a watch could keep
  showing an old snapshot until the next change. Not seen here (a batch
  is one event); a watch could resync on overflow.
- **Each change re-runs every matching watch's query**, per open page.
  Fine at this scale; many viewers of a busy artifact would want shared
  snapshots.
- **One write lock for the whole service:** db writes take the same
  advisory lock as publishes, so they're serialised across all artifacts.
  A per-artifact lock would do.
- **The read rate limit is per artifact** (50/s, shared by all its
  viewers, files and db together), so a popular page could hit 429s;
  `db.js` doesn't retry.
- `!=` matching `null` differs from Firestore (which excludes it).
  Documented as is.
