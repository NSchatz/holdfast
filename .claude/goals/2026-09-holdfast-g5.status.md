# Goal 5 ledger - hardware runtime, argv and detection

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §9; goal file
  `.claude/goals/2026-09-holdfast-g5.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`
  (P3 approved, option (a): the VAAPI/QSV runtime in the default amd64 image, AMD through Mesa
  `radeonsi` with `encoder: vaapi`, `amf` kept for host installs and refused in the image).
- Started: 2026-09-30, from `/workspace/holdfast` in the maker container.
- Goal-start SHA: `bf36b9cd96ab50ef57e3eba96c28b7dab218f5cb` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g4.status.md` (ends
  `COMPLETE (goal 4): 2026-09-30`). `NEEDS-OWNER.md` at start: row 1 (merge
  NSchatz/homelab#208), `OPEN`; the PR is still open (checked 2026-09-30).
- Precondition, as checked:

```
$ git pull --rebase
Already up to date.
$ git log -1 --oneline origin/main
bf36b9c chore(goals): goal 4 ledger - #125 merged, review round 2 recorded, counts at 4e0488f
$ git show origin/main:.claude/goals/2026-09-holdfast-g4.status.md | command grep -n 'COMPLETE (goal 4)'
207:COMPLETE (goal 4): 2026-09-30
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `bf36b9c`, "Per goal"):
"**Goal 5** (hardware runtime, argv and detection): S0165." S0165 is
`per-rule-encoder-selection`: a resolution rule may name its own `encoder`; the VMAF floors stay
per root. `CHECKPOINT-T.approved` approves P1 as proposed and amends no goal-5 row. P3 is
approved as option (a).

