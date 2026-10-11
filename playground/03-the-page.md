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

1. **Validation** (curl, `page-test`): `page` set to a non-`.html` file,
   a file that isn't there, `../index.html`, `/index.html`; `"pages"` (a
   typo) in an `artifact.json` file; an uppercase `INDEX.HTML`; then
   deleting the page, mirroring its folder without it, renaming it
   (delete + add + new `page` in one publish), and `spec: {}`.
2. **Pages for the examples without one**, written as an agent would:
   the Markdown note, the CSV table and the React component. Created from
   the console and checked in headless Chromium.
3. **Folder rules as a viewer** (`dist-test`: `src/build.py` next to a
   page in `dist/`, with `about.html` and `docs/index.html`): clicked
   links to `about.html`, `docs/`, `../src/build.py`, and `/` from the
   about page.

## Saw

- **Validation is clear and all-or-nothing:** "page: "README.md" must be
  an .html file", "is not a file in this version", unknown field
  "pages", relative-path errors. Uppercase `.HTML` is accepted and served
  as `text/html`. Deleting the page or mirroring its folder without it is
  a 400; renaming it in one publish works; `spec: {}` leaves no page and
  viewers get `404 no_page`. (`spec` replaces `artifact.json`, so it also
  dropped the title, as documented.)
- **Markdown note:** `notes/index.html` (page) imports marked and renders
  `README.md`; the SVG in it resolves next to the page. ~20 lines.
- **CSV table:** the page fetches `data/sales.csv` (`cache: "no-cache"`)
  and draws the table. The `build` job ran in a Firecracker VM on create
  (`run_now`), its run made v2, and the page showed its 5 regions without
  being changed: page + data written by a job is the natural split.
- **React is where "the page renders itself" costs something.** A
  multi-file JSX app can't run in a browser without compiling: the old
  renderer hid that with an in-browser bundler. Without a build, the clean
  way is htm (JSX-like tagged templates): `App.js`/`Tally.js` are plain ES
  modules, the browser follows their relative imports, an import map
  supplies `react`. Works, no tooling. JSX proper needs a build (esbuild in
  the agent's sandbox), and fired's `base` template is Python-only.
- **Folders as a viewer:** `about.html` and back to `/` work; `docs/` is
  a 404 ("is a folder; link to a file in it"); `../src/build.py` can't
  leave the page's folder (the browser resolves it to `/src/build.py`,
  which is `dist/src/build.py`): 404.

## Fixed

- **View 404s leaked the tree:** "no file "dist/src/build.py"" told a
  viewer where the page lives. View errors now name only the URL path
  ("no /src/build.py here"); the files API still names tree paths.
- **The page-went-away error** now says what to do: "(publish it, or set
  "page" to the file to open)".
- Examples: the note, table and React app have pages; the React one is
  now "A React app without a build" (htm).

## Questions

- Viewers get JSON error bodies (`{"error": …}`) for a broken link. Fine
  behind a backend that styles its own error pages; worth a plain page if
  the service's view is ever shown directly.
- JSX: offer a template with Node (so an agent can run esbuild in its
  sandbox), or document htm as the no-build way? Probably both, when an
  agent use case needs it.
