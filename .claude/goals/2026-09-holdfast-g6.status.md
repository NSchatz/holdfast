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
| `make check` under `flock -o` (log `gate-baseline.log`) | TODO | TODO |
| `internal/engine` under `go test -race` (from that run) | TODO | TODO |
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
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DOING |
| 1.3 | Baselines with timings | DOING |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | P1 rows for goal 6 | DONE (`d4d70a6`): P1 assigns none |

## Phase 3 - Hardware decode (line C)

| # | Item | State |
|---|---|---|
| 3.1 | `hw_decode` key, off by default, per-vendor `-hwaccel` pipelines keeping 10-bit and HDR, frames downloaded where a gate needs them | TODO |
| 3.2 | Golden argv and the fidelity gate on fakes | TODO |

## Phase 4 - New encoders (line D)

| # | Item | State |
|---|---|---|
| 4.1 | `libx264`, `h264_nvenc`, `h264_qsv`, `h264_vaapi`, `h264_amf`, `av1_qsv`, `av1_vaapi`, `av1_amf` in the registry with golden argv and quality scales | TODO |
| 4.2 | Codec-family skip rules | TODO |
| 4.3 | `holdfast validate` accepts a config naming each | TODO |
| 4.4 | `NOTICE` names libx264 (GPL-2.0-or-later) | TODO |

## Phase 5 - Hardware reports (line E)

| # | Item | State |
|---|---|---|
| 5.1 | `scripts/hw-report.sh` writes a redacted `testdata/hw-reports/<encoder>-<date>.json`; a test proves the redaction | TODO |
| 5.2 | `NEEDS-OWNER.md`: one entry each for NVENC, QSV, VAAPI on Intel, VAAPI on AMD, AMF on a host | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-30: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g6/` (goal 3's
  finding: a tmpfs `TMPDIR` is refused by `fsclass`), and goal shells use `command grep`, `rg` or
  `git grep` (brief §0.11; the shell's `grep` function fails here).

## NEEDS-OWNER (this goal)

## Proposals awaiting the owner

## Resume here

Phase 1: the baseline gate runs in `/cache/wt/holdfast/g6-baseline` (log
`/cache/tmp/holdfast-g6/gate-baseline.log`). Next: read the encoder registry, encode plan, fidelity
gate and config validation, then cut the tracks.
