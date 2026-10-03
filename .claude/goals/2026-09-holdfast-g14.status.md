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

## Phase 3b - Request #229 (track `holdfast-g14/io-request`)

| # | Item | State |
|---|---|---|
| 3b.1 | A mutation run's temp files and build cache on a tmpfs, its workers capped at 4, and the heavy-lock rule in `CLAUDE.md` | DONE (PR #168, `59a73f3`): the same diff-scoped run (171 mutants) went from 960 s and 878 MB written in a sampled minute to 111 s and 0.4 MB written in all (`getrusage` of the run's children, `io-mutation.log`), score unchanged at 100.00%; gate exit 0 in 2109 s on `e61df2d` (`internal/engine` 1846.9 s, 51.3% of `TEST_TIMEOUT` 60m; `cmd/holdfast` 807.1 s); CI green (`build`, `package`, `mutation`); 0 fix rounds (D10) |

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
- D8 (2026-10-03): request #229 is served in this goal although §0.13 expected none: the spec's
  `requests.md` has every goal serve what is addressed to its program, and the spec wins. It is
  one commit on the UI track's branch, not a third PR, because a third PR is a third 40-minute
  gate on the disk the request is about. From its arrival, no whole-package or `-race` run of
  this goal happens outside the heavy locks.
- D9 (2026-10-03): the session died at about 20:45Z with the API branch's gate at
  `internal/engine`; the supervisor relaunched it and the gate went with it. Gates now run
  detached from the session (`/cache/tmp/holdfast-g14/run-gate.sh`, `setsid nohup`), so a second
  loss leaves the gate running and its log readable.
- D10 (2026-10-03): the owner's words, relayed at about 21:00Z: serve request #229 now, in its
  own PR, ahead of the rest of the goal. So D8's plan to ride the UI PR is replaced: the API
  branch's gate was stopped, `holdfast-g14/io-request` was gated and merged as #168, and the API
  gate runs after it on the new `main`. Spec 1.1.6 was read from the plugin's 1.5.0 directory and
  is followed from there: gates run as `goals lock goals-heavy -- goals lock holdfast-heavy`. The
  first gate of #168 ran with the tests' `TMPDIR` on the RAM tmpfs, as the update advised, and
  was red in 8 tests (`TestWatch_*` in `internal/engine`, two in `scripts/hwreport`): holdfast
  classifies a tmpfs as not positively local storage, so a root under one is not watched and a
  run over one is refused. That is the gate's environment and not the branch (unchanged at
  `e61df2d`), so it is no fix round. Gates now run with `GOTMPDIR` on the tmpfs and `TMPDIR` on
  disk, and are not declared io-light (the gate wrote 3.8 GB, `locks.log`).
- D5 (2026-10-03): the baseline's `internal/engine` took 2984.4 s, 82.9% of `TEST_TIMEOUT`, against
  1983.0 s in goal 13's last gate on the same tree. The difference is load: the four builders ran
  package tests beside it. §4 says to raise `TEST_TIMEOUT` once the engine passes 80%, in its own
  commit with the measurement, so the API track raises it, and this goal's gates run while no
  builder is running tests.

## Requests

Filed: none. Addressed to holdfast: request #229 from the goals program (2026-10-03, opened
during this goal): a mutation run's temp files on a tmpfs, its workers capped, and no
whole-package suite outside `goals-heavy`. State: DONE (PR #168, `59a73f3`; D8, D10), set with
`goals request state 229 done`. The owner's queue: no new item so far.

## Resume here

Request #229 is DONE (PR #168, `59a73f3`). Phase 2: PR #167 (`holdfast-g14/api-facts`) is merged up
to `main` at `64c655f` and pushed; its CI reruns on that head; its local gate is running detached
(`/cache/tmp/holdfast-g14/run-gate.sh`; log `api-gate.log`, ends with an `exit <n> WALL` line; the
gated SHA in `api-gate.sha`). When both are green: put the gate's last 20 lines, its wall-clock
and the `internal/engine` seconds in the PR body, then `gh pr merge 167 --squash --delete-branch`.
Phase 3: `holdfast-g14/ui-views` (worktree `/cache/wt/holdfast/g14-ui`, local only, head
`68cadf0`, which also carries the first commit of #168) holds the views, the controls, the docs
rewrite and the review's fixes. After #167 merges: merge `origin/main` into it, check it against
the merged API on a real `serve`, `make mutation-diff REF=origin/main`, the gate, PR, CI, merge.

## Supervisor actions

- 2026-10-03T20:48:24Z relaunched dead session 8d62d37d-3da2-4a4f-b9e4-3d2f6483ab9a in window holdfast-g14 with --resume (try 1 of 2)
- 2026-10-03T20:49:33Z re-issued the goal in the relaunched session
