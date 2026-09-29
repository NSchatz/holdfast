# Research: node protocol (T46), Plex / Sonarr / Radarr clients (T28), SPA toolchain (T18/T21/T22)

Read-only research for the holdfast program. Nothing in /workspace was changed. Date read for
every web source: 2026-09-29. "ASSUMED" = from memory, not verified today. "LEAD" = forum or
secondary source, not authoritative.

Repo context read: `CLAUDE.md`, `internal/server/server.go` (chi router; `/api` read group behind
`requireReadToken`, mutating group behind `requireToken`, bearer tokens; `/` plain-text source
offer; no TLS code anywhere under `cmd/` or `internal/server/` - the documented posture is a
reverse proxy, `docs/docker.md` ~l.145), `internal/fsclass/fsclass.go` (Local / NonLocal /
Undetermined, only a positive Local counts), `docs/design/swap.md` (temp beside the source,
fsync, rename, parent-dir fsync; `scratch_dir` copies accepted bytes into a temp beside the
source, never renames across), `docs/docker.md:220-300` (`POST /api/scan` + *arr Custom Script /
Webhook-via-shim), `scripts/check-pins.sh` section 8, `Makefile` `check:` target.

---

## 1. Node protocol - how the incumbents do it

### Tdarr (server + nodes)
- Transport: node opens a **persistent outbound Socket.IO connection** (WebSocket or HTTP
  long-polling) to the server on port 8266; "The Node initiates this connection and does not
  require an inbound port." - https://docs.tdarr.io/docs/troubleshooting/ (2026-09-29)
- Path mapping: "Mapped" nodes "on the same file system as the server or have mapped
  volumes/shares which use Tdarr path translators" - https://docs.tdarr.io/docs/nodes/nodes
  (2026-09-29). Config shape `"pathTranslators": [{"server": "/media", "node": "/mnt/media"}]`
  (LEAD, search-result snippet of the same docs).
- File transfer: "Unmapped" nodes - "Download and upload of working files is handled
  automatically"; enabling them makes library + cache "accessible through Tdarr's network API.
  It's best to have authentication enabled." (same page). Free tier caps unmapped transfers at
  10 MB, Pro lifts it (LEAD, diymediaserver.com guide via search, 2026-09-29).
- Leasing/heartbeat: not documented publicly. Disconnect detection is implicitly the socket
  drop (ASSUMED). Tdarr's replace-before-verify data loss is the reason holdfast exists
  (`docs/design/swap.md`).

### FileFlows (processing nodes)
- Path mapping: node maps every server path used by libraries/flows to a local path, and
  normalises separators per OS - https://docs.fileflows.com/nodes and
  https://docs.fileflows.com/guides/external-processing-node (search snippets 2026-09-29; the
  docs host did not resolve for a direct fetch, so treat details as LEAD).
- Node keeps a local copy of configuration (flows, libraries, plugin settings) and a private temp
  dir the server never touches (same source, LEAD).
- Transport: HTTP REST from node to server, node polls for work (ASSUMED; not confirmed today).

### Unmanic (linked / remote installations)
- "A local installation connects to one or more remote installations. Tasks are queued on the
  local installation. When a remote installation is available ... tasks are assigned to it."
  HTTP API between installations. Routing by matching library NAME. -
  https://docs.unmanic.app/docs/configuration/linking/link_overview/ (2026-09-29)
- File transfer: if the remote can reach the file via the shared library path it reads it there;
  otherwise the file is uploaded through the API, and results come back by the local
  installation downloading over HTTP from the remote's cache (same page).
- Cautionary bug: https://github.com/Unmanic/unmanic/issues/635 (opened 2026-08-14, open): the
  remote deletes the handoff file before the origin downloads it, and the origin intermittently
  accepts a **~55-byte HTTP 404 body as the transcoded media**. Exactly the class of failure a
  size+checksum+status check at the receiver prevents. (2026-09-29)

