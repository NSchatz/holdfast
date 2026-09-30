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
| `make check` under `flock -o` (detached worktree at `9e27c1a`; log `gate-baseline.log`) | running | - |
| `internal/engine` under `go test -race` (from that run) | running | - |
| `cmd/holdfast` under `go test -race` (from that run) | running | - |
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
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DOING |
| 1.3 | Baselines with timings | DOING: the gate runs in `/cache/wt/holdfast/g3-baseline` |

## Phase 2 - Golden argv, before the refactor (line C, foundation)

| # | Item | State |
|---|---|---|
| 2.1 | Golden argv tests for every registry encoder and every option combination the existing fixtures use, merged BEFORE any refactor commit | TODO |

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

## NEEDS-OWNER (this goal)

None so far. `NEEDS-OWNER.md` row 1 is goal 2's and stays `OPEN`.

## Resume here

Start-up. The baseline gate runs in `/cache/wt/holdfast/g3-baseline` (log
`/cache/tmp/holdfast-g3/gate-baseline.log`). Next: the golden argv PR (Phase 2), merged before
any refactor commit.
