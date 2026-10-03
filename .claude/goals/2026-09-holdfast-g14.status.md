# Goal 14 ledger - web UI: views and controls

- Brief: `.claude/goals/2026-09-holdfast.md` §0, §4 and §18 (spec v1.1, the `/goals:spec` skill);
  goal file `.claude/goals/2026-09-holdfast-g14.goal.txt`. Amended by
  `.claude/goals/CHECKPOINT-T.approved`: "P1 proposal-triage.md: approved as proposed."
- Started: 2026-10-03 17:29Z, from `/workspace/holdfast` in the maker container, by the goals
  supervisor's `/goal` condition.
- Goal-start SHA: `dfaca59b22701c932fb9f219a0f4c18a684c4b59` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g13.status.md` (ends
  `COMPLETE (goal 13): 2026-10-03`). The owner's queue at start (`goals needs list`): the open
  holdfast items are queue #50 to #56, #202 to #204 and #219.
- Precondition, as checked:

```
$ git fetch origin && git rev-parse HEAD origin/main
dfaca59b22701c932fb9f219a0f4c18a684c4b59
dfaca59b22701c932fb9f219a0f4c18a684c4b59
$ git grep -n "COMPLETE (goal 13)" origin/main -- .claude/goals/2026-09-holdfast-g13.status.md
origin/main:.claude/goals/2026-09-holdfast-g13.status.md:244:COMPLETE (goal 13): 2026-10-03
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
$ goals admit --agents 4
decision: admit a goal
agents: 4 of 4 allowed (requested)
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md`, line 67, "Per goal", last
changed in `d4d70a6`): "**Goal 14** (web UI: views and controls): S0167, S0169, S0170, S0171,
S0172." Their rows (lines 35 and 37 to 40): S0167 `scan-records-source-facts` (history view),
S0169 `summary-per-root-totals` (summary and savings view), S0170 `history-filter-paging`
(history view), S0171 `queue-row-decision-facts` (queue view), S0172
`would-transcode-state-label` (summary view); each "keep". The specs' full text was read in goal
1's read-only umbrella clone (push URL disabled), at its commit of 2026-09-24.

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g14-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g14/tmp` and `PATH=/cache/ffmpeg/bin:/cache/opt/dynhdr-tools/bin:$PATH`
(goal 13's D4). Logs under `/cache/tmp/holdfast-g14/`.

| What | Value | Source |
|---|---|---|
| `make check` under `goals-heavy` then `flock -o` `holdfast-heavy`, at `dfaca59` | exit 0 in 3339 s, while this goal's four builders ran their own tests on the same CPUs (load average 50 to 58) | `gate-baseline.log` |
| `internal/engine` under `go test -race` (from that run) | ok, 89.1% coverage, 2984.4 s (82.9% of `TEST_TIMEOUT` 60m; D5) | `gate-baseline.log` |
| `cmd/holdfast`, `internal/server` (from that run) | ok, 1170.7 s; 622.0 s | `gate-baseline.log` |
| The last gate on this tree (goal 13, `76786b5`; `main` has moved by ledger commits only since) | exit 0 in 2211 s; `internal/engine` 1983.0 s (55.1% of `TEST_TIMEOUT` 60m); `cmd/holdfast` 803.9 s | goal 13's ledger, row 4.0 |
| `func Test` count, all packages | 2033 | `rg -c '^func Test' -g '*_test.go'`, `functest-start.txt` |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines | 66, 80 | `wc -l` |
| `CLAUDE.md` lines | 199 | `wc -l` |
| Node and pnpm on PATH in the container | Node 24.21.0, pnpm 12.8.1 (the pins) | `node --version`, `pnpm --version` |
| Usage headroom | five_hour 4%, seven_day 62%; 4 of 4 agents allowed | `goals admit --agents 4` |
| Host load at start | load average 39 on 56 CPUs (quota 28) | `uptime` |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`dfaca59`): goal 13's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (the commit that adds this file): fast docs checks passed first |
| 1.3 | Baseline gate with timings | DONE (`dfaca59`): `make check` exit 0 in 3339 s; the table above |

## Phase 2 - Triage rows and the API facts the views need (line B; track `holdfast-g14/api-facts`)

| # | Item | State |
|---|---|---|
| 2.1 | S0167 `scan-records-source-facts`: a skipped row records the source codec and dimensions the probe already read, with no extra probe | TODO |
| 2.2 | S0171 `queue-row-decision-facts`: an encoding or verifying row carries this attempt's decision facts | TODO |
| 2.3 | S0169 `summary-per-root-totals`: `/api/summary` gains the per-root block and the top-level held figure | TODO |
| 2.4 | S0172 `would-transcode-state-label`: a live engine reports dry-run rows inside `pending` on the summary, the SSE snapshot and the queue-depth gauge | TODO |
| 2.5 | S0170 `history-filter-paging`: `/api/history` takes a terminal-status filter and an opaque cursor | TODO |
| 2.6 | The two reads the views need and the API lacks: a queue row's `priority`, and a read of the worker nodes and their leases (D2) | TODO |

## Phase 3 - Views, controls and the statement (lines C, D, E; track `holdfast-g14/ui-views`)

