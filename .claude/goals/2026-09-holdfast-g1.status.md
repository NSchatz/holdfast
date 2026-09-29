# Goal 1 ledger - start-up, triage and proposals

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §5; goal file
  `.claude/goals/2026-09-holdfast-g1.goal.txt`. Ends at Checkpoint T (§20); this goal never writes
  `CHECKPOINT-T.approved`.
- Started: 2026-09-29.
- Goal-start SHA: `4ac983a24061d76baf8699a6f49b689341098ef0` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: none (this is goal 1). `NEEDS-OWNER.md` at start: header only, no rows.
- Precondition, as checked:

```
$ git ls-tree --name-only origin/main .claude/goals/
.claude/goals/2026-09-holdfast-g1.goal.txt
.claude/goals/2026-09-holdfast-g10.goal.txt
.claude/goals/2026-09-holdfast-g11.goal.txt
.claude/goals/2026-09-holdfast-g12.goal.txt
.claude/goals/2026-09-holdfast-g13.goal.txt
.claude/goals/2026-09-holdfast-g14.goal.txt
.claude/goals/2026-09-holdfast-g15.goal.txt
.claude/goals/2026-09-holdfast-g2.goal.txt
.claude/goals/2026-09-holdfast-g3.goal.txt
.claude/goals/2026-09-holdfast-g4.goal.txt
.claude/goals/2026-09-holdfast-g5.goal.txt
.claude/goals/2026-09-holdfast-g6.goal.txt
.claude/goals/2026-09-holdfast-g7.goal.txt
.claude/goals/2026-09-holdfast-g8.goal.txt
.claude/goals/2026-09-holdfast-g9.goal.txt
.claude/goals/2026-09-holdfast-research
.claude/goals/2026-09-holdfast.md
.claude/goals/NEEDS-OWNER.md
$ echo "$GOFLAGS $GOMAXPROCS"
-p=2 2
```

## Baselines

