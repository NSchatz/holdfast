# Verify: research-nodes-clients-spa.md (adversarial)

Verifier pass on `/cache/tmp/plan-2026-09-holdfast/research-nodes-clients-spa.md`. Nothing in
/workspace was changed. Scratch: `/scratch/verify-spa` (every test dir is kept there). All sources
read 2026-09-29 (02:00-02:10 UTC). `make check` was not run.

## Verdict table

| # | Claim | Verdict |
|---|---|---|
| 1 | Versions: svelte 5.57.1, vite 8.3.1, vite-plugin-svelte 7.3.1, svelte-check 4.7.6, vitest 5.0.2, eslint 10.11.0, pnpm 12.6.0, Node 24.21.0 LTS, TS 6.0.3 newest supported | CONFIRMED (one nuance: pnpm 12.8.1 is published, on `next-12`) |
| 2 | Node 26 becomes LTS 2026-10-28 | CONFIRMED (planned date in the schedule) |
| 3 | pnpm 11+ ignores `ignore-scripts` in `.npmrc`; only workspace yaml / `pnpm_config_` env stop a root postinstall | CONFIRMED, but "only" is OVERSTATED: `--ignore-scripts` and global `~/.config/pnpm/config.yaml` also stop it |
| 4 | pnpm 12 default `minimumReleaseAge` blocks packages < 1 day old | PARTLY REFUTED: the 1440 default exists, but it is non-strict, so it does NOT block an explicit young pin |
| 5 | Vite 8 has no esbuild dependency; toolchain install has zero install-script packages | PARTLY REFUTED: no esbuild CONFIRMED; the lockfile does carry `fsevents@2.3.3` with `install: node-gyp rebuild` |
| 6 | `//go:embed all:dist` builds with only `.gitkeep`; empty or missing dir fails build/vet | CONFIRMED |
| 7 | `node:24.21.0-bookworm-slim` index digest `sha256:0e0ff40c...f9b6` | CONFIRMED |
| 8 | Plex official spec has POST refresh `?path=`, GET `/status/sessions`, X-Plex-Token | CONFIRMED (nuance: both need the `admin` token scope) |
| 9 | Sonarr v4.0.20 / Radarr v6.4.4, API v3, Rescan commands, upgrade = Download + isUpgrade | CONFIRMED |
| 10 | Unmanic #635 exists and describes a ~55-byte 404 body accepted as the transcoded file | CONFIRMED (a single, untriaged reporter) |

## Evidence

### 1. Versions - CONFIRMED
- `npm view <pkg> dist-tags` shows `latest` values: svelte 5.57.1, vite 8.3.1,
  @sveltejs/vite-plugin-svelte 7.3.1, svelte-check 4.7.6, vitest 5.0.2, eslint 10.11.0,
  @eslint/js 10.0.1, eslint-plugin-svelte 3.23.0, typescript-eslint 8.71.0, typescript 7.0.2,
  pnpm 12.6.0. All match the research.
- Peer ranges: svelte-check@4.7.6 `typescript: "^5.0.0 || ^6.0.0"`; typescript-eslint@8.71.0,
  8.70.0, @typescript-eslint/parser and typescript-estree 8.71.0 all `">=4.8.4 <6.1.0"`.
  The typescript publish list has 6.0.2 (2026-03-23), 6.0.3 (2026-04-16), 7.0.2 (2026-07-08) and
  no 6.1.x. So 6.0.3 is the newest version both peers accept, and TS 7 is excluded by both.
- Node: https://nodejs.org/dist/index.json shows v24.21.0 `lts: "Krypton"`, 2026-09-07, the
  newest v24. v22.23.3 is Jod and v26.10.0 is Current. Matches.
- Nuance (not a refutation): 12.6.0 is `latest`, but pnpm 12.7.0 (09-25), 12.8.0 and 12.8.1
  (09-28) are published under `next-12`. Also, `vite@8.3.1` declares the peer `esbuild: "^0.27.0 ||
  ^0.28.0"` as optional (see 5).

### 2. Node 26 LTS 2026-10-28 - CONFIRMED
- https://raw.githubusercontent.com/nodejs/Release/main/schedule.json gives
  `v26: {start 2026-05-05, lts 2026-10-28, maintenance 2027-10-20, end 2029-04-30}` and
  `v24: {lts 2025-10-28, maintenance 2026-10-20, end 2028-04-30}`. These match the research.
  It is a plan, not an event yet.

### 3. pnpm 11+ and `.npmrc ignore-scripts` - CONFIRMED (the word "only" is overstated)
Local re-test uses a root `postinstall` that writes a marker file, run in fresh dirs with
`/scratch/verify-spa/c3/run.sh` and `run2.sh`. The container env has no `pnpm_config_*` or
`npm_config_ignore_scripts` set, but `~/.npmrc` does contain `ignore-scripts=true`, so the
isolating runs use a clean `HOME`.