### Lessons for holdfast
1. Worker-initiated outbound connection (Tdarr) is the right NAT/firewall shape.
2. Path mapping as an ordered prefix table (Tdarr/FileFlows) is the proven UX.
3. Nobody publishes a lease/heartbeat/fencing contract; holdfast must define one.
4. Unmanic #635 is the exact argument for: receiver-side digest verification, never treating
   a response body as media without status + length + digest, and the server (not the worker)
   owning every file lifecycle in the library.

---

## Proposal T46 - the node protocol

### Constraints inherited from decisions and the invariant
- Worker only encodes; the **server re-runs every gate and performs the rename** (T16). So the
  server must hold both the source and the output on its own filesystem at gate time.
- The output must end up as a **temp beside the source** (same directory, same fs) because the
  swap is a same-directory rename; `scratch_dir` rule says never rename across filesystems.
- Media access per node: shared mount + path map, OR HTTP streaming (T17). Auth: `node_token`
  by reference, joins `config.SecretBearingKeys` (T23).

### Options

**A. HTTP+JSON lease API on the existing chi server + `holdfast worker` subcommand (pull).**
- Endpoints (sketch, all under `/api/node/v1`, behind a new `requireNodeToken` group):
  - `POST /lease` body `{node_id, version, slots_free, caps:{encoders, hw}}` -> `200 {lease_id,
    epoch, job_id, ttl_sec, source:{server_path, size, mtime_ns, sha256?}, encode_spec,
    transfer:"mapped"|"http"}` or `204` + `Retry-After` (long-poll <= 30 s).
  - `POST /lease/{id}/heartbeat {progress, fps}` -> `200 {ttl_sec}` or `410 Gone` (expired /
    revoked -> worker kills ffmpeg, deletes its local temp).
  - `GET /lease/{id}/source` (http mode) - `Range` supported, `Repr-Digest: sha-256=...`
    (RFC 9530) as an HTTP **trailer** computed while streaming, so the server never pre-reads.
  - `PUT /lease/{id}/output` with `Content-Length` and `Content-Digest: sha-256=...`.
  - `POST /lease/{id}/complete {output_sha256, size, stats}` / `POST /lease/{id}/fail {reason}`.
- Costs: zero new dependencies (net/http, chi, crypto/sha256); reuses auth, metrics, SSE,
  `api-schema-diff`; curl-debuggable; fakes are trivial (`httptest`). You write the lease state
  machine and the retry logic yourself (~the same in every option). Heartbeat latency bound =
  TTL, not instant disconnect detection.

**B. gRPC (bidi stream for leases/heartbeats, client-stream for upload).**
- Costs: google.golang.org/grpc + protobuf + protoc/buf toolchain to pin in the Makefile and
  check-pins; generated code; does not mount on the chi mux without h2c or a second port;
  reverse proxies need end-to-end HTTP/2; not curl-able; the UI (T18) still needs JSON anyway, so
  two API styles. Gains: instant disconnect via stream close, typed schema. Licence Apache-2.0
  (ASSUMED) - fine for AGPL.

**C. Persistent WebSocket (Tdarr-style) + HTTP for bulk bytes.**
- Costs: a websocket dependency (stdlib has none), reconnection/replay semantics, still needs
  leases because a socket drop is not proof the worker stopped encoding or writing. Gains:
  push assignment, instant disconnect signal.

**D. External broker (NATS/Redis).** Rejected: breaks "a stranger can `docker run` it".

### Recommendation: Option A, with these rules

1. **Lease expiry / crashed worker.** Lease TTL default 60 s, heartbeat every 15 s (TTL/4).
   Leases live in `internal/store` (durable across server restart). On expiry the server marks
   the attempt `abandoned`, deletes only temps it named for that `lease_id`, and requeues with
   `attempt+1`; after N attempts (default 3) the job SKIPS with a logged reason (fail-safe rule,
   no poison loop). **Fencing:** every call carries `lease_id` + `epoch`; any call on an expired
   lease gets `410` and its bytes are discarded - a stale worker can never contribute an output
   to a swap. On server restart, all in-flight leases are treated as expired unless their
   worker heartbeats within one TTL (the worker's heartbeat after restart gets `410` if the
   server already reassigned).
