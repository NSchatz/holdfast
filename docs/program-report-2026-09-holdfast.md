# holdfast program report (2026-09)

The 2026-09 program took holdfast from a single-process, software-first transcoder to one that
carries every area the owner chose in the planning interview: one declared encode plan, an output
fidelity gate, hardware encoders and decode, audio and subtitle work, dynamic HDR and crop, a
declared queue order and a health sweep, media-server clients, worker nodes and an embedded web UI.
It ran as 15 goals from 2026-09-29 to 2026-10-04. Goals 1 to 14 merged 59 pull requests
(`gh pr list --state merged`, the `holdfast-g<n>/` branches), and the `func Test` count went from
1200 to 2145 (ledger g1 baseline; `rg -c '^func Test' -g '*_test.go'` at goal 15's start). Every
new transformation is off until configured, so an existing configuration decides as it did before
(brief §1, I5). What no agent could do is still open: no hardware encoder has run on a real
device, no client has spoken to a live service, and no worker has run on a second host. The ten
open queue items that record this, and the one goal 15 adds, are listed under NEEDS-OWNER. The
program ends with the minor release `v0.4.0`, and "The closing record" below states the gate on
`main`'s head, the hardware-report status and gate integrity since goal 15's start.

Sources are cited inline: "ledger gN" is [`.claude/goals/2026-09-holdfast-g<N>.status.md`](https://github.com/NSchatz/holdfast/tree/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals), a row is
a row of one of its phase tables, and "D<n>" is an entry of its "Decisions taken". The brief is
[`.claude/goals/2026-09-holdfast.md`](https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/2026-09-holdfast.md).

## The release

`v0.4.0`, a minor release cut by `docs/release.md` on 2026-10-04, is the program's release: every
area above is in it. Each run below is on `c95dbb9`, the squash commit of PR #171.

| Step | Evidence |
|---|---|
| CI on the release commit | run 37166745058, `build` and `package` green, 01:01:36Z to 01:26:11Z (24m35s) |
| The dry run (`docs/release.md` step 2) | run 37166751433, `workflow_dispatch` on `main`, green in 25m51s; its plan line reads `publish=false  version=0.0.0-dev-c95dbb9`; the `publish` job is `skipped`; the artifact holds both tarballs and `SHA256SUMS`, each tarball `holdfast`, `LICENSE` and `NOTICE` |
| The tag (step 7) | annotated `v0.4.0`, tag object `c6ca619a`, on commit `c95dbb9`; higher than `v0.3.0`, the newest tag before it (`git ls-remote --tags origin`) |
| The release run | run 37168110898, tag push, green end to end, 01:28:21Z to 01:57:57Z (29m36s): `build` (the full gate, both images, both smokes) and every step of `publish` |
| The image (step 8) | `ghcr.io/nschatz/holdfast:v0.4.0` and `:latest` both answer `sha256:019f4a722222428f95d285d96c63c9e2d15882c7d0941681d6f19b8cc9288c8b` to `crane digest` (crane 0.22.1, no credentials, no Docker daemon); the index lists `linux/amd64` and `linux/arm64` |
| The GitHub release | tag `v0.4.0`, cut by the workflow: `holdfast_v0.4.0_linux_amd64.tar.gz`, `holdfast_v0.4.0_linux_arm64.tar.gz`, `SHA256SUMS` (`gh api repos/NSchatz/holdfast/releases/tags/v0.4.0`) |
| The compose pin and the baseline (step 9) | `docker-compose.yml` pins `v0.4.0` by that digest and `docs/api-schema.json` records 36 endpoints at `holdfast v0.4.0` (23 at `v0.3.0`), taken on the tag; both are in the pull request that first carried this report, PR #172 (`d3d1266`; ledger g15 row 5.4) |

Run URLs are `https://github.com/NSchatz/holdfast/actions/runs/<id>`. The gate on `main`'s head
at the program's end is under "The closing record" below.

## What was built

Each pull request below is merged; the SHA is its squash commit on `main` (the ledgers, checked
against `gh pr list --state all`). Pull requests #145 to #150, #114, #119 and #170 are the owner's
and the goals program's amendments to this program, not a goal's work, and are listed at the end.

### Goal 1 - start-up, triage and proposals

- The identity scan: the owner's name and email appear only in `LICENSE` and `NOTICE`, read from
  the repository's first commit at run time (`scripts/identity-scan.sh`, `make identity-scan` and
  its selftest in `check:`). PR #96 (`33c80fc`).
- The progress fake treats only a writable descriptor 3 as its channel, so the gate passes under a
  plain `flock`. PR #97 (`26d88b1`).
- The owner's reversals R1 to R6 recorded in the files whose rules they change, and the T34
  cleanup. PR #98 (`213258f`).
- Proposals P1 to P6 under [`.claude/goals/2026-09-holdfast-research/`](https://github.com/NSchatz/holdfast/tree/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/2026-09-holdfast-research), and the `docs/test-mass.md`
  record. PR #99 (`d4d70a6`).
- Six branches and one tag deleted on GitHub, exactly T35's list (ledger g1 rows 5.1 to 5.3).

### Goal 2 - carried specs and the Debian 13 base

- S0159, the owner-checked stale-temp sweep of a bounded run, carried from PR #94 by cherry-pick.
  PR #100 (`ca4968b`).
- Every image stage on Debian 13; the runtime base is distroless `cc-debian13`, pinned by tag and
  digest (`Dockerfile`, `docs/docker.md`). PR #101 (`cab897a`).
- S0166, the ledger survey reads a banded root's row at the band its stored height selects.
  PR #102 (`02aa553`).
- P6 (`internal/corpus` skips `.claude/`) and S0176 (log time fields state their UTC offset).
  PR #103 (`f5bb64f`).
- S0151, `.github/dependabot.yml` for the actions, the base images and Go modules.
  PR #104 (`901c472`).
- S0177, working files and retained originals end in non-video extensions. PR #105 (`a9ec91f`).
- `TEST_TIMEOUT` from 30m to 45m. PR #106 (`3996107`).
- S0173, `run` narrates each encode in flight on stderr. PR #107 (`550db38`).
- S0163, `workers: auto` from the CPU quota, and several jobs in flight made safe on one drive.
  PR #111 (`22f5185`).
- S0168 (the walk prunes excluded directories) and S0180 (`plan` publishes each root's scope).
  PR #112 (`fb9ef66`).

### Goal 3 - the encode plan

- Golden command lines for every registry encoder, merged before any refactor commit: 1611 cases
  under `internal/engine/testdata/golden-argv` (ledger g3 row 2.1). PR #113 (`b94cd2a`).
- `EncodePlan` (`internal/engine/encodeplan.go`): one declared plan per job, read by the command
  line builder and by every gate. Design record `docs/design/encode-plan.md#encode-plan`.
  PRs #115 (`eef7216`), #116 (`ba35133`), #117 (`518fe8e`), #118 (`c401660`).

### Goal 4 - the fidelity gate and per-encoder quality

- The output fidelity gate (`GateFidelity`, `hdr.Fidelity`, `probe.OutputFacts`): bit depth,
  chroma, colour tags and the HDR10 blocks equal what the plan declares. Design record
  `docs/design/encode-plan.md#fidelity`. PR #120 (`41c44bd`).
- S0162, the libvmaf log streamed through a pipe instead of a temp file (`internal/vmaf`).
  PR #121 (`169583b`).
- An explicit pixel format on every command line and the `quality.<key>` keys
  (`internal/encoder/quality.go`). PR #122 (`90b8ed8`).
- The declared primaries and transfer reach every encoder through `setparams`.
  PR #123 (`3dc9132`).
- Review corrections: PR #124 (`bb66d7e`), and PR #125 (`4e0488f`), which refuses a source whose
  side data went unread instead of declaring it absent.

### Goal 5 - hardware runtime, command lines and detection

- The start-time probe runs through a job's own command line at 8 and 10 bits; render nodes are
  found over DRM (`internal/hwdevice`); `amf` is refused in the image with its reason. Design
  record `docs/design/hardware.md` (`#probe`, `#detection`, `#amf`). PR #126 (`0fab196`).
- The VAAPI and QSV runtime in the amd64 image from 34 pinned Debian packages, each named in
  `NOTICE` (ledger g5 row 3.1). PR #127 (`84198b7`).
- S0165, a resolution rule may name its encoder. PR #128 (`075e3ba`).
- `encoder: auto` and per-root `hw_fallback: skip|software` (`docs/design/hardware.md#auto`,
  `#fallback`). PR #129 (`8e29c42`).
- Review corrections. PRs #130 (`ef6b611`), #131 (`37da90c`).

### Goal 6 - hardware decode, new encoders and hardware reports

- Eight encoders (`x264`, `h264_nvenc`, `h264_qsv`, `h264_vaapi`, `h264_amf`, `av1_qsv`,
  `av1_vaapi`, `av1_amf`) and the codec-family skip rule `better-codec-family`
  (`internal/encoder`). PR #132 (`0d8fce0`).
- `scripts/hw-report.sh` and `scripts/hwreport`: a redacted report under `testdata/hw-reports/`,
  with a test that no host identity survives (`docs/hardware-reports.md`). PR #133 (`a956f4c`).
- `hw_decode: software|hardware` per root (`docs/design/hardware.md#decode`). PR #134 (`8b52288`).

### Goal 7 - audio and subtitles

- Subtitle sidecars under `subtitle_sidecars: text` with a parse-back gate (`internal/subtitle`,
  `docs/design/subtitles.md#sidecars`, `#sidecar-gate`). PR #135 (`eb424d5`).
- Audio re-encode, stereo downmix and two-pass EBU R 128 loudness with their own gates
  (`internal/audio`, `docs/design/audio.md#reencode`, `#loudness`, `#audio-gates`).
  PR #136 (`a530a56`).
- Which files the audio and sidecar keys reach (`#which-files` in both records).
  PR #137 (`df90fbb`).

### Goal 8 - dynamic HDR and crop

- `dovi_tool` and `hdr10plus_tool` pinned per architecture by version and sha256
  (`scripts/install-dynhdr-tools.sh`, `NOTICE`). PR #138 (`3968508`).
- Dolby Vision profile 8.1, opt-in profile 7 (`dolby_vision_p7: convert`) and HDR10+ carried
  through libx265 behind the dynamic-HDR gates (`internal/dynhdr`,
  `docs/design/dynamic-hdr.md#dynamic-hdr`). PR #139 (`62f1c9c`).
- `crop: auto`: consensus-sampled crop with a blackness gate and a cropped VMAF reference
  (`internal/crop`, `docs/design/crop.md#crop`). PR #140 (`6fa5379`).
- A Dolby Vision source cropped only to its RPU's own active area, with L5 zeroed and gated (P5
  option (c)). PR #141 (`3bf286d`).

### Goal 9 - queue priority and the health sweep

- S0174, `run --limit-encodes N` and a one-run `--queue-order` override. PR #142 (`80501dc`).
- S0164, `queue_order: savings_per_hour` and a sequence-only `priority` (`internal/queuekey`,
  `internal/config/priority.go`, `docs/design/queue-order.md#queue-order`). PR #143 (`b363b29`).
- A scheduled, resumable, report-only health sweep with `GET /api/health` and four metrics
  (`internal/health`, `docs/design/health-sweep.md#health-sweep`). PR #144 (`8c6084c`).

### Goal 10 - media-server clients

- `TEST_TIMEOUT` from 45m to 60m. PR #151 (`b139fcb`).
- S0178, statement half only: `validate` and startup state the effective `preserve_mtime` choice;
  the default did not change. PR #152 (`2787ea6`).
- S0179 and T28: Radarr, Sonarr and Plex are told about a swap after it commits, and a file being
  played in Plex is held (`internal/mediaclient`, `docs/design/media-clients.md#media-clients`,
  `docs/post-swap-hook.md`). PR #153 (`138c0cc`).
- `scripts/client-report.sh` and `scripts/clientreport`: the redacted live check
  (`docs/client-reports.md`). PR #154 (`21ee6fc`).
- The authenticated Sonarr and Radarr webhook intake behind `webhook_token`
  (`internal/server`, `docs/design/media-clients.md#webhook-intake`). PR #155 (`c727f7a`).

### Goal 11 - worker nodes: protocol, shared mount, server re-gate

- The lease protocol's server side: durable lease rows (store schema v25), `internal/node`, the
  `/api/node/v1` group behind `node_token` (`docs/design/nodes.md#leases`). PR #157 (`7b434e1`).
- `holdfast worker` (`internal/nodeworker`, `cmd/holdfast/worker.go`), the engine's one node seam
  (`internal/engine/nodes.go`) and the server's own re-gate and rename. PR #158 (`81d7c1b`).

### Goal 12 - worker nodes: HTTP streaming, TLS and deployment

- `worker_mode: http`, the source digest checked both ways, built-in TLS (`server_tls_cert`,
  `server_tls_key`, `worker_tls_ca`) and the worker's transport refusals
  (`docs/design/nodes.md#transport`, `#http-mode`). PR #159 (`f48d4e0`).
- The worker deployment and the rewritten statements (`README.md`, `docs/migration.md`,
  `docs/docker.md#worker-nodes`). PR #160 (`e679d0a`).

### Goal 13 - web UI: toolchain, gate, embed and image

- `web/` (Svelte 5, Vite, TypeScript, vitest) on a pinned Node and pnpm; `scripts/ui.sh`; the UI
  steps in `check:`; `internal/ui` embedded and served at `/` to a request for HTML; a pinned Node
  build stage in the image (`docs/design/web-ui.md#embed`, `#root`). PR #161 (`0c3b25d`).
- S0175, the read-surface statements scoped to the command that can vouch for them.
  PR #165 (`ff3f4f5`).
- A race in a goal-12 test of `internal/node` removed. PR #166 (`2f1f4ed`).

### Goal 14 - web UI: views and controls

- S0167, S0169, S0170, S0171 and S0172, a job row's `priority`, and `GET /api/nodes`
  (`internal/server`, `internal/store`, `docs/api-reference.md`). PR #167 (`eb2dd01`), which also
  raised `TEST_TIMEOUT` from 60m to 90m (`Makefile`; ledger g14 D5).
- Request #229: a mutation run uses 4 workers and keeps its temp files and build cache on a tmpfs
  (`.gremlins.yaml`). PR #168 (`59a73f3`).
- The views (summary, queue, history, health, nodes) and the controls (pause, resume, scan,
  exclusions), with the control token held in a variable of the running page
  (`web/src/views/`, `web/src/lib/token.ts`, `docs/design/web-ui.md#views`, `#controls`, `#token`).
  PR #169 (`7bcfbcd`).

### Goal 15 - finale

- The documentation pass: `README.md` describes every shipped area with its default, read from
  `internal/config`; `docs/comparison.md` matches what ships; `CLAUDE.md`'s Layout is complete at
  198 lines; each of the 68 explicit anchors under `docs/design/` is linked from `README.md`,
  `CLAUDE.md` or a file under `docs/` (29 had no inbound link); and `docs/docker.md` says what
  the UI shows before a read token is typed (ledger g15 rows 3.1 to 3.5; goal 14's D15). The same
  pull request removes a race from a goal-14 test of `internal/node` that its first CI run met
  (ledger g15 D6). PR #171 (`c95dbb9`).
- The release, its record in `docs/release.md`, the compose pin, the HTTP surface baseline and
  the re-recorded `docs/test-mass.md`: see "The release" above.
- A pull request in the owner's private homelab repository that moves its deployment to the
  release and leaves every new key unset (ledger g15 rows 6.1 and 6.2, D8).
- This report.

### The triage rows (P1)

P1 has 21 rows: the 20 carried specs (S0151, S0162 to S0180) and PR #94 (ledger g1 row P1). Each
is merged above.

| Row | Status | Merged as |
|---|---|---|
| PR #94 (S0159) | DONE | PR #100 |
| S0151 | DONE | PR #104 |
| S0162 | DONE | PR #121 |
| S0163 | DONE | PR #111 |
| S0164 | DONE | PR #143 |
| S0165 | DONE | PR #128 |
| S0166 | DONE | PR #102 |
| S0167 | DONE | PR #167 |
| S0168 | DONE | PR #112 |
| S0169 | DONE | PR #167 |
| S0170 | DONE | PR #167 |
| S0171 | DONE | PR #167 |
| S0172 | DONE | PR #167 |
| S0173 | DONE | PR #107 |
| S0174 | DONE | PR #142 |
| S0175 | DONE | PR #165 |
| S0176 | DONE | PR #103; its second half is the homelab pull request the owner merged (queue #50, closed) |
| S0177 | DONE | PR #105 |
| S0178 | DONE | PR #152, the statement half only, as approved |
| S0179 | DONE | PR #153 |
| S0180 | DONE | PR #112 |

Two are narrower than their spec by the owner's approval
([`.claude/goals/CHECKPOINT-T.approved`](https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/CHECKPOINT-T.approved)): S0178 built only its
statement half, and S0176's second half is a pull request in the owner's private homelab
repository, which the owner merged on 2026-10-03; the owner closed its queue item, #50, on
2026-10-04.

### Pull requests that amended the program

| PR | Squash | What |
|---|---|---|
| #95 | `4ac983a` | the brief and the 15 goal files |
| #114 | `4c9143f` | the program runs from the maker container |
| #119 | `60e39d8` | the maker container's CPUs (`GOFLAGS=-p=4`, `GOMAXPROCS=12`) |
| #145 | `6257dbd` | amendment: the new goal supervisor |
| #146 | `2b1ab45` | amendment: spec v1.0 |
| #147 | `ce30994` | amendment: the owner's queue as issues |
| #148 | `991f06f` | `make tier-fast` and `make tier-full`, and their amendment |
| #149 | `6c83d7a` | amendment: shared packages and releases |
| #150 | `22ae917` | the program onto spec 1.1 |
| #170 | `1507a2b` | CI is the whole gate |

## Tests and runtimes

### `func Test` per goal

The ledgers counted with `git grep -c '^func Test' <sha> -- '*_test.go'` up to goal 9 and with
`rg -c '^func Test' -g '*_test.go'` from goal 10 on. The count includes one test behind the
`hwlive` build tag that never runs in the gate or CI (ledger g5 row 5.3).

| Goal | At start | At end | Source |
|---|---|---|---|
| 1 | 1200 | 1201 | ledger g1 baseline, row 8.2 |
| 2 | 1201 | 1322 | ledger g2 baseline, row 6.1 |
| 3 | 1322 | 1330 | ledger g3 baseline, row 5.1 |
| 4 | 1330 | 1388 | ledger g4 baseline, row 6.1 |
| 5 | 1388 | 1446 | ledger g5 baseline, row 5.1 |
| 6 | 1446 | 1463 | ledger g6 baseline, row 6.1 |
| 7 | 1463 | 1542 | ledger g7 baseline, row 6.1 |
| 8 | 1542 | 1634 | ledger g8 baseline, row 6.1 |
| 9 | 1634 | 1724 | ledger g9 baseline, row 6.1 |
| 10 | 1724 | 1834 | ledger g10 baseline, row 6.1 |
| 11 | 1834 | 1958 | ledger g11 baseline, row 5.1 |
| 12 | 1958 | 2004 | ledger g12 baseline, row 5.1 |
| 13 | 2004 | 2033 | ledger g13 baseline, row 5.1 |
| 14 | 2033 | 2145 | ledger g14 baseline, row 4.1 |
| 15 | 2145 | 2145 | ledger g15 baseline; "The closing record" below, counted at `a06bd51`: goal 15 added and removed no test function |

Every goal's gate-integrity row also records that no package's count fell and gives a reason for
each deleted `*_test.go` line: 8 lines in goal 1, 93 in goal 2, 0 in goal 3, 15 in goal 4, 70 in
goal 5, 41 in goal 6, 16 in goal 7, 17 in goal 8, 34 in goal 9, 1 in goal 10, 23 in goal 11, 15
in goal 12, 10 in goal 13 and 27 in goal 14 (the same rows as the table above). Goal 15's ledger
stopped before its gate-integrity row was filled; its count, 1 line, is under "The closing
record" below.

### The web UI's unit tests

vitest ran 21 of 21 when the toolchain merged (ledger g13 row 3.3) and 241 of 241 in 16 files
when the views merged (ledger g14 row 3.1). Goal 15 changed no file under `web/`.

### The local gate, goal by goal

Until 2026-10-03 the merge rule was a local `make check` and green CI (brief §1, T36). Each
ledger's baseline is one `make check` at the goal-start SHA. They are not one series: goals 1
and 2, and goal 3's baseline, ran in a container with 2 CPUs (`GOFLAGS=-p=2`); from goal 3's
re-run on they ran in a shared container with `GOFLAGS=-p=4` and `GOMAXPROCS=12` (ledger g3
header), and the host's load differs from run to run (ledger g11 and g14 baselines name it).

| Goal | `make check` at the goal-start SHA | `internal/engine` in that run | `TEST_TIMEOUT` then | Source |
|---|---|---|---|---|
| 1 | 1472 s | 1246.7 s | 30m | ledger g1 baseline |
| 2 | 1565 s | 1314.6 s | 30m | ledger g2 baseline |
| 3 | 1764 s | 1513.4 s | 45m | ledger g3 baseline |
| 4 | 1429 s | 1330.3 s | 45m | ledger g4 baseline |
| 5 | 1615 s | 1512.5 s | 45m | ledger g5 baseline |
| 6 | 1579 s | 1472.0 s | 45m | ledger g6 baseline |
| 7 | 2002 s | 1887.9 s | 45m | ledger g7 baseline |
| 8 | 1981 s | 1859.2 s | 45m | ledger g8 baseline |
| 9 | 2180 s under the lock (4560 s with the wait for it) | 2036.9 s | 45m | ledger g9 baseline |
| 10 | 2488 s | 2283.9 s | 45m | ledger g10 baseline |
| 11 | 2647 s, the wait for the lock included | 2251.7 s | 60m | ledger g11 baseline |
| 12 | 2476 s, the wait for the lock included | 2293.1 s | 60m | ledger g12 baseline |
| 13 | 2021 s | 1869.2 s | 60m | ledger g13 baseline |
| 14 | 3339 s, beside four builders' own tests | 2984.4 s | 60m | ledger g14 baseline |
| 15 | none: no gate runs on the development host | not recorded | 90m | ledger g15 baseline, D2 |

Other gate runs the ledgers record:

- The fresh-clone gate at goal 1's end: 1514 s, `internal/engine` 1280.2 s (ledger g1 row 7.1).
- The last local gate on the tree goal 14 started from: 2211 s, `internal/engine` 1983.0 s
  (ledger g13 row 4.0, repeated in ledger g14's baseline).
- The last local gates of the program, in goal 14: 2109 s for PR #168, 1932 s for PR #167 and
  2015 s for PR #169, with `internal/engine` at 1846.9 s, 1756.0 s and 1779.5 s (ledger g14 rows
  3b.1, 2.1 and 3.1).
- The slowest `internal/engine` run in a passing gate: 2701.8 s, in PR #155's gate under host
  load (ledger g10 row 4.1).

`TEST_TIMEOUT` was raised three times, each in the pull request the ledger names and each with
its measurement in the `Makefile`: 30m to 45m in PR #106 after `internal/engine` reached 1627.4 s
(ledger g2 D of 2026-09-29), 45m to 60m in PR #151 at 2283.9 s (ledger g10 D5), and 60m to 90m in
PR #167 at 2984.4 s under the builders' load (ledger g14 D5). No proof was deleted to fit the
clock (brief §1, I15).

### CI

Ledger-recorded CI times are few: the `package` job took 3m50s on PR #101 (ledger g2 row 3.3) and
the `build` job 23m22s on PR #161 (ledger g13 row 3.6). No ledger records a CI wall-clock for
goals 3 to 12.

The completed green runs of `ci.yml` on `main` in its last 20 runs
(`gh run list --workflow=ci.yml --branch main --limit 20`, read 2026-10-04; the time is the run's
creation to its last update, so it includes queueing):

| Run | Head | Time |
|---|---|---|
| 37139249199 | `0ade223` | 18m58s |
| 37140737417 | `4b42267` | 23m46s |
| 37144153547 | `303ac56` | 19m33s |
| 37148305830 | `937bb5d` | 23m10s |
| 37153021825 | `9ed8a05` | 22m20s |
| 37157998235 | `4af8282` | 19m18s |
| 37159924388 | `5a5d7ab` | 23m53s |
| 37162014945 | `d688105` | 24m27s |

Of the other twelve, nine were cancelled by the next push to `main` (a push cancels the run in
progress on the older head: ledger g5 row 5.2, ledger g15 D3), two failed (37133866011 on
`25e0c9c` and 37136887105 on `fe5c7bf`, the `internal/node` race that PR #166 removed: ledger g13
row 4.0 and D15), and one was in progress when the list was read.

`ci.yml` also runs nightly on `main` (`cron: '37 7 * * *'` in `.github/workflows/ci.yml`).

### The mutation floor

The floor is 70% efficacy, killed over killed plus lived (`.gremlins.yaml`
`unleash.threshold.efficacy`; `docs/mutation-testing.md`, "The floor"). `make mutation-shape`
holds the two files equal.

- In the domain: every Go file of the module outside the six excluded paths, among them
  `internal/config`, `internal/encoder`, `internal/probe`, `internal/hdr`, `internal/audio`,
  `internal/subtitle`, `internal/crop`, `internal/dynhdr`, `internal/queuekey`, `internal/health`,
  `internal/mediaclient`, `internal/node`, `internal/nodeworker`, `internal/hwdevice` and
  `internal/ui`.
- Out of the domain, each with its measured reason in `.gremlins.yaml`: `internal/engine`,
  `internal/vmaf`, `cmd/holdfast`, `internal/store`, `internal/server` and `internal/metrics`. An
  excluded path is unmeasured; what covers `internal/engine` is the fixture suite, the gates and
  the image smoke test (`docs/mutation-testing.md`). The program added and removed no exclusion
  (brief §1, T34; ledger g14 D7).
- A pull request runs the diff-scoped mutation; the whole domain runs on a schedule, Saturdays at
  03:23 UTC (`.github/workflows/mutation.yml`).
- The runner uses at most 4 workers since PR #168. The same diff-scoped run of 171 mutants went
  from 960 s to 111 s with its temp files and build cache on a tmpfs, the score unchanged at
  100.00% (ledger g14 row 3b.1).
- Every diff-scoped run a ledger records for a merged pull request scored 100% of covered mutants
  (ledgers g2 to g14, the rows that name "mutation-diff").

### CI is the whole gate

On 2026-10-03 the owner decided that holdfast's gate is pull-request CI alone
([`.claude/goals/AMENDMENT-2026-10-03-owner-2026-09-holdfast.md`](https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/AMENDMENT-2026-10-03-owner-2026-09-holdfast.md), merged as PR #170, `1507a2b`). A
pull request merges when `build`, `package` and `mutation` are green on the branch merged up to
`origin/main`. No gate, tier, whole-package or `-race` suite, mutation run or UI build runs on
the development host; one focused test is the inner loop. Every pull request of goals 1 to 14 had
merged under both the local gate and CI before the change (ledger g14 D16); goal 15 ran no local
gate, and its baseline is CI's last green run on `main` (ledger g15 baseline, D2).

## The closing record

Goal 15's ledger was stopped on 2026-10-04 with three rows unfilled: the gate on `main`'s head
(row 4.1), the hardware-report re-check (row 7.1) and gate integrity (row 9.1). This section is
those three records, read on 2026-10-04 at `origin/main`'s head `a06bd51` (the squash commit of
PR #173, which retired the goal-program files and changed no Go file). The goal-start SHA is
`1507a2b` (ledger g15 header).

### The gate on `main`'s head

| | |
|---|---|
| Run | [37178056937](https://github.com/NSchatz/holdfast/actions/runs/37178056937), `ci.yml`, the push of `a06bd51` to `main` |
| Head SHA | `a06bd515e9fb4763cc3ffa852d89e0082b66280d` |
| Conclusion | `success` |
| `build` | `success`, 04:48:27Z to 05:03:54Z (15m27s) |
| `package` | `success`, 04:48:28Z to 04:52:23Z (3m55s) |
| Wall-clock | 2026-10-04 04:48:25Z to 05:03:55Z (15m30s), the run's creation to its last update |

`gh run view 37178056937 -R NSchatz/holdfast --json headSha,conclusion,jobs,createdAt,updatedAt`
prints these values. `mutation` is a pull-request check and has no job in a push run.

### Hardware reports

```
$ ls testdata/hw-reports/
README.md
```

NEEDS-OWNER: no hardware report has arrived, so no hardware path has run on a real device. Every
`--encoder` path `docs/hardware-reports.md` documents is without a report:

| Path | Queue |
|---|---|
| `--encoder nvenc`, and `nvenc --hw-decode` | #52 |
| `--encoder qsv` | #53 |
| `--encoder h264_qsv --pixel-format yuv420p` | #53 |
| `--encoder vaapi` on the Intel host | #54 |
| `--encoder vaapi` on the AMD host | #55 |
| `--encoder amf` (a host install; the image cannot carry AMF) | #56 |

The queue items ask for more than the document's examples, and none of that has a report either:
`--hw-decode` for `qsv`, `vaapi` and `amf`; `h264_nvenc` (and `av1_nvenc` where the card has it)
in #52; `av1_qsv` in #53; `h264_vaapi` and `av1_vaapi` in #54 and #55; `h264_amf` and `av1_amf`
in #56. The start-time probe on each GPU host is queue #51. `ls testdata/client-reports/` also
lists only `README.md` (queue #202 to #204).

### Gate integrity, `1507a2b..a06bd51`

- **Deleted test lines: 1.** `git diff 1507a2b a06bd51 -- '*_test.go' | grep -c '^-[^-]'` prints
  `1`. The line is `f.stop()` in `internal/node/view_test.go`, in
  `TestView_ARecoveredLeaseNamesANodeWhoseModeIsNotKnown`. Reason: the fixture stopped the hub
  and then closed the ledger, and a stopping hub ends its lease as cancelled in another
  goroutine, so that write raced the close and the test went red on a loaded CI runner (run
  37164247250, PR #171's first). The line became `f.srv.Close()`, the killed-process restart the
  sibling fixtures use, with a comment saying why. No assertion changed (ledger g15 D6; PR #171,
  `c95dbb9`).
- **`func Test` per package: none lower.** `git grep -c '^func Test' <sha> -- '*_test.go'`,
  summed by directory, gives 2145 in 39 directories at `1507a2b` and 2145 in 39 directories at
  `a06bd51`, and the two per-directory lists are identical.
- **The two invariant documents: 0 lines removed.**
  `git diff 1507a2b a06bd51 -- docs/design/swap.md docs/design/quality-gate.md | grep -c '^-[^-]'`
  prints `0`.

## NEEDS-OWNER

Eleven steps are open, each physically impossible for an agent (brief §0.6). The order is safety
first, then what unblocks the most: the hardware paths first, because the pixel formats and
quality defaults a hardware job runs with are `ASSUMED` until a device has run them; then the live
services; then the second host; then the pull requests in the owner's private homelab repository, which change nothing in holdfast.
`ls testdata/hw-reports/ testdata/client-reports/` lists only each directory's `README.md` on
2026-10-04, so no report has arrived ("The closing record" above names each hardware path). The exact commands for queue #51 to #56 are in the old list
(`git show 22ae917:.claude/goals/NEEDS-OWNER.md`, rows 2 to 7) and in each queue item.

| Queue | What to do | Why an agent cannot |
|---|---|---|
| #51 | On each GPU host (Intel, AMD, NVIDIA), build the image from `main` and run the start-time probe once (`encoder: auto`, `hw_fallback: software`, an empty library); paste back the `hardware:` lines. | No goal may open a real GPU (T9). |
| #52 | On the NVIDIA host: `scripts/hw-report.sh --encoder nvenc`, again with `--hw-decode`, and `--encoder h264_nvenc --pixel-format yuv420p` (`av1_nvenc` if the card has it); `--verify` each and commit `testdata/hw-reports/`. | T9; the owner commits a hardware report (T43). |
| #53 | On the Intel host: the same for `qsv`, `qsv --hw-decode`, `h264_qsv` and `av1_qsv`, with `/dev/dri` and the render group passed to the container. | T9, T43. |
| #54 | On the Intel host: the same for `vaapi`, `vaapi --hw-decode`, `h264_vaapi` and `av1_vaapi`, with `--out` names that say `intel`. | T9, T43. |
| #55 | On the AMD host: the commands of #54 with `--out` names that say `amd`; the item asks which AMD GPU it is. | T9, T43. |
| #56 | On the AMD host with AMD's AMF runtime installed, outside the image: `scripts/hw-report.sh --encoder amf --holdfast ./holdfast`, with `--hw-decode`, `h264_amf` and `av1_amf`; the item asks whether the runtime and Go are on that host. | T9, T43; AMF cannot ship in the image (P3). |
| #202 | Run `scripts/client-report.sh --service plex` against the owner's Plex and commit the report under `testdata/client-reports/`. | No goal speaks to a live service (T49). |
| #203 | The same with `--service sonarr`. | T49. |
| #204 | The same with `--service radarr`. | T49. |
| #219 | Run one `holdfast worker` on a real second host against the owner's server, once in `mapped` mode and once in `http` mode (`docs/docker.md#worker-nodes`). | A real second host is outside every goal (T49; ledger g11 D19, ledger g12 row 4.4). |
| #274 | Review the pull request goal 15 opened in the owner's private homelab repository (it moves the deployment's pin to `v0.4.0` by digest, adds comments only to its configuration and leaves every new key unset), run that repository's `make ci`, merge it, then apply it on the host by hand. | T32; that repository's gate needs Docker, and holdfast is deployed there by hand only. |

Queue #50 (S0176's second half, a pull request in the owner's private homelab repository) is
no longer open: the owner merged that pull request on 2026-10-03 and closed the item on
2026-10-04.

What each answer changes, in short: #52 to #56 calibrate the `ASSUMED` `quality.<key>` defaults
(`internal/encoder/quality.go`), the order `encoder: auto` tries, and the upload formats
(`internal/encoder/formats.go`); a false 10-bit probe or a lost HDR10 block points at the
command-line shape of that vendor, and the fidelity gate is what keeps the source in the meantime
(old list, rows 3 to 7). Queue #51 predates the report script, and #52 to #56 supersede its
purpose once done (ledger g6, "NEEDS-OWNER (this goal)").

## Proposals awaiting the owner

P1 to P6 are decided: the owner approved them at Checkpoint T on 2026-09-29
([`.claude/goals/CHECKPOINT-T.approved`](https://github.com/NSchatz/holdfast/blob/2fd9d5986a101e4ae9d5394d2a42a3a158e3a4bf/.claude/goals/CHECKPOINT-T.approved)), each as its recommendation says.

| Proposal | Approved as |
|---|---|
| P1 `proposal-triage.md` | as proposed: 20 keep, S0179 merged into goal 10, 0 dropped, PR #94 cherry-picked first in goal 2; with S0178 built as its statement half only, S0164's value named `savings_per_hour`, S0151 as a committed Dependabot configuration, and S0176's second half a never-merged homelab pull request |
| P2 `proposal-hw-gates.md` | option (a): the same gates and floors for every encoder, per-encoder quality keys and an additive output fidelity gate |
| P3 `proposal-amd-image.md` | option (a): the VAAPI and QSV runtime in the default amd64 image with no `-hw` tag; AMD through Mesa with `encoder: vaapi`; `amf` kept for host installs and refused in the image |
| P4 `proposal-node-protocol.md` | option (a): HTTP and JSON leases on the existing server, with a `holdfast worker` subcommand |
| P5 `proposal-crop-dv.md` | option (c): a Dolby Vision source is cropped only to the active area its own RPU names, with L5 zeroed and gated |
| P6 `proposal-docs-corpus.md` | option (a): `internal/corpus` skips `.claude/` |

The approval also accepted the inferred rows I1 to I20 as written and confirmed I6.

What follows is still open: a decision a goal took as a fallback, a default it had to assume, or
a finding it left for the owner. None blocks anything.

### Fallbacks a goal took where the brief or a proposal was silent

| What was built | The alternative | Where |
|---|---|---|
| `hw_fallback` defaults to `skip` | `software` as the default | ledger g5 D of 2026-09-30; `docs/design/hardware.md#fallback` |
| `encoder: auto` tries `nvenc`, `qsv`, `vaapi`, `amf` in that order (`ASSUMED`) | an order measured from the hardware reports | ledger g5 D of 2026-09-30 |
| The codec-family guard skips an AV1 source under an HEVC target (it used to be re-encoded) | change the rank table in `internal/encoder.BetterFamily` | ledger g6 D of 2026-09-30, "Proposals awaiting the owner" |
| The Plex play hold fails open when Plex cannot be asked | fail closed | ledger g10 D3 |
| The wait before the swap while a file is played has no upper bound | a bound | ledger g10 D8, "Proposals awaiting the owner" |
| A `preserve_mtime` that is a number in the file, or an empty `HOLDFAST_PRESERVE_MTIME`, now refuses to start | restore the two older readings | ledger g10 D7 |
| A lease the node could not run at all is not charged to the file's `max_failures` | P4 rule 3 as written charges every `fail` | ledger g11 D14 |
| At the retry bound the row is parked `failed` | the goal file's "ends in a SKIP" | ledger g11 D6 |
| Only software-encoder plans are leased to a node | hardware encoders on a node, gated by the node's own start-time probe | ledger g11 D4 |
| A lease has no maximum lifetime | a bound on a node that keeps heartbeating | ledger g11, "Proposals awaiting the owner" |
| Files queued by `POST /api/scan`, a webhook or the watch are encoded by the server | offer them to nodes too | ledger g11, "Proposals awaiting the owner" |
| No server key forbids `http` mode: any holder of `node_token` can read the source of a job leased to it | a key that keeps a server mapped-only | ledger g12 D3 |
| The worker takes only a `200` as the source, never a `206` | P4 rule 6 names both | ledger g12 D7 |
| A lease's mode is not durable across a server restart | a schema step | ledger g12 D7 |
| `priority` is an integer from -1000 to 1000, first of rule, encode profile, root | the brief was silent | ledger g9 D of 2026-10-01 |
| The control token lives in a variable of the running page; a reload asks again | session storage | ledger g14 D4 |
| `/` answers a request for HTML with the UI and every other request with the plain-text page | the UI for every client | ledger g13 D6 |

### `ASSUMED` values waiting for a measurement

- An absent `quality.<key>` inherits the job's `crf` for `nvenc`, `av1_nvenc`, `qsv`, `vaapi` and
  `amf`, and for the goal-6 encoders (ledger g4 row 4.2; queue #52 to #56).
- The VAAPI upload formats, and that AMF decodes through Mesa VAAPI (ledger g6 D of 2026-09-30;
  queue #54 to #56).
- `h264_vaapi` and `h264_qsv` skip every file under `pixel_format: auto`, because every derived
  plan is at least 10-bit; the documents say to pair them with `pixel_format: yuv420p` (ledger g6
  D of 2026-09-30).
- Audio: the per-layout bitrates other than Opus and the AC-3 mono and stereo ones, and the
  loudness range target of 20 LU (ledger g7 D of 2026-10-01).
- Crop: the loose consensus within 9 px and the 2 px tolerance against a Dolby Vision L5
  rectangle (ledger g8 row 5.1 and D of 2026-10-01).
- The savings estimate: one figure of 0.065 bits per pixel for every encoder family and an
  assumed frame rate; calibrating it from this install's own finished encodes is S0164's named
  follow-up (ledger g9 D of 2026-10-01, "Proposals awaiting the owner").
- The health sweep decodes at nice 19; the benefit is assumed (ledger g9 D of 2026-10-01).
- Nodes: a node with 3 unrunnable leases in a row is offered nothing for 5 minutes; terminal
  lease rows are pruned after 7 days; 3 download attempts, a 60 s idle cut and the source write
  deadline (ledger g11 D11 and D14, ledger g12 D7).

### Findings left for the owner

- `POST /api/scan` is not held back by `run_window` or `max_load`, and files a root's watch
  offers are held back by none of `run_window`, `max_load` or pause. `docs/docker.md` states
  both; whether to gate those routes is the owner's call (ledger g2, "Proposals awaiting the
  owner").
- Every MPEG-TS and M2TS source is skipped `multi-video-stream`, because ffprobe prints a program
  section ahead of the stream line; `ts` and `m2ts` are default `video_exts`. Fail-safe, and not
  fixed because the fix changes existing decisions (ledger g3 D of 2026-09-30; still open in
  ledger g6, "Proposals awaiting the owner"). Files skipped for the related trailing-field case
  that PR #133 fixed stay skipped until `holdfast requeue --guard multi-video-stream`.
- There is no stall watchdog on an encode: a libx265 encode hung for 30 minutes at 0% CPU in a
  goal-5 gate, and the same hang in production would hold a worker (ledger g5 D of 2026-09-30).
- The dynamic-HDR carry and the Dolby Vision crop are unreachable for HEVC sources on the `cpu`
  encoder, because the codec guard skips a source already at the target codec first. An opt-in
  key that re-encodes such a source would be a new kind of job (ledger g8 D of 2026-10-01;
  `docs/design/dynamic-hdr.md#reach`).
- A file already in the target codec gets no audio rework and no sidecar, `remux_only` included
  (ledger g7 D of 2026-10-01; `docs/design/audio.md#which-files`).
- Crop has no acknowledgement key for a closed undo window, unlike `max_height`'s
  `downscale_acknowledged` (ledger g8, "Proposals awaiting the owner").
- The added stereo track carries its source track's title; holdfast writes no titles (ledger g7,
  "Proposals awaiting the owner").
- A completion that arrives before the streamed digest's record exists is not held to the
  transport comparison; the engine's own hash of the source still is (ledger g13 D15).
- A `would-transcode` row whose file later gains a second hard link keeps being counted in
  `pending` under a live engine (ledger g14 D7).
- The owner's PRs #114 and #119 carry an AI co-author trailer in their squash messages; no goal
  may rewrite history (ledger g3 D of 2026-09-30). One ledger commit of goal 7 carries a session
  link line (ledger g7 D of 2026-10-01).
- No minor release was cut between goals 5 and 14, each time by a recorded decision (T37 makes it
  optional): ledger g5, g6, g7, g8 and g9 D, ledger g10 D12, g11 D20, g12 D13, g13 D17, g14 D13.

## Follow-ups

| Follow-up | Where it is recorded |
|---|---|
| The old branch of PR #94, `sdd/S0159-holdfast-bounded-run-temp-sweep`, is still on GitHub at `9b016d4`. Its work is merged as PR #100; the owner may delete it (I12; it is not in T35's list). | ledger g2 row 2.2 and "Proposals awaiting the owner" |
| Dependabot's open pull requests #108 (the Go image), #109 (Go modules) and #110 (GitHub Actions). #108 is red by design until `ARG GO_IMAGE` and every workflow's `GO_VERSION` move with it. No agent merges or closes them, by S0151 as approved. | ledger g2 D of 2026-09-29; ledger g12 D6 |
| Dependabot's open pull requests #162 (the Node image), #163 (the distroless base) and #164 (the `web/` packages), the same rule. | ledger g13 D13 |
| PR #156 (`speed/gate`, a faster local gate) is another session's. It was closed unmerged on 2026-10-04 (`gh pr view 156`), after the program stopped; its branch is still on GitHub at `863203e`. The program touched none of it. | ledger g10 D11, g11 D10, g12 D6, g14 D14 |
| `docs/test-mass.md`'s measurement block is re-recorded at the release commit (40683 production lines, 104684 test lines, 2.57 to 1 at `c95dbb9`; `scripts/test-mass.sh -check` agrees). The check is still wired into nothing, so the block trails the tree again at the next change to a Go file. | ledger g2, "Proposals awaiting the owner"; ledger g15 row 5.4 |
| The image copies the build stage's zone database over the base's own (2026b under 2026c at goal 2's pins). | ledger g2 D of 2026-09-29 |
| The homelab census reading `plan --json`'s `roots` stays an item for the owner's private homelab repository (S0180). | ledger g2, "Proposals awaiting the owner" |
| A comment in `TestRetention_DoesNotPruneRowsForMerelyExcludedFiles` still gives a retired rule's reasoning. | ledger g2, "Proposals awaiting the owner" |
| A remux-only root with a `max_height` below the source still meets the `downscale-unacknowledged` guard, though a remux scales nothing (`internal/engine/downscale.go`). | ledger g3, "Proposals awaiting the owner" |
| Terminal rows record `crf`, not `quality.<key>`, so changing a per-encoder quality key re-opens no row; `holdfast requeue` is the lever. | ledger g4, "Proposals awaiting the owner" |
| `holdfast validate` does not statically refuse `crf: 0` inherited by a hardware encoder whose scale excludes 0; the job fails at plan derivation instead. | ledger g4, "Proposals awaiting the owner" |
| `hevc_vaapi`'s quality scale accepts 52, which the encoder clips to 51. | ledger g6 D of 2026-09-30 |
| The audio keys, `subtitle_sidecars` and `crop` are recorded on no row, so turning one on re-opens nothing; `holdfast requeue` is the lever. | ledger g7 D of 2026-10-01; ledger g8 D of 2026-10-01 |
| Profile 7 conversion is proven only over a record rewritten to 7, because `dovi_tool` cannot generate a profile 7 stream; the bitmap-subtitle skip has no end-to-end real bitmap stream for the same kind of reason. | ledger g8 row 6.2; ledger g7 row 6.2 |
| A candidate whose key or height cannot be read goes last in the queue whatever its priority. | ledger g9 row 6.2 |
| `savings_per_hour` is documented in `docs/enumeration.md` and `config.example.yaml`, not in `docs/profiles.md`. | ledger g9 row 6.2 |
| A failing `fsync` or `close` on a node upload is answered 500 and logged, and has no test. | ledger g11 row 3.6 |
| Two fixtures in `internal/engine/nodes_test.go` go red on their regression only by reaching their timeout, and one stays green when only the expiry comparison is removed (its `internal/node` twin covers it). | ledger g11 D21; ledger g12 D11 |
| `worker_slots` above 1 needs `node_max_leases_per_node` raised to match. | ledger g11 D21 |
| The read group is open when `server_read_token` is unset, so the node token is refused there only where a read token is set. | ledger g11 row 5.2 |
| One test file uses a private-range example address where the documents use RFC 5737 ones. | ledger g12 D14 |
| The end-to-end `http` mode test shows "no mount" by configuration, not by OS isolation; queue #219 is the real proof. | ledger g12 D14 |
| Node 26 becomes the LTS line on 2026-10-28; the bump from Node 24.21.0 is left to later work (see #162). | ledger g13 D5 |
| The image smoke test's UI step runs on amd64 only; `make fmt` walks `web/node_modules`. | ledger g13 D9, D11 |
| `scripts/ui.sh toolchain` can rewrite a block of `web/pnpm-lock.yaml` when the pin and the lockfile disagree; the `packageManager` hash is checked on the corepack route only. | ledger g13 D18 |
| `serve` with an absent `server_addr` is graded at the unit and on `validate`, not on a live `serve`. | ledger g13 D14 |
| A full suite cannot put its `TMPDIR` on a tmpfs: holdfast refuses a tmpfs as local storage, so `TestWatch_*` goes red there. `GOTMPDIR` may be on one. | ledger g3 D of 2026-09-30 (re-run); ledger g14 D10 |
| `TestServeSmoke`'s 3 s readiness wait missed under load in goals 7 and 10. Goal 11 gave the `waitHTTP` helper a one-minute floor; the ledgers do not say in so many words that this closes the two earlier notes. | ledger g7 and g10, "Proposals awaiting the owner"; ledger g11 D16 |

## Worker variables to remove

`.claude/settings.json` on `main` reads:

```json
{
  "env": {
    "GOFLAGS": "-p=4",
    "GOMAXPROCS": "12"
  },
  "autoMemoryEnabled": false,
  "attribution": {
    "commit": ""
  }
}
```

- **Why they were set.** T40 capped the program at 4 agents at once and committed
  `GOFLAGS=-p=4` and `GOMAXPROCS=12` for every session in this repository (raised on 2026-09-30
  from `-p=2` and `2`, when the program moved into a larger shared container; PR #119). `-p`
  stays low because one package dominates `make check` and encode tests are memory-heavy (brief
  §1, T40).
- **They stay after the finale.** The decisions were silent on the end of the program, so the
  `env` block stays, harmless, and this report lists it (brief §1, I4; brief §0.12). No goal
  removes it.
- **Removing them is the owner's.** Delete the whole `"env"` object, its two entries
  `"GOFLAGS": "-p=4"` and `"GOMAXPROCS": "12"` and the comma after its closing brace, and leave
  the rest of the file as it is. Since PR #170 nothing heavier than one focused test runs on the
  development host, so the two values no longer bound a gate there; CI sets its own.
- **What stays.** `"autoMemoryEnabled": false` and `"attribution": {"commit": ""}` stay. The
  empty `attribution.commit` is what keeps the CLI from adding a commit trailer, which
  `CLAUDE.md` forbids (brief §1, I18 and T38; brief §0.12).
