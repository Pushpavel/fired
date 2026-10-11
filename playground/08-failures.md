# 08. Failures

Artifact `u08-fail` (quota key `u08`), real Firecracker runners, `timeout:
10s`. Its `job.py` picks a failure from the trigger's payload: `exit3`,
`term` (cleans up on SIGTERM), `stubborn` (ignores SIGTERM), `orphan`
(starts a detached child, `setsid`, that writes the db 15s later), `loud`
(2.6 MB of output), `egress` (a host off the allowlist). Then fired
stopped, and fired refusing the runner over the key's memory limit.
Deleted afterwards.

## Saw

- **exit3:** `failed`, "exit status 3", stdout and stderr in the logs.
- **term:** `timed_out` after 10.3s, exit 143; the SIGTERM handler's db
  write landed (the token is valid until the run is recorded), so a job
  can save its progress when stopped.
- **stubborn:** SIGKILL ~8s after SIGTERM (18.2s in all), no exit code.
- **orphan (first try): a bug.** The job exited 0 at once, but the run was
  `timed_out` after 15s with exit code 0: its child held the job's stdout
  open, and fired's agent waited for the pipe, not the process. The
  child's write landed, since the run was still "running".
- **loud:** the last 256 KiB kept (`runner.log_bytes`), `truncated: true`;
  the console says "earlier output dropped". Tracebacks are at the end,
  so the tail is the right half to keep.
- **egress:** Python reports a bare `HTTP Error 403: Forbidden`; fired's
  reason ("not in this sandbox's allowlist") is in the body, which
  urllib's message doesn't show.
- **fired stopped (first try): a bug.** The run failed at once ("dial tcp
  127.0.0.1:8443: connection refused"); only fired's `503` meant "wait".
  A fired restart or a network blip failed every run due then.
- **Over the memory limit** (`PUT /v1/quotas/u08/artifacts
  {"max_memory_mb": 256}` on fired, runner deleted): the run stayed
  `pending` with "waiting: fired: 429 quota_exceeded: u08 has 0 MB running
  in pool artifacts; starting 1024 MB more would exceed its 256 MB limit",
  and started a minute after the override was removed (`quota_backoff`).
- `make dev-vm` lost every sandbox: it ran `docker rm -f`, so fired never
  got to archive them ("sandbox data lost (hot storage was cleared…)").

## Fixed

- **A background process is done when it exits** (fired's agent): the
  wait for children holding its output is capped at 2s, as foreground
  execs already were, and its exit code is the process's.
- **A run takes its children along:** the runner kills the job's process
  group when the run ends (fired's agent now signals the group even after
  its leader exited). A child that left the group (`setsid`) survives,
  but its token is dead: the orphan's write got `401`. Tested with the
  fake driver too.
- **fired unreachable or 502/503/504 means wait:** the run goes back to
  `pending` with `reason: "waiting: …"` (so the console shows why), and
  every run waits 15s before trying fired again. Only the steps before the
  job starts are retried, so a trigger never runs twice. Tested: stopped
  fired for 20s, the run started once it was back.
- **A runner from an older template build is replaced:** runners matched
  their template by name, so rebuilding `base` (needed here for the agent
  fix) left them on the old one; they're matched by template ID now.
- **`db.py` without a db** says "this fired-artifacts has no runner.db_url,
  so jobs have no db" instead of a `KeyError`.
- **`make dev-vm` stops fired first** (`docker stop`, 120s), so it archives
  its sandboxes before the container is removed.

## Questions

- A run whose fired connection drops while the job runs (`follow` fails)
  is `failed`, though the job may still be running in the VM. Retrying
  would risk running it twice; reconnecting to the same process would be
  better.
- ~~The broker being unreachable fails the run; should it wait like fired
  does?~~ Yes (decided after 08): an unreachable broker or its `502`/`503`/
  `504` puts the run back to pending ("waiting: broker unavailable: …")
  with the same 15s backoff; a refusal still fails it. Tested with a stub
  broker answering 503.
- After rebuilding a template, fired could tell clients; here each run
  asks for the template (one extra call per run).
