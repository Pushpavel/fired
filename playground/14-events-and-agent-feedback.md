# 14. Events and agent feedback

Two recorders on `/v1/events` (everything, and `?artifact=u14-app`) while
acting as agent session `s-42`: publish a job with a bug (`run_now`,
`notify: s-42`), fix it and publish again (same), publish nothing new with
`run_now` and `notify: s-43`, restore v1, fork, overflow a queue of one,
write from the owner's page, write a 300-document batch, delete. Real
Firecracker runners; deleted afterwards.

## Saw

The u14 events, in order (times, version, details):

```
artifact.created  u14-app
artifact.version  u14-app  v1 edit
run.started       u14-app  v1
artifact.version  u14-app  v2 edit                (the fix, published while v1's run ran)
run.finished      u14-app  v1 failed "exit status 1"  notify s-42
run.started       u14-app  v2
db.changed        u14-app  1 path
run.finished      u14-app  v2 succeeded               notify s-42
run.started       u14-app  v2                     (run_now on the unchanged publish)
db.changed        u14-app  1 path
run.finished      u14-app  v2 succeeded               notify s-43
artifact.version  u14-app  v3 restore
artifact.created  u14-fork
artifact.version  u14-fork v1 fork
run.started / run.finished ×2 (the queue: one ran, one waited; the third trigger got 429)
db.changed        u14-app  1 path                 (the owner's page)
db.changed        u14-app  300 paths              (the batch)
artifact.deleted  u14-fork, u14-app
```

- **The agent loop works:** `s-42` hears that its first version failed,
  then that its fix succeeded; an unchanged publish with `run_now` and a
  new `notify` is heard on the next run of the current version. Runs of
  schedules and other triggers carry no `notify`.
- `?artifact=` filters to one artifact (19 events, all `u14-app`).
- **A failed run says only "exit status 1":** the cause (`KeyError:
  'price'`) is in its logs, one more request away.
- **No event says a run is waiting** (for fired, the quota key's limits or
  the broker): the reason is only on the run.
- **Events have no IDs and aren't kept:** the stream is in memory. When
  fired-artifacts restarted, both recorders' streams just ended, and the
  next artifact's `artifact.created` reached nobody. A backend relaying
  `run.finished` to agent sessions would miss what happened while it was
  reconnecting (or deploying).
- **A batch's `db.changed` listed all 300 paths;** deleting a collection
  of 10,000 documents would be one event with 10,000.

## Fixed

- `db.changed` lists at most 100 paths in the stream, with `paths_total`
  when there were more (watches inside the service still see them all).
  Checked: the 300-document batch arrives as 100 paths, `paths_total: 300`.

## Built after 14

- **Durable events:** every event is stored (Postgres, `events`) with an
  increasing `id`, kept for `limits.event_retention` (24h). The stream
  sends `id:`; a client reconnecting with `Last-Event-ID` (or `?after=`)
  gets what it missed, then live events, each once; if some are gone, a
  `gap` event first. The Go client's `Events` takes the last id.
- **`log_tail` on `run.finished`** when a run didn't succeed: the end of
  its output (about 1 KB, redacted like the logs).
- **`run.waiting`** when a run goes back to pending, with `waiting`
  (`quota_exceeded`, `broker_unavailable`, `fired_unavailable`, fired's
  `capacity` / `no_space`) and the reason; only when that changes. The
  console shows it, and the failed run's last line in its toast.

Tried with real VMs and a monitor that resumes after its last id: a
failing job's `run.finished` said "FileNotFoundError: … 'missing.json'";
fired-artifacts restarted and a run was triggered the moment it was back,
and the monitor, resuming after 4, got 5 and 6 (nothing missed, nothing
twice); with fired stopped, `run.waiting` (`fired_unavailable`), then the
run once fired was back. Tests cover replay, a filtered resume, the gap,
the log tail and `run.waiting` for an unavailable broker.