| surface | pnpm 10.34.6 (control) | 11.27.1 | 12.6.0 | 12.8.1 |
|---|---|---|---|---|
| nothing (clean HOME) | RAN | RAN | RAN | RAN |
| project `.npmrc ignore-scripts=true` | did-not-run | RAN | RAN | RAN |
| user `~/.npmrc ignore-scripts=true` | did-not-run | RAN | RAN | RAN |
| `npm_config_ignore_scripts=true` | - | RAN | RAN | - |
| `pnpm-workspace.yaml ignoreScripts: true` | - | did-not-run | did-not-run | - |
| `pnpm_config_ignore_scripts=true` | - | did-not-run | did-not-run | - |
| `--ignore-scripts` | - | did-not-run | did-not-run | - |
| global `~/.config/pnpm/config.yaml ignoreScripts: true` | - | - | did-not-run | - |
| same file, kebab `ignore-scripts: true` | - | - | RAN (`config get` = undefined) | - |

- The control run shows the harness can tell the two outcomes apart: pnpm 10 honours both
  `.npmrc` files, and 11 and 12 honour neither.
- The "only" is overstated because the CLI flag and the global pnpm `config.yaml` (camelCase
  key only) also stop the script. Neither is a committable repo file, so the research's
  check-pins section-8 conclusion stands: in a repo, `pnpm-workspace.yaml` is the decision
  surface.
- Side finding: the container's global CLAUDE.md says "npm/pnpm run with ignore-scripts=true
  by default" via `~/.npmrc`. That is false for the container's own pnpm 12.6.0 shim, which ran
  the root postinstall. By the same rule, the container env's
  `npm_config_manage_package_manager_versions=false` is presumably inert under pnpm 11+
  (inferred, not tested).

### 4. `minimumReleaseAge` default - PARTLY REFUTED ("blocks" is overstated)
- Docs, https://pnpm.io/settings/dependency-resolution: `minimumReleaseAge` "Default: 1440
  minutes (since v11)". Also `minimumReleaseAgeStrict` "Default: true if minimumReleaseAge is
  explicitly configured; false otherwise ... When false, pnpm falls back to a version that
  doesn't meet the minimumReleaseAge constraint."
- Local re-tests with pnpm 12.6.0, using typescript-eslint 8.71.0 (published
  2026-09-28T17:12Z, under 10 h old):
  - `pnpm add typescript-eslint@8.71.0`: exit 0. pnpm printed "Added 11 entries to
    minimumReleaseAgeExclude in pnpm-workspace.yaml (set minimumReleaseAgeStrict to true to gate
    these updates with a prompt)". The young pin was **not blocked**; pnpm wrote itself an
    exemption.
  - Exact `8.71.0` in package.json, then `pnpm install`: exit 0, with the same auto-exclude.
  - Range `^8.69.0`: exit 0, and it resolved 8.70.1. The young version was avoided by
    fallback, not by an error.
  - `--frozen-lockfile` against a lockfile holding 8.71.0 with no excludes, **warm store**:
    exit 0 (also with `CI=true`).
  - The same on a **cold store**: exit 1, `ERR_PNPM_MINIMUM_RELEASE_AGE_VIOLATION`. This
    reproduces the research's observation, but it depends on store state.
  - With `minimumReleaseAge: 1440` set **explicitly** in pnpm-workspace.yaml, the add fails
    with `ERR_PNPM_NO_MATURE_MATCHING_VERSION`, exit 1, and no exclude is written.
- Consequence: the research's advice to "leave `minimumReleaseAge` at default" is wrong for a
  tripwire. Set `minimumReleaseAge: 1440` explicitly, which makes it strict, and have
  check-pins refuse a non-empty `minimumReleaseAgeExclude`, because pnpm writes that key on its
  own.

### 5. No esbuild, zero install scripts - PARTLY REFUTED
- vite@8.3.1 `dependencies` are postcss, rolldown ~1.2.9, picomatch, tinyglobby and
  lightningcss. esbuild is only an optional peer (`peerDependenciesMeta.esbuild.optional`).
  After a fresh pnpm 12.6.0 install of the exact probe set (219 packages,
  `/scratch/verify-spa/c5`), there are 0 esbuild dirs. The 2 lockfile mentions are the peer
  spec only. **No esbuild: CONFIRMED.**
- Installed set (Linux): of 219 real package dirs, none declares
  `preinstall`/`install`/`postinstall`, and none ships a `binding.gyp`. Only `prepare` scripts
  appear, and those do not run for registry deps. The lockfile has 0 `requiresBuild` entries.
- **But the lockfile holds 244 packages, not 219.** The registry abbreviated metadata
  (`hasInstallScript`) for all 244 gives **1 true: `fsevents@2.3.3`**, vite's darwin-only
  optional dep, whose manifest says `"install": "node-gyp rebuild", "gypfile": true, "os":
  ["darwin"]`. So "zero packages with install scripts in the lockfile" is false. It holds only
  for a Linux install.
