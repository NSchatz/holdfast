# Goal 3 ledger - the encode plan

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §7; goal file
  `.claude/goals/2026-09-holdfast-g3.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`.
- Started: 2026-09-30.
- Goal-start SHA: `9e27c1a9311c9c8db3ac29626e9e92336c02afbf` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` (ends `COMPLETE (goal 1): 2026-09-29`) and
  `2026-09-holdfast-g2.status.md` (ends `COMPLETE (goal 2): 2026-09-29`). `NEEDS-OWNER.md` at
  start: row 1 (merge NSchatz/homelab#208, goal 2's S0176 `TZ` half), `OPEN`.
- Precondition, as checked:

```
$ git -C /workspace pull --rebase
Already up to date.
$ git log -1 --oneline origin/main
9e27c1a chore(goals): goal 2 ledger - gate-integrity count corrected to 93 deleted test lines (review finding), COMPLETE (goal 2)
$ git show origin/main:.claude/goals/2026-09-holdfast-g2.status.md | grep -n 'COMPLETE (goal 2)'
244:COMPLETE (goal 2): 2026-09-29
$ echo "$GOFLAGS $GOMAXPROCS"
-p=2 2
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `9e27c1a`, "Per goal"):
"**Goal 3** (encode plan): none." `CHECKPOINT-T.approved` approves P1 as proposed and amends no
goal-3 row. So this goal's work is §7 items 2-4 only: the encode plan, its golden argv tests
written before the refactor, and `docs/design/encode-plan.md` linked from `CLAUDE.md`.

## Baselines

