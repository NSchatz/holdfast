# Goal 9 ledger - queue priority and the health sweep

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §13; goal file
  `.claude/goals/2026-09-holdfast-g9.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`:
  P1 approved as proposed, and "S0164: the new queue_order value is named savings_per_hour".
- Started: 2026-10-01, from `/workspace/holdfast` in the maker container.
- Goal-start SHA: `5080e732edf777792f8255cc1d5d0facc5770c98` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g8.status.md` (ends
  `COMPLETE (goal 8): 2026-10-01`). `NEEDS-OWNER.md` at start: rows 1-7, all `OPEN`.
- Precondition, as checked:

```
$ git fetch -q origin && git log -1 --format='%H %s' origin/main
5080e732edf777792f8255cc1d5d0facc5770c98 chore(goals): goal 8 ledger - adversarial review recorded, COMPLETE
$ git show origin/main:.claude/goals/2026-09-holdfast-g8.status.md | command grep -n 'COMPLETE (goal 8)'
172:COMPLETE (goal 8): 2026-10-01
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `d4d70a6`, line 62, "Per
goal"): "**Goal 9** (queue priority and health sweep): S0164, S0174." S0164 is the
savings-per-hour `queue_order` value; S0174 is `run --limit-encodes N` and the one-run
`run --queue-order` override. `CHECKPOINT-T.approved` names the S0164 value `savings_per_hour`.
The specs are read from the read-only umbrella clone (`pipeline/active/S0164-*`, `S0174-*`).

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g9-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g9/tmp`. Logs under `/cache/tmp/holdfast-g9/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o`, dynhdr tools on PATH (log `gate-baseline.log`) | exit 0 | 2180 s under the lock (4560 s including the wait) |
| `internal/engine` under `go test -race` (from that run) | ok, 88.6% coverage | 2036.9 s (75.4% of `TEST_TIMEOUT` 45m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 89.2% coverage | 638.2 s |
| `func Test` count, all packages | 1634 in 32 directories (`functest-start.txt`) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 66, 80 | - |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`5080e73`): goal 8's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`cd5ab73`) |
| 1.3 | Baselines with timings | DONE (`5080e73`): the table above; `make check` exit 0 in 2180 s |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | S0164 `savings_per_hour` order (AC-1 to AC-15) | TODO |
| 2.2 | S0174 `run --limit-encodes` and `run --queue-order` (AC-1 to AC-18) | DONE (PR #142, `80501dc`): `internal/engine/limit_encodes_test.go` (`TestLimitEncodes_AC1_...` to `AC10`), `cmd/holdfast/run_limit_encodes_test.go`, `cmd/holdfast/run_queue_order_flag_test.go` (AC-7, AC-9, AC-11 to AC-16); the existing `--limit` tests' bodies untouched (AC-17); startup record `queue_order_source=cli|config`; gate exit 0 (`cmd/holdfast` 701.5 s, `internal/engine` 2176.5 s on the previous valid run, cached on the final one since engine code was unchanged); CI green (runs 36889021576, 36889021540); 1 fix round. One existing test line changed: `cmd/holdfast/flags_test.go:56`, the pinned `run` flag set gains `limit-encodes` and `queue-order` |

## Phase 3 - Priority and the savings order (line C)

| # | Item | State |
|---|---|---|
| 3.1 | `priority` on resolution rules, `library_roots` entries and `encode_profiles`; higher first, sequence only | TODO |
| 3.2 | `queue_order: savings_per_hour`: estimator inputs documented, checked against a worked example | TODO |
| 3.3 | Fixture queue ordered as specified (priority, then the order) | TODO |

## Phase 4 - Health sweep (line D)

| # | Item | State |
|---|---|---|
| 4.1 | Scheduled full-decode sweep under `serve`, inside the run window | DONE (PR #144, `8c6084c`): `health_sweep_interval_hours` (0 = off, default), `health_sweep_workers`; due from the last finished sweep in the store; `TestHealthSweep_IsScheduledFromTheLedger`, `TestHealthSweep_HonoursTheRunWindow` (outside the window 0 decodes; a window closing mid-sweep holds the next decode until it reopens) |
| 4.2 | Resumable after a restart (progress in the store) | DONE (PR #144, `8c6084c`): store v24 `health_sweeps`, `health_checks`; `TestHealthSweep_ResumesAfterARestartWithoutDecodingCheckedFilesAgain` (a file checked before the restart is not decoded again; one whose mtime changed is) |
| 4.3 | Results in the store, the read API, a notification on corruption, a metric | DONE (PR #144, `8c6084c`): `GET /api/health` (read token; `TestHealthEndpoint_ReportsASweepThroughTheReadAPI`), one summary notification per sweep with problems, metrics `holdfast_health_sweep_files_checked_total{result}`, `holdfast_health_sweep_corrupt_files`, `holdfast_health_sweep_unreadable_files`, `holdfast_health_sweep_last_completed_timestamp_seconds` |
| 4.4 | Report-only: a test proves no file moved, renamed or deleted | DONE (PR #144, `8c6084c`): `TestHealthSweep_IsReportOnly_NoFileMovedRenamedOrDeleted` (name, size, mtime, mode, inode, sha256 of 10 entries identical before and after a real pinned-ffmpeg sweep; 3 ok, 4 corrupt, a FIFO unreadable). Gate exit 0 (2259 s, `internal/engine` 2113.8 s = 78.3%, `cmd/holdfast` 665.9 s); mutation-diff 100% (92 killed, 0 lived); CI green (runs 36897212191, 36897212216) |

## Phase 5 - Docs (line E)

| # | Item | State |
|---|---|---|
| 5.1 | Keys and metric in `docs/profiles.md`, `config.example.yaml`, the metrics docs, README | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-10-01: the value is spelled `savings_per_hour`, as `CHECKPOINT-T.approved` amends S0164;
  the brief's and goal file's `savings_rate` is the same order under its pre-approval name. No
  alias is accepted: S0164 AC-2 refuses every value outside the closed set, and the owner chose
  one name.
- 2026-10-01: `priority` semantics (T31, §0.2 fallback where the brief is silent). An integer,
  default 0, higher offered first; it decides sequence and never membership, any decision, any
  gate or any digest a terminal row records. Allowed on a `library_roots` entry, on a resolution
  rule inside one and on an `encode_profiles` entry. A file's priority is the first that names
  one of: its matching rule, its matching encode profile, its root; else 0. The queue sorts by
  priority, then by `queue_order`, then by path. With no `priority` written anywhere, every order
  is exactly what it was (I5): `path` still streams with no buffering.
- 2026-10-01: tracks. `holdfast-g9/queue-order` (priority and `savings_per_hour`, S0164),
  `holdfast-g9/limit-encodes` (S0174) and `holdfast-g9/health-sweep`, each built by one agent in
  its own worktree and merged by the rule of §0.3.
- 2026-10-01: PR #142 (S0174) merged as `80501dc`. Its own decisions: a per-file slot spent when the job enters the encoding state (dry run: at the would-transcode decision, before its row is written) and handed back if none was reached; both count bounds given -> the first reached stops the offer, recorded as `bound=limit,limit_encodes`; `--limit-encodes ""` refused; the `--queue-order` override is applied after `loadConfig`, so an invalid configured order still exits 1 (S0174 advisory F2), and only in memory.
- 2026-10-01: PR #144 (health sweep) merged as `8c6084c`. Its own decisions: off by default; the first sweep is due when `serve` starts if none is on record; before each decode it asks pause, then the scheduler (window, `max_load`, Tautulli); decodes at nice 19 (the benefit `ASSUMED`); it runs beside encodes; ffmpeg's clean exit with an error printed marks a file corrupt (truncation); a FIFO is never opened; a file that changed under two attempts is unreadable; per-file results older than the last finished sweep are pruned at a sweep's start. Changed existing test lines: the newest-migration wind-back fixtures moved from v23 (`crop` column) to v24 (`health_sweeps`, `health_checks` tables) in `internal/store/migrate_test.go`, `readonly_test.go`, `resolution_migrate_test.go` and `cmd/holdfast/export_test.go` (`olderSchemaVersion` 22 -> 23); the rest are additions.
- 2026-10-01: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g9/` (a tmpfs
  `TMPDIR` is refused by `fsclass`), goal shells use `command grep`, `rg` or `git grep`, and
  commits carry no trailer (T38).
- 2026-10-01: finding. The first baseline gate went red in `internal/dynhdr` and `internal/crop`
  only because `dovi_tool` and `hdr10plus_tool` were not on this session's PATH (the fixtures
  require them by design and refuse to skip). They were installed rootless with the repo's own
  pinned, checksum-verifying installer: `scripts/install-dynhdr-tools.sh /cache/opt/dynhdr-tools`
  -> "dovi_tool 2.3.4 and hdr10plus_tool 1.7.2 installed ... (checksums verified; versions
  confirmed)". Every gate in this goal runs with `PATH=/cache/opt/dynhdr-tools/bin:$PATH`; the
  baseline was re-run that way.

## Proposals awaiting the owner

- None yet.

## Resume here

Baseline green. Three track agents running in `/cache/wt/holdfast/holdfast-g9-{queue-order,limit-encodes,health-sweep}`; each stops at a green gate and green CI, and the lead merges.