- Why it may matter: pnpm 12.6.0 defaults hard-fail on an unapproved dependency build. A test
  with esbuild 0.25.10 gave `ERR_PNPM_IGNORED_BUILDS`, exit 1. Whether a real macOS
  `pnpm install` trips on fsevents is **UNCERTAIN**. A Linux simulation with
  `supportedArchitectures.os: [current, darwin]` installed fsevents and exited 0, but pnpm
  likely skips builds for foreign-platform packages, so that run proves nothing. The rule "no
  `allowBuilds` entries" may need an explicit `fsevents: false` for macOS contributors.

### 6. go:embed - CONFIRMED
`mise exec go@1.25.14`, module `/scratch/verify-spa/c6`:

| case | go build | go vet |
|---|---|---|
| `all:dist` + only `dist/.gitkeep` | ok | ok |
| `all:dist` + empty `dist/` | fail: "cannot embed directory dist: contains no embeddable files" | same |
| `all:dist` + no `dist/` | fail: "pattern all:dist: no matching files found" | same |
| `dist` (no `all:`) + only `.gitkeep` | fail: "contains no embeddable files" | same |
| `dist` + `index.html` | ok | ok |

Git does not track an empty dir, so a clone without a committed `.gitkeep` lands in the
"missing" row.

### 7. Docker digest - CONFIRMED
Anonymous token from auth.docker.io, then a `HEAD` on
registry-1.docker.io/v2/library/node/manifests/24.21.0-bookworm-slim:
`content-type: application/vnd.oci.image.index.v1+json`,
`docker-content-digest: sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6`.
The index lists linux/amd64, arm64v8 and ppc64le plus attestations. The Hub tag API shows
`tag_last_pushed 2026-09-19T05:40:39Z`. `24-bookworm-slim` currently resolves to the same digest.
`24.21.0-trixie-slim` is `sha256:8ec5d7557396cfe32d21c3f9c13072355ceab22b584578ca4bb28af31120cffe`,
which also matches. Tags are mutable (rebuilds), so the digest pin is the right call.

### 8. Plex - CONFIRMED
https://developer.plex.tv/pms/ embeds an OpenAPI spec (`info.title "Plex Media Server"`,
`version "1.2.3"`, changelog "1.2.3 (Supported in PMS >= 1.43.4)"):
- `"/library/sections/{sectionId}/refresh"` defines `post` (operationId
  `librarySectionPostRefresh`, query `force` enum 0|1, query `path` "Restrict refresh to the
  specified path") and `delete`. It has no `get`.
- `"/status/sessions"` `get`: "List all current playbacks on this server".
  `/status/sessions/terminate` is `post`.
- `securitySchemes.user_token = {type: apiKey, in: header, name: X-Plex-Token}`.
- Nuance the research omits: both endpoints declare `security: [{user_token: ["admin"]}]`, so
  holdfast needs the server-owner/admin token.

### 9. Sonarr / Radarr - CONFIRMED
- `gh api repos/{Sonarr/Sonarr,Radarr/Radarr}/releases/latest` returns v4.0.20.3014
  (2026-09-16T16:39Z) and v6.4.4.10685 (2026-09-16T18:13Z), neither a prerelease. Sonarr's
  default branch is `v5-develop`.
- At those tags, `src/` holds only `Sonarr.Api.V3` / `Radarr.Api.V3`, and `CommandController` is
  `[V3ApiController]`. `RescanSeriesCommand { int? SeriesId }` and
  `RescanMovieCommand { int? MovieId }` match.
- `WebhookEventType` enums match the research's lists exactly. Neither has an `Upgrade` member.
- `WebhookBase.BuildOnDownloadPayload` sets `EventType = WebhookEventType.Download` with
  `IsUpgrade = message.OldFiles.Any()` (Sonarr) / `message.OldMovieFiles.Any()` (Radarr), plus
  `DeletedFiles`. Sonarr's `BuildOnImportCompletePayload` also uses `EventType = Download`, with
  `EpisodeFiles`. `WebhookImportPayload` fields match.

### 10. Unmanic #635 - CONFIRMED (with a caveat)
`gh api repos/Unmanic/unmanic/issues/635`: "[Bug]: Remote worker result can be deleted before
the origin instance downloads it". Opened 2026-08-14T13:42:12Z by Arturoe1; state open;
0 comments. The body says: "the origin instance receives a tiny invalid file instead of the
completed media ... only ~55 bytes and contained the HTTP 404 response body", with root cause
`post_process_remote_file()` removing `source_path == final_destination`.
Caveat: this is a single user report with no maintainer triage. The claim "accepted as the
transcoded media" is the reporter's framing; the issue does not show the library file being
replaced.