Already done by goal 4 (not rebuilt here): VAAPI uploads a 10-bit plan as `p010le` with
`-profile:v main10`, and NVENC and QSV name `-pix_fmt` explicitly (goal-4 ledger row 4.1).

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g5-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g5/tmp` (goal 3's finding). Logs under `/cache/tmp/holdfast-g5/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (log `gate-baseline.log`) | exit 0 | 1615 s (26m55s) |
| `internal/engine` under `go test -race` (from that run) | ok, 87.7% coverage | 1512.5 s (56% of `TEST_TIMEOUT` 45m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 89.4% coverage | 477.6 s |
| `func Test` count, all packages | 1388 in 25 packages (table below) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 62, 76 | - |
| `rg -n hwlive .github Makefile` | prints nothing (exit 1) | - |

`func Test` per package at the goal-start SHA (`git grep -c '^func Test' <sha> -- '*_test.go'`,
summed per directory):

| Package | Count | Package | Count |
|---|---|---|---|
| `cmd/holdfast` | 217 | `internal/metrics` | 19 |
| `internal/config` | 107 | `internal/notify` | 10 |
| `internal/corpus` | 3 | `internal/probe` | 21 |
| `internal/cpuquota` | 10 | `internal/schedule` | 13 |
| `internal/diskfree` | 4 | `internal/secret` | 13 |
| `internal/docscheck` | 29 | `internal/secretscan` | 10 |
| `internal/encoder` | 20 | `internal/server` | 105 |
| `internal/engine` | 486 | `internal/sourceoffer` | 9 |
| `internal/fsclass` | 6 | `internal/startup` | 80 |
| `internal/hdr` | 20 | `internal/store` | 149 |
| `internal/heapmeasure` | 5 | `internal/vmaf` | 39 |
| `internal/logging` | 5 | `scripts/mutation-gate` | 5 |
| `internal/memlimit` | 3 | | |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`bf36b9c`): goal 4's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`189bb12`) |
| 1.3 | Baselines with timings | DONE (`bf36b9c`): the table above; `make check` exit 0 in 26m55s |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | S0165 `per-rule-encoder-selection` | DOING (PR #128) |

## Phase 3 - Runtime in the image (line C)

| # | Item | State |
|---|---|---|
| 3.1 | libva, libva-drm, libdrm, Intel iHD, `libmfx-gen1.2` (amd64) and Mesa `radeonsi` VA with their closure, pinned, each in `NOTICE` | DOING (PR #127) |
| 3.2 | `NVIDIA_DRIVER_CAPABILITIES=compute,video,utility` and the compose/docs snippet | DOING (PR #127) |
| 3.3 | Image smoke: a VAAPI init on a missing device prints a device error, not an abort | DOING (PR #127) |

## Phase 4 - Argv and detection (lines D, E)

| # | Item | State |
|---|---|---|
| 4.1 | Every VAAPI device opens with `connection_type=drm` | DOING (PR #126) |
| 4.2 | `Available()` probes through the real argv builder, including a 10-bit probe | DOING (PR #126) |
| 4.3 | Device discovery (`/dev/dri/renderD*`, sysfs vendor, a permission error naming `group_add`) | DOING (PR #126) |
| 4.4 | `amf` in the image refused at start with a named reason; `validate` still accepts it; never aliased to `vaapi` | DOING (PR #126) |
| 4.5 | `encoder: auto` choosing per job | DOING (branch `holdfast-g5/encoder-auto`) |
| 4.6 | Per-library `hw_fallback: software|skip`, default stated with its reason | DOING (branch `holdfast-g5/encoder-auto`) |
| 4.7 | `docs/docker.md` hardware section and `docs/design/hardware.md` | TODO |

## Phase 5 - Report

| # | Item | State |
|---|---|---|
| 5.1 | Gate integrity counted from the goal-start SHA | TODO |
| 5.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-30: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g5/` (goal 3's
  finding: `TMPDIR=/scratch` is a tmpfs `fsclass` refuses, which reds `TestWatch_*`), and goal
  shells use `command grep`, `rg` or `git grep` (brief §0.11).

- 2026-09-30: tracks. PR #127 (`holdfast-g5/hw-runtime`, a build agent) carries the image
  runtime (phase 3); PR #128 (`holdfast-g5/s0165-rule-encoder`, a build agent) carries S0165;
  PR #126 (`holdfast-g5/hw-detect`) carries the argv, the probe, device discovery and the
  `amf` refusal; `holdfast-g5/encoder-auto` (cut from #126's head, brought up to date by merge
  once #126 lands) carries `encoder: auto` and `hw_fallback`. Reasoning: T52 batching; the
  last track needs the probe's `Capability` from #126.
- 2026-09-30: finding. The maker container resolves `libnvidia-encode` (`ldconfig -p`; the
  host's Quadro through `claude-gpu`), so `TestHardwareEncoders_AvailabilityTable`, which ran
  a real NVENC encode whenever `encoder.Available` said yes, could open the real GPU inside the
  gate, against T9. It moved behind the `hwlive` build tag in #126 (the brief's own guard,
  §0.7); `rg -n hwlive .github Makefile` prints nothing, so neither the gate nor CI builds it.
  Every new hardware test runs against a stand-in ffmpeg that refuses or re-routes the hardware
  codec.
- 2026-09-30: the image's binary is marked by a `holdfast_image` build tag on the Dockerfile's
  `go build` (`internal/version.Packaging`), not a fifth `-X` ldflag: the source-offer wiring
  test pins the ldflags to exactly four `-X` assignments shared with the Makefile, and a tag
  keeps a host build (Makefile, `go install`) free of the marker by construction.
- 2026-09-30: `hw_fallback` default is `skip` (T11: the program picks and documents it).
  Reasons, in `docs/design/hardware.md#fallback`: an existing configuration keeps its
  decisions and argv (I5) - under `skip` a named hardware encoder that does not work refuses
  the start exactly as before; a silent software encode can cost many times the wall-clock on a
  shared host (T6); and nothing is lost, because a `hardware-unavailable` skip is mutable
  (re-decided every pass) and the source is untouched. A hardware encode that FAILS at run time
  under `skip` fails the job as before (not a new skip), so `max_failures` still governs it.
- 2026-09-30: `encoder: auto` tries `nvenc`, `qsv`, `vaapi`, `amf` in that order (ASSUMED, not
  measured; the hardware reports of goal 6 are what would change it), hands hardware only 4:2:0
  plans (the layout the probe encodes), and every choice and its fallback writes HEVC, so the
  target codec never depends on the host. The row records the encoder that ran; the decision
  inputs record `auto`.
- 2026-09-30: QSV now gets a device of its own (`-init_hw_device vaapi=hfva:<node>,connection_type=drm
  -init_hw_device qsv=hfqsv@hfva`): without one, `hevc_qsv` opens its own VAAPI display with no
  node, which reaches the same X11 fallthrough that aborts in the image. Read from the pinned
  ffmpeg source (`fftools/ffmpeg_enc.c:133-175`, `libavcodec/qsvenc.c:2762-2765`), not run on a
  device; the hardware report is the proof (NEEDS-OWNER, goal 6).

## NEEDS-OWNER (this goal)

None yet.

## Proposals awaiting the owner

None yet.

## Resume here

Baseline done. PRs #126, #127, #128 open; #126's gate running (log `gate-pr126-r1.log`).
`holdfast-g5/encoder-auto` written and tested locally, to be merged up to `main` after #126 and
opened as its own PR. Then `docs/docker.md` hardware section, the image smoke for the `amf`
refusal (after #127), the report.
