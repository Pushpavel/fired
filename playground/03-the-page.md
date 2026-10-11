# 03. The page

What an artifact's view is. Decided before trying the use case, while
finishing 02 (2026-10-11).

## Decided

**The view is an HTML page plus its folder, like Claude's artifacts**
(2026-10-11). The road here, in 02: a list of views became one view
(`type` + `entry`), the default view inferred from `index.html` went away,
and serving every file like a web server didn't fit "the view decides".
Claude's artifacts are a page plus supporting files, and the agent builds
the page itself: full control, and it sees exactly what viewers see.

- `artifact.json` has `"page": "dist/index.html"`: an `.html` file in the
  version. No `page`, nothing to view (no inference from `index.html`).
- Viewers (header routing, console preview origins) get the page at `/`
  and the files of its folder at their paths: other pages (multi-page
  sites work: `about.html`, `docs/setup.html`), scripts, styles, data a job
  writes. Nothing outside the folder (sources next to `dist/` stay private),
  no index files (`docs/` is a 404; link `docs/index.html`), and
  `artifact.json` isn't served. No page: `404 no_page`.
- The service renders nothing: `view.type`, the type list and
  `/.fired/view` are gone. A report page renders its own Markdown, a
  dashboard fetches its CSV.
- The files API is the owner's raw access to any file; it lost its
  "empty path or `/` serves `index.html`" rule.
- Console: the View tab frames the preview origin's `/`. The Files tab
  gained Source / Preview for md, csv, json, txt, images, svg and
  jsx/tsx: the renderer (`ui/view.html`) runs in a sandboxed frame with no
  origin and gets files from the console by message; the console reads
  them with the files API, or from staged edits.
- Examples: the html ones have a page; the Markdown note, CSV table and
  React component have no page yet (we'll give them pages as we reach
  their use cases) and open on the Files tab.

Tried live: `hello-page` v11 has `"page": "index.html"`; via header
routing `/`, `/about.html`, `/style.css` are 200 and `/artifact.json`,
`/nope/` are 404. `preview-test` (a page, `docs/about.html`, Markdown with
an SVG, a CSV, two JSX files): in headless Chromium the View tab showed
the page and followed its link to the about page; the Files tab previews
rendered the Markdown (with its image), the table and the React component
(with its relative import).

## Tried

(Use case not started: artifact.json validation, a page that goes away,
pages for the Markdown / CSV / React examples.)
