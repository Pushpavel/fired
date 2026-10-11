# 13. Quotas and limits

Tiers defined for the test: fired-artifacts `default` (3 MB storage, 5m
minimum interval) and `pro` (1 GB, 1m); fired already had `default` (2 GB
of runner memory in the `artifacts` pool, 12 GB-hours a day) and `pro`.
Keys `u13` (default) and `u13p` (pro on fired); ten keys `u13-k0…9` for a
burst; `u13g` with a 0.01 GB-hours override. Real Firecracker runners on
dev (fired: 8 GB of VM memory, an 8 GB tmpfs hot tier). The tiers,
overrides and artifacts were removed afterwards.

## Saw

- **Tiers:** a key resolves to its override, else its tier (fired's
  mapping), else `default`; an override on `u13` (10 MB) showed in
  `effective`; fired refused mapping a key to a tier it doesn't have.
- **Storage (3 MB):** 2 MB of files, then another 2 MB refused ("2000003
  used + 2000000 new > 3000000"); 900 kB of documents fit, 100 kB more was
  refused; shrinking a document always works; **deleting an artifact frees
  its storage at once** (15's open question).
- **Minimum interval:** an every-minute schedule fires every 5 minutes for
  a `default` key (18:01, 18:06; `next_run` says 18:11), every minute for a
  `pro` key.
- **A restart gave a `pro` key the default tier's interval once:** after
  fired-artifacts restarted, its every-minute job's next run was 5 minutes
  out (18:04 → 18:09). The tier cache starts empty, the scheduler only
  reads the cache, and the next slot is stored when it's computed.
- **Reads:** 300 quick requests to one artifact's page: 114 served (the
  burst of 100 plus 50/s), 186 `429` with `Retry-After: 1`; `/.fired/db/`
  the same. Other artifacts unaffected. The limit is per artifact, shared
  by every viewer, files and db together.
- **The service-wide cap:** ten runs in ten keys at once: 8 ran
  (`max_concurrent_runs`), 2 waited (fired at exactly 8192 MB running).
- **Hot storage filled up, and runs failed.** When the 8 finished, their
  runners were suspended at once (since 09), each writing a 1 GB memory
  snapshot to the 8 GB hot tier: some snapshots failed ("stopping VM (disk
  is kept)"), and creating the last two runners failed with `500 internal:
  mkfs.ext4: No space left on device`, which failed their runs. After the
  fix below they **wait** ("503 no_space: the host's hot storage is full
  (0 MB free, this sandbox needs about 1280 MB)"), but the tier stays full
  until the suspended runners are archived (`archive_after`, 1h for
  runners), and every new sandbox on the host waits or fails meanwhile.
- **GB-hours a day (0.01):** run 1 (31s) and run 2 ran; run 2 was admitted
  under budget and ran past it (0.018 of 0.01): the budget is checked when
  a sandbox starts or resumes, not while it runs. Run 3 waits: "u13g has
  used 0.02 of its 0.01 GB-hours today in pool artifacts", retried every
  minute until the day ends (UTC).

## Fixed

- **fired: out of hot storage is capacity, not an error.** When starting
  or restoring a sandbox fails and the hot directory has less free space
  than the sandbox's memory plus 256 MB, the answer is `503 no_space`, so
  clients wait and retry (fired-artifacts runs go back to pending). A
  sandbox's own disk being full is still its own problem.
- **Tiers before schedules:** the scheduler looks up a due job's tier from
  fired (usually cached) before computing its next slot, and the service
  learns the tiers of every scheduled key at startup (at most 10s).

## Decided and built (after 13)

- **fired makes room in hot storage itself:** `capacity.min_free_hot_mb`
  (2048) is kept free; before a start, a memory snapshot or an unarchive,
  and on every reaper tick, fired archives the least recently used
  suspended or stopped sandboxes to cold storage until there's room. Room
  is reserved for writes in flight, so concurrent snapshots don't all
  count the same free space (the first version didn't: 8 runners
  suspending in the same millisecond all saw ~8 GB free, and 8 snapshots
  failed). Tested (fake driver, a free-space hook) and with real VMs: the
  ten-run burst again, all ten `succeeded`, 7 sandboxes archived for room,
  no snapshot failures, the hot tier at 54% afterwards.
- **Reads are limited per viewer**, with a higher limit per artifact (see
  below).
- **GB-hours at admission, and waiting until the day resets:** kept as is.
