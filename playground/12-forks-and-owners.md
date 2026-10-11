# 12. Forks and owners

Source `u12-src` (quota key `u12a`, labels `owner: alice`, `meta`): two
versions, a 2 MB random file (stored as chunks), an every-5-minutes job
with `timeout: 1h` and a 30-day grant, and a db with `stats/main` and
`_private/state`. Forked to other keys, to the same key, from an older
version, from a fork; then the source deleted. Another source with 500 kB
of documents, forked into a key limited to 100 kB. Deleted afterwards.

## Saw

- **What a fork gets:** version 1 (`origin: fork`, "fork of u12-src v2")
  with the source's files and job; the request's `title` (written into its
  `artifact.json`), `labels`, `quota_key` and `id`; `forked_from`. Not the
  source's labels or `meta` (your backend's), not its grant.
- **The job:** `blocked` again (the 1h timeout needs the new owner's
  approval; the source's grant stays with the source), and no schedule
  window until the new owner publishes. After bob published with a 10m
  timeout it was `scheduled` in the default window.
- **The db:** `stats/main` copied with `rev` back to 1; `_private/state`
  not copied.
- **Storage:** both keys are charged the 2 MB file in full (2,097,416 and
  2,097,378 bytes), but it's on disk once (2.1 MB in the blob store). A
  fork into the same key added 9 bytes.
- **An older version:** forking v1 gave v1's page, with the source's
  *current* db (`{"v2": 42}`), which v1's page never knew.
- **Errors:** a missing version `404`, a taken id `409 exists`, no quota key
  `400`, a deleted source `404`.
- **Fork of a fork, then the source deleted:** both forks keep serving the
  2 MB file (blobs are shared, not owned); `forked_from` still names the
  deleted source.
- **A bug:** a fork into a key limited to 100 kB succeeded with 500 kB of
  documents (`storage_used` 500,604 of 100,000): the files were checked
  against the new key's storage, the copied documents weren't.

## Fixed

- **A fork's documents count against its new key's storage:** the same
  fork is now `413 quota_exceeded` ("500600 > 100000") and leaves nothing;
  with 1 MB it goes through. Tested.

## Questions

- **Should forking an older version copy the db?** The db is the current
  version's data; an older version's page may expect another shape (the
  same reason pinned views are read-only). Options: copy it only when
  forking the current version (an older version starts empty), or always
  copy it, or never.
