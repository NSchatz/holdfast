# holdfast survey (pre-interview, 2026-09-29)

Read-only survey of `/workspace` (github `NSchatz/holdfast`, PUBLIC, AGPL-3.0) at `main` = `3c229da`
(S0158, PR #93). Nothing was changed. The gate baseline log is at
`/cache/tmp/plan-2026-09-holdfast/check-baseline.log`.

## 1. Purpose and layout

holdfast is a config-as-code, self-hosted media transcoder in Go, written as a Tdarr replacement. It
re-encodes non-HEVC/non-AV1 video to HEVC (libx265) or AV1, and it swaps a source only after the
output passes every gate: codec, duration and packet parity, strictly smaller, stream-count parity,
full decode, and VMAF (both the average and the worst frame).

It began as a Bash transcoder in the private `homelab` repo (`homelab: media/transcoder/`, see
`docs/migration.md:19`). Git holds 99 commits, all by the owner, from v0.1.0 on 2026-07-18 to
S0158 on 2026-09-24.

### Size

Measured by `scripts/test-mass.sh` at HEAD, counting non-comment Go lines:

| Measure | Value |
|---|---|
| Production lines | 23,474 |
| Test lines | 56,225 |
| Test-to-production ratio | 2.40 |
| `.go` files | 336 |
| Markdown in README, CLAUDE.md and `docs/` | about 4,785 lines |

### Raw lines per package (production / test)

| Package | Prod | Test | Notes |
|---|---|---|---|
| cmd/holdfast | 4183 | 10949 | CLI: run, serve, analyze, plan, resolve, restore, requeue, export, validate, version |
| internal/engine | 11409 | 30817 | orchestrator; 91 files; real libx265 encodes in tests |
| internal/store | 5110 | 8672 | SQLite ledger (modernc.org/sqlite, pure Go) |
| internal/config | 4766 | 4635 | koanf layered config, rules, profiles |
| internal/server | 3343 | 7114 | chi JSON API + SSE |
| internal/startup | 2732 | 4068 | not in CLAUDE.md layout |
| internal/probe | 1436 | 935 | |
| internal/docscheck | 1301 | 994 | not in layout; `docs/test-mass.md:65` says it was retired |
| internal/vmaf | 686 | 2002 | |
| internal/schedule | 410 | 459 | run_window, max_load, Tautulli |
| internal/secret | 390 | 516 | |
| internal/secretscan | 391 | 285 | |
| internal/metrics | 376 | 1258 | |
| internal/cpuquota | 354 | 371 | not in layout (S0160/S0161) |
| internal/hdr | 349 | 261 | |
| internal/fsclass | 294 | 190 | |
| internal/encoder | 231 | 228 | |
| internal/notify | 228 | 473 | |
| internal/deinterlace | 169 | 0 | not in layout, no tests |
| `internal/downscale` | 160 | 0 | not in layout, no tests |
| internal/heapmeasure | 132 | 161 | not in layout |
| internal/memlimit | 119 | 146 | not in layout (S0157) |
| internal/diskfree | 106 | 131 | not in layout (S0158) |
| internal/sourceoffer | 93 | 346 | not in layout (AGPL s13 offer) |
| internal/corpus | 89 | 96 | |
| internal/logging | 39 | 38 | |
| internal/version | 20 | 0 | |
| scripts/* (Go tools) | about 3,200 | 381 | api-schema, compose-image-ref, govulncheck-gate, mutation-gate, mutation-shape, mutation, secret-scan |

The `scripts/` directory also holds shell tools: check-pins(-selftest), install-ffmpeg(-selftest),
check-pin-live, govulncheck(-selftest), secret-scan(-selftest), api-schema-diff-selftest,
happy-path-log-selftest, mutation(-selftest), smoke-image, release-promote, release-resmoke,
resolve-compose-image and test-mass. Four leftover `regress_0057_*.js` files sit there too (see
section 9).

## 2. Languages and toolchain

| Item | Value | Where |
|---|---|---|
| Go | `go 1.25.0` in go.mod; builds and CI use 1.25.x | `check-pins.sh` section 3 enforces one Go version across Dockerfile, ci and release |
| Local Go | `mise exec go@1.25.14` | no global go |
| staticcheck | `2025.1.1` | Makefile |
| govulncheck | `v1.1.4` | Makefile |
| gremlins (mutation) | `v0.6.0` | Makefile |

The ffmpeg pin is a BtbN autobuild:

| Field | Value |
|---|---|
| `FFMPEG_BUILD` | `autobuild-2026-07-31-14-10` |
| `FFMPEG_VERSION` | `N-125875-g5d4d3bdc61` |
| SHA-256 (amd64) | `16161335...` |
| SHA-256 (arm64) | `a38f9976...` |

- The pin lives in `Dockerfile:55-58`. `scripts/install-ffmpeg.sh` parses it from the Dockerfile rather than restating it.
- The ffmpeg on PATH (`/cache/mise/shims/ffmpeg`) reports `N-125875-g5d4d3bdc61-20260731`, which **matches the pin**.
- `check-pins.sh` requires a month-end build.
- `pin-health.yml` checks every Monday that the build is still served upstream.

Other languages: shell, plus 4 stray Node `.js` probes. There is no frontend: it was removed in
`4ad6f03`.

## 3. Build, test and lint

`make check` is the gate (`Makefile:277`). CI and the release workflow both run exactly this target. Its
sub-targets, in order:

1. `check-pins` (`scripts/check-pins.sh`, 628 lines):
   - 0: every file it reads exists
   - 1: NOTICE names the ffmpeg pin
   - 2: the pin is a month-end build
   - 3: one Go version
   - 4: the rename guard (`TRANSCODE_`, `transcode_`, old module and ghcr paths; `rename-guard-allow` marker)
   - 5: Actions pinned by SHA
   - 6: compose image pinned by digest
   - 7: base images pinned by tag and digest
   - 8: `package.json` needs an `.npmrc` decision
2. `check-pins-selftest`
3. `install-ffmpeg-selftest`, which drives each installer failure mode offline
4. `secret-scan`
5. `secret-scan-selftest` (33 cases)
6. `api-schema-diff`, which compares the generated HTTP surface with `docs/api-schema.json` (the v0.3.0 baseline); `.api-schema-breaks.yaml` is `[]`
7. `mutation-shape`, which checks that `.gremlins.yaml`, `docs/mutation-testing.md` and `mutation.yml` agree
8. `fmt` (gofmt -l)
9. `vet`
10. `build` (CGO_ENABLED=0)
11. `test`, which runs `go test -race -covermode=atomic -timeout 30m ./...`
12. `staticcheck`
13. `govulncheck`, stricter than upstream: every advisory must be fixed or suppressed, and `.govulncheck-suppressions.yaml` is `[]`
14. `govulncheck-selftest`

These targets are deliberately outside `check`:

| Target | Where it runs |
|---|---|
| `check-pin-live` | pin-health schedule |
| `snapshot-bench` | by hand |
| `check-enumeration-memory` | by hand |
| `api-schema-baseline` | release step |
| `api-schema-diff-selftest` | not wired into CI as far as I can see |
| `happy-path-log-selftest` | CI step |
| `mutation-diff`, `mutation-full`, `mutation-selftest` | CI and schedule |
| `image`, `image-smoke`, `compose-check` | by hand; needs Docker |
| `install-hooks` | by hand |
| `tidy`, `clean` | by hand |

CI adds two things beyond `check`: the config-schema self-test (`validate` must fail on each of the 4
`testdata/` bad configs, `ci.yml:100-101`) and the image smoke test (`scripts/smoke-image.sh`: a real
amd64 encode, plus an arm64 run with `--no-encode`).

### Baseline run

`timeout 1500 mise exec go@1.25.14 -- make check` was run with the full log at
`/cache/tmp/plan-2026-09-holdfast/check-baseline.log`. **Result: TIMED OUT (exit 124 after 1501 s),
not failed.**

Every step before `test` passed:

| Step | Result |
|---|---|
| check-pins | pass |
| check-pins-selftest | pass |
| install-ffmpeg-selftest | pass |
| secret-scan | clean |
| secret-scan-selftest | 33/33 cases bite |
| api-schema-diff | pass |
| mutation-shape | "configuration, document and workflow agree" |
| fmt | pass |
| vet | pass |
| build | pass; version stamp `v0.3.0-8-g3c229da` |

`go test -race` then ran until `timeout` killed make (`make: *** [Makefile:85: test] Terminated`).
Go prints package results in alphabetical order once a package finishes, and every reported package
passed:

| Package | Time | Coverage |
|---|---|---|
| cmd/holdfast | 351.7 s | 88.1% |
| internal/config | 2.1 s | 81.6% |
| internal/corpus | 1.0 s | 61.8% |
| internal/cpuquota | 1.1 s | 93.1% |
| internal/deinterlace | no tests | 0.0% |
| internal/diskfree | 1.0 s | 50.0% |
| internal/docscheck | 6.4 s | 88.8% |
| `internal/downscale` | no tests | 0.0% |
| internal/encoder | 1.9 s | 96.2% |

Nothing from `internal/engine` onward was reported. The engine suite was still running, and that
blocked output for every package after it alphabetically. There were no FAIL or panic lines.

**Why it timed out: this container has a cgroup CPU quota of 2 CPUs** (`cpu.max` = `200000 100000`),
even though `nproc` reports 56. The Makefile comment measures the engine suite at 524-568 s alone,
under `-race`, on a 56-core machine. With a 2-CPU quota it cannot finish in the 1500 s budget, after
roughly 5 minutes of selftests and build.

- This is **environmental, not a real failure**: the pinned ffmpeg is present and matches the pin.
- CI runs the same gate on ubuntu-latest in about 8-12 minutes, and it is green on main.
- For a local full-green proof in this container, expect well over 30 minutes: `TEST_TIMEOUT=30m` per package may itself trip for internal/engine.

`staticcheck`, `govulncheck` and `govulncheck-selftest` did not run, because they come after `test`.
The tracked tree is unchanged. The only artifact is the gitignored `./holdfast` binary.

## 4. CI (.github/workflows)

| Workflow | Triggers | Jobs | Recent duration |
|---|---|---|---|
| `ci.yml` | push to main, pull_request | `build` (`make check`, `happy-path-log-selftest`, `mutation-selftest`, config self-test), `package` (amd64 smoke with real encode, arm64 smoke with `--no-encode`) | push about 8.5-12 min; PR about 10-13 min |
| `mutation.yml` | pull_request (diff-scoped), Saturdays 03:23 UTC (full), workflow_dispatch | `mutation` | PR about 1 min; scheduled full run about 7 min |
| `pin-health.yml` | Mondays 06:17 UTC, dispatch | `ffmpeg-pin` (`make check-pin-live`) | about 9 s |
| `release.yml` | tag `v*`, dispatch | `build` (`make check` plus both smokes), `publish` (ghcr login, re-smoke, promote/retag, resolve compose image) | n/a |

- **Recent run results:** the last 25 runs are all `success` except two PR CI runs on the S0155 branch, which were `cancelled` because a newer push superseded them.
  - Latest scheduled mutation run: 36230570966 on 2026-09-26, success.
  - Latest pin-health run: 36432082252 on 2026-09-28, success.
- **Other PR checks:** a GitGuardian Security Checks app also runs on PRs.
- **Dependency updates:** there is no Dependabot or Renovate config. That is spec S0151, which is still pending.

The last full mutation run (36230570966) reported:

| Figure | Value |
|---|---|
| Score | 100.00% against a floor of 70% |
| Killed | 1233 |
| Lived | 0 |
| Not covered | 672 |
| Files mutated | 60 |

- Individual gremlins package runs with 0 killed and 0 lived printed "ERROR: below efficacy-threshold" in the log. The aggregate gate still passed, so the log contains noise that looks like a failure.
- The mutation domain excludes the six heaviest packages: `internal/engine`, `internal/vmaf`, `cmd/holdfast`, `internal/store`, `internal/server` and `internal/metrics`.
- Together those packages are about 25k of the roughly 36k raw production lines (rough share, from the table above). So the 100% score covers only the small leaf packages.

## 5. Docs and the rules they state

### Documents

- **Top level:** README.md (hero text, status, the "Why another transcoder" argument, non-goals, provenance), CLAUDE.md, NOTICE, LICENSE.
- **docs/:** api-reference, api-schema.json, comparison, docker, encode-memory, enumeration, filesystem, migration, mutation-testing, profiles, release, requeue, scan-cost, scratch, secrets, test-mass, undo.
- **docs/design/:**
  - `swap.md#swap-invariant`
  - `quality-gate.md#vmaf-pooling` (bound the worst frame)
  - `ledger-totals.md#null-is-not-zero`
- These three anchors come from S0102 documentation consolidation. `internal/docscheck` tests enforce doc agreement.

### Rules

- **The invariant:** never mutate a source before every gate passes. The swap is an atomic same-filesystem `rename()` from the source's own directory.
- **Fail safe:** skip with a logged reason, or return a typed error.
- **Identifiers:** keep `TRANSCODE-1..15`; underscore forms are banned.
- **Gate:**
  - `make check` needs the pinned ffmpeg and must not skip when it is absent.
  - Engine changes must extend the fixture suite.
- **Secrets:**
  - Pass by reference only (`file:` or `cmd:`), for `server_auth_token`, `server_read_token`, `notify_url` and `tautulli_api_key`.
  - `config.SecretBearingKeys` is the closed list.
  - A resolved secret is a `secret.Value` that renders as `<redacted>`.
  - `Expose()` is the only way to read the plaintext.
- **Commits:**
  - Author <the owner's git identity>.
  - **No Co-Authored-By and no AI trailer.** This conflicts with the harness attribution reminder; the repo rule should win.
  - Conventional Commits. In practice the history uses `S0NNN-holdfast-<slug>: <summary> (#PR)` squash titles instead.
- **Style:**
  - Plain hyphens only.
  - No dates or narrated history in CLAUDE.md.
- **Mutation:** a 70% efficacy floor, KILLED/(KILLED+LIVED), stated in both `.gremlins.yaml` and `docs/mutation-testing.md`.
- **Operator decisions recorded in super:**
  - `requeue` and `restore` stay local and never become HTTP endpoints.
  - Library management is a gated non-goal (S0105).
  - Multiple nodes are a non-goal (`docs/migration.md:120-126`).
  - Audio re-encoding is a non-goal.

### The roadmap reference

`documentation/roadmaps/holdfast.md` **does not exist anywhere reachable**.

- The umbrella is the private repo `NSchatz/super`, where holdfast is a submodule.
- `super/documentation/` has no `roadmaps/` directory; the contents API returns 404.
- super does have `.sdd/bin/roadmap_*.py` tooling and `.sdd/schemas/roadmap-phase.json`, but no holdfast roadmap file.
- So CLAUDE.md (last line) and README ("the roadmap names each phase") point at a document that does not exist.

The real planning state lives in `super/pipeline/`:

- **archive:** 631 archived holdfast items under `pipeline/archive/2026/` (count of tree paths; the number of spec folders is lower).
- **backlog:** `S0181-homelab-holdfast-env-example-stale.md`.
- **active:** 20 spec folders, listed in section 6.

Other super documents that bear on holdfast:

- `documentation/operator-decision-holdfast-backlog-review.md` (2026-09-11: requeue stays on the CLI; rules split from `max_height`; scope a verified deinterlace)
- `documentation/conductor-ruling-holdfast-backlog-coherence.md`
- `documentation/conductor-ruling-test-mass-is-on-target.md`
- `documentation/retired-conventions/{frontend,interface-craft,styling}.md`, which record the frontend removal
- `documentation/known-defects.md`
- `.sdd/state/cards/holdfast.md`, the generated interface card (pin 3c229da)

## 6. Open work

(TODO/FIXME: rg finds zero, apart from two `mktemp XXXXXX` templates.)

- **TODO/FIXME/XXX/HACK:** zero in tracked files.
- **GitHub issues:** none, open or closed (`gh issue list --state all` is empty).
- **PR #94** `S0159-holdfast-bounded-run-temp-sweep`: OPEN since 2026-09-24, not a draft, MERGEABLE.
  - Checks: build, mutation, package and GitGuardian are all SUCCESS (CI run 35992269762, about 13 min).
  - Spec: `super/pipeline/active/S0159-holdfast-bounded-run-temp-sweep/spec.md`.
  - It adds owner records under `<state_dir>/temp-owners/` with a lock held open for the temp's lifetime. A bounded run (`--limit` or `--file`) then sweeps temps whose owner is provably dead.
  - It is waiting only on merge.

### Active specs in super (all `status: approved`, `impl_verdict: pending`)

| ID | Pri | Gist |
|---|---|---|
| S0151 dependency-update-bot | p3 | tell someone when any pin (actions, images, Go, tools, ffmpeg) goes stale |
| S0162 vmaf-log-off-tmpfs | p2 | the per-frame VMAF JSON log goes to the process temp dir (tmpfs in compose) and is read whole into memory |
| S0163 workers-scale-cpu-quota | p2 | size workers from the cgroup CPU quota; safe concurrency on one drive |
| S0164 savings-per-hour-queue | p2 | a queue order by expected savings rate instead of size |
| S0165 per-rule-encoder-selection | p3 | a resolution rule can pick its own encoder (NVENC on the host's NVIDIA workstation card) |
| S0166 restart-survey-overcount | p2 | the ledger survey overcounts on banded roots |
| S0167 scan-records-source-facts | p3 | skipped rows record source codec and dimensions |
| S0168 prune-excluded-directories | p3 | the startup walk skips `exclude_paths` dirs (for example `.Trash-0` warnings) |
| S0169 summary-per-root-totals | p2 | `/api/summary` gains candidate bytes, projected savings, free space and `bytes_held_by_undo_window` (which api-reference.md already claims) |
| S0170 history-filter-paging | p3 | `/api/history` status filter plus cursor (today it returns the newest 200 only) |
| S0171 queue-row-decision-facts | p3 | in-flight queue rows show source facts |
| S0172 would-transcode-state-label | p2 | stale dry-run `would-transcode` bucket on a live engine |
| S0173 run-mode-progress | p2 | `holdfast run` reports encode progress |
| S0174 limit-encodes-and-queue-order | p2 | `--limit` counts encodes, not skips |
| S0175 server-warning-scope | p3 | fix the read-API exposure warning |
| S0176 log-timezone-offset | p3 | one clock across restore and serve logs |
| S0177 working-file-extensions | p2 | in-progress and undo files are playable media inside library folders |
| S0178 swap-mtime-choice | p3 | revisit the `preserve_mtime` default |
| S0179 post-swap-rescan-hook | p2 | optional Radarr/Sonarr/Plex rescan after a swap |
| S0180 census-scope-parity | p3 | `plan --json` as the library-scope tool |

S0159 is also in active (PR #94). Missing numbers S0152-S0154 were probably archived or retired.

### Remote branches

| Branch | State |
|---|---|
| `remove-comment-density` (9025a49) | MERGED, an ancestor of main (0 ahead, 26 behind). Safe to delete. |
| `remove-dashboard` (4ad6f03) | MERGED, an ancestor of main (0 ahead, 27 behind). Safe to delete. |
| `sdd/S0022-holdfast-ffmpeg-rot` (1528735) | PR #22 CLOSED unmerged; 1 ahead, 79 behind. Superseded by the later pin work: the month-end pin rule, pin-health.yml and install-ffmpeg exit codes. Stale. |
| `sdd/S0035-holdfast-dashboard-ui` (3cd2adf) | PR #26 CLOSED; 16 ahead, 76 behind. Dashboard work; the frontend has since been removed entirely. Dead. |
| `sdd/S0126-holdfast-depth-state-matrix-graders` (024e349) | PR #69 CLOSED; 4 ahead, 28 behind. Interface-craft graders; interface-craft conventions have since been retired. Dead. |
| `webui-e2e-playwright-graders` (6cdac0f) | no PR; 1 ahead, 62 behind; 68 files +5510/-2822 under `internal/webui/`. The webui is gone. Dead. |
| `sdd/S0159-holdfast-bounded-run-temp-sweep` | the live PR #94 |

- **Tags:** `v0.1.0` (2026-07-18), `v0.2.0` (2026-09-09), `v0.3.0` (2026-09-19; GitHub release 2026-09-19, marked Latest).
- **Stray tag:** `abandoned/S0098-pre-d911314` (c89a4c0).
- **Unreleased on main since v0.3.0:** S0160, S0161, S0155, S0156, S0157, S0158.

## 7. The S0NNN commit series

- **Scheme:** `S0NNN-<repo>-<slug>` is a spec ID from the umbrella's SDD (spec-driven development) pipeline in `NSchatz/super`. The numbers are global across repos (for example S0181-homelab-...).
- **How work flows:** each spec lives in `super/pipeline/{inbox,backlog,active,archive}/`. It passes a spec gate (`verdict-spec-N.md`, `gate-state.json`) and an impl gate, then lands as one squash-merged PR from branch `sdd/S0NNN-holdfast-<slug>`, titled with the spec ID. Regress probes ("failing probe for impl-gate finding Fnn") are committed on the branch.
- **Counts:** the highest ID merged is S0161; the highest in flight is S0180. There are 67 distinct S-IDs in holdfast's git history.

Themes of the last ~40 commits, 2026-09-13 to 2026-09-24:

- **Resource safety under containers:**
  - S0157 bounds mux queues and aborts an encode at 85% of the cgroup memory limit.
  - S0158 refuses a job beside the source when free space is insufficient.
  - S0160 and S0161 set libvmaf threads and the libx265 pool from the cgroup CPU quota.
  - S0159 (open) sweeps stale temps.
- **Input robustness:**
  - S0156 skips a source whose demuxer reports it damaged.
  - S0155 carries cover art (attached_pic) as mkv attachments.
  - S0107 adds deinterlace plus field-order classification.
  - S0106 defers the dynamic-HDR skip.
- **Operator control and scope:**
  - Scoping and filters: S0086 path filters; S0088 audio/subtitle stream selection; resolution rules (#74); S0118 optional `max_height`.
  - Queue and runs: S0095 queue ordering; S0100 bounded run (`--limit`, `--file`); S0099 streaming enumeration; S0098 per-file scan cost.
  - Commands and endpoints: S0122 requeue on profile change; S0092 library census; S0103 `holdfast plan`; S0093 `POST /api/scan`.
  - Behaviour: S0081 dry run records decisions; S0091 filesystem watch.
- **Observability and API:**
  - S0104 Prometheus metrics; S0096 SSE performance.
  - S0101 `server_read_token`.
  - S0128 self-describing `/api/schema` plus the break gate.
  - S0130 happy-path log grader.
- **Process and quality:**
  - S0133 mutation floor.
  - S0150 test-mass composition, which retired off-target tests.
  - S0102 doc consolidation; S0105 library-manager non-goal; S0108 competitive positioning.
  - Frontend removed (`4ad6f03`), then the comment-density measurement removed (`9025a49`).
- **Earlier, now dead:** dashboard and interface-craft work (S0035, S0069 DASH-9, S0094, S0123, S0125, S0126).
- **Releases:** v0.3.0 on 2026-09-19 (`b7c26ca`, `e561553`).

## 8. Feature state against a Tdarr replacement

### Encoders

The registry is in `internal/encoder/encoder.go`:

| Config key | ffmpeg encoder | Output |
|---|---|---|
| `cpu` (default) | libx265 | HEVC |
| `svtav1` | libsvtav1 | AV1 |
| `nvenc` | hevc_nvenc | HEVC |
| `av1_nvenc` | av1_nvenc | AV1 |
| `qsv` | hevc_qsv | HEVC |
| `vaapi` | hevc_vaapi | HEVC |
| `amf` | hevc_amf | HEVC |

- A raw ffmpeg encoder name is accepted as an alias.
- There is no H.264 output and no videotoolbox.
- At startup, `Available()` probe-encodes a tiny clip. A failing encoder stops the run; there is no CPU fallback (`cmd/holdfast/main.go` about 511-532).
- The CRF value maps to `-cq`, `-global_quality` or `-qp` for the hardware encoders.
- **Hardware support is real only for NVENC, via the NVIDIA Container Toolkit.** `docs/docker.md:338-415`, `Dockerfile:135-141` and `docker-compose.yml:109-134` all say QSV, VAAPI and AMF are NOT supported by the image (no vendor libraries).
- **Hardware paths are tested only as argv; nothing runs on a GPU in CI.**
- There is no `-hwaccel` anywhere, so decoding is always on the CPU.
- The target host has an NVIDIA workstation card (S0165).

### HTTP API

The router is chi (`internal/server/server.go` about 120-195). There is no UI.

| Access | Endpoints |
|---|---|
| Open | `GET /` (plain text plus AGPL offer), `GET /api/schema`, `GET /metrics` (when `metrics_enable`) |
| Read, gated by `server_read_token` if set | `GET /api/summary`, `/api/queue`, `/api/history`, `/api/events` (SSE) |
| Control, gated by `server_auth_token`; 403 when that token is unset | `POST /api/rescan`, `/api/scan` (`{"paths":[...]}`), `/api/pause`, `/api/resume`; `GET /api/search`; `GET\|POST\|DELETE /api/exclusions/` |

- Default bind is `127.0.0.1:8080`.
- Token comparison is constant-time.

### CLI

| Command | Flags |
|---|---|
| `run` | `--file`, `--limit` |
| `serve` | |
| `analyze` | `--json`, `--health` (full-decode health report) |
| `plan` | `--json` |
| `resolve` | `--id`, `--determination`, `--replacement` |
| `restore [path]` | |
| `requeue [path]` | `--guard`, `--failed` |
| `export` | `--out` |
| `validate` | |
| `version` | |

### Integrations

- **Tautulli** (`internal/schedule/tautulli.go`): holds new work while anyone is streaming. If Tautulli is unreachable, work continues.
- **Plex, Jellyfin, Sonarr and Radarr:** no client code. The *arrs call `POST /api/scan` through a custom script (`docs/docker.md:230-300`). S0179 plans a post-swap rescan hook.
- **shoutrrr notifications:** failed, indeterminate and applied-despite-error files, plus scan start and finish.
- **Prometheus metrics:**
  - `holdfast_files_total`, `holdfast_skips_total`, `holdfast_failures_total`
  - `holdfast_bytes_reclaimed_total`, `holdfast_bytes_held_by_undo_window`, `holdfast_queue_depth`
  - `holdfast_encode_duration_seconds`
  - `holdfast_vmaf_score`, `holdfast_vmaf_min`, `holdfast_vmaf_chroma`

### Scheduling and scanning

- **Scheduling:** `run_window` (can wrap past midnight), `max_load` per core from loadavg, the Tautulli hold, and `queue_order` (path, largest, smallest, newest, oldest). There is no priority queue.
- **Workers:** `workers` sets a pool on one host (default 1); S0163 would scale it from the CPU quota.
- **Multiple nodes:** an explicit non-goal.
- **Watch:** fsnotify per root (`watch: true`, `watch_settle_sec`), which speeds up discovery on top of the periodic scan.

### Other features

- **Profiles and rules:** ordered `encode_profiles` (first match wins); `rules` for resolution bands per root; `exclude_paths` and `include_paths`; `bitrate_kbps`.
- **Streams:** audio and subtitle language selection and `keep_commentary`, all stream-copied. Audio is never transcoded (non-goal); subtitles are never extracted to sidecars.
- **Remux and container:** `remux_only`; `container_ext: source` (the default).
- **HDR:** Dolby Vision and HDR10+ are skipped. HDR10 mastering metadata is kept only with the `cpu` encoder.
- **Picture processing:** deinterlace on request; optional `max_height` with a double opt-in.
- **Undo window:** `undo_window_hours`, default 0, restored with `restore`. `scratch_dir` and `scratch_min_free_gb` (default 50) control where the encode is written.
- **Recovery:** crash-safe SQLite ledger, `resolve` for indeterminate jobs, `requeue` for stale terminal rows.
- **Quality gate:** VMAF on by default with worst-frame pooling (`vmaf_min_pool`, `vmaf_min_chroma`).

### Absent compared with Tdarr and its peers

- Web UI
- Distributed nodes
- Plugins and flows
- Audio transcode or downmix
- Subtitle extraction
- H.264 output
- Hardware decode
- QSV, VAAPI or AMF in the shipped image
- Automatic hardware selection or CPU fallback
- Dolby Vision and HDR10+ transcoding
- Native Plex, Jellyfin or *arr clients
- Priority queue
- Crop and black-bar detection

`docs/comparison.md` compares mostly against Alchemist, FileFlows and Unmanic, and concedes that
Alchemist is ahead on several of these.

## 9. Rough edges

0. **The local gate cannot finish here**: `make check` timed out at 1501 s in `go test` because the container has a 2-CPU cgroup quota (nproc says 56). Everything before `test` passed, and so did every test package reported before internal/engine. CI (about 10 min) is the only place the full gate completes.
1. **Dangling roadmap:** `documentation/roadmaps/holdfast.md` (CLAUDE.md) and README's "the roadmap names each phase" point at a file that does not exist in `NSchatz/super`.
2. **README status is stale:** `README.md:10` says "`v0.1.0` released (2026-07-18)"; the latest release is v0.3.0 (2026-09-19). The status line is also dated.
3. **CLAUDE.md layout drift:**
   - Nine packages are unlisted: cpuquota, deinterlace, diskfree, docscheck, `downscale`, heapmeasure, memlimit, sourceoffer, startup.
   - The `cmd/holdfast` entry omits `analyze` and `plan`.
   - CLAUDE.md:51 says `serve` runs "the API and UI", which contradicts line 67 ("ships no frontend").
4. **TRANSCODE labels:** TRANSCODE-16 and 17 exist in code (`engine.go:2230-2398`, `durability_test.go:3`, `hardening_test.go:3`). CLAUDE.md and `check-pins.sh:245` both say 1..15. TRANSCODE-10 never appears.
5. **Web UI and dashboard leftovers:**
   - In the CLI help: `cmd/holdfast/main.go:59` ("HTTP API + web UI"), `:893` ("embedded web UI"), `:994` ("embedded dashboard").
   - Elsewhere: `cmd/holdfast/export.go:113`, `internal/server/controller.go:2`, `internal/engine/engine.go:195,1685,1803,2043`, `internal/store/aggregate.go:12`, `retention.go:365`, `sqlite.go:337`.
6. **test-mass drift:**
   - `scripts/test-mass.sh -check` fails. `docs/test-mass.md` records commit `e09a819...`, which is not in this repo (lost to a squash merge).
   - The figures block lacks cpuquota, docscheck, memlimit, deinterlace and `downscale`.
   - Lines 144-148 still describe the mutation runner as future work; line 96 mentions comment density.
   - `docs/test-mass.md:65` says `internal/docscheck` was retired, but its 12 files exist and run under `go test`.
7. **Dead scripts:** `scripts/regress_0057_{a15e,anchor,extra,probe}.js` (479 lines, from commit 4cfad15 / PR #34). They hard-code `/workspace/.worktrees/S0057-holdfast-pinning-1/...` and nothing references them.
8. **Missing scripts named in docs:**
   - `scripts/release-shape-gate`, `scripts/release-shape-selftest.sh`, `make release-shape`: named only in the test-mass register.
   - `homelab/scripts/test-transcoder.sh`: named in `internal/engine/engine_test.go:4`.
9. **Dashes:** 613 en/em dashes across 52 tracked files (including the Makefile at lines 1, 4, 7, 117 and 330, ci.yml, check-pins.sh, .gitignore, NOTICE, config.example.yaml, docs/docker.md, docs/migration.md, and the usage string at `main.go:52`). This breaks the "plain hyphens only, anywhere" rule, and nothing enforces it.
10. **Docs that contradict the code:**
    - `docs/comparison.md` says holdfast has no audio stream rules and one token; the code has language and commentary selection (S0088) and a read token (S0101).
    - `docs/migration.md` says "no remuxing-as-a-feature, no audio/subtitle mangling"; `remux_only` and track selection exist.
    - `docs/api-reference.md` claims `/api/summary` carries `bytes_held_by_undo_window`; it does not (S0169).
11. **Duplicates and overlap:**
    - The heading "We are not the only tool that verifies before it replaces" appears in both README and comparison.md.
    - scratch.md and design/swap.md overlap.
    - filesystem.md, scan-cost.md and enumeration.md overlap.
    - `docs/release.md` has a status table that goes stale with every release.
12. **What the mutation gate covers:** the 100% score excludes the six heaviest packages (engine, vmaf, cmd, store, server, metrics). The gremlins log prints "ERROR: below efficacy-threshold" for packages with nothing covered even when the run succeeds.
13. **Untested new packages:** `internal/deinterlace` and `internal/downscale` have no package-level tests (they may be tested through engine).
14. **Stale branches:** 6 dead or merged remote branches, plus the tag `abandoned/S0098-pre-d911314`.
15. **Container hooks:** in this container, `core.hooksPath` is set globally to `/home/claude/.claude-hooks` (from `~/.gitconfig`). So the repo's `.githooks/pre-commit` secret scan is not active here unless `make install-hooks` is run. That sets a local override, which is a config change.
16. **Commit convention mismatch:** CLAUDE.md says Conventional Commits; the history uses S-ID squash titles. CLAUDE.md also forbids Co-Authored-By trailers.
17. **Unreleased work:** six specs have landed since v0.3.0, and the compose file still pins `ghcr.io/nschatz/holdfast:v0.3.0@sha256:7c588db6...`.

## 10. Dependencies

Direct dependencies in go.mod:

| Module | Version |
|---|---|
| bmatcuk/doublestar/v4 | v4.10.0 |
| fsnotify/fsnotify | v1.10.1 |
| go-chi/chi/v5 | v5.3.0 |
| go-viper/mapstructure/v2 | v2.5.0 |
| knadh/koanf/v2 | v2.3.5 |
| koanf parsers/yaml | v1.1.0 |
| koanf providers/confmap | v1.0.0 |
| koanf providers/env | v1.1.0 |
| koanf providers/file | v1.2.1 |
| nicholas-fedor/shoutrrr | v0.16.1 |
| prometheus/client_golang | v1.23.2 |
| go.yaml.in/yaml/v3 | v3.0.4 |
| modernc.org/sqlite | v1.53.0 (pure Go, CGO off) |

Notable indirect dependencies: `golang.org/x/net` v0.56.0, `x/sys` v0.46.0, `paho.golang` v0.23.0 (through shoutrrr), `gorilla/websocket` v1.5.3.

External tools: the BtbN ffmpeg build (which includes libvmaf, libx265 and libsvtav1), staticcheck, govulncheck and gremlins, fetched with `go run` or at pinned versions.

Dependencies on other repos:

- `NSchatz/super` is the umbrella. It holds specs, gates, the interface card, conventions (`.sdd/conventions/testing.md`) and operator decisions, and it has holdfast as a submodule.
- `NSchatz/homelab` (private) holds the Bash predecessor and the deployment (backlog item S0181 says homelab's env example for holdfast is stale).

## 11. Hardware, services and external systems

- **GPU:** NVIDIA through the Container Toolkit (the host has an NVIDIA workstation card, per S0165). QSV, VAAPI and AMF exist in code only.
- **cgroups:** reads the CPU quota and memory limit (cpuquota, memlimit) and free disk space (diskfree).
- **Media services:** Tautulli (optional). Plex, Radarr and Sonarr are reached only indirectly, through `POST /api/scan`.
- **Notifications:** shoutrrr targets (any URL scheme).
- **Metrics:** a Prometheus scrape endpoint.
- **Registry:** `ghcr.io/nschatz/holdfast`, published by `release.yml`, which logs in to ghcr, re-smokes, and then promotes/retags. Images are multi-arch (amd64 and arm64), distroless and non-root. Compose pins the image by digest.
- **Releases:** v0.1.0, v0.2.0 and v0.3.0 (Latest, 2026-09-19). `docs/release.md` is the runbook: a dry-run dispatch, then the tag, a pull check, and `make api-schema-baseline`.
- **Upstream download:** the BtbN GitHub releases, checked weekly by pin-health.
- **PR scanning:** GitGuardian on PRs.

## 12. Secrets and identity

- **Scanner:** `internal/secretscan`, `scripts/secret-scan` and `scripts/secret-scan.sh`. It looks only for high-signal vendor prefixes and forbidden filenames, with no entropy heuristic. Exit 3 means a credential was found; exit 4 means the scan could not run. The self-test has 33 cases. Both ride `make check`.
- **Pre-commit hook:** `.githooks/pre-commit` runs `secret-scan.sh --staged`. `make install-hooks` sets `core.hooksPath=.githooks`. That is not active in this container (see rough edge 15).
- **Runtime credentials:** `file:` or `cmd:` references only; a literal refuses to start. `config.SecretBearingKeys` is the closed list. `secret.Value` redacts itself, and `Expose()` is the single read site. `docs/secrets.md` is the reference.
- **Ignored files:** `.gitignore` ignores `config.yaml`, `*.local.yaml`, `control-token.txt`, `.env` and `*.db*`. `config.example.yaml` is synthetic.
- **Identity:** git user is <the owner's git identity> (global config), and all 99 commits carry it.
- **Release credentials:** the release workflow's permissions default to deny-shaped read; ghcr login is scoped to the publish job.

## 13. .claude directory

There is no `/workspace/.claude/` directory and no project settings. The only agent guidance is
`/workspace/CLAUDE.md`. The umbrella `super` has its own `.claude/`.
