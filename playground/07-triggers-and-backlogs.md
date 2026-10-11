# 07. Triggers and backlogs

Artifacts `u07-latest` (`on_busy: latest`) and `u07-queue` (`on_busy:
queue`), quota key `u07`, real Firecracker runners. Both run `handle.py`:
it sleeps as long as the payload says, then records its trigger (type,
payload, when it was queued and done) in `handled/<run id>` in the db.
Deleted afterwards, with `u07-nojob` and `u07-blocked`.

## Tried

1. **latest:** five manual triggers a second apart, each sleeping 8s.
2. **queue, `max: 3`:** six triggers at once, sleeping 4s each.
3. **max_age 20s:** a 75s run, then a trigger waiting behind it; with an
   every-minute schedule on.
4. **A schedule firing into a full queue:** `max: 1`, a long run, one run
   waiting, the minute ticks.
5. **Payloads:** over 1 MiB, a bare string, not JSON, no body at all.
6. **Jobs that can't run:** an artifact without a job, a blocked job
   (`timeout: 2h`), an unknown artifact; the schedule window closed.
7. **From the owner's page** (built here, see Fixed): the inbox example's
   form and the clock's Refresh now, as owner and as viewer, in Chromium.

## Saw

- **latest:** the first trigger ran; the next three were `superseded`
  ("replaced by a newer trigger") as each newer one arrived; the fifth ran
  after the first. Two documents in `handled/`: n=1 and n=5.
- **queue:** four accepted (one running, three waiting: `max` counts
  waiting runs, not the running one), triggers 5 and 6 got `429
  queue_full`. The four ran in order, ~4.5s apart.
- **max_age:** the waiting run expired, but only when the next trigger
  touched the queue (46s after it was due), and the job's `pending` count
  showed it as waiting until then. A schedule run queued behind the long
  run expired the same lazy way.
- **Full queue:** the schedule's firing was recorded as `dropped`,
  `queue_full`, and the API trigger got `429`.
- **Payloads:** 1 MiB + a bit is `413 too_large`; a string payload is
  fine (the job got `"just text"`); invalid JSON is `400`; no body is a
  trigger with no payload.
- **Can't run:** `404 no_job`, `409 needs_approval` (listing what needs
  it), `404 not_found`. A job whose schedule window has closed still runs
  when triggered: only the schedule stops.
- **The every-minute schedule I forgot** kept firing for an hour (64 runs)
  until I turned it off: the 12h window working as designed, and a
  reminder of why it exists.
- **Seven scheduled runs failed in a row** (15:07 to 15:13): the job's
  `db.set` got `502 fired egress: lookup 127-0-0-1.nip.io … i/o timeout`.
  WSL's DNS stalled for a few minutes, and dev jobs reached the db through
  a public DNS name.

## Fixed

- **Dev jobs don't depend on public DNS:** `make dev-vm` maps
  `fired-artifacts.internal` to 127.0.0.1 in fired's container, and
  `make dev-artifacts` sets `db_url` to it.
- **`db.py` retries** connection errors, timeouts and 502-504 four times
  with backoff (1, 2, 4, 8s), so a blip doesn't fail a run.
- **A runner with an old allowlist is replaced:** a changed `db_url` or
  `runner.egress` used to keep existing runner sandboxes with the old
  allowlist (a job then can't reach the db); the runner is now recreated
  when its allowlist differs. Tested.
- **Queued runs expire on time:** every scheduler tick drops runs past
  `max_age`, instead of waiting for the next trigger.
- Messages: `queue_full` says the limit ("queue.max is 3");
  `needs_approval` lists its reasons instead of printing a Go slice.
- **The owner's page runs its job.** Triggers were API-only, but a
  personal app wants a Refresh button or a form the job handles.
  `/.fired/job` gives the job's status to anyone viewing (so a page can
  say when it next updates); `POST /.fired/job/runs {"payload"}` runs it,
  owner only (`403` otherwise), as trigger `page`. In `db.js`:
  `job.status()`, `job.run(payload)`, `job.canRun`. The inbox example now
  has a Send form (three messages queued and handled in order) and the
  clock a Refresh now button and "Next run" (the viewer sees the time,
  not the button).

## Questions

- The page learns a triggered run's result only through the db. A page
  wanting "running… done / failed" would need the run's status; `job.run`
  returns the run, but there's no way to follow it from the page yet.
- A trigger's payload is visible only to the job (and the API). Payloads
  from the owner's page are the owner's input, so that's fine; if viewers
  could ever trigger, the payload would be untrusted input to the job.
