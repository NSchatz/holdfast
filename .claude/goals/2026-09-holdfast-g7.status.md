# Goal 7 ledger - audio and subtitles

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §11; goal file
  `.claude/goals/2026-09-holdfast-g7.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`
  (no amendment names goal 7; P1 approved as proposed).
- Started: 2026-10-01, from `/workspace/holdfast` in the maker container.
- Goal-start SHA: `3df61626c42cfa19f5faabdf0c794f5a2aa1c55e` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g6.status.md` (ends
  `COMPLETE (goal 6): 2026-10-01`). `NEEDS-OWNER.md` at start: rows 1-7, all `OPEN`.
- Precondition, as checked:

```
$ git fetch -q origin && git log -1 --format='%H %s' origin/main
3df61626c42cfa19f5faabdf0c794f5a2aa1c55e chore(goals): goal 6 ledger - the final review's corrections, main CI green on 400ff42
$ git show origin/main:.claude/goals/2026-09-holdfast-g6.status.md | command grep -n 'COMPLETE (goal 6)'
261:COMPLETE (goal 6): 2026-10-01
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `d4d70a6`, line 60, "Per
goal"): "**Goal 7** (audio and subtitles): none." `CHECKPOINT-T.approved` approves P1 as proposed
and amends no goal-7 row.

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g7-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g7/tmp`. Logs under `/cache/tmp/holdfast-g7/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (log `gate-baseline.log`) | running | - |
| `func Test` count, all packages | 1463 in 28 packages (`functest-start.txt`) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 62, 76 | - |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`3df6162`): goal 6's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DOING |
| 1.3 | Baselines with timings | DOING |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | P1 rows for goal 7 | DONE (`d4d70a6`): P1 assigns none |

## Phase 3 - Audio (lines C, D)

| # | Item | State |
|---|---|---|
| 3.1 | Audio keys, off until configured; re-encode lossless or bulky tracks per an explicit codec and layout matrix, replace by default, `keep_original_audio` keeps both | TODO |
| 3.2 | Added stereo downmix track | TODO |
| 3.3 | Two-pass EBU R128 loudness, `aresample` to the source rate, the mode that ran recorded | TODO |
| 3.4 | Audio gates (duration, channel layout, sample rate, full decode, loudness), one red fixture each; stream-count parity learns the declared changes | TODO |
| 3.5 | Keys unset: argv and decisions equal the pre-goal ones (golden) | TODO |

## Phase 4 - Subtitles (line E)

| # | Item | State |
|---|---|---|
| 4.1 | SubRip, ASS, WebVTT to `<name>.<lang>[.forced].<ext>` sidecars, exclusive create; bitmap and mov_text skipped with a reason; ASS fonts logged | TODO |
| 4.2 | Sidecar parse-back event-count gate, red on a truncated sidecar | TODO |

## Phase 5 - Docs (line F)

| # | Item | State |
|---|---|---|
| 5.1 | R1 rewritten in `README.md` and `docs/migration.md`; `docs/design/audio.md`, `docs/design/subtitles.md` anchored; `docs/profiles.md`, `config.example.yaml` keys | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-10-01: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g7/` (a tmpfs
  `TMPDIR` is refused by `fsclass`), and goal shells use `command grep`, `rg` or `git grep`.

## Resume here

Ledger created; baseline gate running. Next: read the encode plan, the argv builder and the gates,
then plan the audio and subtitle tracks.
