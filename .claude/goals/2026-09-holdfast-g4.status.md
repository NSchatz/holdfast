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
| `make check` under `flock -o` (log `gate-baseline.log`) | running | - |
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
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DOING |
| 1.3 | Baselines with timings | DOING |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | S0162 `vmaf-log-off-tmpfs` | TODO |

## Phase 3 - The fidelity gate (line C, foundation)

| # | Item | State |
|---|---|---|
| 3.1 | Output fidelity gate: bit depth, chroma subsampling, primaries, transfer, matrix, range, HDR10 mastering and content-light side data equal the source's or what the plan declares it changes | TODO |
| 3.2 | One fixture per field reds when that field is lost; the source is byte-identical afterwards | TODO |

## Phase 4 - Explicit pixel formats and per-encoder quality (line D)

| # | Item | State |
|---|---|---|
| 4.1 | Every argv sets its pixel format explicitly (golden argv) | TODO |
| 4.2 | Per-encoder quality keys replace the CRF reused as `-cq`/`-global_quality`/`-qp`; unmeasured defaults marked `ASSUMED` | TODO |

## Phase 5 - Design record (line E)

| # | Item | State |
|---|---|---|
| 5.1 | `docs/design/encode-plan.md` gains the fidelity anchor; `CLAUDE.md` links it | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-30: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g4/` (goal 3's
  finding: `TMPDIR=/scratch` is a tmpfs `fsclass` refuses, which reds `TestWatch_*`). Reasoning:
  goal 3's ledger, "Decisions taken"; no code or test changes for it.
- 2026-09-30: the shell's `grep` is a function that runs the Claude CLI binary, which is not
  installed here, so every `grep` prints an install error; goal shells use `command grep`, `rg`
  or `git grep` (brief §0.11 names this).

## NEEDS-OWNER (this goal)

None yet.

## Resume here

Phase 1: the baseline gate is running in `/cache/wt/holdfast/g4-baseline`. Next: read the S0162
spec (super, read-only), the plan, the gates and the encoder; plan the tracks.
