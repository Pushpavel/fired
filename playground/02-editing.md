# 02. Editing over time

Artifact `hello-page`, continued from 01 (it was at v3). Real VMs
(`make dev-vm`), fired-artifacts from the image (`make dev-artifacts`).
Everything below was done with curl; the console's Compare still to look at.

## Tried

1. Patch publish on v3: add `about.html`, change `style.css`, leave
   `index.html` out, label "add about page".
2. Publishes with a stale `base_version` (3), none at all, and one ahead
   (99); then one with the same content as v4, and one with `files: []`.
3. `PATCH` the artifact's labels and meta; list with `?label=k=v`.
4. Restore v1 with a stale base, a good base, and a missing version.
5. Export v4 and `current` as zip.

## Saw

- **Patches work as documented.** v4 has 3 files: `index.html` kept its
  hash from v3 (not listed, carried over), `about.html` added, `style.css`
  replaced. The response is just `{"version": …}`, no `jobs` key when the
  artifact has none.
- **Conflicts:** a stale or future `base_version` is `409 conflict` with a
  good message ("current is 4; read it, merge your changes and publish
  again").
- **A missing `base_version` is read as 0:** the 409 says "base_version 0
  is not the current version", which is confusing; the caller never sent 0.
- **Publishes that change nothing still make versions:** re-sending v4's
  files made v5, and `files: []` made v6, both identical to v4 (same
  hashes, same size). Runs don't do this: `finishRun` makes no version when
  a run's writes change nothing (`internal/artifacts/runner.go`). So the
  rule differs between edits and runs. Also, `files: []` is allowed, though
  01 decided "no version can be empty"; it isn't empty because the old tree
  carries over, but it's a no-op.
- **Version labels** are one free-text `label` per version; restores get
  `label: "restore v1"` and `labels: {"restored_from": "1"}` automatically.
- **Artifact `PATCH` replaces maps whole:** `{"labels": {"stage": "review"}}`
  dropped `usecase`; `{"meta": {"title": …}}` dropped `description`;
  `{"labels": {}}` cleared them (and the field is then omitted from the
  response). Omitted fields are left alone. The docs don't say which.
- **PATCH isn't activity:** `last_active_at` didn't move, and no version
  was made (as decided in 01).
- **`?label=k=v` filters** as expected (`stage=draft` found it, `usecase=01`
  didn't after the relabel).
- **Restore** copies the whole tree of the old version (v7 = v1: no
  `about.html`), origin `restore`. Stale base is 409, but without the "read
  it, merge…" hint that publish gives. Missing version is a clear 404.
- **Export:** `hello-page-v4.zip`, `Content-Disposition: attachment`, modes
  kept (0644). `current` works as a version. No `artifact.json` in the zip
  when the version has none (the default spec isn't written out), though
  the docs say "artifact.json included".
- **Compare is console-only.** There's no compare API; a backend can diff
  two `GET /versions/{n}` file lists by hash.

History now: v1 first page, v2 from sandbox, v3 restore v1, v4 add about
page, v5/v6 no-ops (before the fix), v7 restore v1, v8 restore v4.

## Decided

**Fixed, all of "Could change" below** (2026-10-11):

- **No-op publishes make no version.** A publish or restore whose result
  has the current version's exact files (paths, hashes, modes) answers
  `200` with `"unchanged": true` and the current version; no version, no
  `artifact.version` event, no new schedule window. Its label is dropped;
  `run_now` and `notify` still apply to the current version (re-running a
  job without a change is a fair thing for an agent to do). Same rule as
  runs now.
- **`base_version` is required** on publish and restore: left out is `400
  base_version is required … (current is 7)`. Every artifact has a version,
  so 0 is never valid; no type change was needed. Restore also requires
  `version`.
- **`PATCH` merges `labels` and `meta`** (JSON merge patch, RFC 7386):
  keys given are set, `null` removes one, the rest stay. In the Go client
  `UpdateArtifactRequest.Labels` is now `map[string]*string`. Meta numbers
  keep their digits through the merge. The console's settings tab sends
  `null` for labels and title/description you cleared.
- **Restore's 409** has the same "read it, merge…" hint as publish (one
  `checkBase` for both).
- **Docs:** PATCH semantics, what export includes, no compare endpoint
  (diff the file lists by hash).

Checked live after the fix: no-base publish 400; `files: []` on v7 → 200
unchanged; restore v3 (same files as v7) → 200 unchanged; stale restore
409 with the hint; restore v4 → v8; PATCH `{"labels":{"stage":"review"}}`
kept `usecase`, and `null`s removed `stage` and `description`.

**Exports carry a `.fired/` folder** (2026-10-11). An artifact's settings
that aren't files (title, description, and the spec when it's the default)
were lost in a zip, so a zip uploaded somewhere else couldn't be set up the
same way. Now:

- Export adds `.fired/export.json`: `{artifact, version, exported_at,
  meta, spec}`, with the effective spec even when it's the default.
- `.fired/` is reserved at a tree's root and never stored. A publish
  whose files include `.fired/export.json` uses it only as a **fallback**:
  its spec when nothing else gives one (written as `artifact.json` only if
  it isn't the default, so a round trip is the same tree), and its meta on
  a create, under the request's meta. An agent that unzips and names the
  copy itself wins; a bare re-upload gets the old title back.
- Labels, quota key and retention stay out: they're the backend's (who
  owns it, how long it's kept), and a new home sets its own.
- Tried live: exported `hello-page` v8, unzipped, created `hello-copy` from
  the files with `meta.title` "Hello copy": that title won, 3 files, no
  `.fired`, no `artifact.json`. Without meta, the copy got "Hello page".

## Questions

- ~~Should an edit that changes nothing be a no-op?~~ Yes, done.
- ~~Should `PATCH` merge `labels` and `meta`?~~ Yes, done.
- ~~Should export include the effective `artifact.json` even when it's the
  default?~~ Yes, in `.fired/export.json` with the meta (see Decided).
- Still to look at: the console's Compare (v4 vs v3, v8 vs v7) and the
  settings tab saving after the merge change.

## Could change

All done (see Decided).


- Require `base_version` on publish and restore (400 `base_version
  required`) instead of reading a missing one as 0 (service).
- Edits that change nothing: no new version (service), or at least say so
  in the response (`"unchanged": true`).
- Same 409 hint for restore as for publish (service, minor).
- Document PATCH's replace semantics for `labels`/`meta` (docs).
- Export: clarify "artifact.json included (if the version has one)" or
  write the effective spec (docs or service).
