# Console walkthrough, 2026-10-10

Everything that existed after the first console walkthrough (the examples,
a few edits, restores and forks), exported before the artifacts were deleted.
Each folder has `artifact.json` (metadata), `versions.json`, every version's
files under `versions/v<N>-<origin>/`, `runs.json`, run logs under `runs/`,
and `jobs.json` (job status at export time).

The raw service state (SQLite + blobs) is in `.dev/archive/2026-10-10-console-walkthrough/`
(not in git). To bring it back, stop fired-artifacts and copy it over `.dev/artifacts/`.

| Artifact | ID | Quota key | Versions | Runs |
|---|---|---|---|---|
| A web page | `art-a2c7532876ad` | dev | 3 | 0 |
| A note with two views | `art-1305e93f13fb` | dev | 1 | 0 |
| A page a job keeps fresh | `art-7a5ffb1dc3a5` | dev | 8 | 7 |
| A table a job rebuilds | `art-84297eff6525` | dev | 3 | 2 |
| An inbox that queues triggers | `art-27521b55c40f` | dev | 1 | 5 |
| A React component | `art-d8805794b400` | dev | 1 | 0 |
| A job that needs approval | `art-271bf0893b97` | dev | 1 | 1 |
| A job that fails | `art-777fd23a812d` | dev | 1 | 1 |
| A web page (fork) | `art-a49e9503897f` | alice | 4 | 0 |
| A web page | `art-a3a90c890371` | dev | 3 | 0 |
| A web page (fork) | `art-96a10b883457` | alice | 1 | 0 |
| An inbox that queues triggers | `art-3bc3fba50059` | dev | 5 | 4 |
| Scratch | `art-60c6d38ab2f1` | dev | 1 | 0 |
