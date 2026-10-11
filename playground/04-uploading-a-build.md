# 04. Uploading a build

Artifacts `u04-*` (quota keys `u04`, `u04b`, `u04q`). All artifacts from
01–03 were deleted first (2026-10-11), with their runner sandboxes.

## Tried

1. **Blobs:** `/blobs/missing` with new and unknown hashes and a malformed
   one; `PUT` with wrong content, a malformed hash, twice; a publish naming
   a hash never uploaded.
2. **Only changes go up:** a 201-file build, then the same with one file
   changed; the same build for another quota key.
3. **Mirroring:** `replace_dirs: ["dist"]` with files removed and added,
   `replace: true`, a file/folder clash, `replace_dirs` of `""`, `.`,
   `../x` and a folder that doesn't exist, mode 0755 and a bad mode.
4. **Console upload:** a 301-file, 6 MB folder (25 × 200 KB bundles, 274
   small modules, a PNG) with Upload folder in Chromium, then again with
   one bundle changed and one module removed.
5. **Sandbox build:** a Firecracker sandbox ran a Python build into
   `/workspace/dist`; published with `from_sandbox` (excluding `*.map`),
   rebuilt with one change, republished unchanged, then wrote a
   `secret_env` value into the build.
6. **Limits:** inline 4 MiB, body 9 MiB, paths of 1024/1025 bytes, 5000 and
   5001 files, a storage quota of 1000 bytes, a 256 MiB + 1 blob, a 512 MiB
   version.
7. **Scale:** 5000 distinct files: upload, create, missing check, read,
   export.
8. **Orphans and GC:** deleted the 5000-file artifact, aged every blob 3h
   on disk, uploaded a fresh orphan, asked `/blobs/missing` for the 5000,
   ran GC (`gc_interval: 10s` in a scratch config), republished them.

## Saw

- Blobs behave as documented: 400 for malformed hashes, 422
  `hash_mismatch`, 422 `missing_blob`, re-`PUT` is a no-op.
- A rebuild with one change uploads 1 blob; the same build under another
  key uploads none (stored once, counted for each key).
- Mirroring: removed files disappear from `dist/`, other folders stay;
  `replace: true` keeps `artifact.json`; clashes and bad paths are clear
  400s; the view keeps working across rebuilds.
- `from_sandbox`: 52 downloads on the first publish, 2 on the rebuild (the
  changed `index.html` and `app.js`, per fired's request log), none when
  unchanged; `*.map` excluded; the leaked key was a 422 `secret_found`
  naming `config.txt` and `DEPLOY_KEY`, never the value.
- Scale is fine: 5000-file create 0.6s, missing check of 5000 hashes 26ms,
  version read 23ms, export 0.3s. (5000 PUTs took 10s from a sequential
  client.)
- A rejected publish leaves its inline content as orphan blobs until GC
  (the console's failed upload left most of its bundles behind, and the
  retry reused them).

## Fixed

- **The console couldn't publish a normal build:** text files up to 256 KB
  went inline even when hashed, so 25 bundles made a 5.2 MB request and a
  413 ("inline content exceeds 4194304 bytes"). Now every hashed upload
  goes as a blob (only missing ones are sent, 6 at a time), and only text
  typed in the editor goes inline (as a blob past 256 KB). The same build
  is a 30 KB request.
- **Over-size bodies said "invalid JSON body: unexpected EOF"** (400): now
  `413 too_large` "request body exceeds 9437184 bytes; upload large files
  as blobs".
- **The file limit counted the service's `artifact.json`:** exactly 5000
  files failed with "got 5001". It's not counted now.
- **GC could delete blobs a publish was about to use:** `/blobs/missing`
  said "present" for old unreferenced blobs (e.g. from a deleted
  artifact), GC removed them, and the publish got `missing_blob` (seen
  live: 10087 removed, then 422). The missing check and publish now
  refresh a present blob's mtime, so it gets the grace period again; GC
  then removed 1 blob and the republish worked. `from_sandbox` uses the
  same check.
- `replace_dirs: [""]` now says to use `"replace": true`.

## Decided: chunked storage and uploads (2026-10-11)

Rows weren't the cost to fix; content is. A job that appends to a 100 MB
CSV stored another 100 MB per run, and re-uploaded all of it. Looked at
git (trees, packfile deltas), BitTorrent v2 (per-file Merkle trees),
restic/borg/casync (content-defined chunking) and Dropbox (4 MB blocks,
upload only missing ones). Git-style trees aren't needed: artifacts don't
have that many files.

- **Files of 1 MiB or more are stored as FastCDC chunks** (256 KiB min,
  1 MiB average, 4 MiB max), each chunk a blob; dedup is global (any
  file, artifact or user). Library: `github.com/PlakarKorp/go-cdc-chunkers`
  v1.1.0, the versioned `fastcdc-v1.0.0` variant (pinned, since the cut
  points are part of the storage format). jotfs/fastcdc-go was the first
  pick but writes its gear table on every NewChunker (a data race).
- **A file is still named by its whole SHA-256** everywhere (publish,
  ETags, `from_sandbox`, the files API); the service keeps file → chunks
  in Postgres (`chunked`, `chunks`) and joins them when reading.
- **Chunk-level uploads:** `/blobs/missing` with the file and chunk
  hashes, `PUT` the missing chunks, then `PUT /blobs/{file}/chunks` with
  the list (checked: every chunk present, the whole hashes to the file).
  The Go client's `Upload`/`UploadContent` do it; `pa.SplitChunks` is the
  shared cutter. A whole-file `PUT` (the console) is chunked by the
  service: same storage saving, full upload.
- **Storage counts distinct chunks per key**; `GET /quotas/{key}` shows
  `storage_used` and `storage_logical` (no sharing at all).
- **GC follows file → chunks** with the same grace period and touch as
  blobs.

Tried live with the Go client: a 21 MB CSV (15 chunks), then 5 versions
each appending a row: each sent 1 chunk (~1 MB) instead of 21 MB; after 6
versions storage used 27 MB against 126 MB logical, and the file reads
back identical (`chunk-demo`, key `chunkdemo`).

## Questions

- ~~The console hashes with `crypto.subtle`, only on secure origins.~~
  The console is reached over plain http, so it now falls back to a
  built-in SHA-256 (matches Node's at every edge length; 6 MiB in 250 ms),
  and Copy source / Copy as curl fall back from `navigator.clipboard` to
  `execCommand`. Tried on `127.0.0.1.nip.io` (not a secure origin): the
  301-file upload sent 1 changed blob in a 30 KB request, and Copy source
  worked.
- **File rows, decided to leave:** contents are stored once (blobs); each
  version stores its file list as rows (path, hash, size, mode, ~150 B), so
  a 5000-file artifact with 24 run versions is ~125k rows, ~20 MB of
  SQLite. That's fine, and it keeps serving a single indexed lookup. If it
  ever hurts: store each version's list as one content-addressed blob (git
  trees), one row per version, cached in memory for serving.
