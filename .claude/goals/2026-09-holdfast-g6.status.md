# Goal 6 ledger - hardware decode, new encoders and hardware reports

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §10; goal file
  `.claude/goals/2026-09-holdfast-g6.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`
  (P2 option (a): the same gates and floors for every encoder; P3 option (a): the VAAPI/QSV
  runtime in the default amd64 image, `amf` kept for host installs and refused in the image).
- Started: 2026-09-30, from `/workspace/holdfast` in the maker container.
- Goal-start SHA: `7985818320157f0dffdc4b75cbc75d1dd1b2c178` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g5.status.md` (ends
  `COMPLETE (goal 5): 2026-09-30`). `NEEDS-OWNER.md` at start: rows 1 and 2, both `OPEN`.
- Precondition, as checked:

```
$ git fetch -q origin && git log -1 --format='%H %s' origin/main
7985818320157f0dffdc4b75cbc75d1dd1b2c178 chore(goals): goal 5 ledger - final review recorded, CI history stated as it ran
$ git show origin/main:.claude/goals/2026-09-holdfast-g5.status.md | rg -n 'COMPLETE \(goal 5\)'
229:COMPLETE (goal 5): 2026-09-30
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `d4d70a6`, line 59, "Per
goal"): "**Goal 6** (hardware decode, new encoders, reports): none." `CHECKPOINT-T.approved`
approves P1 as proposed and amends no goal-6 row. S0165's AC-16 (a `max_height` crossing into
another codec's band is refused; goal 5, PR #128) must keep agreeing with this goal's
codec-family skip rules.

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g6-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g6/tmp`. Logs under `/cache/tmp/holdfast-g6/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (log `gate-baseline.log`) | exit 0 | 1579 s (26m19s) |
| `internal/engine` under `go test -race` (from that run) | ok, 88.0% coverage | 1472.0 s (55% of `TEST_TIMEOUT` 45m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 89.2% coverage | 625.8 s |
| `func Test` count, all packages | 1446 in 27 packages (table below) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 62, 76 | - |
| `rg -n hwlive .github Makefile` | prints nothing (exit 1) | - |

`func Test` per package at the goal-start SHA (`git grep -c '^func Test' <sha> -- '<dir>/*_test.go'`,
summed per directory):

| Package | Count | Package | Count |
|---|---|---|---|
| `cmd/holdfast` | 226 | `internal/memlimit` | 3 |
| `internal/config` | 123 | `internal/metrics` | 19 |
| `internal/corpus` | 3 | `internal/notify` | 10 |
| `internal/cpuquota` | 10 | `internal/probe` | 21 |
| `internal/diskfree` | 4 | `internal/schedule` | 13 |
| `internal/docscheck` | 29 | `internal/secret` | 13 |
| `internal/encoder` | 30 | `internal/secretscan` | 10 |
| `internal/engine` | 500 | `internal/server` | 105 |
| `internal/fsclass` | 6 | `internal/sourceoffer` | 9 |
| `internal/hdr` | 20 | `internal/startup` | 80 |
| `internal/heapmeasure` | 5 | `internal/store` | 149 |
| `internal/hwdevice` | 8 | `internal/version` | 1 |
| `internal/logging` | 5 | `internal/vmaf` | 39 |
| | | `scripts/mutation-gate` | 5 |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`7985818`): goal 5's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`c5c363b`) |
| 1.3 | Baselines with timings | DONE (`7985818`): the table above; `make check` exit 0 in 1579 s |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | P1 rows for goal 6 | DONE (`d4d70a6`): P1 assigns none |

## Phase 3 - Hardware decode (line C)

| # | Item | State |
|---|---|---|
| 3.1 | `hw_decode` key, off by default, per-vendor `-hwaccel` pipelines keeping 10-bit and HDR, frames downloaded where a gate needs them | TODO |
| 3.2 | Golden argv and the fidelity gate on fakes | TODO |

## Phase 4 - New encoders (line D)

| # | Item | State |
|---|---|---|
| 4.1 | `libx264`, `h264_nvenc`, `h264_qsv`, `h264_vaapi`, `h264_amf`, `av1_qsv`, `av1_vaapi`, `av1_amf` in the registry with golden argv and quality scales | TODO |
| 4.2 | Codec-family skip rules | TODO |
| 4.3 | `holdfast validate` accepts a config naming each | TODO |
| 4.4 | `NOTICE` names libx264 (GPL-2.0-or-later) | TODO |

## Phase 5 - Hardware reports (line E)

