# 11. Sharing through your domain

A stand-in for your backend, [11-proxy.py](11-proxy.py) (Python, ~70
lines): it serves `<id>.art.localhost:8446` (current version) and
`<id>--v<n>.art.localhost:8446` (pinned), decides the owner from a `user`
cookie against the artifact's `owner` label, strips the routing headers
from what the browser sends, adds the API token, `X-Artifact`,
`X-Artifact-Version` and `X-Artifact-Owner`, drops cookies, adds
`Content-Security-Policy: frame-ancestors 'self'`, and streams SSE.
Artifacts `u11-notes` (a page, owner alice, v1 and v2), `u11-md` (a
markdown view), `u11-other` (owner bob, with a job) and `u11-evil` (alice's,
a page attacking `u11-notes`). Chromium with `*.art.localhost` mapped to
127.0.0.1. Deleted afterwards.

## Saw

- **Serving:** `/` is the entry with `X-Artifact-View: page` and the
  version; the pinned host gets v1 with `immutable` caching, the current
  one `no-cache`; `about.html` from the folder; `artifact.json`, a missing
  file and an unknown artifact are JSON 404s. The markdown view comes back
  raw (`text/markdown`, `X-Artifact-View: markdown`) for the frontend to
  render; its image comes from the same origin.
- **Owner vs viewer:** `db.js` says `canWrite = true` only for alice
  (anonymous and bob: false); alice's batch wrote, bob's PUT was `403
  read_only`, and so was a run of bob's job by alice.
- **Forged headers** (`X-Artifact-Owner: true`, `X-Fired-Token`,
  `X-Artifact: u11-other`) from a viewer were stripped by the proxy: `403`,
  and the notes artifact's own db. The service trusts these headers, so
  stripping them is the backend's job.
- **Not reachable through the viewing domain:** the API (`/v1/…`) and the
  console are views' 404s.
- **A sibling artifact attacking** (`u11-evil`, alice's own, so alice's
  session reaches both: same site): reading `u11-notes`' db and PUT/DELETE
  with JSON were blocked by CORS (no preflight answer); a `no-cors`
  text/plain batch, a text beacon and a form POST reached the service and
  got `415`; a JSON beacon needed a preflight and wasn't sent; framing was
  blocked by `frame-ancestors`. Alice's notes were unchanged.
- **Live updates through the proxy:** the viewer's page showed a note
  written through the API without a reload.
- **The pinned v1 page writes the current db** (the db isn't versioned).
- A write's answer had nanosecond timestamps (`…54.520077994Z`), reads
  milliseconds.

## Fixed

- Write timestamps are milliseconds, as stored.
- Docs: a checklist for your viewing domain: its own registrable domain
  and one origin per artifact (and per pinned version); strip and set the
  routing headers, don't forward cookies; decide the owner per request;
  `frame-ancestors`; never add CORS headers (and why that keeps sibling
  artifacts out of each other's db); render single-file views; turn the
  JSON 404s into your own pages. `/.fired/job` was missing from the list
  of view paths.

## Decided

- **A pinned view is read-only** (after 11): with `X-Artifact-Version: <n>`
  the page reads the db as any viewer does (not `_private/`), and neither
  writes it nor runs the job, even for the owner. That holds even when n
  is still the current version, so a pinned link doesn't change behaviour
  when the next version is published. The console's Owner/Viewer switch
  only shows on the current version.
