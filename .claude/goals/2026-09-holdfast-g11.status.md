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
| 4.1 | The engine hands a job's encode to a node at its one encode seam; the temp, the reservation, every gate and the rename stay `ProcessFile`'s | DONE (PR #158, `81d7c1b`): `internal/engine/nodes.go` (`encodeAt`, `leasable`, `encodeOnNode`, `startFeeders`, `nodeGate`, `AdoptLeases`); `TestNodes_OffByDefaultThePoolAndArgvAreUnchanged`, `TestNodes_AJobThatIsNotLeasableIsEncodedByTheServer`, `TestNodes_TheWrongSourceDigestFailsBeforeTheGates`, `TestNodes_ANodesCommandLineIsTheServersOwn`, `TestNodes_FeedersLeakNoGoroutineAndNoTicket`, `TestNodes_TheNodeGateBoundsTheServersOwnWork` |
| 4.2 | `holdfast worker`: acquire, heartbeat, read the source through the mount by its path map, encode, upload, complete or fail | DONE (PR #158): `internal/nodeworker`, `cmd/holdfast/worker.go`; `TestWorker_RefusesPlainHTTPToANonLoopbackServer`, `TestWorker_AnUnmappedSourceFailsTheLeaseAndNeverGuesses`, `TestWorkerFixture_AnAcquireAnswerThatIsNotALeaseIsNeverEncoded`, `TestWorker_ALeasedCommandLineOutsideTheServersShapeIsRefusedUnrun`, `TestWorker_ARedirectIsNeverFollowed`; mutation-diff 100.00% against the 70% floor (killed 132, lived 0) |
| 4.3 | End to end on loopback with a real tiny encode over a shared mount with a path map (line C) | DONE (PR #158): `TestWorkerEndToEnd_SharedMountPathMapServerRegatesAndRenames` (the real `serve` and `worker` commands; the server ran no encode for the file, ran its decode-integrity pass and its VMAF run on its own temp and source path, and the final file's inode is the temp's) and `TestWorkerEndToEnd_AWorkerVerdictNeverLicensesASwap`; gate exit 0 (run 1 on `e539a97` in 2511 s, `internal/engine` 2366.4 s; final run on `26fd334` in 926 s with `cmd/holdfast` 797.9 s and the unchanged packages reused from the run on `7e38518`, `internal/engine` 2314.3 s, 64% of `TEST_TIMEOUT`); CI green; 2 fix rounds (D16) |
| 4.4 | Fixtures: a free-space reservation refusal at grant and at upload start; a server restart with live leases | DONE (PRs #157, #158): `TestWorkerFixture_AFreeSpaceReservationRefusalAtGrantIs503AndTheSourceIsUntouched`, `TestNodeFixture_AFreeSpaceRefusalAtUploadStartIs503`, `TestWorkerFixture_AServerRestartWithLiveLeasesAdoptsOrAbandonsBeforeAnyGrant`, `TestNodeFixture_AServerRestartKeepsLiveLeases`, `TestNodes_ARestartTakesTwoLiveLeasesBackAtOnce`, `TestNodes_ARecoveredLeaseWhoseJobDoesNotComeBackIsAbandonedBeforeReady`; and `TestWorkerFixture_A404BodyOfferedAsMediaFailsTheServersGates`, `..._AnExpiredLeaseUploadIsDiscardedAndTheJobIsRetried`, `..._ADuplicateUploadFromTheRealWorkerRewritesNothing`, `..._ADigestMismatchIsRetriedWithinTheLeaseThenFails` |
| 4.5 | The retry bound: an expiry, a `fail` or a refused upload counts against `max_failures`; the park names the node attempts | DONE (PR #158): as refined by D14; `TestWorkerFixture_TheRetryBoundParksTheFileNamingTheNodeAttempts`, `TestNodes_OneMisconfiguredWorkerDoesNotParkTheLibrary`, `TestNodes_ANodeThatCannotRunALeaseCostsTheFileNothing`, `TestNodes_APollThatLeftCostsTheFileNothing` |
| 4.6 | Adversarial review of the branch before its gate | DONE (PR #158): no route past the server's gates; 1 HIGH (availability), 3 MED, 9 LOW, all fixed on the branch (D14, D15) |

## Phase 5 - Report

| # | Item | State |
|---|---|---|
| 5.1 | Gate integrity counted from the goal-start SHA | DONE (counted at `81d7c1b`): `func Test` 1834 -> 1958, no package fell (`cmd/holdfast` 259 -> 264, `internal/config` 163 -> 170, `internal/engine` 574 -> 597, `internal/node` 0 -> 54, `internal/nodeworker` 0 -> 23, `internal/server` 125 -> 129, `internal/store` 155 -> 163, every other package unchanged); `git diff --numstat 8d9c23b origin/main -- docs/design/swap.md docs/design/quality-gate.md` empty (66 and 80 lines); `*_test.go` +8471 -23, the 23 deleted lines in D12 and D17; zero `co-authored-by` in `git log 8d9c23b..origin/main --format=%B` |
| 5.2 | Adversarial review of the report | DONE: a fresh subagent (2026-10-03) checked lines A-G against `8629e94` and GitHub in its own copy of the tree, re-running the line C, D and E tests (`-count=1`), mutating the product code to see fixtures go red (the digest comparison, the free-space refusal at upload start, the duplicate upload: 6 fixtures failed by assertion), recounting `func Test` per directory, listing the 23 deleted test lines, and checking the merge rule of PRs #157 and #158: "VERDICT: none false". MEDIUM, recorded (D21): two fixtures go red only by hanging to their timeout, and the engine-level expired-lease fixture stayed green when only the expiry comparison was removed (its `internal/node` twin covers the item). LOW, taken: the report says under F that `waitHTTP` gained a one-minute floor, and keeps the cached final gate run visible. LOW, recorded: the read group is open when `server_read_token` is unset (as before this goal), so the node token is refused there only where a read token is set |

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

- D16 (2026-10-03): PR #158's two fix rounds. Round 1: the local gate passed, CI's `build` was
  red - a new engine test failed on the runner and its cleanup then waited on a long-poll until
  the package's 60m timeout; fixed in `7e38518` (the assertion no longer races the feeders, and
  no test this goal added can block past a deadline in cleanup). Round 2: CI was green, the local
  gate was red under host load on an existing serve test's 3-second readiness wait
  ("server never became ready"); fixed in `26fd334` by a one-minute floor in the test helper
  `waitHTTP`, which returns on the first 200. Both red tails are comments on the PR. The final
  local run reused Go's cached results for the packages the last commit did not touch.
- D17 (2026-10-03): the one `*_test.go` line PR #158 deleted against the goal-start tree is
  `cmd/holdfast/flags_test.go`'s `for _, cmd := range []string{"run", "serve", "validate"} {`,
  which gained `worker`: the pinned flag list now covers the new command too. (PR #158 also
  rewrote 17 lines of `internal/node/scenarios_test.go`, a file this goal created in #157: a
  reserved poll is now answered 204 at its bound and a dead ticket grants nothing.)
- D18 (2026-10-03): PR #158's own decisions are in its PR body and `docs/design/nodes.md`: the
  worker's configuration goes through the shared loader, so it names its own mount as a library
  root; the worker hashes the source file after the encode; stream-copy plans are not leased; the
  park record names the node attempts this process saw; `internal/engine` imports `internal/node`
  for the job and ticket types.
- D19 (2026-10-03): no item is added to the owner's queue by this goal. The only physical step
  is a run against a real second host, which T49 keeps out of every goal and which waits for
  goal 12 (until then the worker refuses plain `http://` to a server that is not loopback, D8);
  goal 12 files it with the deployment it documents. The ten open holdfast items (#50-#56,
  #202-#204) are unchanged.
- D20 (2026-10-03): no minor release is cut (T37 optional): nodes are off by default and the
  HTTP mode, the TLS stance and the deployment docs are goal 12's.

- D21 (2026-10-03): what the report's adversarial review left open, for goal 12, which works in
  the same packages: `TestNodeFixture_UploadOnAnExpiredLeaseIs410AndLeavesNoFile` and
  `TestWorkerFixture_AFreeSpaceReservationRefusalAtGrantIs503AndTheSourceIsUntouched` go red on
  their regression by running into their timeout rather than by a named assertion, and
  `TestWorkerFixture_AnExpiredLeaseUploadIsDiscardedAndTheJobIsRetried` did not go red when only
  the expiry comparison in `checkLive` was removed (the sweep and the attached-wait check refuse
  the same upload). Also noted: `worker_slots` above 1 needs `node_max_leases_per_node` raised
  to match; `node_max_leases` and `node_gate_slots` are keys beyond the two P4 names (additions);
  this goal created `docs/design/nodes.md` (goal 12 adds the transport warnings of P4 rule 10).

## Proposals awaiting the owner

- Hardware encoders on a node (D4): a node's own start-time probe would have to gate the job.
- Which lease endings count against a file's `max_failures` (D14): as built, only a lease that was
  really attempted; P4 rule 3 as written charges every `fail`.
- At the retry bound the row is parked `failed` (P4 rule 3), where the goal file says "ends in a
  SKIP" (D6).
- No maximum lease lifetime: a node that keeps heartbeating holds its job, as a hung local encode
  holds a worker today.
- Files queued by `POST /api/scan`, a webhook or the watch are encoded by the server; only a
  pass's feed is offered to nodes.

## Resume here

Both PRs are merged (#157 at `7b434e1`, #158 at `81d7c1b`); no branch, worktree or open PR of this
goal remains (#156 is another session's). The adversarial review found none false. Goal complete.

COMPLETE (goal 11): 2026-10-03