| # | Item | State |
|---|---|---|
| 5.1 | `scripts/hw-report.sh` writes a redacted `testdata/hw-reports/<encoder>-<date>.json`; a test proves the redaction | TODO |
| 5.2 | `NEEDS-OWNER.md`: one entry each for NVENC, QSV, VAAPI on Intel, VAAPI on AMD, AMF on a host | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-30: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g6/` (goal 3's
  finding: a tmpfs `TMPDIR` is refused by `fsclass`), and goal shells use `command grep`, `rg` or
  `git grep` (brief §0.11; the shell's `grep` function fails here).

- 2026-09-30: tracks. `holdfast-g6/encoders` (the eight T27 encoders, the codec-family guard,
  `NOTICE`), `holdfast-g6/hw-decode` (cut from the encoders branch, brought up to date by merge
  once it lands; it needs `Spec.API`), and `holdfast-g6/hw-report` (a build agent in its own
  worktree). Reasoning: T52 batching; hw_decode's per-vendor pipelines read the API field the
  encoders track adds.
- 2026-09-30: registry keys. The seven hardware encoders are keyed by their ffmpeg codec names
  (as `av1_nvenc` already is); libx264 is keyed `x264`, spelled like `svtav1`, with `libx264` its
  alias, so both are accepted. `Spec.API` (nvenc, qsv, vaapi, amf) carries the vendor, and the
  device, decode and rate-control shapes are built per API and then per codec, so every existing
  encoder's command line is byte-identical (the goldens prove it).
- 2026-09-30: the codec-family guard (`better-codec-family`, H.264 < HEVC < AV1) applies to every
  target, as the brief states the rule (§10.3: "a source already in an equal-or-better family is
  not re-encoded into a worse one"). It changes one existing decision, in the fail-safe
  direction: an AV1 source under an HEVC target was re-encoded and is now skipped. A codec with no
  rank (MPEG-2, VC-1, VP9, FFV1, ...) is decided as before. Listed for the owner.
- 2026-09-30: `h264_vaapi` uploads `nv12` only and `h264_qsv` lists no 10-bit format, while every
  derived plan is at least 10-bit, so with `pixel_format: auto` both skip every file
  (`exotic-pixel-format`); the docs say to pair them with `pixel_format: yuv420p`. The derivation
  is not changed per codec (fail-safe; I5). ASSUMED until the QSV/VAAPI reports.
- 2026-09-30: `av1_vaapi` has no `-qp`; its target is `-rc_mode CQP -global_quality N` (the AV1
  q_idx, 1-255), naming the mode so a driver offering ICQ cannot read the same number on the
  1-51 ICQ scale (`vaapi_encode.c:1318-1319, 1419-1425`, `vaapi_encode_av1.c:140`).
- 2026-09-30: finding. `hevc_vaapi`'s scale is 1-52 (goal 4), but the encoder clips the P-frame
  QP to 1-51 (`vaapi_encode_h265.c:979`), so 52 encodes as 51. Not changed (narrowing it would
  refuse a configuration that validates today; I5); `h264_vaapi`'s new scale stops at 51.
- 2026-09-30: finding, fixed in the encoders track. `TestS0161_AC4_NoOtherEncodersArgvMovesWithTheFigure`
  ran the real ffmpeg for every non-libx265 encoder; this container resolves `libnvidia-encode`
  and has `/dev/nvidia0` (a Quadro P2200 through `claude-gpu`), so `nvenc` ran a real NVENC
  encode inside every gate since S0161, against T9. The test now takes hardware argv lists from
  the golden stand-in, and the cmd preflight stand-ins refuse every hardware codec by pattern
  (`*_nvenc|*_qsv|*_vaapi|*_amf`) rather than by the old closed list.
- 2026-09-30: `hw_decode` is a string knob, `software|hardware` (default `software`), per root
  with a top-level default like `hw_fallback`, digest-silent at the default. String values, not
  a YAML boolean or `on`/`off`, so no YAML 1.1 reading can turn them into something else. The
  decode runs on the job's encoder's vendor: CUDA for NVENC, VAAPI on the encoder's node for VAAPI
  and QSV (the native decoders reach Intel through VAAPI; the pinned build's QSV decode is the
  separate `*_qsv` decoders), VAAPI on the VAAPI node for AMF (ASSUMED until the AMF report). No
  `-hwaccel_output_format`, so every frame is downloaded with its properties
  (`fftools/ffmpeg_demux.c:1687-1688`, `ffmpeg_dec.c:389-393, 370`); keeping frames on the device
  would need hardware filters that change what the perceptual gate measures. Reasoning:
  `docs/design/hardware.md#decode`.

## NEEDS-OWNER (this goal)

## Proposals awaiting the owner

## Resume here

Phase 1 done. Branches: `holdfast-g6/encoders` (worktree `/cache/wt/holdfast/holdfast-g6-encoders`,
code committed, golden argv to regenerate and diff), `holdfast-g6/hw-decode` (worktree
`/cache/wt/holdfast/holdfast-g6-hw-decode`, cut from the encoders branch, code committed, goldens
after the encoders branch's), `holdfast-g6/hw-report` (a build agent). Gates run one at a time under
`/cache/locks/holdfast-heavy.lock` with `/cache/tmp/holdfast-g6/gate.sh <dir> <log>`.