Measured at the goal-start SHA in this container (2 CPUs by cgroup quota). Logs under
`/cache/tmp/holdfast-g1/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (detached worktree at `4ac983a`) | running | - |
| `internal/engine` under `go test -race` (from that run) | running | - |
| `go test -race ./internal/engine -run FailurePathIsByteIdentical -count=3` under plain `flock` (no `-o`) | FAIL 3 of 3: the no-progress error gains `cat: write error: Bad file descriptor` | 3.2 s |
| `scripts/test-mass.sh -check` | exit 1: `docs/test-mass.md` records commit `e09a819`, which this repository does not have | under 1 s |
| `go test ./internal/docscheck/ ./internal/corpus/` | ok | 2.3 s |
| `scripts/check-pins.sh` | exit 0, "pins agree" | under 10 s |
| `make secret-scan` | exit 0, clean | under 10 s |
| `func Test` count, all packages | 1200 in 25 packages (table below) | - |
| `CLAUDE.md` lines (`wc -l`) | 138 | - |
| tracked `scripts/regress_0057_*` (`git ls-files`) | 4 | - |

`func Test` per package at the goal-start SHA (`git grep -c '^func Test' <sha> -- '*_test.go'`,
summed per directory):

| Package | Count | Package | Count |
|---|---|---|---|
| `cmd/holdfast` | 158 | `internal/metrics` | 19 |
| `internal/config` | 90 | `internal/notify` | 10 |
| `internal/corpus` | 2 | `internal/probe` | 17 |
| `internal/cpuquota` | 10 | `internal/schedule` | 13 |
| `internal/diskfree` | 4 | `internal/secret` | 13 |
| `internal/docscheck` | 28 | `internal/secretscan` | 10 |
| `internal/encoder` | 12 | `internal/server` | 105 |
| `internal/engine` | 422 | `internal/sourceoffer` | 9 |
| `internal/fsclass` | 6 | `internal/startup` | 70 |
| `internal/hdr` | 9 | `internal/store` | 149 |
| `internal/heapmeasure` | 5 | `internal/vmaf` | 30 |
| `internal/logging` | 1 | `scripts/mutation-gate` | 5 |
| `internal/memlimit` | 3 | | |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`4ac983a`): both checks pass |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (this commit) |
| 1.3 | Baselines with timings | DOING |

## Phase 2 - Identity (T41, I1; line B, foundation)

| # | Item | State |
|---|---|---|
| 2.1 | `CLAUDE.md` commit-identity line becomes "commit as the repository's configured git identity", keeping the no-trailer rule | TODO |
| 2.2 | The owner's name in the `internal/server/server_test.go` fixture becomes a synthetic one | TODO |
| 2.3 | Identity scan in `make check`: tokens derived at run time, whole-word match, `LICENSE` and `NOTICE` exempt | TODO |
| 2.4 | Its selftest proves it bites on a synthetic identity | TODO |
| 2.5 | In CI it derives the identity or fails loudly, never passes vacuously | TODO |

## Phase 3 - The fd-3 fixture (line C)

| # | Item | State |
|---|---|---|
| 3.1 | Investigation: is production affected by an inherited read-only fd 3? | TODO |
| 3.2 | `progressFake` probe true only when fd 3 is actually writable, with a test that bites on the old probe | TODO |
| 3.3 | Plain `flock` over the `FailurePathIsByteIdentical` run passes after the merge | TODO |

## Phase 4 - Reversals R1-R6 and the T34 cleanup (lines D, E)

| # | Item | State |
|---|---|---|
| 4.1 | R1 audio (`README.md`, `docs/migration.md`) | TODO |
| 4.2 | R2 worker nodes (`README.md`, `docs/migration.md`) | TODO |
| 4.3 | R3 frontend (`README.md`, `CLAUDE.md`, `docs/api-reference.md`, `docs/docker.md`) | TODO |
| 4.4 | R4 Dolby Vision / HDR10+ note under the `README.md` anchor | TODO |
| 4.5 | R5 release acts (`docs/release.md`, `README.md`) | TODO |
| 4.6 | R6 plan of record (`CLAUDE.md`, `README.md`, `cmd/holdfast/main.go`) | TODO |
| 4.7 | `README.md` status line: no stale date, points at the releases page | TODO |
| 4.8 | `CLAUDE.md` Layout: every `internal/` package, `analyze` and `plan`, no "API and UI", the TRANSCODE label range the code uses (and the `check-pins.sh` comment) | TODO |
| 4.9 | Web-UI and dashboard wording in `cmd/` and `internal/` | TODO |
| 4.10 | `docs/test-mass.md` and `scripts/test-mass.sh -check` agree | TODO |
| 4.11 | `scripts/regress_0057_*.js` deleted | TODO |
| 4.12 | `docs/comparison.md`, `docs/migration.md`, `docs/api-reference.md` match the code | TODO |

## Phase 5 - GitHub deletions (T35; line F)

| # | Item | State |
|---|---|---|
| 5.1 | Ref names before (`git ls-remote --heads --tags origin`) | TODO |
| 5.2 | Delete the 6 branches and 1 tag by full refname | TODO |
| 5.3 | Ref names after, and the diff | TODO |

## Phase 6 - Proposals (T48; line G)

| # | Item | State |
|---|---|---|
| P1 | `proposal-triage.md` (T30, T33): a row per S0151, S0162-S0180 and PR #94 | TODO |
| P2 | `proposal-hw-gates.md` (T10) | TODO |
| P3 | `proposal-amd-image.md` (T45, I8, I9) | TODO |
| P4 | `proposal-node-protocol.md` (T46) | TODO |
| P5 | `proposal-crop-dv.md` (I7) | TODO |
| P6 | `proposal-docs-corpus.md` (I16) | TODO |

## Phase 7 - Fresh-clone gate (line B)

| # | Item | State |
|---|---|---|
| 7.1 | Clone `origin/main` to `/cache/tmp/holdfast-g1-fresh` and run the full gate there | TODO |

## Phase 8 - Checkpoint T packet and the report (lines H-K)

| # | Item | State |
|---|---|---|
| 8.1 | Checkpoint T packet printed | TODO |
| 8.2 | Gate integrity counted from the goal-start SHA | TODO |
| 8.3 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-29: the goal-start baseline gate runs in a detached worktree
  (`/cache/wt/holdfast/g1-baseline`) so edits in `/workspace` cannot touch what it measures.
  Reasoning: §0.1 step 5 and §0.4 (worktrees for anything that writes).
- 2026-09-29: one branch and PR per track: `holdfast-g1/identity-scan`,
  `holdfast-g1/fd3-fixture`, `holdfast-g1/reversals-cleanup`, `holdfast-g1/proposals`. The
  proposals live under `.claude/goals/` but are not ledger files, so they go through a PR and the
  gate (§0.3 lets only the ledger and `NEEDS-OWNER.md` go straight to `main`).

## Resume here

Phase 1: the baseline gate is running (`/cache/tmp/holdfast-g1/gate-baseline-main.log`). Next:
the identity track (phase 2) on `holdfast-g1/identity-scan`, then the fd-3 fixture, then the
reversals and cleanup PR, the deletions, the proposals, the fresh-clone gate and the packet.
