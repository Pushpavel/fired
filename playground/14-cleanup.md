# 14. Cleanup

## Tried

2026-10-10: deleted all 13 artifacts from the first walkthrough with
`DELETE /v1/artifacts/{id}`, after exporting them (see the archive).

## Saw

- Each delete returned 204, and `GET /v1/artifacts` was empty right after.
- All 6 runner sandboxes in fired (`fired-artifacts.artifact=<id>` labels)
  were gone within a few seconds, both running and suspended ones.
- The blob store still held 66 blob files. Deleting an artifact doesn't
  remove blobs: GC does, every `gc_interval` (1h), and only for blobs older
  than 2h (the grace period for uploads waiting on a publish). So disk usage
  drops up to ~3h after a delete.

## Questions

- Is there a way to trigger GC by hand (for tests, or after a big delete)?
  The service has `GC()`, but no endpoint calls it.
- Does a deleted artifact's storage stop counting against its quota key
  immediately, even while its blobs wait for GC? (Check in 12.)

## Could change

- Ops endpoint or CLI command to run GC now (service).
