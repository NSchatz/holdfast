# Web UI

How the web UI is built, how it reaches the binary, and what the root path answers. This
document is that argument's single home: `CLAUDE.md` names the rule and links here rather than
restating it. The source is `web/`; the embed is `internal/ui`; the routes are in
`internal/server/server.go` (`handleRoot`, `handleUIAsset`); the daemon's wiring is
`cmd/holdfast/ui.go`; the gate's steps are `scripts/ui.sh`; the pins are held by
`scripts/check-pins.sh` sections 8 and 12.

What exists is the toolchain, the gate, the embed, the serving and the image stage, with a
shell page that states which holdfast answered and that its API is reachable. The views are
not built yet.

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
