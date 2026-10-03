# Web UI

How the web UI is built, how it reaches the binary, and what the root path answers. This
document is that argument's single home: `CLAUDE.md` names the rule and links here rather than
restating it. The source is `web/` (the views in `web/src/views`, the API module, the token
holder and the formatting in `web/src/lib`); the embed is `internal/ui`; the routes are in
`internal/server/server.go` (`handleRoot`, `handleUIAsset`); the daemon's wiring is
`cmd/holdfast/ui.go`; the gate's steps are `scripts/ui.sh`; the pins are held by
`scripts/check-pins.sh` sections 8 and 12.

The page states which holdfast answered and that its API is reachable, and then shows five
views and one set of controls: [what each view reads](#views), [the controls and why only
these](#controls), and [where the token lives](#token). The JSON API remains the full
interface: the page calls only endpoints `docs/api-reference.md` lists, so there is nothing it
can do that a script with the same token cannot.

## The rule

<a id="embed"></a>

**The web UI is built by one pinned toolchain, embedded in the binary, and served at `/` only to
a request that asks for HTML; every root response carries the source offer, and a page that
cannot carry it is not served in any part.**

The owner decided a web UI, a single-page application with its own Node toolchain, tested
without a browser (decisions T14, T18, T21, T22).

## The toolchain and its pins

<a id="toolchain"></a>

Each version has one home, and the gate reads it from there:

| What | Home | Read by |
|---|---|---|
| Node | `web/.node-version`, an exact version | `scripts/ui.sh`; the workflows' `node-version-file`; the Dockerfile's `ui` stage restates it on its `FROM` line, and section 12 compares the two |
| pnpm | `packageManager` in `web/package.json`, with its sha512 | corepack, which refuses a package that does not hash to it. pnpm 12's package is a launcher: it fetches the native pnpm of that version and checks it against npm's registry signatures, so the hash pins the launcher and the version pins the rest |
| Svelte, Vite and every other package | exact versions in `web/package.json` | `pnpm install --frozen-lockfile` against the committed `web/pnpm-lock.yaml` |

The versions were read from the npm registry and the Node release index on 2026-10-03
(`npm view <package> dist-tags time`, https://nodejs.org/dist/index.json). Node 24 is the Active
LTS line until 2026-10-20 and Node 26 becomes LTS on 2026-10-28
(https://raw.githubusercontent.com/nodejs/Release/main/schedule.json, read 2026-09-29).
TypeScript is held at 6.x: `svelte-check` 4.7.6 declares the peer range `^5.0.0 || ^6.0.0` and
`typescript-eslint` 8.71.0 declares `<6.1.0`, so neither accepts TypeScript 7 (`npm view`, read
2026-10-03). `eslint` is one release behind the newest, which was under a day old when the pins
were taken and so not installable under the rule below.

`scripts/ui.sh` runs every step on exactly the pinned Node and pnpm. It takes them from `PATH`
where they are the pinned versions, from corepack or from mise where they are not, and refuses
where none of those yields the pair: a UI gate run on some other Node would be a gate about that
Node.

### What pnpm is told

`web/pnpm-workspace.yaml` is where pnpm's decisions live, because pnpm 11 and later read no
non-auth setting from `.npmrc` (https://pnpm.io/blog/releases/11.0, read 2026-09-29; measured
with a root `postinstall` probe under pnpm 11.27.1, 12.6.0 and 12.8.1: an `.npmrc` carrying
`ignore-scripts=true` did not stop the script, and the workspace file's `ignoreScripts: true`
did).

- `ignoreScripts: true`: no lifecycle script runs at install, this project's or a dependency's.
- `minimumReleaseAge: 1440`: a version is installable once it has been public for a day. It is
  set explicitly because the default is not strict: pnpm then installs a younger pin anyway and
  writes itself a `minimumReleaseAgeExclude` list. Section 8 refuses that list.
- `allowBuilds: fsevents: false`: `fsevents`, an optional macOS-only dependency of Vite, is the
  one package in the lockfile that declares an install script. It is named so that not building
  it is a written decision.

A command-line flag or a `pnpm_config_*` variable overrides that file (`--ignore-scripts=false`
ran a root `postinstall` past it, pnpm 12.8.1, 2026-10-03), and a pnpmfile runs at install
whatever it says. So section 12 holds the Dockerfile and `scripts/ui.sh` to one
`pnpm install --frozen-lockfile` with no other flag and refuses a `pnpm_config_*` variable in
anything a build reads, and section 8 refuses a pnpmfile.

`scripts/check-pins.sh` section 8 holds a pnpm project to the first two and refuses the exclude
list, a release age that is not strict and any granted build; its selftest proves that a project whose only decision is an
`.npmrc` fails.

## The gate

`make check` runs `ui-lint` (eslint, no warning allowed), `ui-typecheck` (svelte-check, a
warning fails), `ui-test` (vitest under jsdom, no browser) and `ui-build` (the production
build), between `vet` and `build`. `ui-build` copies the build into `internal/ui/dist`, so the
binary the gate builds and the Go tests it runs embed the UI that tree produces.

## The embed

Vite builds into `web/dist`, and the result is copied into `internal/ui/dist`, which
`internal/ui` embeds with `//go:embed all:dist`. That directory holds one committed file, the
placeholder `.gitkeep`, and git ignores everything else in it. The placeholder is what lets
`go build` and `go vet` pass on a tree where no UI was built: the embed directive refuses a
directory with no embeddable file. Vite is not pointed at that directory because its
`emptyOutDir` would delete the placeholder.

A binary therefore embeds one of two things, and `serve` says which at startup:

- **a built UI**, which `ui.Load` reads once: the page with the source offer written into it,
  and each file under `assets/` with the media type its extension names, from a closed list;
- **the placeholder alone** (a plain `go build`), which is a state and not a fault: the root
  path serves the plain-text page.

A tree that has a page and cannot be served as declared is refused whole and logged at WARN:
a page that does not carry the offer's slot exactly once, an asset of a kind the list does not
name, a page that names an asset the build does not hold. No part of it is served; the root stays
the plain-text page, which carries the offer.

## The root path

<a id="root"></a>

`GET /` has two representations:

- **the UI's page**, `text/html`, for a request whose `Accept` header names `text/html` or
  `application/xhtml+xml` with a quality above zero, on a binary that embeds a UI. A browser
  navigation always does.
- **the plain-text page**, for every other request: no `Accept` header, a wildcard, anything
  unreadable, and every request on a binary with no UI. It is what `/` answered before there was
  a UI, byte for byte, so a client that reads it (a health probe, a script) is not changed by
  the UI's arrival, and the surface document's description of `GET /` stays true of a request
  that states no preference.

Both carry the AGPL section 13 Corresponding Source offer (`internal/sourceoffer`): the build
identity, the licence and the source URL in effect. In the page it is a `<footer>` the server
writes in place of an empty slot in `web/index.html`, outside the element the UI mounts in, so
it is on screen whether or not any script runs. The values are escaped where they are rendered.

`GET /assets/*` serves the files the page names, each a map lookup of a name `ui.Load` read
from the build; nothing is resolved against a path at request time, and every other path under
it is 404. Asset names carry a content hash, so they are served as immutable; the page is
`no-cache`.

Neither the page nor the assets need a credential in any configuration, for the reason the
plain-text page never did: they carry no library datum. Every read stays behind
`server_read_token` where one is set.

The page is sent with `Content-Security-Policy: default-src 'none'`, allowing scripts, styles,
images, fonts and connections from this server alone, with no inline script or style, no
framing and no form target. `web/src/page.test.ts` holds the page to having no inline script or
style, and the build inlines no asset.

## The views

<a id="views"></a>

**A view shows what one read endpoint answered, says when it read it, and writes a figure the
server did not give as unavailable or not recorded, never as a zero.**

| View | Reads | Shows |
|---|---|---|
| Summary and savings | `GET /api/summary` | jobs per status, paused and scanning, bytes reclaimed (lifetime and this run), bytes held by the undo window, the per-root table (candidate files and bytes, excluded, projection basis, projected savings, held, free space, and the row for files under no configured root), and the whole-ledger aggregates with the set each covers |
| Queue | `GET /api/queue` | each pending and active row: status, path, `priority`, source size, codec, dimensions, library root, the encoder's own progress where the row carries it, how long it has been in its state; and `queue_total` as "showing N of M", with its age where it has one |
| History | `GET /api/history` with `limit`, `status` and `cursor` | each terminal row's recorded outcome: status, reason, sizes, saving, source facts, scores, when; `history_total`; a filter over the six terminal statuses, a page size, a next page that follows `next_cursor` until it is null, and a way back to the first page |
| Health | `GET /api/health` | the sweep's state and schedule, the sweep under way, the last finished one, their counts and the files found corrupt or unreadable |
| Nodes | `GET /api/nodes` | each worker node (its mode, `mapped` or `http`, its encoders, whether it is polling, cooling off, active leases) and each lease (node, path, its state - `granted`, `uploaded`, `completed`, `failed` or `expired` - reason, epoch, times, sizes), with `leases_total` |

Each rule here has a reason:

- **One view, one endpoint, no arithmetic across them.** A view adds no figure of its own beyond
  a difference of two sizes on one row. A total summed in the page over a capped list would be a
  statistic about the most recent rows that reads as one about the library
  ([ledger totals](ledger-totals.md#null-is-not-zero)); the server's totals are over the whole
  ledger, so the page shows those and says "showing N of M".
- **A figure's age is stated beside it.** The server answers every total (`queue_total`,
  `history_total`, `leases_total`) and every whole-ledger aggregate with `age_seconds`: it may
  serve them from a cache up to 30 seconds old, while the rows beside them are read fresh. Where
  that age is above zero the page writes it in words ("total as of 12 s before this read"); an
  age that is zero, null or absent is claimed as nothing. Where more rows are shown than the
  total counts, the page does not write "N of M": it says the total is the older of the two.
- **Null is written in words.** A field that is absent, null or of another type is read as null
  (`web/src/lib/shape.ts`) and shown as "unavailable" or "not recorded". A count beside
  `available: false` is not read at all. A row with no progress shows none rather than 0%.
- **A body of another shape is not rendered.** Where the answer is JSON but not the endpoint's
  shape, the view shows an alert and no table: a table drawn from some other document would be
  a confident wrong answer.
- **A refusal is shown in the server's words**, with its status. A 400 from the history shows
  the envelope's `error` and each parameter's `error`, and none of its rule tokens
  (`invalid-query`, `status-not-terminal`, `cursor-undecodable`, `cursor-filter-mismatch`); the
  page never branches on that prose. A sentence over 300 characters is cut there and ends in
  "...", never dropped: the status text that would stand in for it is empty under HTTP/2. A
  404 says the server predates the view. A 401 on a read says a token is needed.
- **The history is filtered and paged by the server.** The page sends the ticked statuses and
  the cursor it was given and shows what comes back; it never filters or reorders rows itself,
  so `history_total` and the rows are about the same set. `next_cursor` is always in the
  answer, a string or null, and null is the last page; a server that answers without it
  predates paging, and the view says its filter is not applied.
- **Polling, not the event stream.** A view reads on an interval while the page is visible, and
  on a manual refresh; leaving it aborts the read in flight. `GET /api/events` is not used
  because the browser's `EventSource` cannot send an `Authorization` header, and the only other
  ways to authenticate a stream are a credential in the URL or a cookie, both of which
  [the token rule](#token) forbids. Polling `GET /api/summary` costs the server no more than
  one refresh of its figures per interval, however often it is asked.
- **The health view and the nodes view have no control.** The sweep reports and repairs nothing
  ([health sweep](health-sweep.md#health-sweep)), and a lease is granted and ended by the
  protocol alone ([nodes](nodes.md#leases)).
- **Times are local with a numeric offset; sizes are binary units** (KiB, MiB, GiB, TiB) with
  the exact byte count in the element's `title`.

Which view is shown is state of the running page. It is not written to the address, so the page
has one URL and nothing about a session can be read from it.

The views are shown once `GET /api/schema` has answered: a server whose surface cannot be read
is one whose reads cannot either.

## The controls

<a id="controls"></a>

**The page drives exactly the mutating endpoints the API offers - pause, resume, a library scan,
a scan of named paths, and the withheld paths - shows what the server answered, and claims
nothing it was not told. `restore` and `requeue` are not on the page in any form.**

| Control | Request |
|---|---|
| Pause, resume | `POST /api/pause`, `POST /api/resume` |
| Start a library scan | `POST /api/rescan` |
| Scan named paths | `POST /api/scan` with `{"paths": [...]}` |
| Withheld paths: list, add, remove | `GET`, `POST`, `DELETE /api/exclusions` |

- **Only these, because these are the ones that cannot touch a file.** Each starts a scan,
  toggles the feeding of new files, or takes a path out of the pipeline. `restore` overwrites a
  library file with older bytes, and `requeue` re-opens a row a terminal decision already
  answered; both are local commands by the owner's decision, the server registers no route for
  either, and the control token does not authorise them. So the page has no route, button,
  text or identifier for either: `web/src/sources.test.ts` reads every file under `web/` and
  fails on either word, and `internal/server/no_undo_routes_test.go` walks the real router and
  fails on a route pattern that contains one.
- **The answer shown is the server's.** After a pause the page writes the `paused` and
  `scanning` the response carried, not the state it asked for; an answer that carries no boolean
  `paused` is an alert saying the server did not state it, whatever its status. A library scan
  is reported as started only where the body says `started: true`; a 409 shows the server's
  `reason`. A named
  scan shows the accepted and refused counts and, per path, the rule and detail the server
  gave. An exclusion is reported as changed only where `changed` is true. A request that got
  no answer says nothing is known to have changed.
- **A refusal says which refusal it is.** 403 means no control token is configured on the
  server, so the controls are off for every caller; 401 means the token sent is not the control
  token (a read token is answered 401 on a control). The page reads `GET /api/exclusions` when
  the controls are opened, with whatever token it holds, and that one answer tells the two
  apart before anything is pressed. Without an accepted token the controls are disabled and the
  page says why: they are enabled only once that read, made with the token held, has answered
  2xx. While it is in flight, refused with any other status, or unanswered, every control stays
  disabled, the page states which of those it is, and one button asks again. A token that is
  changed or forgotten drops the previous answer and the list it carried before the next read
  returns.
- **Priority is shown and is not a control.** Nothing on the API sets it; it is what the
  configuration says for the file.

## The token

<a id="token"></a>

**The token lives in a variable of the running page and nowhere else. It is sent only as
`Authorization: Bearer` to paths under `/api/` on the origin the page came from, and no
redirect is followed. A reload forgets it.**

One field takes one token. The control token drives the controls and is accepted on the reads;
a read token opens the reads where `server_read_token` is set. The page does not ask which it
was given: the server's answers say.

- **Not in storage.** `localStorage`, `sessionStorage`, IndexedDB and cookies outlive the page
  and are readable by any script that later runs on the origin, by other tabs and by anyone at
  the machine. A token in a variable is gone when the tab closes, so the page holds the control
  token for the browser session at most, and a stolen disk or profile carries none.
- **Not in a URL.** A query string, a fragment or the history state reaches the address bar,
  the browser's history, a copied link and a proxy's access log. The API module builds every
  query itself and refuses a path that is not under `/api/`, so the token cannot be sent to
  another origin or a non-API path; the request is not made.
- **Not along a redirect.** Every request is made with `redirect: "error"`,
  `mode: "same-origin"` and `credentials: "omit"`. A browser that followed a same-origin 3xx
  would send the `Authorization` header again to wherever it pointed, which need not be under
  `/api/`; with `redirect: "error"` the request fails instead and the page reports it as no
  answer, in a sentence that names a redirect as one cause. `same-origin` makes a request to any
  other origin fail before it is sent, and `omit` keeps cookies and HTTP authentication out of
  every request, so the header is the only credential. The token is in that one header and no
  other.
- **Not in a log, not on the page.** The field is `type="password"` with `autocomplete="off"`,
  is emptied when its value is taken, and is not inside a form, since a form is what a browser
  offers to remember a password from and the page's policy gives a form no target. The token is
  rendered nowhere, including inside an error: a token that is not printable ASCII without a
  space is refused before `fetch` is called, in a fixed sentence, because an engine may quote a
  header value it cannot send; and a failure or refusal whose words repeat the token is replaced
  by a fixed sentence before any view sees it.
- **Forget.** One button drops the token and empties the field; the next request carries no
  credential and the controls say they are unavailable.

The cost is typing the token again after a reload. That is the intended trade: the default
bind is loopback and the controls are off until an operator sets `server_auth_token`, and a
page that remembered the credential would be the one place it is kept outside the secret store
it is referenced from ([secrets](../secrets.md)).

`web/src/token.page.test.ts` holds the page to this: it spies on the storage and cookie
setters, on IndexedDB, on the history and on the console, uses the token through every view
and a control, and asserts that none was called with it, that the address is unchanged, that
the token is in every request's `Authorization` header and in no request's URL or body, and
that it is nowhere in the rendered page. The Content-Security-Policy is the second line: with
`connect-src 'self'` and no inline script, a script that reached the page could not send the
token elsewhere.

## The image

The Dockerfile's `ui` stage builds the UI on `node:<version>-trixie-slim`, pinned by tag and
digest on its `FROM` line, with `pnpm install --frozen-lockfile` and the same `vite build`. It
checks that the image's Node is the pinned one. The `build` stage copies the result into
`internal/ui/dist` and checks it arrived before compiling. The runtime image is unchanged: it
is distroless, and it gains nothing but the bytes embedded in the binary. No Node, pnpm or
`node_modules` ships.

`scripts/smoke-image.sh` starts the real `serve` in the built image and reads `/` both ways:
the page with the offer and its script for a request for HTML, the plain-text page with the
offer otherwise.

## What ships inside the bundle

The built JavaScript contains this repository's own source and the Svelte runtime, and no other
package (read from the module list of a production build, 2026-10-03). Svelte is MIT; `NOTICE`
carries its copyright and permission notice, and section 12 holds the version it names to
`web/package.json`. Every other package is a build or test tool and is not distributed. The
licences across the lockfile (`pnpm licenses list`, 2026-10-03): MIT 166, Apache-2.0 17, ISC 8,
BSD-2-Clause 8, BSD-3-Clause 3, MIT-0 2, MPL-2.0 2, BlueOak-1.0.0 2, CC0-1.0 1.
