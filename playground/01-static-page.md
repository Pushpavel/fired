# 01. A static page

Artifact `hello-page` (quota key `dev`, label `usecase=01`).

## Tried

1. `POST /v1/artifacts` with an ID, meta (title, description) and a label,
   without `publish`.
2. `POST /v1/artifacts/hello-page/versions` with `base_version: 0`,
   `index.html` and `style.css` as inline `content`, label "first page",
   and no artifact.json.
3. Looked at the SQLite rows and blob files.
4. `GET /files/…` with and without `?version=`, `If-None-Match`, a missing
   file, a missing version, `../`, no token.
5. Console preview links for version 0 (current) and version 1.

## Saw

- **An artifact can exist without versions:** `current_version: 0`, empty
  version list, no jobs. It is only a record: ID, quota key, labels, opaque
  `meta`, timestamps.
- **Publishing returns the effective spec.** With no artifact.json, the
  default is one `html` view of `index.html`. No artifact.json is added to
  the tree (`file_count: 2`).
- **Storage:**
  - one `versions` row (origin, label, counts and spec as JSON) and one
    `files` row per file per version (path, sha256, size, mode 420 = 0644);
  - contents in `blobs/<2 hex>/<sha256>.zst`;
  - tiny files grow when compressed: 94 B of CSS became 107 B.
- **Every version stores all its file rows**, even unchanged ones. Blobs
  are shared, but rows are copied: a 5000-file artifact with 24 retained
  run versions is ~120k rows.
- **Serving:**
  - `files/` serves `index.html`;
  - ETag is the sha256, and revalidation gives 304;
  - current version: `Cache-Control: private, no-cache`;
  - `?version=N`: `max-age=31536000, immutable`;
  - `X-Artifact-Version` says which version answered;
  - `../` is normalized by the router (a redirect, no escape);
  - no token gives 401.
- **Preview origins** (console):
  - `hello-page-0-<sig>` follows the current version, `hello-page-1-<sig>`
    is pinned;
  - they need no token: the HMAC in the hostname is the capability;
  - they never expire and can only be revoked by rotating the API token;
  - reads count as activity for `delete_after`.

## Decided

**An artifact is always created with its first version** (2026-10-10).
"Create" and "publish" read as two ways to do one thing, and the state in
between (an artifact with no versions) caused the odd "version 0" errors.
Now:

- `POST /v1/artifacts` requires `publish` (the first version), with
  `base_version` left out. One call creates the artifact and v1.
- `POST /v1/artifacts/{id}/versions` adds the next version. One call.
- `PATCH /v1/artifacts/{id}` changes settings (title, labels, owner,
  retention) and never makes a version.
- No version can be empty: a create without files, or a publish that
  deletes every file, is a 400.
- The backend can choose the ID; a taken ID is `409 exists`.
- Create is one transaction, so a rejected create leaves no artifact and
  emits no events. Before, it created the artifact, emitted
  `artifact.created`, then deleted it again on a bad publish.

The step-1 behavior above (an empty artifact, `current_version: 0`) no
longer exists.

**Publishing from a sandbox: `from_sandbox`** (2026-10-10). Before this,
the API only took inline content or uploaded blobs, so every backend had
to copy sandbox folders itself (manifest, missing check, download, upload)
and run its own secret scan. Now a publish file op can be
`{"path": "site", "from_sandbox": {"id", "path", "exclude", "fired_url"}}`:

- `fired_url` comes in the request so several fired hosts work. One shared
  token from the service config is used for all of them; no allowlist,
  because the caller is the backend, which already holds that token. The
  backend must never pass a URL that came from an agent.
- The folder is mirrored at `path` (`""` = the artifact root).
- **Secrets:** only fired knows a sandbox's secret values, so fired got
  `GET /files/scan`. It checks every file for those values (and their
  base64/URL encodings) and returns names only (`422 secret_found`).
- **Race:** the scan returns hashes computed over the bytes it checked.
  The service downloads only blobs it's missing and checks each download
  against the scanned hash: a file changed after the scan is a `409
  changed`. Files whose scanned content is already stored aren't
  downloaded, so the published bytes are always the scanned ones. This is
  the content-addressed manifest + verify-on-fetch pattern (Docker
  layers, Git, Nix, apt).
- Tried live: an agent sandbox built `dist/`; published it as
  `hello-page` v2 (debug.log excluded); then the agent wrote its
  `DEPLOY_KEY` into `dist/config.txt` and the next publish got 422,
  naming the file and `DEPLOY_KEY`, never the value.

**Limits are tiers, with one mapping** (2026-10-10). The storage limit
(2 GiB) and the other per-user defaults were config-file values, with
per-key overrides in each service's database. Now:

- Tiers (plans) are data: `PUT /v1/tiers/{name}` on each service, for the
  limits it enforces (fired: memory and GB-hours per pool; fired-artifacts:
  storage and schedule interval). No limits in config files; a fresh
  install has none until the backend defines tiers.
- **One key → tier mapping, on fired** (`PUT /v1/quotas/{key}
  {"tier": "pro"}`). fired-artifacts reads it (cached a minute, last known
  tier if fired is down). A `pro` user is `pro` in both.
- Resolution: key override > key's tier > `default` tier > no limit.
- Tried live: fired's schema migrated v1 → v2 (new tables) with data kept;
  set `dev` to `pro` on fired and fired-artifacts showed `pro`'s 20 GiB
  storage.

## Limits and sandboxes

Dev defaults (`deploy/artifacts.yaml`):

- **Per publish:** inline content 4 MiB in total (decoded); request body
  9 MiB.
- **Per version:** 5000 files, 256 MiB. A blob is at most 256 MiB.
- **Paths:** up to 1024 bytes, relative, no `.`/`..`; a path can't be both
  a file and a folder; modes 0644/0755.
- **IDs:** `[a-z0-9][a-z0-9-]{2,47}`.
- **Per quota key:** storage from the key's tier (the dev `default` tier
  has 2 GiB, `pro` 20 GiB), counted over that key's distinct blobs, so
  unchanged files count once. Disk dedup is global: the same file
  from two users is stored once but counted for both.
- **Serving:** 50 reads/s per artifact, bursts of 100.
- Inline content is stored as blobs before the publish is validated, so a
  rejected publish leaves orphan blobs until GC.

Sandboxes: publishing and serving never touch fired (except `from_sandbox`,
which reads from one). Only jobs create a runner sandbox (one per artifact,
on its first run). `hello-page` has none.

## Questions

- Should `meta.title` be a convention the service knows about (for
  listing and search), or stay fully opaque?
- Does the 5000-file / 256 MiB version limit apply to the rows above, or
  only to the content?

## Could change

- Clearer 404s (service):
  - ~~an artifact with no versions says `no file "index.html" in version
    0`~~: gone, artifacts always have a version now;
  - `?version=2` when only v1 exists says `no file "index.html" in
    version 2`; it should say there's no version 2.
- Scan inline content and uploaded blobs too? Only `from_sandbox` and run
  outputs are scanned by the system; inline/blob publishes rely on the
  backend (it knows what it handed out). Open.
- `secrets_checked` isn't surfaced in the publish response, so a backend
  can't tell a clean scan from one with nothing to check (redaction off)
  (service).
- Store blobs uncompressed when zstd doesn't make them smaller (service,
  minor).
- Share file rows between versions, e.g. a tree table keyed by a hash of
  the tree, if row counts become a problem (service, later).
- Expiring or revocable preview links, if the console is ever exposed
  beyond dev (console).
