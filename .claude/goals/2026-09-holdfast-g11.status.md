# Goal 11 ledger - worker nodes: protocol, shared mount, server re-gate

- Brief: `.claude/goals/2026-09-holdfast.md` §0, §4 and §15 (spec v1.1, the `/goals:spec` skill);
  goal file `.claude/goals/2026-09-holdfast-g11.goal.txt`. Amended by
  `.claude/goals/CHECKPOINT-T.approved`: "P4 proposal-node-protocol.md: approved, option (a):
  HTTP+JSON leases on the existing server, with a holdfast worker subcommand."
- Started: 2026-10-03, from `/workspace/holdfast` in the maker container, by the goals supervisor's
  `/goal` condition.
- Goal-start SHA: `8d9c23bfc40a05e6b700ba580f790b673e552b26` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g10.status.md` (ends
  `COMPLETE (goal 10): 2026-10-03`). The owner's queue at start (`goals needs list`): ten open
  holdfast items, queue #50 to #56 and #202 to #204.
- Precondition, as checked:

```
$ git fetch origin && git rev-parse HEAD origin/main
8d9c23bfc40a05e6b700ba580f790b673e552b26
8d9c23bfc40a05e6b700ba580f790b673e552b26
$ git grep -n "COMPLETE (goal 10)" origin/main -- .claude/goals/2026-09-holdfast-g10.status.md
origin/main:.claude/goals/2026-09-holdfast-g10.status.md:187:COMPLETE (goal 10): 2026-10-03
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
$ goals admit --agents 4
decision: admit a goal
agents: 4 of 4 allowed (requested)
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md`, line 64, "Per goal"):
"**Goal 11** (worker nodes: protocol, shared mount): none (S0163 lands first, in goal 2)." So line
B has no row to build.

P4 (`.claude/goals/2026-09-holdfast-research/proposal-node-protocol.md`), option (a), with its
eleven rules, is the protocol this goal builds. Goal 12 owns HTTP streaming of the source, the TLS
keys, `worker_insecure_http`, `docs/design/nodes.md`, the worker deployment in `docs/docker.md`
and the rewrite of the distributed statement in `README.md` and `docs/migration.md` (§16).

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g11-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g11/tmp` and the pinned dynamic-HDR tools on PATH. Logs under
`/cache/tmp/holdfast-g11/`.

| What | Value | Source |
|---|---|---|
| `make check` under `goals-heavy` then `flock -o` `holdfast-heavy` | exit 0 in 2647 s (lock wait included) | `gate-baseline.log` |
| `internal/engine` under `go test -race` (from that run) | ok, 89.0% coverage, 2251.7 s (62.5% of `TEST_TIMEOUT` 60m) | `gate-baseline.log` |
| `cmd/holdfast`, `internal/server` (from that run) | ok, 855.7 s; ok, 285.0 s | `gate-baseline.log` |
| `func Test` count, all packages | 1834 | `rg -c '^func Test' -g '*_test.go'`, `functest-start.txt` |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines | 66, 80 | `wc -l` |
| `CLAUDE.md` lines | 199 | `wc -l` |
| `TEST_TIMEOUT` | 60m | `Makefile:95` |
| `config.SecretBearingKeys` | 8 keys | `internal/config/config.go:817` |
| Usage headroom | five_hour 24%, seven_day 36%; 4 of 4 agents allowed | `goals admit --agents 4` |
| Host load at start | load average 53 on 56 CPUs (quota 28) | `uptime` |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`8d9c23b`): goal 10's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (the commit that adds this file): fast docs checks passed first |
| 1.3 | Baseline gate with timings | DONE (`8d9c23b`): `make check` exit 0 in 2647 s; the table above |

## Phase 2 - Triage rows (line B)

| # | Item | State |
|---|---|---|
| 2.1 | Rows P1 assigns to goal 11 | DONE (`proposal-triage.md` line 64): P1 assigns none |

## Phase 3 - The lease protocol (track `holdfast-g11/protocol`)

