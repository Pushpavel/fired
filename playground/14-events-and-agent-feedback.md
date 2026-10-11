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

## Questions

- **Durable events:** keep events in Postgres with increasing IDs for a
  while (a day), send `id:` on the stream, and replay from
  `Last-Event-ID` (or `?after=`) on reconnect, so a backend can't miss a
  `run.finished` across restarts and deploys.
- **A failed run's last lines in `run.finished`:** say the last 1 KB of
  stderr (redacted, as the logs are), so an agent hears why it failed
  without another request.
- **`run.waiting`:** an event when a run goes back to pending, with the
  reason (fired unavailable, over the key's limits, broker unavailable,
  hot storage full), so an agent can say "waiting for capacity" instead of
  nothing.
