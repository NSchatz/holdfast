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
| 3.1 | `hw_decode` key, off by default, per-vendor `-hwaccel` pipelines keeping 10-bit and HDR, frames downloaded where a gate needs them | DONE (PR #134, `8b52288`): `hw_decode: software|hardware` per root with a top-level default, digest-silent at `software`; NVENC `-hwaccel cuda`, VAAPI/QSV VAAPI on the encoder's node, AMF VAAPI on the VAAPI node, every VAAPI device named and DRM-only; no `-hwaccel_output_format`, so every frame is downloaded with its properties; `docs/design/hardware.md#decode`; `TestHardware_HWDecode*` |
| 3.2 | Golden argv and the fidelity gate on fakes | DONE (PR #134): `hw-decode/*` golden cases in both layers for every encoder (existing lines unchanged); `TestHWDecode_EveryVendorKeeps10BitAndHDR10` (10 hardware encoders replace a 10-bit HDR10 source through their own pipeline, the fidelity gate passing depth, primaries, transfer and both HDR10 blocks), `TestHWDecode_EightBitOnlyEncodersDecodeOnTheirVendor`, `TestHWDecode_TheFidelityGateRejectsALossyHardwarePath` (8-bit cut or dropped mastering display rejected naming the field, source byte-identical; CUDA and VAAPI; HEVC, AV1, H.264), `TestHWDecode_DeclaredPerVendorAndRefusedWhenForged`; gate round 1 exit 0 (run 1848 s, `internal/engine` 1737.1 s, 64%); mutation-diff 100% (3 killed); CI green |

## Phase 4 - New encoders (line D)

| # | Item | State |
|---|---|---|
| 4.1 | `libx264`, `h264_nvenc`, `h264_qsv`, `h264_vaapi`, `h264_amf`, `av1_qsv`, `av1_vaapi`, `av1_amf` in the registry with golden argv and quality scales | DONE (PR #132, `0d8fce0`): keys `x264` (alias `libx264`) and the seven codec names; `Spec.API`; scales from the pinned binary and source (`TestQualityScales_*`, `TestPixelFormats_MatchThePinnedBinary`); golden files `encoder-<key>.txt`/`engine-<key>.txt` for each, every existing file byte-identical but appended blocks |
| 4.2 | Codec-family skip rules | DONE (PR #132): `better-codec-family` (H.264 < HEVC < AV1), `TestBetterFamily_RanksH264BelowHEVCBelowAV1`, engine goldens `source-hevc`/`source-av1/every-encoder` |
| 4.3 | `holdfast validate` accepts a config naming each | DONE (PR #132): `TestValidate_AcceptsAConfigNamingEachT27Encoder`, `TestPreflight_T27HardwareEncodersProbeTheirOwnCommandLine` |
| 4.4 | `NOTICE` names libx264 (GPL-2.0-or-later) | DONE (PR #132): cited from the x264 source, read 2026-09-30 |

## Phase 5 - Hardware reports (line E)

| # | Item | State |
|---|---|---|
| 5.1 | `scripts/hw-report.sh` writes a redacted `testdata/hw-reports/<encoder>-<date>.json`; a test proves the redaction | DONE (PR #133, `a956f4c`): built by a build agent; `TestHWReport_CPUReportCarriesTheFiguresAndNoHostIdentity` (a real run on `cpu` through stand-in `hostname`/`nvidia-smi`/`vainfo` printing planted host, user, home, GPU UUID, serial and MAC: none survives, nor the real host name, user, HOME or temp paths), `TestHWReport_VerifyRefusesAReportCarryingAForbiddenToken`, `TestHWReport_UnavailableEncoderWritesNoReport`, `TestHWReport_RefusesWithoutJqAndNamesIt`; with the `probe.VideoStreams` trailing-field fix; gate exit 0 on `92c3dd2` (run 1883 s, `internal/engine` 1733.5 s, 64%); mutation-diff 100% (5 killed); CI green |
| 5.2 | `NEEDS-OWNER.md`: one entry each for NVENC, QSV, VAAPI on Intel, VAAPI on AMD, AMF on a host | DONE: rows 3 (NVENC), 4 (QSV), 5 (VAAPI on Intel), 6 (VAAPI on AMD), 7 (AMF on a host install), each with the exact `scripts/hw-report.sh` commands (plain, `--hw-decode`, and the H.264/AV1 siblings), tools, the expected report and what each answer changes |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | DONE (counted at `8b52288`): `func Test` 1446 -> 1463, no package fell (`cmd/holdfast` 226 -> 228, `internal/config` 123 -> 125, `internal/encoder` 30 -> 33, `internal/engine` 500 -> 504, `internal/probe` 21 -> 22, `scripts/hwreport` 0 -> 5, every other package unchanged); `docs/design/swap.md` 62 and `docs/design/quality-gate.md` 76 lines, `git diff --numstat 7985818 8b52288` empty for both; 41 lines deleted in `*_test.go` (+1449 -41), each with its reason below |
| 6.2 | Adversarial review of the report | DONE: a fresh subagent checks the GOAL REPORT and the repositories after this commit; its verdict is the report's line H, and any correction it asks for lands in its own commit |

### The 41 deleted `*_test.go` lines and why

Every one is replaced in the same hunk; no assertion was removed.

- `internal/encoder/formats_test.go` (12, PR #132): the seven rows of the quality-scale table are
  the same values re-aligned by gofmt beside the eight new rows; the `QualityKeys` expectation and
  the named-edges list gained the new keys; `TestUploads_OnlyVAAPI`'s comment and condition read
  the `API` field (three VAAPI encoders now upload) instead of the key `vaapi`.
- `internal/engine/stray_replacement_test.go` (13, PR #132): the control arm of two stray-temp
  tests was "an H.264 temp, a codec no encoder here writes"; x264 and h264_* now write H.264, so
  the arm reads MPEG-4 Part 2 (`mkMPEG4From` replaces `mkH264From`, its comment, the calls and the
  two `h264` preconditions); every assertion kept.
- `internal/encoder/encoder_test.go` (4, PR #132): `TargetCodecs` must now be exactly
  `[av1 h264 hevc]`, not `[av1 hevc]` (the comment and the check).
- `internal/engine/encode_bitrate_test.go` (3, PR #132): the S0079 pin tests call
  `buildArgs(spec, ts, pinPlan(key), ...)` (the 8-bit-only h264_qsv/h264_vaapi are pinned at
  `yuv420p`), and the no-quality-target list gained `-rc_mode`.
- `internal/encoder/available_test.go` (2, PR #132): "only amf is refused in the image" became
  "only the AMF encoders", keyed on `API`, with a new check that h264_amf and av1_amf name their
  VAAPI sibling.
- `cmd/holdfast/hardware_preflight_test.go` (2, PR #132): the two stand-in `case` patterns refuse
  every hardware codec by pattern (`*_nvenc|*_qsv|*_vaapi|*_amf`) instead of the old closed list,
  so a new hardware codec can never reach the real binary (T9).
- `internal/config/hardware_test.go` (2, PR #134): "hw_fallback is the last knob rendered" became
  "the knob before hw_decode", which is appended after it.
- `internal/engine/golden_argv_test.go` (1, PR #132): the encoder-layer `quality-set` map gained
  the new keys (old encoders' lines byte-identical).
- `internal/engine/x265parallelism_s0161_test.go` (1, PR #132): the encoder binary is the argv
  stand-in for hardware encoders, so no NVENC encode reaches this host's GPU (T9).
- `internal/probe/props_test.go` (1, PR #133): the snapshot-agreement loop's file list gained the
  two side-data cases.

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

- 2026-09-30: finding, NOT fixed (outside §10, and fixing it changes existing decisions). A source
  whose video stream carries stream-level side data - an MPEG-2 stream's CPB properties, or a
  Matroska file's container-level HDR10 mastering-display block (an FFV1 or VP9 HDR10 file) -
  makes `ffprobe -select_streams v -show_entries stream=index:stream_disposition=attached_pic
  -of csv=p=0` print `0,0,` (a trailing empty field for the side-data section), which
  `probe.VideoStreams` refuses, so every such file is skipped `multi-video-stream`. Fail-safe,
  but it means no MPEG-2 source is ever encoded; with goal 3's MPEG-TS finding it is the same
  parser. The goal-6 fixtures use MPEG-4 Part 2 and a PQ FFV1 without a stream-level block
  instead. Listed for the owner.
- 2026-09-30: PR #132 (`holdfast-g6/encoders`) opened; goldens regenerated twice (the first set of
  new fixtures, MPEG-2 and FFV1-HDR10, hit the finding above), every existing golden file
  byte-identical but for appended blocks and `encoder-none.txt`'s list of known encoders.

- 2026-10-01: PR #132 (encoders) merged as `0d8fce0`: gate round 1 red on two stray-temp tests
  whose "codec no encoder writes" arm was H.264 (now written by x264 and h264_*; the sweep rightly
  holds it), fixed by reading MPEG-4 Part 2 with every assertion kept; round 2 exit 0
  (`internal/engine` 1612.5 s, 60% of `TEST_TIMEOUT`); mutation-diff 100% (18 killed, 0 lived);
  CI green.
- 2026-10-01: PR #133 merged as `a956f4c` after a re-gate on the branch merged up to `0d8fce0`
  (two re-gate attempts were cut off by container restarts and left no result; the third exit 0).
  PR #134 (`holdfast-g6/hw-decode`) opened; it adds `--hw-decode` and `--pixel-format` to the
  report script (an 8-bit-only H.264 encoder would otherwise report only skips, since every
  derived plan is at least 10-bit).
- 2026-10-01: the stream-side-data finding above is FIXED after all, in PR #133. The hw-report
  agent's HDR10 clip (FFV1 carrying the mastering-display block in its Matroska Colour element,
  as real HDR10 Matroska files do) hit it, and it fixed `probe.VideoStreams` to accept trailing
  fields only when they are empty (anything non-empty still refuses). Accepted rather than
  reverted: the skip reason was false (one video stream, reported as several), the fix keeps the
  fail-safe for every row it cannot read, and without it no HDR10 Matroska source and no MPEG-2
  source is ever encoded - which also defeats this goal's own report clip and the H.264 targets'
  natural sources. It changes decisions only for files not yet decided: a terminal
  `multi-video-stream` row records no configuration input, so it stays skipped until the owner
  requeues it (`holdfast requeue --guard multi-video-stream`, CLI-only). Goal 3's MPEG-TS
  program-section case is a different shape and is unchanged. Listed in the GOAL REPORT.

- 2026-10-01: PR #134 (hw-decode) merged as `8b52288`: gate round 1 exit 0, mutation-diff 100%,
  CI green; `main` had moved only by the ledger-only `3b7a38e`. `NEEDS-OWNER.md` rows 3-7 added.
- 2026-10-01: no minor release is cut at the end of this goal (T37 makes it optional): none of the
  new encoders or the hardware decode has run on a real device, and rows 3-7 are the first
  evidence; a later goal can release on it.

## NEEDS-OWNER (this goal)

A real GPU run is physically impossible here under T9, so each hardware path is a row of
`.claude/goals/NEEDS-OWNER.md`, safety first then what unblocks the most:

- Row 3: NVENC report (`nvenc`, `--hw-decode`, `h264_nvenc`; `av1_nvenc` if the card has it).
- Row 4: QSV report on Intel (`qsv`, `--hw-decode`, `h264_qsv`, `av1_qsv`).
- Row 5: VAAPI on Intel report (`vaapi`, `--hw-decode`, `h264_vaapi`, `av1_vaapi`).
- Row 6: VAAPI on AMD report.
- Row 7: AMF on a host install report (needs AMD's runtime and Go on that host; asked in the row).
- Row 2 (goal 5, the start-time probe) is still open; rows 3-7 supersede its purpose once done.

## Proposals awaiting the owner

- MPEG-TS sources are still skipped `multi-video-stream` (goal 3's finding: a program section
  precedes the stream in ffprobe's csv output). Reading the stream list as JSON would fix it; it
  changes existing decisions, so it is not done here. Files a past run skipped
  `multi-video-stream` for the trailing-field bug that PR #133 fixed stay skipped until requeued
  (`holdfast requeue --guard multi-video-stream`).
- The codec-family guard now skips an AV1 source under an HEVC target (it used to be re-encoded).
  If the owner wants AV1 sources re-encoded to HEVC, the guard's rank table is the one place to
  change (`internal/encoder.BetterFamily`).

## Resume here

All tracks merged: #132 (encoders), #133 (hw-report), #134 (hw_decode). No branch, worktree or
open PR of this goal remains. Next: the fresh adversarial review of the GOAL REPORT (line H); its
corrections, if any, go in their own commits before the report is printed.

COMPLETE (goal 6): 2026-10-01
