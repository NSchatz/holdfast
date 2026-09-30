# Goal 4 ledger - the fidelity gate and per-encoder quality

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §8; goal file
  `.claude/goals/2026-09-holdfast-g4.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`
  (P2 approved, option (a)).
- Started: 2026-09-30, from `/workspace/holdfast` in the maker container.
- Goal-start SHA: `9bde81d193fd1f2f9fd87ac2a67db40aeea9dc65` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md`, `2026-09-holdfast-g2.status.md`,
  `2026-09-holdfast-g3.status.md` (ends `COMPLETE (goal 3): 2026-09-30`). `NEEDS-OWNER.md` at
  start: row 1 (merge NSchatz/homelab#208), `OPEN`.
- Precondition, as checked:

```
$ git pull --rebase
Already up to date.
$ git log -1 --oneline origin/main
9bde81d chore(goals): goal 3 ledger - re-run: #118 gated and merged, stale branch removed, TMPDIR finding, #119 trailer recorded
$ git show origin/main:.claude/goals/2026-09-holdfast-g3.status.md | command grep -n 'COMPLETE (goal 3)'
249:COMPLETE (goal 3): 2026-09-30
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `9bde81d`, "Per goal"):
"**Goal 4** (fidelity gate and per-encoder quality): S0162." S0162 is `vmaf-log-off-tmpfs`: the
per-frame VMAF log is written to the process temp dir and read whole into memory; the fix must
keep the pooled scores bit-identical. `CHECKPOINT-T.approved` approves P1 as proposed and amends
no goal-4 row. P2 is approved as option (a): the same gates and floors for every encoder, plus
per-encoder quality keys and an additive output fidelity gate, built in this goal.

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g4-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g4/tmp` (goal 3's finding: the `/scratch` tmpfs reds `TestWatch_*`).
Logs under `/cache/tmp/holdfast-g4/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (log `gate-baseline.log`) | exit 0 | 1429 s (23m49s) |
| `internal/engine` under `go test -race` (from that run) | ok, 87.5% coverage | 1330.3 s (49% of `TEST_TIMEOUT` 45m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 89.4% coverage | 461.0 s |
| `func Test` count, all packages | 1330 in 25 packages (table below) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 58, 76 | - |

`func Test` per package at the goal-start SHA (`git grep -c '^func Test' <sha> -- '*_test.go'`,
summed per directory):

| Package | Count | Package | Count |
|---|---|---|---|
| `cmd/holdfast` | 217 | `internal/metrics` | 19 |
| `internal/config` | 99 | `internal/notify` | 10 |
| `internal/corpus` | 3 | `internal/probe` | 17 |
| `internal/cpuquota` | 10 | `internal/schedule` | 13 |
| `internal/diskfree` | 4 | `internal/secret` | 13 |
| `internal/docscheck` | 29 | `internal/secretscan` | 10 |
| `internal/encoder` | 12 | `internal/server` | 105 |
| `internal/engine` | 467 | `internal/sourceoffer` | 9 |
| `internal/fsclass` | 6 | `internal/startup` | 80 |
| `internal/hdr` | 10 | `internal/store` | 149 |
| `internal/heapmeasure` | 5 | `internal/vmaf` | 30 |
| `internal/logging` | 5 | `scripts/mutation-gate` | 5 |
| `internal/memlimit` | 3 | | |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`9bde81d`): goal 3's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`eab9418`) |
| 1.3 | Baselines with timings | DONE (`9bde81d`): the table above; `make check` exit 0 in 23m49s |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | S0162 `vmaf-log-off-tmpfs` | DONE (PR #121, `169583b`): stream route - libvmaf writes its JSON log to a pipe handed to ffmpeg as fd 3 (`log_path=/proc/self/fd/3`) and `decodeLog` streams it token by token, keeping only the four pooled statistics; no log file exists, so AC-9's second half and AC-10 do not apply; tests AC-1..AC-9 in `internal/vmaf/logpipe_s0162_test.go` and AC-8 in `internal/engine/verify_test.go`, with the F1 control (whole-file decode of a 200,000-frame log +154 MB vs +14 KB streamed) and the F2 control (10,181,551 bytes under `$TMPDIR` the old way vs 0); AC-4 bit-identical against the pinned ffmpeg; `docker-compose.yml` comment corrected; gate exit 0 in 1765 s (`internal/engine` 1469.6 s); CI green; 0 test lines deleted |

## Phase 3 - The fidelity gate (line C, foundation)

| # | Item | State |
|---|---|---|
| 3.1 | Output fidelity gate: bit depth, chroma subsampling, primaries, transfer, matrix, range, HDR10 mastering and content-light side data equal the source's or what the plan declares it changes | DONE (PR #120, `41c44bd`): `hdr.Fidelity` declared on the plan (`MetadataPlan.Fidelity`), gate 5b `GateFidelity` in `verifyAgainst`, output read by `probe.OutputFacts` at stream and first-frame level; round 1 red (5 engine tests: probe ceiling, pre-plan adapter, an 8-bit stand-in), fixed in `5838884`; round 2 exit 0 on `73bcfaa` (`internal/engine` 1422.9 s, `cmd/holdfast` 453.9 s; wall-clock 55m36s incl. about 22 min waiting for the lock); mutation-diff 100% of covered mutants (39 killed, 0 lived, 2 not covered); CI green |
| 3.2 | One fixture per field reds when that field is lost; the source is byte-identical afterwards | DONE (PR #120, `41c44bd`): `TestFidelityGate_RedsWhen{BitDepth,ChromaSubsampling,Transfer,Matrix,Range,MasteringDisplay,ContentLightLevel}IsLost`, `TestFidelityGate_RedsWhenPrimariesAreLost` and `TestFidelityGate_RedsAFakeHardwareEncodeThatWrites8Bit`, with three passing controls |

## Phase 4 - Explicit pixel formats and per-encoder quality (line D)

| # | Item | State |
|---|---|---|
| 4.1 | Every argv sets its pixel format explicitly (golden argv) | DONE (PR #122, `90b8ed8`): `VideoPlan.InputFormat` from `encoder.Spec.InputFormat` (the plan's format where the encoder lists it, else the planar or semi-planar spelling of the same chroma and depth, else refused); `-pix_fmt` for every encoder but VAAPI, which uploads `format=nv12|p010le,hwupload` (`-profile:v main10` for p010le) and gets no `-pix_fmt`; the engine's `exotic-pixel-format` guard also skips a plan the encoder cannot carry (inputs `pixel_format`, `encoder`); `TestGoldenArgv_EveryArgvNamesItsPixelFormat`; recorded argv change in the hardware and forced-`nv12` goldens only; gate exit 0 in 33m27s on `a5cb382` (`internal/engine` 1433.7 s); mutation-diff 100% of covered mutants (17 killed, 0 lived, 10 not covered: constant string joins in the format and scale tables); CI green |
| 4.2 | Per-encoder quality keys replace the CRF reused as `-cq`/`-global_quality`/`-qp`; unmeasured defaults marked `ASSUMED` | DONE (PR #122, `90b8ed8`): top-level `quality.<key>` for `nvenc` (-cq 1-51), `av1_nvenc` (-cq 1-63), `qsv` (-global_quality 1-51), `vaapi` (-qp 1-52), `amf` (-qp_i/-qp_p 0-51); `crf` stays for `cpu` and `svtav1`; an absent key inherits the job's crf (byte-identical argv), marked `ASSUMED` in `internal/encoder/quality.go` pending a hardware report; `HOLDFAST_QUALITY_<KEY>` supported; values off an encoder's scale refused by `validate` and at plan derivation |

## Phase 4b - The declared colour tags reach every encoder (found by the fidelity gate)

| # | Item | State |
|---|---|---|
| 4b.1 | Stamp the declared primaries and transfer onto the frames (`setparams`) for every encoder but libx265 | DONE (PR #123, `3dc9132`): `hdr.Color.SetParams` at the head of the video chain, before any hwupload; 156 golden lines of the non-libx265 encoders each the old line plus the filter (checked by script), no `cpu` line moved; `TestFidelityGate_EveryEncoderCarriesTheDeclaredPrimariesAndTransfer` red for svtav1 without it; gate exit 0 in 26m54s (`internal/engine` 1513.6 s, 56% of `TEST_TIMEOUT` 45m); mutation-diff 100% (6 killed, 0 lived, 0 not covered); CI green |

## Phase 5 - Design record (line E)

| # | Item | State |
|---|---|---|
| 5.1 | `docs/design/encode-plan.md` gains the fidelity anchor; `CLAUDE.md` links it | DONE (PR #120, `41c44bd`): anchor `fidelity`; `CLAUDE.md` Design rationale links `docs/design/encode-plan.md#fidelity`; `docs/design/swap.md` +4 lines, none removed |

## Phase 5b - Corrections from the report's review

| # | Item | State |
|---|---|---|
| 5b.1 | The `videoArgs` comment's svtav1 claims and `CLAUDE.md`'s invariant gate list | DONE (PR #124, `bb66d7e`): comments and docs only; `CLAUDE.md` 167 lines; gate exit 0 in 26m25s on `d8622a0` (`internal/engine` 1476.8 s); CI green |
| 5b.2 | Round 2: an unread source side data must be refused, not declared absent (a fail-open the review found); two older `encode.go` comments | DONE (PR #125, `4e0488f`): `probe.VideoProps.SideDataAnswered`; `deriveEncodePlan` refuses a re-encode whose source side data went unread; `TestFidelityGate_AnUnreadSourceSideDataIsRefusedNotDeclaredAbsent` records `done` (the lossy output swapped in) without the refusal and a failed job with the source byte-identical with it; `TestVideoProps_SideDataAnswered`; 0 test lines deleted; gate exit 0 in 27m35s on `3f265f5` (`internal/engine` 1554.9 s, 58% of `TEST_TIMEOUT` 45m); mutation-diff 100% (2 killed, 0 not covered); CI green |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | DONE (recounted at `4e0488f`): `func Test` 1330 -> 1388, no package fell (config 99 -> 107, encoder 12 -> 20, engine 467 -> 486, hdr 10 -> 20, probe 17 -> 21, vmaf 30 -> 39, every other package unchanged); `docs/design/swap.md` 58 -> 62 (+4 -0), `docs/design/quality-gate.md` 76 -> 76 (+0 -0); 15 lines deleted in `*_test.go` (`git diff --numstat 9bde81d origin/main`: +2634 -15), each with its reason below |
| 6.2 | Adversarial review of the report | DONE: round 1 (a fresh subagent, 2026-09-30) found lines A-G true on the repositories and asked for corrections: the 5 in-goal deleted lines of `fidelity_gate_test.go` (added above), the not-covered mutant counts (added to rows 3.1, 4.1, 4b.1), stale local remote-tracking refs (pruned with `git fetch --prune`), the `videoArgs` comment still crediting `-color_*` with svtav1's primaries and transfer, and `CLAUDE.md`'s invariant gate list lacking the fidelity gate (both fixed by PR #124). Round 2 (a fresh subagent) found lines A-G true ("none false") and three things to correct: row 3.2's brace-expanded test name (the primaries test is `RedsWhenPrimariesAreLost`; corrected), two older `encode.go` comments, and a fail-open - an unread source side data declared as no HDR10 block - fixed by PR #125 (row 5b.2). A fresh subagent checks the report and the repositories after this commit; its verdict is the report's line H |

### The 15 deleted `*_test.go` lines and why

- `internal/engine/encode_bitrate_test.go` (8, PR #122): five `pinArgs` lines that pinned
  `-pix_fmt yuv420p10le` for the hardware encoders and one that pinned VAAPI's
  `-vf format=nv12,hwupload` - the recorded argv change of the explicit pixel format (now
  `p010le`, and for VAAPI `format=p010le,hwupload` with Main 10 and no `-pix_fmt`); the
  bitrate-path check `hasArgPair(got, "-pix_fmt", "yuv420p10le")` and its comment line, replaced
  by `hasPixelFormatOf(got, pinArgs[key])`, which requires the exact explicit format for every
  encoder and refuses any other `-pix_fmt` (stricter; the old one accepted VAAPI's contradictory
  `-pix_fmt`).
- `internal/engine/encodeplan_adapters_test.go` (4): the `buildArgs` adapter body and its comment
  line now resolve the input format and quality through the production functions (#122); the
  `verifyOutput` adapter's `return e.verifyAgainst(ctx, &EncodePlan{` and `})` became a
  `job :=` literal plus a fidelity declaration made as `deriveEncodePlan` would (#120).
- `internal/engine/engine_test.go` (2, PR #120): the probe-budget constant 11 -> 13 and the
  comment's figure, moved in the commit that moved the cost, as that test requires.
- `internal/engine/profiles_test.go` (1, PR #120): a stand-in encoder's `-pix_fmt yuv420p` became
  `yuv420p10le`, the format its job's plan declares; the fidelity gate caught the 8-bit output,
  and the assertion is unchanged.
- Not in the net count: PR #122 deleted 5 lines of `internal/engine/fidelity_gate_test.go`, a
  file #120 added inside this goal - a fixture refactor (`mkFidelitySourceAs`,
  `fidelityRunFrom`) and the fake-hardware stand-in moved from 8-bit 4:2:2 to 8-bit 4:2:0 on a
  4:2:0 10-bit source, because #122's guard skips a 4:2:2 plan on VAAPI before any encode (now
  its own subtest); no assertion removed. Found by the report's adversarial review.

## Decisions taken

- 2026-09-30: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g4/` (goal 3's
  finding: `TMPDIR=/scratch` is a tmpfs `fsclass` refuses, which reds `TestWatch_*`). Reasoning:
  goal 3's ledger, "Decisions taken"; no code or test changes for it.
- 2026-09-30: the shell's `grep` is a function that runs the Claude CLI binary, which is not
  installed here, so every `grep` prints an install error; goal shells use `command grep`, `rg`
  or `git grep` (brief §0.11 names this).

- 2026-09-30: S0162 takes the spec's stream route (no log directory), chosen by the lead: a
  pipe needs no new configuration key, directory validation or startup sweep, and leaves
  nothing on disk after a killed run. Reasoning: PR #121's body and the `vmaf.go` section
  "Where the log goes".
- 2026-09-30: finding (measured with the pinned `N-125875-g5d4d3bdc61`): `-color_primaries`
  and `-color_trc` do not reach an encode's output; the encoder takes those two tags from the
  decoded frames (the frames win where they carry them; a Matroska output's container elements
  stay unset where they do not). `-colorspace` and `-color_range` do take effect, and libx265
  writes all three tags into the bitstream from `-x265-params`. So the fidelity gate reads the
  tags at both levels (stream and first decoded frame) and a declared tag must be signalled at
  some level and contradicted at none. Reasoning: `docs/design/encode-plan.md#fidelity`.
- 2026-09-30: consequence of that finding: an `svtav1` (or hardware) encode of an HDR10 source
  whose bitstream under-signals primaries and transfer (the plan declares the HDR10 defaults)
  writes PQ samples with no primaries or transfer tag at all - a silent loss on `main` before
  this goal. The fidelity gate now rejects it and keeps the source. `-vf setparams=...` was
  measured to carry the tags; the argv fix is its own track after the pixel-format track
  merges (it moves golden argv).
- 2026-09-30: the output is read in two ffprobe runs (`probe.OutputFacts`), not the four the
  first draft used; the per-job probe ceiling in `TestProbeBudget_ProgressAddsNoSubprocess`
  moves 11 -> 13 in the commit that moved the cost, as that test's own comment requires (the
  precedent is S0089's 10 -> 11). Two test lines change for it (the constant and the comment's
  figure); no assertion is removed.
- 2026-09-30: a zero fidelity declaration is refused by the gate, never read as "nothing to
  check": a plan the derivation did not make would otherwise be a fail-open path. The pre-plan
  `verifyOutput` test adapter therefore declares what `deriveEncodePlan` would
  (`adapterFidelity`).

## NEEDS-OWNER (this goal)

None added. The physical steps this goal's work points at - calibrating the `ASSUMED`
per-encoder quality defaults and confirming the explicit hardware formats (above all VAAPI's
`format=p010le,hwupload`) on real hardware - are the per-encoder hardware reports that P2's
test plan and brief §10 assign to goal 6's `scripts/hw-report.sh` (T43); that script does not
exist yet, so there is no exact command to write (§0.6). Goal 6 adds those rows. Row 1 (merge
NSchatz/homelab#208) stays `OPEN`; the PR is still open (checked 2026-09-30).

## Proposals awaiting the owner

- Follow-up (PR #122): terminal rows record `crf`, not `quality.<key>`, as a decision input, so
  changing a per-encoder quality key does not re-open rows decided under the old value (the
  `requeue` lever still does). Fail-safe: nothing is re-encoded unasked.
- Follow-up (PR #122): `holdfast validate` does not statically refuse `crf: 0` inherited by a
  hardware encoder whose scale excludes 0 (`nvenc`, `av1_nvenc`, `qsv`, `vaapi`); such a job
  fails at plan derivation with a message naming `quality.<key>`, before anything runs.

## Resume here

Goal 4 is complete. Merged: #121 (S0162), #120 (the fidelity gate), #122 (explicit pixel formats
and quality keys), #123 (the declared colour tags reach every encoder), #124 and #125 (the
review rounds' corrections). No branch, worktree or open PR of this goal remains. Goal 5's
precondition is this ledger's COMPLETE line.

COMPLETE (goal 4): 2026-09-30