Measured at the goal-start SHA in this container (2 CPUs by cgroup quota). Logs under
`/cache/tmp/holdfast-g3/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (detached worktree at `9e27c1a`; log `gate-baseline.log`) | exit 0 | 1764 s (29m24s) |
| `internal/engine` under `go test -race` (from that run) | ok, 87.1% coverage | 1513.4 s (56% of `TEST_TIMEOUT` 45m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 89.4% coverage | 589.6 s |
| `func Test` count, all packages | 1322 in 25 packages (table below) | - |
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
| `internal/engine` | 460 | `internal/sourceoffer` | 9 |
| `internal/fsclass` | 6 | `internal/startup` | 80 |
| `internal/hdr` | 9 | `internal/store` | 149 |
| `internal/heapmeasure` | 5 | `internal/vmaf` | 30 |
| `internal/logging` | 5 | `scripts/mutation-gate` | 5 |
| `internal/memlimit` | 3 | | |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`9e27c1a`): goal 2's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`716b789`) |
| 1.3 | Baselines with timings | DONE (`9e27c1a`): the table above; `make check` exit 0 in 29m24s |

## Phase 2 - Golden argv, before the refactor (line C, foundation)

| # | Item | State |
|---|---|---|
| 2.1 | Golden argv tests for every registry encoder and every option combination the existing fixtures use, merged BEFORE any refactor commit | DOING (PR #113): 1611 cases in 15 files under `internal/engine/testdata/golden-argv` (encoder layer 202 per registry encoder plus 3 alias cases each, and 4 that name no encoder; engine layer 76 for `cpu` and 16 core for each other encoder); compare runs after each writing run passed; local gate exit 0 in 1642 s on `62f1824` (`internal/engine` 1511.2 s, `cmd/holdfast` 537.7 s); CI green on `62f1824`, re-running on `38966f7` (a ledger-only merge of #114) |

## Phase 3 - The encode plan (line C, foundation)

| # | Item | State |
|---|---|---|
| 3.1 | One declared plan per job (video encoder and device, decode path, pixel format, quality; audio, subtitle and picture operations; metadata carriers) | TODO |
| 3.2 | The argv builder reads the plan; the golden argv tests pass unchanged | TODO |
| 3.3 | Every gate reads the plan; no existing engine test assertion removed or relaxed | TODO |

## Phase 4 - Design record (line D)

| # | Item | State |
|---|---|---|
| 4.1 | `docs/design/encode-plan.md` with the plan's anchor; `CLAUDE.md` links it by rule | TODO |

## Phase 5 - Report

| # | Item | State |
|---|---|---|
| 5.1 | Gate integrity counted from the goal-start SHA | TODO |
| 5.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-30: the goal-start baseline gate runs in a detached worktree
  (`/cache/wt/holdfast/g3-baseline`) so edits in `/workspace` cannot touch what it measures.
  Reasoning: §0.1 step 5 and §0.4 (worktrees for anything that writes).

- 2026-09-30: what "every option combination the existing fixtures use" covers. A read-only
  census of every engine and cmd test that drives the real encoder (or the per-family builder)
  listed each argv input's values and their co-occurrences; the golden cases take all of them,
  plus combinations no test drove but the refactor touches (deinterlace with `max_height`,
  filters under VAAPI, HDR10 or a filter with a bitrate target, PQ, HLG and full-range
  sources). The ENCODER layer runs every case for every registry encoder (202 each, plus 3
  through the raw-codec alias). The ENGINE layer (a whole pass per case) runs every case for the default
  encoder and 16 core cases - those whose resolution meets an encoder-specific part of the
  command line - for every registry encoder. Reasoning: a pass costs about 0.4 s under the race
  detector, all cases for all 7 encoders would add about 4 minutes to every gate, and the
  per-encoder shape of the other combinations is built by the encoder layer from the same
  profile, map and snapshot the engine hands over. The test's header says the same.
- 2026-09-30: the engine layer shares one ledger across its cases (each case's library is its
  own directory, so no row names another case's source); a store's opening costs about 0.6 s
  under `-race` (measured: store 0.56-0.62 s, a pass 0.35 s, four probes 0.24 s).
- 2026-09-30: the pinned build's libx264 writes only the matrix of `-color_primaries`,
  `-color_trc` and `-colorspace` into the stream (primaries and transfer probe as `unknown`,
  checked with ffprobe), so the PQ, HLG and fully tagged bt709 fixtures write their colour
  description with the `h264_metadata` bitstream filter into the VUI. The existing bt709
  fixture's shape (matrix only) is kept as its own case.
- 2026-09-30: finding, NOT fixed (this goal is behaviour-preserving): every MPEG-TS/M2TS
  source is skipped as `multi-video-stream`. `ffprobe -select_streams v -show_entries
  stream=index:stream_disposition=attached_pic -of csv=p=0` prints `0`, an empty line, then
  `0,0` for a `.ts` file (a program section precedes the stream), and `probe.VideoStreams`
  refuses the comma-less line, so the shape is never established. Fail-safe (skipped, never
  harmed), but `ts` and `m2ts` are default `video_exts`. The golden files pin today's
  behaviour; listed for the owner and a later goal in the GOAL REPORT.
- 2026-09-30: `deinterlace: yadif=send_field` is not an engine-layer case: `config.Validate`
  refuses it at load, so no validated configuration reaches an engine with it. The encoder
  layer grades the backstop refusal.

- 2026-09-30: a read-only adversarial review of the refactor draft (a subagent, 18 min) found
  no difference in any command line, in any refusal's text or order on the direct path, or in
  `DeriveColorArgsFrom`'s output, and no concurrency hazard. Its findings and what was done:
  (1) a remux-only job under a `max_height` below its source recorded `downscaled: true` and a
  scaler for a copy that scaled nothing, and the plan makes it record false - kept as a
  deliberate correction of a false record (not a decision, not a command line), held by a new
  test and stated in `docs/design/encode-plan.md`; the older guard that skips such a remux as
  if it would scale is unchanged and listed as a follow-up; (2) an encoder handed a plan
  ignores its own `Cfg` - documented (production shares one configuration); (3) a wrapper
  that forwards `ForProfile`/`ForStreamPlan` but not `ForEncodePlan` leaves its encoder to
  derive its own plan - documented on the interface, no existing test changed; (4) the design
  record over-claimed (the row, the settings, attached pictures) and `TranscodeIn` ran twice
  per job - the derivation now takes the job's resolved settings and the record was corrected;
  (5) two refusals were not pinned by the goldens - 38 cases added to PR #113 before it merged
  (every earlier golden line byte-identical), and the gate started on its first commit was
  stopped; (6)-(8) edges no running holdfast reaches (a stand-in encoder no longer called for
  an underivable plan, no prober needed with a plan, the encode seam not announced for an
  underivable plan) - documented; (9) `buildable` now refuses a picture operation on a copy
  and a plan no derivation made, and `SameEncodePlan` no longer equates two underived plans;
  (10) stale comments refreshed.
- 2026-09-30: `main` moved by the owner's PR #114 (`4c9143f`, authored and merged by the owner's
  account from another session): `.claude/goals/` only (goal files 4-15 and brief §0.1/§0.11 for
  the maker container; it says goal 3 runs in the standalone container, where `/workspace` is
  right). Ledger-only under §0.3, so #113's gate on `62f1824` stands; the branch was brought up
  to date by merge (`38966f7`) and CI re-run. Its squash message carries a `Co-authored-by`
  trailer, so `git log 9e27c1a..origin/main --format=%B` counts one this goal did not write.
  No goal may rewrite history (T7), so it stays; the GOAL REPORT counts this goal's own commits
  separately and lists it for the owner.

## NEEDS-OWNER (this goal)

None so far. `NEEDS-OWNER.md` row 1 is goal 2's and stays `OPEN`.

## Resume here

Phase 2: PR #113 (`holdfast-g3/golden-argv`, worktree `/cache/wt/holdfast/holdfast-g3-golden-argv`)
carries the golden argv; its local gate runs (log `/cache/tmp/holdfast-g3/gate-golden.log`), then
CI, then merge. The refactor is drafted in `/cache/wt/holdfast/holdfast-g3-encode-plan-wip`
(local branch, never pushed); it becomes `holdfast-g3/encode-plan` from `origin/main` once #113
has merged, so no refactor commit precedes the golden files on `main`.
