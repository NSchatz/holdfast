# Goal 8 ledger - dynamic HDR and crop

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §12; goal file
  `.claude/goals/2026-09-holdfast-g8.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`:
  P5 (`proposal-crop-dv.md`) approved as option (c) - a Dolby Vision source is cropped only to
  the active area its own RPU names, with L5 zeroed and gated; every other DV source keeps the I7
  refusal and encodes uncropped. I6 confirmed: DV profile 8 and HDR10+ carried by default on the
  `cpu` encoder once the metadata gates pass.
- Started: 2026-10-01, from `/workspace/holdfast` in the maker container.
- Goal-start SHA: `0d752f6cb1c7070f387ed653dfc28957b80bb4cf` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g7.status.md` (ends
  `COMPLETE (goal 7): 2026-10-01`). `NEEDS-OWNER.md` at start: rows 1-7, all `OPEN`.
- Precondition, as checked:

```
$ git fetch -q origin && git log -1 --format='%H %s' origin/main
0d752f6cb1c7070f387ed653dfc28957b80bb4cf chore(goals): goal 7 ledger - adversarial review recorded, COMPLETE
$ git show origin/main:.claude/goals/2026-09-holdfast-g7.status.md | command grep -n 'COMPLETE (goal 7)'
195:COMPLETE (goal 7): 2026-10-01
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `d4d70a6`, line 61, "Per
goal"): "**Goal 8** (dynamic HDR and crop): none." `CHECKPOINT-T.approved` approves P1 as proposed
and amends no goal-8 row.

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g8-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g8/tmp`. Logs under `/cache/tmp/holdfast-g8/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (log `gate-baseline.log`) | exit 0 | 1981 s (33m01s) |
| `internal/engine` under `go test -race` (from that run) | ok, 88.4% coverage | 1859.2 s (69% of `TEST_TIMEOUT` 45m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 89.2% coverage | 653.8 s |
| `func Test` count, all packages | 1542 in 30 packages (`functest-start.txt`) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 62, 76 | - |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`0d752f6`): goal 7's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`ca9d9fc`) |
| 1.3 | Baselines with timings | DONE (`0d752f6`): the table above; `make check` exit 0 in 1981 s |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | P1 rows for goal 8 | DONE (`d4d70a6`): P1 assigns none |

## Phase 3 - Tools (line C)

| # | Item | State |
|---|---|---|
| 3.1 | `dovi_tool` and `hdr10plus_tool` pinned per arch by version and sha256 in the Dockerfile and a CI installer; `check-pins.sh` and its selftest cover them; `NOTICE` names them (MIT) | TODO |

## Phase 4 - Dynamic HDR (lines D, E)

| # | Item | State |
|---|---|---|
| 4.1 | DV profile 8 carried through libx265 (`-dolbyvision 1`, VBV, mastering display) on the `cpu` encoder | TODO |
| 4.2 | HDR10+ extracted with `hdr10plus_tool` and passed as `dhdr10-info`; unextractable metadata skips | TODO |
| 4.3 | Opt-in P7 to P8.1 (`dovi_tool -m 2 convert --discard` pre-pass, VFR refused, FEL or MEL logged); P5 stays skipped; non-`cpu` encoders keep skipping | TODO |
| 4.4 | Gates: DOVI profile and compatibility id, RPU count, HDR10+ count; one red fixture each | TODO |
| 4.5 | R4: `README.md` statement and `internal/docscheck/dynamichdr.go` rewritten in one PR, bite tests pass | TODO |
| 4.6 | `docs/design/dynamic-hdr.md` with anchors | TODO |

## Phase 5 - Crop (line F)

| # | Item | State |
|---|---|---|
| 5.1 | Opt-in consensus crop (fractional limit, spread samples, invalid results discarded, disagreement refused) | TODO |
| 5.2 | Blackness gate over the removed area of the source; VMAF against the identically cropped source; even 4:2:0 dimensions; crop rectangle on the job row | TODO |
| 5.3 | Fixtures: letterbox, text in the bar, mixed aspect refused, 10-bit | TODO |
| 5.4 | DV crop per P5 (c): refusal first, then crop to the RPU's own L5 rectangle with L5 zeroed and the L5 gate | TODO |
| 5.5 | `docs/design/crop.md` with anchors | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-10-01: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g8/` (a tmpfs
  `TMPDIR` is refused by `fsclass`, as goal 7 found), and goal shells use `command grep`, `rg` or
  `git grep` (the shell's `grep` wrapper is broken here, §0.11).
- 2026-10-01: commits carry no session-link trailer (T38; goal 7 recorded one such slip). PR
  bodies may end with the session link.

- 2026-10-01: tracks. `holdfast-g8/tools` (pins, installer, check-pins, NOTICE), `holdfast-g8/dynhdr`
  (P8, HDR10+, opt-in P7, the three gates, R4's statement and its check, `docs/design/dynamic-hdr.md`)
  and `holdfast-g8/crop` (opt-in crop, blackness gate, cropped VMAF reference, the I7 refusal first),
  each built by one agent in its own worktree and merged by the rule of §0.3. P5 option (c) (DV
  crop to the RPU's own L5 rectangle) is a second crop phase built after `dynhdr` merges, on its
  pre-pass machinery, as P5's ship order says.

## Proposals awaiting the owner

(none yet)

## Resume here

Baselines done. Three agents building `holdfast-g8/tools`, `holdfast-g8/dynhdr`, `holdfast-g8/crop`
in worktrees under `/cache/wt/holdfast/`. Next: review and merge each PR; then crop phase 2 (P5 (c)).
