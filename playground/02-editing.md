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
page, v5/v6 no-ops (before the fix), v7 restore v1, v8 restore v4, v9 link
the about page, v10 title into artifact.json.

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

**Exports carry the artifact's name** (2026-10-11). The title and
description were in `meta` on the artifact record, so a zip lost them and a
zip uploaded elsewhere couldn't be set up the same way.

- *First try (superseded the same day):* export added `.fired/export.json`
  with the meta and the effective spec, read back as a fallback. It
  overlapped with `artifact.json` (both described the view) and needed
  special rules (`.fired/` reserved, spec written only if not the default).
- *Now:* **every version has an `artifact.json`,** and it holds `title`
  and `description` as well as the view and jobs. The service writes the
  default when a create brings none, keeps the current one when files leave
  it out (`replace`, a mirrored folder), and refuses deleting it. A zip is
  complete: published again, it's the same artifact. `.fired/export.json`
  is gone.
- **Title is versioned:** renaming makes a version (the console labels it
  "rename"), and restore and fork bring the name along. `Artifact.title`
  and `description` show the current version's. Publish (and fork) take
  `title`/`description` to set just those fields, winning over the files'
  `artifact.json`: an agent naming an unzipped copy keeps its view and jobs.
- **The view is explicit.** Writing a default view into `artifact.json`
  froze it (a mirrored folder that gained `index.html` kept no view; one
  that lost it failed). A first fix recomputed a default view from the
  files, which was more inference to explain. Now there's no default: no
  `"view"`, no view, `index.html` or not; a new artifact without
  `artifact.json` gets `{}` (plus its title). Files coming and going never
  change it; republishing the same folder is `unchanged`. The console's
  new-artifact dialog and "A web page" example set the view explicitly.
- **No backwards compatibility** (not in prod yet): the legacy `views`
  list, the read-back of stored multi-view specs and export's
  write-a-missing-artifact.json are gone. Dev versions from before (e.g.
  `hello-page` v1–v9) simply have no `artifact.json`.
- **`meta` is backend-only again:** opaque, not exported or forked, not
  shown to jobs. Labels, quota key and retention stay settings (PATCH).
- Moved `hello-page` (v10) and `hello-copy` (v2) titles from meta into
  `artifact.json` and cleared their meta. Exports of v10 and of v4 (which
  had no `artifact.json`; the zip gets one written from its spec) both
  include it.

**One view per artifact** (2026-10-11). In the console, `about.html` (v4,
v8) wasn't in the preview: the view is `index.html`, and v1's page has no
link to it, so it was only reachable by typing `/about.html`. That raised
what a list of views is for. A version is one tree served from `/`; the
viewer's domain shows one thing; HTML and React reach their other files
themselves; everything else is a file with its own URL. So:

- `artifact.json` has `"view": {"type", "entry"}` (optional; default html
  of `index.html`), not a `"views"` list. The console's view tabs are gone.
- A legacy `"views"` with one entry is read as `"view"`; several are a
  400 ("an artifact has one view: … link the other files from it").
  Versions stored with several keep the first when read back.
- The console examples that had two views now have one (the note embeds
  its SVG; the table's README is a file).
- v9 of `hello-page` links `about.html` from `index.html`, which is how
  it's meant to be reached.

## Questions

- ~~Should an edit that changes nothing be a no-op?~~ Yes, done.
- ~~Should `PATCH` merge `labels` and `meta`?~~ Yes, done.
- ~~Should export include the effective `artifact.json` even when it's the
  default?~~ Yes: every version has one now (see Decided).
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