| # | Item | State |
|---|---|---|
| 3.1 | Views: summary and savings, queue with priority, history with filters and paging, health results, nodes; unit tests | TODO |
| 3.2 | Controls: pause, resume, scan and exclusions, with the control token held in memory for the session only; no `restore` or `requeue` route in `web/` or `internal/server` | TODO |
| 3.3 | The no-frontend statement rewritten in `README.md`, `CLAUDE.md`, `docs/api-reference.md` and `docs/docker.md`; `docs/design/web-ui.md` describes the views and controls | TODO |
| 3.4 | Adversarial review of each branch before its gate | TODO |

## Phase 4 - Report

| # | Item | State |
|---|---|---|
| 4.1 | Gate integrity counted from the goal-start SHA | TODO |
| 4.2 | Adversarial review of the report | TODO |

## Decisions taken

- D1 (2026-10-03): tracks. `holdfast-g14/api-facts` carries the five triage rows and the two
  reads of row 2.6 in one PR (T52: few larger PRs; the gate is about 37 minutes and
  `holdfast-heavy` is exclusive). Three builders work in their own worktrees on the parts that do
  not share files (engine and store writes; the summary; history, nodes and priority) and their
  work is merged into the one track branch before its gate. `holdfast-g14/ui-views` carries the
  views, the controls and the statement rewrite, built beside it against the wire shapes the
  specs and D2 fix, and merged second, after it is checked against the merged API.
- D2 (2026-10-03): the views of §18 item 2 need two facts no endpoint serves at the goal-start
  SHA: a queue row carries no priority, and no read names a worker node or a lease (the lease
  routes are the workers' own, behind `node_token`). Both are added as reads, additions only on
  the surface gate: `priority` on a job row (what `Config.PriorityOf` answers for the file, null
  where a banded rule's priority needs a source height the row does not record), and
  `GET /api/nodes` in the read group (nodes and leases, never a lease id, a working path or a
  digest). Neither is a control: T31's declined UI or API priority bump stays declined, and the
  node read grants, ends and re-opens nothing.
- D3 (2026-10-03): every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g14/`
  and `PATH=/cache/ffmpeg/bin:/cache/opt/dynhdr-tools/bin:$PATH` (goal 13's D3 and D4), goal
  shells use `rg` or `git grep`, and commits carry no trailer (T38).
- D4 (2026-10-03): the control token in the UI is held in a variable of the running page and
  nowhere else: not `localStorage`, not `sessionStorage`, not a cookie, not a URL. A reload asks
  for it again. That is the narrowest reading of "held for the browser session only", and a test
  holds the page to it.
- D6 (2026-10-03): the branch review of `holdfast-g14/api-facts` (a fresh agent, before the
  gate) found no data-safety fault and four findings, each fixed with a test: the history cursor
  carried the path as a JSON string, which rewrites a byte that is not UTF-8 and so dropped rows
  or never ended (the path and fingerprint are now bytes;
  `TestS0170_AC3_APathThatIsNotUTF8IsAPositionLikeAnyOther`); `serve`'s wiring of the priority
  resolver had no test (`TestServe_CarriesTheConfiguredPriorityToTheJobRows`); a node-stated
  lease reason of 32 characters or more could be another lease's id or a digest and is now
  served as `node_failed`; a request that hung up mid-read could blank the per-root figures for
  the cache interval (the read now outlives its request).
- D7 (2026-10-03): the builders found that `.gremlins.yaml` excludes `internal/engine`,
  `internal/store`, `internal/server`, `internal/metrics` and `cmd/holdfast`, so of this track's
  code only `internal/node` and `internal/config` are in the mutation domain. No exclusion is
  added or removed (§4, T34). For the owner's eye, not built on: a `would-transcode` row whose
  file later gains a second hard link is skipped before the claim and its row is not rewritten,
  so under a live engine it keeps being counted in `pending` (S0172's builder; the row and the
  file are untouched).
- D5 (2026-10-03): the baseline's `internal/engine` took 2984.4 s, 82.9% of `TEST_TIMEOUT`, against
  1983.0 s in goal 13's last gate on the same tree. The difference is load: the four builders ran
  package tests beside it. §4 says to raise `TEST_TIMEOUT` once the engine passes 80%, in its own
  commit with the measurement, so the API track raises it, and this goal's gates run while no
  builder is running tests.

## Requests

None filed, none addressed to holdfast (T6, §0.13). The owner's queue: no new item so far.

## Resume here

Phase 2: the three API builders finished; their work is merged into `holdfast-g14/api-facts`
(worktree `/cache/wt/holdfast/g14-api`, local only), with the branch review's four findings fixed
and `TEST_TIMEOUT` raised to 90m in its own commit. Running now, in that worktree, under the
heavy locks: `make mutation-diff REF=origin/main` (log `/cache/tmp/holdfast-g14/api-mutation.log`),
then `make check` (log `api-gate.log`, the gated SHA in `api-gate.sha`). Next: push the branch,
open its PR with the gate tail, wait for CI, squash-merge. Phase 3: the UI builder finished on
`holdfast-g14/ui-views` (worktree `/cache/wt/holdfast/g14-ui`, local only); a branch review is
running; after the API PR merges, merge `origin/main` into it, align it with the served shapes
(the node `mode` words are `mapped` and `http`), gate, PR, CI, merge.
