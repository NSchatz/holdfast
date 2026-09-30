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
| 2.1 | S0165 `per-rule-encoder-selection` | DONE (PR #128, `075e3ba`): a resolution rule may name `encoder` (validated by `validateEncoderKey`), refused beside a resolved `remux_only`, every VMAF key in a rule refused by name, a `max_height` crossing into another codec's band refused (AC-16); AC-1..AC-14 and AC-16 each graded by a named test (`TestS0165_AC*` in `internal/config`, `internal/engine`, `cmd/holdfast`), AC-15 by the gate; AC-2 goldens from b7c26ca (byte-identical to `bf36b9c`); mutation-diff 100% (killed 83, lived 0, not covered 10); 2 test lines moved (the unknown-key fixture `encoder: svtav1` became `min_vmaf: 90`); gate exit 0 in 1661 s (`internal/engine` 1554.6 s); CI green. With #129 a rule may name `auto` (read as HEVC by the ceiling check) and its hardware follows its root's `hw_fallback` |

## Phase 3 - Runtime in the image (line C)

| # | Item | State |
|---|---|---|
| 3.1 | libva, libva-drm, libdrm, Intel iHD, `libmfx-gen1.2` (amd64) and Mesa `radeonsi` VA with their closure, pinned, each in `NOTICE` | DONE (PR #127, `84198b7`): 34 pinned Debian 13 amd64 packages from snapshot.debian.org `20260929T202609Z` (version and sha256 each, checked against that snapshot's Packages index), the full closure of `iHD_drv_video.so`, `radeonsi_drv_video.so`, `libmfx-gen.so.1.2`, `libva.so.2`, `libva-drm.so.2`, `libdrm.so.2` (P3 named 12; the closure needed 22 more: `libdrm-intel1`, `libpciaccess0`, `libelf1t64`, `libsensors5`, `libexpat1`, LLVM's 7 dependencies, 10 X client libraries, `libdrm-common`); about 244 MiB; `NOTICE` names each package, version, licence and source URL (iHD flagged DFSG non-free; the GPL/LGPL members' corresponding source at snapshot.debian.org); `scripts/check-pins.sh` section for the pins with NOTICE agreement both ways, selftest 43/43 bite (8 new); arm64 carries none, by design |
| 3.2 | `NVIDIA_DRIVER_CAPABILITIES=compute,video,utility` and the compose/docs snippet | DONE (PR #127, `84198b7`): `ENV NVIDIA_DRIVER_CAPABILITIES=compute,video,utility` (legacy hook path default `utility,compute` lacks `video`; CDI hosts carry the library regardless; NVIDIA and moby sources cited); `docker-compose.yml` and `docs/docker.md` show `/dev/dri` with a numeric `group_add` |
| 3.3 | Image smoke: a VAAPI init on a missing device prints a device error, not an abort | DONE (PR #127, `84198b7`): CI `package` (run 36757872914) on amd64: every driver and library resolves all its dependencies in the image (loader `--list`), a VAAPI init with no render node ends in a device error (exit 234, never 134), and one on `/dev/null` proves libva, libva-drm and libdrm load; on amd64 and arm64 `encoder: amf` is valid to `validate` and refused by `run` with its reason (exit 1) |

## Phase 4 - Argv and detection (lines D, E)

| # | Item | State |
|---|---|---|
| 4.1 | Every VAAPI device opens with `connection_type=drm` | DONE (PR #126, `0fab196`): `-vaapi_device <node>,connection_type=drm`; QSV gets `-init_hw_device vaapi=hfva:<node>,connection_type=drm -init_hw_device qsv=hfqsv@hfva`; golden argv: 427 VAAPI/QSV lines each the old line plus the device options (checked by script), every other encoder byte-identical; `TestDeviceFor_TheAssignedNodeOrTheFirst` |
| 4.2 | `Available()` probes through the real argv builder, including a 10-bit probe | DONE (PR #126, `0fab196`): `encoder.Available` encodes a lossless 4:2:0 clip at 8 and at 10 bits through `engine.ProbeEncode` (the production encoder and plan derivation) and requires codec and depth; `TestProbeEncode_AvailableProbesThroughTheJobsOwnCommandLine` (vaapi, qsv, nvenc, av1_nvenc, amf argv), `TestProbeEncode_A10BitProbeSeesTheDepthTheUploadCarries` (an 8-bit upload fails the 10-bit probe); the device-opening table test moved behind `hwlive` |
| 4.3 | Device discovery (`/dev/dri/renderD*`, sysfs vendor, a permission error naming `group_add`) | DONE (PR #126, `0fab196`): `internal/hwdevice` (98.6% coverage): `/dev/dri/renderD*`, sysfs vendor, open check; a permission failure names `group_add` / `--group-add` and the GID; VAAPI gets the first usable Intel or AMD node, QSV the first Intel; logged once at start; a refusal carries the node reason |
| 4.4 | `amf` in the image refused at start with a named reason; `validate` still accepts it; never aliased to `vaapi` | DONE (PR #126, `0fab196`; image proof PR #127): `holdfast_image` build tag marks the image's binary; `amf` refused before any probe with the EULA reason; `TestAvailable_AMFInTheImageIsRefusedWithTheReasonAndNeverProbed`, `TestPreflight_AMFInTheImageIsRefusedAtStartWithTheNamedReason`; `validate` accepts `amf`; never resolves to `vaapi` |
| 4.5 | `encoder: auto` choosing per job | DONE (PR #129, `8e29c42`): `encoder: auto` resolves per job at the pixel-format guard to the first of `nvenc`, `qsv`, `vaapi`, `amf` whose probe passed at the plan's depth and whose formats carry it, 4:2:0 plans only; always HEVC; the row records the encoder that ran; `TestResolveEncoder_AutoAndFallbackChoosePerJob` (17 cases), `TestAuto_ChoosesTheUsableHardwareEncoderAndRecordsIt` (a VAAPI stand-in runs the job's own VAAPI argv, every gate passes, the row says `vaapi`), `TestAuto_NoUsableHardwareSkipsThenFallsBackToSoftware`, `TestPreflight_AutoWithNoUsableHardwareFollowsHWFallback` |
| 4.6 | Per-library `hw_fallback: software|skip`, default stated with its reason | DONE (PR #129, `8e29c42`): `hw_fallback: skip|software` per library root, top-level default `skip` (reason in "Decisions taken" and `docs/design/hardware.md#fallback`), digest-silent at `skip` (pinned to the goal-start digest `ab5f38d831b37a30`); `hardware-unavailable` a mutable skip (`mutableGuardSkips`; re-decided with no configuration change by `TestHardwareUnavailable_IsReDecidedWhenOnlyTheHardwareChanges`, PR #130) in the metrics vocabulary and `docs/api-reference.md`; run-time fallback on a failed hardware encode under `software`; `TestHardwareEncodeFailure_FallsBackOnlyUnderSoftware`, `TestPreflight_HWFallbackIsDecidedPerLibraryRoot`, `TestPreflight_ARulesHardwareEncoderFollowsItsRootsHWFallback`, `TestHardware_*`; CI round 1 red on `TestSkipsTotal_UsesOnlyTheClosedVocabulary` (the token missing from `engine.SkipVocabulary`), fixed; mutation-diff 100% (7 killed, 0 lived); gate exit 0 in 1630 s (`internal/engine` 1522.3 s); CI green |
| 4.7 | `docs/docker.md` hardware section and `docs/design/hardware.md` | DONE (PRs #126, #127, #129): `docs/design/hardware.md` (anchors `probe`, `detection`, `amf`, `auto`, `fallback`), linked from `CLAUDE.md` twice (173 lines); `docs/docker.md` GPU passthrough (the runtime, NVIDIA capabilities, `group_add`, `amf`, what the start-time check logs, `auto`, `hw_fallback`); `docs/profiles.md`; `config.example.yaml` |

## Phase 5 - Report

| # | Item | State |
|---|---|---|
| 5.1 | Gate integrity counted from the goal-start SHA | DONE (recounted at `37da90c`): `func Test` 1388 -> 1446, no package fell (`cmd/holdfast` 217 -> 226, `internal/config` 107 -> 123, `internal/encoder` 20 -> 30, `internal/engine` 486 -> 500, `internal/hwdevice` 0 -> 8, `internal/version` 0 -> 1, every other package unchanged); `docs/design/swap.md` 62 -> 62 and `docs/design/quality-gate.md` 76 -> 76 (`git diff --numstat bf36b9c 37da90c` empty for both); 70 lines deleted in `*_test.go` (`git diff --numstat bf36b9c 37da90c`: +2793 -70), each with its reason below |
| 5.2 | Adversarial review of the report | DONE: round 1 (a fresh subagent, 2026-09-30) found lines A-G true ("VERDICT: none false", E PARTLY) and asked for corrections: NEEDS-OWNER row 2's command (a tmpfs `/tmp` and a non-root image would refuse the start; fixed in `7f31678`: `$HOME/hf-probe`, `-u`); `holdfast plan` not probing under `auto`, a named alias recorded under its key, the I5 over-claim, and the mutable-skip test changing the configuration (all fixed by PR #130, `ef6b611`); row 2.1's AC-15 wording (fixed); main's CI on `8e29c42` cancelled by the next push (as on `0fab196`, `84198b7`, `ef6b611` and `37da90c`: a push to `main` cancels the run in progress), each code commit's content then green on the ledger-only commit that followed (`7f31678` run 36775905713; the final code, `37da90c`, green on `a5dc965` run 36783302123); 7 added lines carry em dashes, each carried over from an edited or moved pre-existing line (T34 leaves existing dashes). A fresh subagent checks the report and the repositories after this commit; its verdict is the report's line H |
| 5.3 | Corrections from the final review | DONE (PR #131, `37da90c`): a fresh subagent's review of the completed ledger found lines A-G true ("VERDICT: none false") and one defect #127 introduced - four lines of `scripts/check-pins-selftest.sh`'s header comment had lost their `#` and ran as commands in every gate since - and a probe softness (an encode that errored but left a faithful file passed); both fixed, `TestAvailable_AnEncodeThatFailsIsUnavailableEvenWithAGoodOutput`; note that the `func Test` count includes `TestHardwareEncoders_AvailabilityTable`, which sits behind `hwlive` and never runs in the gate or CI; mutation-diff 100% (2 killed); gate exit 0 in 1687 s (`internal/engine` 1580.5 s), 0 "command not found" lines; CI green. A fresh subagent then checked the final state (`a5dc965`) and found lines A-G true ("VERDICT: none false"); its two wording corrections are applied here; its verdict is the report's line H |

### The 70 deleted `*_test.go` lines and why

- `internal/engine/encoder_matrix_test.go` (56, PR #126): the hardware half of the codec matrix -
  its section banner, `TestHardwareEncoders_AvailabilityTable` and the package comment's 4 lines
  about it, plus the `encoder` import only it used - moved whole to
  `internal/engine/encoder_matrix_hwlive_test.go` behind `//go:build hwlive`, because in this
  container it could run a real NVENC encode inside the gate (T9). Only its two probe lines
  changed there, for `Available`'s new signature. `func Test` count unchanged (the test is still
  in the package's files).
- `internal/encoder/encoder_test.go` (8, PR #126): the five `Available`/`RequireAvailable` call
  sites and three of their messages, for the new signature (an `EncodeFunc`, a `Capability`
  return); each assertion kept and made stricter (usable at both depths).
- `internal/config/rules_test.go` (2, PR #128): `TestRules_AnUnknownRuleKeyIsRefusedAgainstTheClosedEnumeration`
  used `encoder: svtav1` as its unknown key, which S0165 makes a rule knob; the fixture became
  `min_vmaf: 90` (still refused) and its `mentions` line followed; the test and its assertions
  are kept (as the spec's T1 says).
- `cmd/holdfast/happy_path_log_test.go` (2) and `cmd/holdfast/sourceoffer_test.go` (2), PR #126:
  one call line and one import each, now calling `requireEncoder`, the function `buildEngine`
  uses, as those tests require ("through the SAME functions buildEngine requires them with").
- Not in the net count: PR #130 changed 1 line of `internal/engine/hardware_test.go`, a file #129
  added inside this goal - the alias case of `TestResolveEncoder_AutoAndFallbackChoosePerJob`
  now expects the alias as written (`hevc_vaapi`), and a case for an alias that falls back was
  added; no assertion removed.

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
  decisions and argv (I5) - under `skip` a named hardware encoder that does not work at any
  depth refuses the start exactly as before (one that passes only the 8-bit probe now starts and
  skips 10-bit plans before encoding, where it used to attempt them and be rejected by the
  fidelity gate - a change in the fail-safe direction, stated in the design record); a silent software encode can cost many times the wall-clock on a
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

- 2026-09-30: PR #126 gate exit 0 in 1568 s (`internal/engine` 1456.0 s, `cmd/holdfast` 575.4 s);
  CI green; merged while `main` was one ledger-only commit ahead (`646f016`), as §0.3 allows.
- 2026-09-30: finding. PR #127's first gate went red (exit 2, 2787 s): `internal/engine` hit its
  45-minute timeout because the ffmpeg child of one libx265 encode
  (`TestObserver_EmitsTransitionsAndReclaimedBytesOnSwap`, a plain `cpu` argv byte-identical to
  `main`) hung for 30 minutes at 0% CPU with all 81 threads in `futex_wait` - a libx265
  thread-pool deadlock in the pinned ffmpeg, not a change of this goal (#127 changes no Go code).
  The orphan was killed and the gate re-run unchanged (round 2: exit 0 in 1743 s). The same hang
  in production would hold a worker forever: holdfast has a memory watchdog but no stall
  watchdog on an encode. Listed for the owner under "Proposals awaiting the owner".
- 2026-09-30: PR #127 also gained the image smoke step for the `amf` refusal (both halves were on
  `main` by then); CI `package` green on amd64 and arm64.
- 2026-09-30: PR #128 gate exit 0 in 1661 s after its merge-up and the rule walk moved onto
  `requireEncoder`; merged. PR #129 brought up to `main` twice (each merge's conflicts were only
  the files both sides added, resolved to the branch side after checking the squash commit equals
  the merged branch's content), the rule walk routed through `encoderChecks.require`, and a
  duplicate rule walk the merge re-added dropped; gate exit 0 in 1630 s; merged `8e29c42`.
- 2026-09-30: a rule's encoder follows the `hw_fallback` of the roots whose rules name it,
  `software` only where every such root says `software` (the rule walk names each encoder once);
  one root that skips keeps the start-time refusal for all. Fail-safe direction.
- 2026-09-30: PR #130 (the review's corrections): gate exit 0 in 1627 s (`internal/engine`
  1518.6 s, 56% of `TEST_TIMEOUT` 45m, `cmd/holdfast` 630.0 s); CI green; merged `ef6b611` with
  `main` one ledger-only commit ahead. `holdfast plan` now probes the encoders `auto` may choose
  where the configuration reaches `auto`, so it agrees with `run`.
- 2026-09-30: no minor release is cut at the end of this goal (T37 makes it optional): the image
  change is large (about 244 MiB of vendor runtime) and no real hardware has run it yet; the
  owner's NEEDS-OWNER row 2 is the first real-hardware evidence, and a later goal can release on it.
- 2026-09-30: `NEEDS-OWNER.md` row 2 added: the start-time hardware probe on each GPU host,
  through an image built from `main`, with the exact commands and what each answer changes.

## NEEDS-OWNER (this goal)

- Row 2 of `.claude/goals/NEEDS-OWNER.md`: run holdfast's start-time probe (`encoder: auto`,
  `hw_fallback: software`, an empty library) on the Intel, AMD and NVIDIA hosts and paste back the
  `hardware:` lines. A real GPU run is physically impossible here under T9.

## Proposals awaiting the owner

- A stall watchdog for encodes: a libx265 encode deadlocked for 30 minutes at 0% CPU in this
  goal's gate (see "Decisions taken"); production has no bound on an encode that stops making
  progress. Not built here (outside §9); the progress stream (`-progress pipe:3`) already carries
  what such a watchdog would read.

## Resume here

Goal 5 is complete. Merged: #126 (argv, probe, detection, `amf` refusal), #127 (the image runtime),
#128 (S0165), #129 (`encoder: auto`, `hw_fallback`), #130 and #131 (the reviews' corrections). No branch,
worktree or open PR of this goal remains. Goal 6's precondition is this ledger's COMPLETE line; its
first input is `NEEDS-OWNER.md` row 2 (the start-time probe on the owner's hosts).

COMPLETE (goal 5): 2026-09-30
