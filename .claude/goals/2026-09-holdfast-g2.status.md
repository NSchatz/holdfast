# Goal 2 ledger - carried specs and the debian13 base

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §6; goal file
  `.claude/goals/2026-09-holdfast-g2.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`.
- Started: 2026-09-29.
- Goal-start SHA: `3bfd42337585ce47e0da2727b43192585677d744` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md`, ending `COMPLETE (goal 1): 2026-09-29`.
  `NEEDS-OWNER.md` at start: header only, no rows.
- Precondition, as checked:

```
$ git show origin/main:.claude/goals/2026-09-holdfast-g1.status.md | grep -n 'COMPLETE (goal 1)'
223:COMPLETE (goal 1): 2026-09-29
$ git ls-tree origin/main .claude/goals/CHECKPOINT-T.approved
100644 blob f24d5520b0aa31f3130b1566c5235da605129ebd	.claude/goals/CHECKPOINT-T.approved
$ git log --diff-filter=A --format='%H %s' origin/main -- .claude/goals/CHECKPOINT-T.approved
3bfd42337585ce47e0da2727b43192585677d744 chore(goals): Checkpoint T approved
$ gh api repos/NSchatz/holdfast/commits/3bfd42337585ce47e0da2727b43192585677d744 --jq '.commit.verification.verified,.commit.committer.name'
false
<the owner's name>
$ gh api repos/NSchatz/holdfast/commits/3bfd423... --jq '.commit.verification.reason'
unsigned
$ echo "$GOFLAGS $GOMAXPROCS"
-p=2 2
```

The approval commit is not GitHub-verified (unsigned), so it was not added in the web UI. I19
accepts a commit from a session the owner told to, and says the check is shown, not enforced; no
goal wrote the file (its only commit is `3bfd423`, authored and committed as the owner).

## What the approval assigns to this goal

From `CHECKPOINT-T.approved` and P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md`
at `3bfd423`, "Per goal"): PR #94 (S0159) first, by cherry-pick; S0151, S0163, S0166, S0168,
S0173, S0176, S0177, S0180; and P6 option (a) (`internal/corpus` skips `.claude/`). The
approval's amendments for this goal: S0151 is a committed `.github/dependabot.yml` (GitHub
Actions, the Docker base images, Go modules; PRs never merged; the ffmpeg pin stays with
`pin-health.yml`; no app); S0176 builds the holdfast half here and opens a never-merged PR in
`NSchatz/homelab` for the `TZ` half, whose merge is a `NEEDS-OWNER.md` row.

## Baselines

Measured at the goal-start SHA in this container (2 CPUs by cgroup quota). Logs under
`/cache/tmp/holdfast-g2/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (detached worktree at `3bfd423`; log `gate-baseline.log`) | running | - |
| `func Test` count, all packages | 1201 in 25 packages (table below) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 58, 76 | - |

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
| `internal/engine` | 423 | `internal/sourceoffer` | 9 |
| `internal/fsclass` | 6 | `internal/startup` | 70 |
| `internal/hdr` | 9 | `internal/store` | 149 |
| `internal/heapmeasure` | 5 | `internal/vmaf` | 30 |
| `internal/logging` | 1 | `scripts/mutation-gate` | 5 |
| `internal/memlimit` | 3 | | |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`3bfd423`): both files on `origin/main`; verification printed |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DOING |
| 1.3 | Baselines with timings | DOING |

## Phase 2 - PR #94 (S0159), by cherry-pick (I12)

| # | Item | State |
|---|---|---|
| 2.1 | Cherry-pick #94's four commits onto `holdfast-g2/s0159-temp-sweep`, gate, merge as a new PR | TODO |
| 2.2 | Close #94 with a link to the new PR; its old branch is left and listed as a follow-up | TODO |

## Phase 3 - The debian13 base (line C, foundation)

| # | Item | State |
|---|---|---|
| 3.1 | `RUNTIME_IMAGE` is `gcr.io/distroless/cc-debian13:nonroot`, pinned by tag and digest | TODO |
| 3.2 | `scripts/check-pins.sh` sections 3 and 7 green | TODO |
| 3.3 | The merged PR's CI `package` job green | TODO |
| 3.4 | `docs/docker.md` follows the image | TODO |

## Phase 4 - Carried specs (line B)

| # | Item | State |
|---|---|---|
| S0177 | working-file extensions | TODO |
| S0163 | `workers: auto` from the CPU quota; several jobs in flight safe on one drive | TODO |
| S0173 | `run` progress | TODO |
| S0168 | prune excluded directories from the walk | TODO |
| S0180 | census scope parity | TODO |
| S0166 | restart survey overcount | TODO |
| S0176 | log time offset (holdfast half), and the homelab PR for the `TZ` half | TODO |
| S0151 | `.github/dependabot.yml` | TODO |

## Phase 5 - P6 (approved option (a))

| # | Item | State |
|---|---|---|
| P6 | `internal/corpus` skips `.claude/`, with a test | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-29: the goal-start baseline gate runs in a detached worktree
  (`/cache/wt/holdfast/g2-baseline`) so edits in `/workspace` cannot touch what it measures.
  Reasoning: §0.1 step 5 and §0.4 (worktrees for anything that writes).

## Resume here

Start-up: the ledger is committed and the baseline gate is running at `3bfd423`. Next: read the
eight specs in the read-only umbrella clone (`/cache/tmp/holdfast-super-ro`, push URL
`DISABLED`), decide the tracks, cherry-pick PR #94.