2. **Idempotent result upload.** The upload target is keyed by `(lease_id, epoch)`. First `PUT`
   writes `<srcdir>/.holdfast-<lease_id>.part`, fsyncs, verifies length + sha256 against the
   header, then renames to `<srcdir>/.holdfast-<lease_id>.out` (same-dir, still a temp) and
   records `output_sha256` in the store. A repeat `PUT`/`complete` with the same digest returns
   the recorded result (`200`, no rewrite); a different digest for an accepted lease -> `409`.
   A `PUT` interrupted mid-body leaves only the `.part`, deleted on retry or on lease expiry.
   (Resumable uploads - IETF httpbis resumable-upload draft / tus - are a later option, LEAD.)
3. **Checksums.** sha256 both directions: server->node as a `Repr-Digest` trailer on the source
   stream (node verifies before encoding; mismatch = `fail`, not a retry of the encode); node->
   server as `Content-Digest` on upload, verified before the file is admitted. In mapped mode the
   server still hashes the output it reads and compares with the node's report before gates, to
   catch a wrong path map or an NFS cache lie. Digest is transport integrity only; gates remain
   the fidelity proof. The lease also records source size+mtime so the existing
   "source rewritten mid-encode" guard fires at swap time.
4. **Where the output lands.** Always a temp in the **source's own directory** on the server's
   filesystem, named by the server (never by the node), then gates, then the existing durable
   rename. The free-space guard (S0158) runs at lease grant as a *reservation* for the expected
   output temp, and again at upload start (`Content-Length` known). The source directory must
   still classify `Local` via `fsclass` on the server; the node's view of the mount is
   irrelevant to the swap. Recommend the default for mapped mode is **read source via mount,
   upload output over HTTP** so the server is the only writer into library directories; a
   node writing its output into the mapped source dir is an opt-in (costs: NFS close-to-open
   semantics, the server must re-hash anyway).
5. **Gates on the server.** Cost to flag: decode-integrity and VMAF on the server are full
   decodes of source and output; the server needs CPU sized for that (ASSUMED, a meaningful
   fraction of encode time). Workers MAY run the gates as a pre-filter to avoid uploading a
   reject; the server verdict is the only one that licenses a swap.
6. **TLS stance.** holdfast has no TLS today. Recommend: optional built-in TLS
   (`server_tls_cert`/`server_tls_key` by file reference, stdlib crypto/tls) *or* a reverse
   proxy; the worker **refuses an `http://` server URL** unless the host is loopback or the
   operator sets an explicit `worker_insecure_http: true` (logged loudly at start), because the
   channel carries `node_token` and, in http mode, the whole library. `node_token` is distinct
   from the control/read tokens (a stolen node token can lease and upload, never pause, exclude
   or restore). Checksums are not a substitute for TLS.
7. **Backpressure.** Node declares `slots`; server caps leases per node and a global
   `max_concurrent_transfers`; lease grant is refused (`204` + `Retry-After`) when the
   free-space reservation or transfer cap fails; uploads use `http.MaxBytesReader` bounded by
   the declared length and the reservation; `503` + `Retry-After` for transient refusal; worker
   uses exponential backoff with jitter. Long-poll avoids tight polling.
