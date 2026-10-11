# 09. Approvals

Artifacts `u09-long` (`timeout: 2h`), `u09-month` (every minute,
`active_for: 720h`) and `u09-short` (every minute, `active_for: 2m`),
quota key `u09`, real Firecracker runners, the defaults (12h window, 30m
runs). Deleted afterwards.

## Tried

1. Created all three with `run_now`.
2. Grants on `u09-long`: `max_run` 1h, then 2h, then a run; `{}`; a bad
   duration; a year.
3. Grants on `u09-month`: `active_until` in 30 days, in the past, zero.
4. After approving `u09-long` for 2h: published a different command with
   the same timeout, then a 3h timeout.
5. `u09-short`: watched its 2-minute window close; a trigger while it was
   closing; a publish that changes nothing, then one that changes a file.
6. The console's Job tab and Approve dialog on the blocked job.
7. All three triggered at once (found while reading 5's runs).

## Saw

- **Blocked:** `u09-long` was `blocked` ("timeout 2h0m0s exceeds the
  allowed max run 30m0s"), `run_now` didn't run, a trigger got `409
  needs_approval` with the same reason.
- **Longer window:** `u09-month` was `scheduled` in the default 12h window,
  with "active_for 720h0m0s exceeds the default 12h0m0s; the schedule runs
  for the default window until approved".
- **Grants:** 1h kept it blocked (timeout 2h exceeds 1h), 2h made it
  `manual` and it ran. `active_until` +30d cleared `needs_approval`; a past
  date and zero both fell back to the default window. `{}` revoked both
  parts: a grant is replaced as a whole. A bad duration is `400`. A
  one-year `max_run` was accepted: there's no upper bound.
- **A grant isn't tied to the job:** a different command with the same
  2h timeout stayed approved; a 3h timeout blocked it again.
- **Windows:** `next_run` disappears once the next slot is after the
  window closes; the window closed on time (`expired`, no more scheduled
  runs) while a manual trigger still ran. A publish that changes nothing
  didn't re-open the window; one that changed a file did (+2m).
- **Console:** the Job tab says "Blocked until approved" with the reason;
  Approve pre-fills the requested 3h, and the job became runnable. It also
  pre-filled a 30-day schedule window for a job with no schedule.
- **Runs stalled for minutes (not about approvals).** `u09-short`'s
  scheduled runs from 16:55 to 16:57 never started: each was superseded by
  the next until 16:58. fired answered its runner's resume with `429`:
  the key was over its memory limit in the `artifacts` pool (dev's default
  tier, one 1024 MB runner), because another artifact's runner kept its
  memory for `idle_suspend` (1m) after its run ended, and the waiting run
  then backed off for another minute (`quota_backoff`).

## Fixed

- **A runner is suspended as soon as its run ends,** so it stops counting
  against the key's memory limit in fired at once (resuming it next time
  took ~150 ms). **A key's backoff is cleared when one of its runs ends,**
  so its waiting runs start right away. All three `u09` artifacts
  triggered together then ran within 4 seconds, and every runner was
  suspended afterwards.
- The Approve dialog pre-fills a schedule window only for a job with a
  schedule.
- Docs: a grant is a budget for the artifact, replaced as a whole, not
  tied to the job's command, with no upper bound from the service; a
  publish that changes nothing doesn't re-open the window.

## Decided

- **Approval stays a budget** (2026-10-11): the owner approves how long an
  artifact's job may run and stay scheduled, not one command. The job can
  change and stay approved while it fits; a bigger `timeout` or window
  asks again.
- **No cap in the service:** what an owner may approve is the backend's
  call (per plan, per user).