| # | Item | State |
|---|---|---|
| 3.1 | Durable lease rows in `internal/store`: id, job, node, epoch, expiry, state, temp, reserved bytes, digests | DONE (PR #157, `7b434e1`): schema v25, `store.LeaseLedger`; `TestLease_*` |
| 3.2 | `internal/node`: the lease state machine (grant, renew, expire, re-grant at a higher epoch, complete, fail), the caps, the digest-checked upload | DONE (PR #157): `TestNodeLease_*`, `TestNodeFixture_*`; mutation-diff 100.00% against the 70% floor (killed 229, lived 0); gate exit 0 in 2768 s (`internal/engine` 2281.6 s, `cmd/holdfast` 881.5 s, `internal/node` 5.0 s); CI green (`build`, `package`, `mutation`); 0 fix rounds (the review's fixes went in before the first gate) |
| 3.3 | `/api/node/v1` behind `requireNodeToken`; `node_token` by reference in `config.SecretBearingKeys`; 403 on every other group | DONE (PR #157): `TestNodeToken_CannotCallAControlOrReadEndpoint`, `TestNodeToken_OtherTokensCannotCallNodeEndpoints`, `TestNodeToken_UnsetAnswers403NamingTheKey`, `TestSecretKeys_NodeTokenIsSecretBearingAndRefusesALiteral`, `TestServe_RefusesANodeTokenThatResolvesToAnotherServerToken` |
| 3.4 | Fixtures: expired-lease upload (410, bytes discarded), stale epoch, duplicate upload, digest mismatch, a 404 body offered as media, 411/413, the caps | DONE (PR #157): `TestNodeFixture_UploadOnAnExpiredLeaseIs410AndLeavesNoFile`, `..._AStaleEpochAfterARegrantIs410`, `..._ADuplicateUploadIs200AndRewritesNothing`, `..._ADigestMismatchIsRefusedAndTheTempDeleted`, `..._A404BodyOfferedAsMediaIsRefused`, `..._AFreeSpaceRefusalAtUploadStartIs503`, `..._AServerRestartKeepsLiveLeases` and the rest in `internal/node` |
| 3.5 | `make api-schema-diff`: additions only, `.api-schema-breaks.yaml` still `[]` | DONE (PR #157): 19 additions, 0 breaks |
| 3.6 | Adversarial review of the branch before its gate | DONE (PR #157, `f44d94c`, `52dcd2b`): no HIGH, 2 MED, 8 LOW, all fixed; the proven ones are permanent tests in `internal/node/regress_test.go`. Not tested: a failing `fsync` or `close` on the upload (answered 500 and logged) |

## Phase 4 - The worker, the engine seam and the server re-gate (track `holdfast-g11/worker`)

| # | Item | State |
|---|---|---|
| 4.1 | The engine hands a job's encode to a node at its one encode seam; the temp, the reservation, every gate and the rename stay `ProcessFile`'s | TODO |
| 4.2 | `holdfast worker`: acquire, heartbeat, read the source through the mount by its path map, encode, upload, complete or fail | TODO |
| 4.3 | End to end on loopback with a real tiny encode over a shared mount with a path map (line C) | TODO |
| 4.4 | Fixtures: a free-space reservation refusal at grant and at upload start; a server restart with live leases | TODO |
| 4.5 | The retry bound: an expiry, a `fail` or a refused upload counts against `max_failures`; the park names the node attempts | TODO |

## Phase 5 - Report

| # | Item | State |
|---|---|---|
| 5.1 | Gate integrity counted from the goal-start SHA | TODO |
| 5.2 | Adversarial review of the report | TODO |

## Decisions taken

- D1 (2026-10-03): tracks. `holdfast-g11/protocol` (the store's lease rows, `internal/node`, the
  `/api/node/v1` group, `node_token`, the protocol fixtures), then `holdfast-g11/worker` (the
  engine seam, `holdfast worker`, the loopback run, the reservation and restart fixtures). They
  run one after the other because the second builds on the first's API; each is built by one
  agent in its own worktree, reviewed by a fresh adversarial agent, and merged by the rule of
  §0.3.
- D2 (2026-10-03): where a node fits. The engine has one encode seam (`Engine.encode`): everything
  before it (the guards, the claim, the server-named temp, the free-space hold, the owner record)
  and everything after it (every gate, the play hold, the copy beside the source, the rename) is
  `ProcessFile`'s and stays so. A node job is a `ProcessFile` whose encode step is "lease it, wait
  for the upload into the temp this job already named"; the gates and the rename that follow are
  the code a local job runs, so the swap invariant text and `docs/design/swap.md` are untouched
  (T16). Reasoning: this entry and the track's PR body.
- D3 (2026-10-03): what a lease carries. The server derives the encode plan as it does for a
  local job and sends the node the ffmpeg argument list that plan builds (the options before the
  input and the options between input and output), not a second description of the plan; the
  node substitutes only its own source path (through its path map) and its own local output path.
  The gates read the server's plan, so "the command line and every gate read one plan" still
  holds. The libx265 pool figures are the node's own (the server sends none).
- D4 (2026-10-03): which jobs are leased in this goal. Only a plan whose command line is
  self-contained on another host: a software encoder, software decode, no loudness
  normalisation (its gate reads the encoder's own report channel), no dynamic-HDR carriage (its
  pre-pass files sit beside the working file) and no picture carried as a Matroska attachment.
  Every other job is encoded by the server itself, exactly as today (fail-safe: never a guess on
  the node). Hardware encoders on a node need the node's own start-time probe to gate the job
  (`docs/design/hardware.md#probe`); that is listed under "Proposals awaiting the owner".
- D5 (2026-10-03): nodes are off until `node_token` is set (I5): with it unset the engine's pools,
  argv and decisions are byte for byte today's and `/api/node/v1` answers 403 naming the key.
- D6 (2026-10-03): at the retry bound the row is parked `failed` by the existing `Claim`
  (`max_failures`), as P4 rule 3 says, with a logged reason naming the node attempts; every later
  pass then holds the file out. The goal file's "ends in a SKIP" is read as that: P4 as approved
  is the detail the goal file defers to ("per the approved P4").
- D7 (2026-10-03): the path map lives in the worker's configuration (`worker_path_map`, the
  `config.PathMap` type the media clients already use), because the node is the host that knows
  its own mounts; a wrong map is caught by the source digest the server compares before the gates
  (P4 rule 5).
- D8 (2026-10-03): until goal 12 builds the TLS stance and `worker_insecure_http`, the worker
  refuses a plain `http://` server that is not loopback outright (fail-safe: the token never
  crosses a network in cleartext by default); `https://` works through any reverse proxy.
- D9 (2026-10-03): every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g11/`
  and `PATH=/cache/opt/dynhdr-tools/bin:$PATH` (goal 9's finding), goal shells use `rg` or
  `git grep`, and commits carry no trailer (T38, `CLAUDE.md`).
- D10 (2026-10-03): PR #156 (`speed/gate`) is another session's and was open at this goal's start;
  this goal touches none of its branch or files (§0.13).

- D11 (2026-10-03): PR #157's own decisions are in `docs/design/nodes.md#leases` and its PR body: a
  lease removes its working file only while the engine waits on it (a lease that ends late removes
  nothing); `Recover` itself removes the working file of an `uploaded` lease it ends, the one
  departure from "leave it to the startup sweep" (the row is the record, and a full-length ungated
  temp would otherwise be held as a stray replacement); an unknown lease id is 404, a digest
  mismatch 400, a stalled upload 408; a failed free-space lookup refuses nothing, as the engine's
  own check does; terminal lease rows older than 7 days (`ASSUMED`) are pruned except each path's
  newest.
- D12 (2026-10-03): the 22 `*_test.go` lines PR #157 deleted are the "newest migration" fixtures
  following the migrations to v25 (`cmd/holdfast/export_test.go`, `internal/store/migrate_test.go`,
  `readonly_test.go`, `resolution_migrate_test.go`: wind-back `DROP` statements, the newest step's
  table name, comments). No assertion is weaker; the review checked each line.
- D13 (2026-10-03): the `holdfast-g11/worker` branch was started on the protocol branch before
  #157 merged (to save hours); it takes `origin/main` by the checked merge of goal 10's D10.

- D14 (2026-10-03): the retry bound, refined after the worker branch's adversarial review proved
  that one node with a wrong path map fails every lease in milliseconds and so parks the library
  (HIGH for availability, no data risk). P4 rule 3 charges "an expiry, a `fail`, or a refused
  upload" to the job. As built: a lease that was really attempted (expiry, an encode failure on
  the node, the digest-mismatch bound, a source digest or output size the server finds wrong) is
  charged, as P4 says; a lease the node could not run at all (unmapped or mismatched source,
  unsupported encoder, a refused plan, the worker stopping, a poll that left) is NOT charged: the
  server encodes that job itself in the same attempt, and a node with 3 such endings in a row
  (`ASSUMED`) is offered nothing for 5 minutes (`ASSUMED`). This departs from the letter of P4
  rule 3 for the second kind and is listed under "Proposals awaiting the owner".
- D15 (2026-10-03): the same review's other findings are fixed on the branch before its gate: a
  poll that left while reserved never costs the file a failure; `Ready` re-graces recovered
  leases and adoption runs them concurrently; the worker refuses a lease whose argument list
  names another input, an attachment, an absolute path or anything before the input; the worker
  refuses redirects and a `worker_server` with userinfo; its work directory is swept at start.

## Proposals awaiting the owner

- Hardware encoders on a node (D4): a node's own start-time probe would have to gate the job.
- Which lease endings count against a file's `max_failures` (D14): as built, only a lease that was
  really attempted; P4 rule 3 as written charges every `fail`.
- No maximum lease lifetime: a node that keeps heartbeating holds its job, as a hung local encode
  holds a worker today.
- Files queued by `POST /api/scan`, a webhook or the watch are encoded by the server; only a
  pass's feed is offered to nodes.

## Resume here

PR #157 (protocol) is merged at `7b434e1`. The `holdfast-g11/worker` builder agent is finishing in
`/cache/wt/holdfast/g11-worker` (branch `holdfast-g11/worker`, stacked on the old protocol
branch). Next: merge `origin/main` into it (D13), a fresh adversarial review, the gate
(`/cache/tmp/holdfast-g11/gate.sh <worktree> <log>`), the PR, CI, merge; then phase 5.