8. **Versioning.** `/api/node/v1`, worker sends its version; server rejects an incompatible major
   with `426`/`409` and a reason (Tdarr's docs call out version mismatch as a disconnect cause).
9. **Tests.** Fake worker and fake server via `httptest`; fixture cases for: expired lease upload
   discarded, duplicate upload idempotent, digest mismatch rejected, 404-body-as-media (the
   Unmanic #635 shape) rejected, disk reservation refusal, server restart with live leases.

---

## 2. Plex / Sonarr / Radarr APIs (T28)

### Plex Media Server
- **Official reference exists**: https://developer.plex.tv/pms/ (Redoc page, "Plex Media Server"
  API version 1.2.3, states PMS >= 1.43.4 for that version; "PMS has never used API versioning
  before the creation of this document") (2026-09-29).
- **Auth**: header `X-Plex-Token` ("An authentication token, obtained from plex.tv");
  `X-Plex-Client-Identifier` ("An opaque identifier unique to the client") is also listed among
  common headers - send both (same page). JSON via `Accept: application/json` (same page).
- **Partial scan**: `POST /library/sections/{sectionId}/refresh` with query params `force`
  (0|1) and `path` - "Restrict refresh to the specified path"; `DELETE` on the same path cancels
  a refresh (same page, extracted from the page HTML). Older docs and scripts use `GET` with
  `?path=`&`X-Plex-Token=` - LEAD: https://www.plexopedia.com/plex-media-server/api/library/scan-partial/
  (support.plex.tv URL-commands article returned 403 to fetch). Use POST per the official spec.
- **Section lookup**: `GET /library/sections`; each section carries `Location` entries with the
  filesystem paths - map a swapped file to its section by longest `Location` prefix (with a
  holdfast->Plex path map, since Plex sees its own mount paths).
- **Sessions**: `GET /status/sessions` - "List all current playbacks on this server" (same page).
  Mapping session -> file: each playing `Metadata` item carries `Media[].Part[].file` (the Part
  schema has a `file` attribute in the spec; the per-session shape is ASSUMED from common use,
  verify against a fake fixture built from a real capture). `Session.id`, `Player.state`
  identify the playback. Terminate: `POST /status/sessions/terminate?sessionId=&reason=`
  (same page) - holdfast should never call it; it only needs to *hold*.
- Design note for "hold a file being played": the existing Tautulli pause
  (`internal/schedule/tautulli.go`) is global and **fails open**. A per-file hold that fails
  open is defensible because rename(2) leaves an already-open fd on the old inode on a local fs
  (ASSUMED; on SMB/NFS the client side can differ) - owner should ratify fail-open vs.
  fail-closed per file.

### Sonarr (latest release v4.0.20.3014, 2026-09-16; default branch is `v5-develop`, v5 not released)
### Radarr (latest release **v6.4.4.10685**, 2026-09-16 - Radarr is on v6, not v5)
Source: `gh api repos/{Sonarr/Sonarr,Radarr/Radarr}/releases/latest` (2026-09-29); code read at
those tags.
- **API**: both `v3` - https://sonarr.tv/docs/api/ , https://radarr.video/docs/api/ (2026-09-29).
- **API key**: header `X-Api-Key`, or query `apikey` (Sonarr
  `src/Sonarr.Http/Authentication/AuthenticationBuilderExtensions.cs` l.61-62 at v4.0.20.3014);
  the handler also accepts `Authorization: Bearer <key>` (`ApiKeyAuthenticationHandler.cs`).
  Radarr shares the codebase lineage (ASSUMED identical).
- **Rescan**: `POST /api/v3/command` body `{"name":"RescanSeries","seriesId":N}` (Sonarr
  `MediaFiles/Commands/RescanSeriesCommand.cs`: `int? SeriesId`) and
  `{"name":"RescanMovie","movieId":N}` (Radarr `MediaFiles/Commands/RescanMovieCommand.cs`:
  `int? MovieId`). JSON camelCase (ASSUMED). There is no rescan-by-path command: resolve
  path -> id via `GET /api/v3/series` / `GET /api/v3/movie` and the `path` field (ASSUMED field
  name; verify in fixture). Omitting the id rescans everything - never send that.
- **Webhook (Settings > Connect > Webhook)**. Envelope `WebhookPayload{eventType, instanceName,
  applicationUrl}`. Sonarr `WebhookEventType`: Test, Grab, Download, Rename, SeriesAdd,
  SeriesDelete, EpisodeFileDelete, Health, ApplicationUpdate, HealthRestored,
  ManualInteractionRequired. Radarr: Test, Grab, Download, Rename, MovieDelete,
  MovieFileDelete, Health, ApplicationUpdate, MovieAdded, HealthRestored,
  ManualInteractionRequired.
  - **There is no "Upgrade" eventType**: an upgrade is `eventType:"Download"` with
    `isUpgrade:true` (and `deletedFiles[]`).
  - Sonarr Download: `series`, `episodes[]`, `episodeFile{id, relativePath, path, size,
    sourcePath, ...}`, `isUpgrade`, `downloadClient`, `downloadId`, `deletedFiles[]`.
    `episodeFile.path` = series path + relativePath (`WebhookEpisodeFile.cs`).
  - Sonarr **also** emits `eventType:"Download"` for *import complete* with a different body:
    `episodeFiles[]` (plural), `fileCount`, `sourcePath`, `destinationPath`
    (`WebhookBase.BuildOnImportCompletePayload`, `WebhookImportCompletePayload.cs`). A parser must
    accept both `episodeFile` and `episodeFiles[]`.
  - Radarr Download: `movie`, `remoteMovie`, `movieFile{... path ...}`, `isUpgrade`,
    `deletedFiles[]`.
  - Rename: Sonarr `renamedEpisodeFiles[]{path, previousPath, relativePath,
    previousRelativePath}`; Radarr `renamedMovieFiles[]` same fields. holdfast should treat a
    rename as: new path = scan candidate; previous path = drop any queued job for it.
  - Test: `eventType:"Test"` - answer 200, do nothing.
  - Fail-safe: unknown eventType or missing path -> 202/200 with a logged skip, never guess.
    Webhook auth: the *arr webhook supports username/password (basic) and, per T23-style
    by-reference, holdfast would need a webhook credential; docs/docker.md notes the current
    bearer-only endpoint cannot take basic auth - a native intake needs its own auth decision
    (basic-auth by reference, or a secret path segment).

---

## 3. SPA toolchain - versions and a real probe

### Versions (npm registry `npm view`, 2026-09-29)
| package | latest | licence | pinned in probe |
|---|---|---|---|
| svelte | 5.57.1 | MIT | 5.57.1 |
| vite | 8.3.1 (2026-09-24; engines node ^20.19 or >=22.12) | MIT | 8.3.1 |
| @sveltejs/vite-plugin-svelte | 7.3.1 (peer vite ^8, svelte ^5.46.4) | MIT | 7.3.1 |
| svelte-check | 4.7.6 (**peer typescript ^5 or ^6**) | MIT | 4.7.6 |
| vitest | 5.0.2 | MIT | 5.0.2 |
| eslint / @eslint/js | 10.11.0 / 10.0.1 | MIT | same |
| eslint-plugin-svelte | 3.23.0 | MIT | 3.23.0 |
| typescript-eslint | 8.71.0 (**peer typescript <6.1**) | MIT | **8.70.0** (see min-age) |
| typescript | **7.0.2 is `latest`** | Apache-2.0 | **6.0.3** |
| jsdom / @testing-library/svelte / globals / @types/node | 30.1.1 / 5.4.2 / 17.12.0 / 24.19.0 | MIT | same |
| pnpm | 12.6.0 (`latest`), 10.34.6 (`latest-10`) | MIT | 12.6.0 |
| Node | **24.21.0 "Krypton" = current LTS**; 22.23.3 "Jod" maintenance; 26.10.0 Current | - | 24.21.0 |

- **TypeScript 7 cannot be used yet**: svelte-check and typescript-eslint peer ranges stop at 6.x.
  Pin `typescript@6.0.3`.
- **Node schedule** (https://raw.githubusercontent.com/nodejs/Release/main/schedule.json,
  2026-09-29): v24 LTS 2025-10-28, maintenance **2026-10-20**, EOL 2028-04-30; v26 becomes LTS
  **2026-10-28**, EOL 2029-04-30. Recommend pin 24.21.0 now, plan a bump to 26 after 2026-10-28.
- Svelte 5.57 is the September 2026 release - https://svelte.dev/blog/whats-new-in-svelte-september-2026 (2026-09-29).

### Probe (/scratch/claude-spa-probe) - what was run
Node 24.21.0 via `mise.toml`, pnpm 12.6.0, Svelte 5 runes component (`$props/$state/$derived`),
vitest + jsdom + @testing-library/svelte test, svelte-check `--fail-on-warnings`, flat eslint
config with typescript-eslint + eslint-plugin-svelte.

| step | result | wall time |
|---|---|---|
| `pnpm install` (cold, 219 pkgs downloaded) | ok | 11.2 s |
| `pnpm install` (warm store, fresh lock) | ok | 6.3 s |
| `pnpm install --frozen-lockfile` (warm) | ok | 0.7-1.3 s |
| `vite build` | ok, 26.2 kB JS (10.6 kB gzip), `dist/` 40 KB | 0.4 s build, 1.8 s incl. startup (7.3 s first run) |
| `vitest run` | 1 file / 1 test passed | 6.5 s |
| `svelte-check` | 0 errors 0 warnings, 307 files | 8.2 s |
| `eslint .` | clean | 3.6-4.3 s |

- node_modules **137 MB**, **219 packages** (221 entries in `.pnpm`), lockfile v9.0.
- **No lifecycle scripts needed.** Scanned every `package.json` in the store: zero packages
  declare `preinstall`/`install`/`postinstall`. Vite 8 uses **rolldown** (`@rolldown/binding-
  linux-x64-gnu`) and `lightningcss-linux-x64-gnu` as platform optional deps - no esbuild at all,
  no postinstall. So a repo `.npmrc` with `ignore-scripts=false` is **not** needed.
- Transitive licences (`pnpm licenses list`): MIT 166, Apache-2.0 17, ISC 8, BSD-2-Clause 8,
  BSD-3-Clause 3, MIT-0 2, MPL-2.0 2 (lightningcss, build-time only), BlueOak-1.0.0 2, CC0-1.0 1.
  Only the Svelte runtime (MIT) ships inside `dist/`; all AGPL-compatible. A shipped-bundle
  licence notice (MIT notice retention) should be generated or asserted (ASSUMED requirement).
- Gotchas found: `test:` in `vite.config.ts` must use `defineConfig` from `vitest/config` or
  svelte-check reds; Svelte 5.57 warns `state_referenced_locally` on `$state(prop)` - fatal under
  `--fail-on-warnings`.

### Finding: pnpm >= 11 ignores `.npmrc` for `ignore-scripts` (important for check-pins section 8)
- pnpm 11 release notes: "pnpm no longer reads non-auth settings from `.npmrc`"; "pnpm no longer
  reads `npm_config_*` environment variables. Use `pnpm_config_*` instead"; `minimumReleaseAge`
  defaults to 1440 (1 day); dependency builds blocked by default via `allowBuilds`;
  `strictDepBuilds` defaults to true - https://pnpm.io/blog/releases/11.0 (2026-09-29).
- **Verified on pnpm 12.6.0** with a root `postinstall` probe (fresh node_modules each time):
  `.npmrc ignore-scripts=true` -> **script RAN**; `.npmrc false` -> ran; nothing -> ran;
  `npm_config_ignore_scripts=true` -> ran; `pnpm-workspace.yaml` `ignoreScripts: true` -> did
  NOT run; `pnpm_config_ignore_scripts=true` -> did NOT run; `--ignore-scripts` -> did NOT run.
  The container's `~/.npmrc ignore-scripts=true` protected `npm install` (no postinstall ran) but
  **not pnpm 12** (ran). Dependency build scripts are still blocked by pnpm's own default.
- Consequence: `scripts/check-pins.sh` section 8 accepts a committed `.npmrc` with
  `ignore-scripts=true` as "the decision" - under pnpm 11+ that file is **inert**, so the tripwire
  would go green on a decision pnpm never reads. Section 8 should (a) read the pinned
  `packageManager` and, for pnpm >= 11, require a committed `pnpm-workspace.yaml` with
  `ignoreScripts: true` (and no `allowBuilds` entries, or each with a reason), (b) keep the
  `.npmrc` rule for npm, (c) be mutation-tested like section 4. Also note its `find` walks the
  working tree and prunes `node_modules` - correct for a `web/` dir.
- `minimumReleaseAge` bit the probe: `--frozen-lockfile` failed `ERR_PNPM_MINIMUM_RELEASE_AGE_
  VIOLATION` for typescript-eslint 8.71.0 (published 2026-09-28T17:12Z). Good default (keep it);
  pins must be >= 1 day old, and the Renovate/Dependabot cadence must respect it.

### Pinning recommendation
- `web/package.json`: exact versions only (no `^`), `"packageManager": "pnpm@12.6.0+sha512.<hash>"`
  (corepack verifies the hash; ASSUMED format), `"engines": {"node": ">=24 <25"}`.
- `web/pnpm-lock.yaml` committed; CI and Docker use `pnpm install --frozen-lockfile`.
- `web/pnpm-workspace.yaml`: `ignoreScripts: true` (the real decision surface for pnpm 11+);
  leave `minimumReleaseAge` at default.
- Node version: one home. `mise.toml` (`node = "24.21.0"`) is what the dev container honours;
  the Dockerfile `NODE_IMAGE` ARG must agree - a new check-pins assertion (same pattern as
  section 3's Go version agreement), since mise.toml and the Dockerfile tag both restate it.
- Keep `web/.npmrc` `ignore-scripts=true` anyway (covers anyone running npm, and satisfies the
  current section 8 until it is extended).

### Docker build stage (sketch)
```dockerfile
ARG NODE_IMAGE=node:24.21.0-bookworm-slim@sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS ui
WORKDIR /src/web
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 pnpm_config_ignore_scripts=true
RUN corepack enable            # Node 24 still bundles corepack (ASSUMED: removed from Node 25+)
COPY web/package.json web/pnpm-lock.yaml web/pnpm-workspace.yaml web/.npmrc ./
RUN pnpm install --frozen-lockfile
COPY web/ ./
RUN pnpm run check && pnpm run build     # outDir -> /src/web/dist

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
...
COPY --from=ui /src/web/dist ./internal/ui/dist
RUN CGO_ENABLED=0 go build ...           # embeds the real UI
FROM ${RUNTIME_IMAGE}                    # distroless unchanged: only the Go binary gains bytes
```
Digests resolved today from registry-1.docker.io (index digest, multi-arch):
`node:24.21.0-bookworm-slim` sha256:0e0ff40c39bc087845bfb27465a0df4ea419520094bc35842ff83dd8cbe6f9b6;
`node:24.21.0-trixie-slim` sha256:8ec5d7557396cfe32d21c3f9c13072355ceab22b584578ca4bb28af31120cffe.
Bookworm matches the repo's existing `golang:...-bookworm` / `debian:bookworm-slim` bases and
satisfies check-pins section 7 (tag AND digest). UI output is arch-independent, so building on
`$BUILDPLATFORM` avoids QEMU; the lockfile carries every platform's optional native binding and
pnpm installs only the builder's (ASSUMED, standard pnpm behaviour).

### go:embed without a committed dist (verified with Go 1.25.14 in /scratch/claude-spa-probe/goembed)
- `//go:embed all:dist` on an **empty** `dist/` -> compile error "cannot embed directory dist:
  contains no embeddable files" (so `go vet`/`go build` break).
- `//go:embed dist` (without `all:`) with only `dist/.gitkeep` -> same error (dotfiles excluded).
- `//go:embed all:dist` with a committed `dist/.gitkeep` -> builds and vets; `fs.Stat(sub,
  "index.html")` reports not-built at runtime. After copying a real build in -> built.
- Recommended pattern: `internal/ui/ui.go` with `//go:embed all:dist`, committed
  `internal/ui/dist/.gitkeep`, `.gitignore` `internal/ui/dist/*` + `!internal/ui/dist/.gitkeep`;
  `ui.Built()` false -> server serves a plain "UI not built" at the SPA route and **keeps `/`'s
  source offer reachable** (AGPL). Caveat: Vite's `emptyOutDir` deletes `.gitkeep` if Vite writes
  straight into `internal/ui/dist` - build to `web/dist` and copy (`make ui`), or set
  `emptyOutDir:false` with a clean step that spares `.gitkeep`.
- Alternative: build tag (`ui_embed.go //go:build embedui` + stub). Cost: default `go build`
  ships no UI, release/Docker must remember `-tags embedui`, and `make check` must vet/test both
  tag sets. The `.gitkeep` pattern is simpler and keeps one code path.
- `make check` additions: `ui-install` (frozen), `ui-lint`, `ui-typecheck` (svelte-check
  `--fail-on-warnings`), `ui-test` (vitest run), `ui-build`, then Go `build` embeds it. Adds
  roughly 20-30 s warm on this box (sum of measured steps). Needs Node in CI via the same
  mise.toml pin.
