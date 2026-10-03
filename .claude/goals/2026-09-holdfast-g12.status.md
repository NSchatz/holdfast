# Goal 12 ledger - worker nodes: HTTP streaming, TLS and deployment

- Brief: `.claude/goals/2026-09-holdfast.md` §0, §4 and §16 (spec v1.1, the `/goals:spec` skill);
  goal file `.claude/goals/2026-09-holdfast-g12.goal.txt`. Amended by
  `.claude/goals/CHECKPOINT-T.approved`: "P4 proposal-node-protocol.md: approved, option (a):
  HTTP+JSON leases on the existing server, with a holdfast worker subcommand."
- Started: 2026-10-03, from `/workspace/holdfast` in the maker container, by the goals supervisor's
  `/goal` condition.
- Goal-start SHA: `e37599788f4bc69930b6c906ef0b8b19306d3764` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g11.status.md` (ends
  `COMPLETE (goal 11): 2026-10-03`). The owner's queue at start (`goals needs list`): ten open
  holdfast items, queue #50 to #56 and #202 to #204.
- Precondition, as checked:

```
$ git fetch origin && git rev-parse HEAD origin/main
e37599788f4bc69930b6c906ef0b8b19306d3764
e37599788f4bc69930b6c906ef0b8b19306d3764
$ git grep -n "COMPLETE (goal 11)" origin/main -- .claude/goals/2026-09-holdfast-g11.status.md
origin/main:.claude/goals/2026-09-holdfast-g11.status.md:233:COMPLETE (goal 11): 2026-10-03
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
$ goals admit --agents 4
decision: admit a goal
agents: 4 of 4 allowed (requested)
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md`, line 65, "Per goal", last
changed in `d4d70a6`): "**Goal 12** (worker nodes: streaming, TLS): none." So line B has no row to
build.

P4 (`.claude/goals/2026-09-holdfast-research/proposal-node-protocol.md`), option (a): this goal
builds what goal 11 left of it - the http mode of rule 1 (`GET /api/node/v1/leases/{id}/source`)
with the source digest of rule 5 and the worker half of rule 6, and the TLS stance of rule 10
(`server_tls_cert`, `server_tls_key` by reference, `worker_insecure_http`, the warnings in
`docs/docker.md` and `docs/design/nodes.md`); and §16 item 4, the rewrite of the distributed
statement in `README.md` and `docs/migration.md` and the worker deployment in `docs/docker.md`.

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g12-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g12/tmp` and the pinned dynamic-HDR tools on PATH. Logs under
`/cache/tmp/holdfast-g12/`.

| What | Value | Source |
|---|---|---|
| `make check` under `goals-heavy` then `flock -o` `holdfast-heavy` | exit 0 in 2476 s (lock wait included) | `gate-baseline.log` |
| `internal/engine` under `go test -race` (from that run) | ok, 89.1% coverage, 2293.1 s (63.7% of `TEST_TIMEOUT` 60m) | `gate-baseline.log` |
| `cmd/holdfast`, `internal/server`, `internal/node`, `internal/nodeworker` (from that run) | ok, 847.1 s; 302.7 s; 4.3 s; 1.8 s | `gate-baseline.log` |
| `func Test` count, all packages | 1958 | `rg -c '^func Test' -g '*_test.go'`, `functest-start.txt` |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines | 66, 80 | `wc -l` |
| `CLAUDE.md` lines | 199 | `wc -l` |
| `config.SecretBearingKeys` | 9 keys | `internal/config/config.go` |
| Usage headroom | five_hour 15%, seven_day 47%; 4 of 4 agents allowed | `goals admit --agents 4` |
| Host load at start | load average 18 on 56 CPUs (quota 28) | `uptime` |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`e375997`): goal 11's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (the commit that adds this file): fast docs checks passed first |
| 1.3 | Baseline gate with timings | DONE (`e375997`): `make check` exit 0 in 2476 s; the table above |

## Phase 2 - Triage rows (line B)

| # | Item | State |
|---|---|---|
| 2.1 | Rows P1 assigns to goal 12 | DONE (`proposal-triage.md` line 65): P1 assigns none |

## Phase 3 - HTTP-streaming mode and the TLS stance (track `holdfast-g12/transport`)

| # | Item | State |
|---|---|---|
| 3.1 | `GET /api/node/v1/leases/{id}/source` on a live lease at its epoch; the server hashes what it streams; `worker_mode: http` downloads, checks and encodes with no mount | DONE (PR #159, `f48d4e0`): `internal/node/source.go`, `internal/nodeworker/source.go`; `TestNodeFixture_AnHTTPModeLeaseIsGrantedInHTTPModeAndItsSourceIsStreamedAndHashed`, `TestWorker_HTTPMode_DownloadsTheSourceEncodesItAndReportsTheDigestOfWhatArrived`; gate exit 0 in 2542 s on `b957e1d` (`internal/engine` 2373.4 s, 65.9% of `TEST_TIMEOUT`; `cmd/holdfast` 985.2 s; `internal/node` 6.0 s; `internal/nodeworker` 2.8 s); CI green (`build`, `package`, `mutation`); mutation-diff 100.00% against the 70% floor (killed 118, lived 0); 0 fix rounds |
| 3.2 | The source digest checked both ways, the output digest as in goal 11; fixtures for an error page offered as the source, a short source and a source changed in transit | DONE (PR #159): `TestNodeLease_ACompletionIsHeldToWhatTheServerStreamed`, `TestNodeFixture_ASourceChangedInTransitFailsTheLeaseBeforeAnyGate`, `TestWorkerFixture_HTTPMode_AnAnswerThatIsNotTheSourceIsNeverEncoded` (12 cases: the 55-byte `404`, short, long, wrong length), `TestNodeFixture_ASourceRequestOnALeaseThatIsNotLiveIs410NoStore`, `TestNodeFixture_AMappedLeasesSourceIsRefused`, `TestNodeFixture_ARangedSourceRequestRecordsNoStreamedDigest`, `TestWorkerFixture_HTTPMode_ARangedDownloadIsStillProvenByTheServersOwnHash`, `TestNodeFixture_SourceStreamsAndUploadsShareTheTransferCap`, `TestNodeFixture_AStalledSourceDownloadIsCutByTheWriteDeadline` |
| 3.3 | End to end on loopback in http mode with a real tiny encode (line C) | DONE (PR #159): `TestWorkerEndToEnd_HTTPModeNoMountSourceAndOutputDigestsCheckedBothWaysServerRegatesAndRenames` in `cmd/holdfast` (the real `serve` and `worker`; the worker has no mount; the lease row's source and output digests are the true sha-256 figures; the server ran no encode and its rename moved the working file's inode). Removing either transport comparison turns it red by a named assertion (the builder's mutations, D8) |
| 3.4 | Built-in TLS: `server_tls_cert`, `server_tls_key` by reference in `config.SecretBearingKeys`; `worker_tls_ca`; the worker refuses plain `http://` to a non-loopback server unless `worker_insecure_http: true`, logged at every start (line D) | DONE (PR #159): `cmd/holdfast/tls.go`; `TestWorkerEndToEnd_TLS_ServeWithItsOwnCertificateAndAWorkerThatTrustsIt`, `TestServeTLS_APairThatCannotBeListenedWithRefusesToStart`, `TestWorker_TLS_AServerWhoseCertificateIsNotTrustedReceivesNoRequest`, `TestSecretKeys_ServerTLSKeyIsSecretBearingAndRefusesALiteral`, `TestServerTLS_BothOrNeither`, `TestWorker_RefusesPlainHTTPToANonLoopbackServer` (goal 11's, unchanged), `TestWorker_InsecureHTTPIsAnOverrideSaidLoudlyAtEveryStart`, `TestWorker_TransportAndModeRefusalsAndTheInsecureOverride` |
| 3.5 | `make api-schema-diff`: additions only | DONE (PR #159): 20 additions, 0 breaks; `.api-schema-breaks.yaml` still `[]` |
| 3.6 | Adversarial review of the branch before its gate | DONE (PR #159, `7ae9c7e`): no HIGH, 1 MED, 5 LOW, all fixed with a fixture each (D9) |

## Phase 4 - The documents (track `holdfast-g12/docs`)

| # | Item | State |
|---|---|---|
| 4.1 | The distributed statement rewritten in `README.md` and `docs/migration.md` | DONE (PR #160, `e679d0a`): `README.md` lines 102 and 174 (`#worker-nodes`), `docs/migration.md` line 132 (`#server-and-nodes`); gate exit 0 in 2572 s on `add09f4` (`internal/engine` 2384.6 s, 66.2% of `TEST_TIMEOUT`; `cmd/holdfast` 908.3 s); CI green (`build`, `package`, `mutation`); 0 fix rounds |
| 4.2 | `docs/design/nodes.md`: the transport rule with anchors and the warnings of P4 rule 10; its `CLAUDE.md` line | DONE (PR #159, `f48d4e0`): `docs/design/nodes.md#transport` and `#http-mode`; `CLAUDE.md` line 71 (199 lines) |
| 4.3 | `docs/docker.md`: a worker deployment (both modes, TLS or a reverse proxy, the read token, the read-only mount) | DONE (PR #160, `e679d0a`): `docs/docker.md#worker-nodes`, line 515 on; checked against #159 as merged (every linked `docs/design/nodes.md` anchor exists) |
| 4.4 | The owner's queue: a run against a real second host (goal 11's D19) | DONE (queue #219, kind physical): one worker on a real second host, once in each mode; T49 keeps it out of every goal |

## Phase 5 - Report

| # | Item | State |
|---|---|---|
| 5.1 | Gate integrity counted from the goal-start SHA | DONE (counted at `e679d0a`): `func Test` 1958 -> 2004, no package fell (`cmd/holdfast` 264 -> 269, `internal/config` 170 -> 174, `internal/engine` 597 -> 598, `internal/node` 54 -> 73, `internal/nodeworker` 23 -> 40, every other package unchanged); `git diff --numstat e375997 origin/main -- docs/design/swap.md docs/design/quality-gate.md` empty (66 and 80 lines); `*_test.go` +3463 -15, the 15 deleted lines in D10; zero `co-authored-by` in `git log e375997..origin/main --format=%B` |
| 5.2 | Adversarial review of the report | DONE: a fresh subagent (2026-10-03) checked lines A-G against `3f3a5e6` and GitHub in its own copy of the tree: it re-ran the line C and D tests (`-count=1`), mutated the product code six times and saw each go red by a named assertion and none by a timeout (the streamed-digest comparison at `complete`, the upload's `Content-Digest` comparison, the engine's own source hash, `CheckTransport` accepting everything, the warning dropped, `server_tls_key` out of `config.SecretBearingKeys`), recounted `func Test` per directory, listed the 15 deleted test lines, ran `run`, `serve`, `plan` and `analyze` against a rootless http-worker file (each refuses), and checked the merge rule of PRs #159 and #160: "VERDICT: none false". LOW, recorded (D14) |

## Decisions taken

- D1 (2026-10-03): tracks. `holdfast-g12/transport` (the http mode, the source digest, the TLS
  keys and the worker's transport refusals, with their reference documents) and
  `holdfast-g12/docs` (the statement rewrite, the design document's transport rule, the worker
  deployment). They are built side by side in their own worktrees on key names fixed here, each
  reviewed by a fresh adversarial agent; the documents merge second, after they are checked
  against the merged code.
- D2 (2026-10-03): names. Server: `server_tls_cert` (a plain path) and `server_tls_key` (a secret
  reference), both or neither, as P4 rule 10 names them. Worker: `worker_mode` (`mapped`, the
  default, or `http`), `worker_insecure_http` (P4's name) and `worker_tls_ca` (a plain path to a
  PEM bundle the worker trusts beside the system roots: P4's test plan has "the worker trusting
  its certificate", and a server with a private certificate is otherwise unreachable over TLS).
  Certificate verification is never switched off: there is no skip-verify key.
- D3 (2026-10-03): the mode is the node's choice, per node (T17), stated in every request for
  work; the server adds no key to permit http mode. P4 rule 10 already says what a `node_token`
  can read in http mode and the documents carry that warning; a lease's source endpoint serves
  only the source of a live lease at its epoch, never a path a request names.
- D4 (2026-10-03): a source download restarts from zero and is not resumed by the worker (the
  proposal's own stance for uploads). The server still answers `Range` through
  `http.ServeContent`, and whatever was streamed it hashes its own copy of the source once before
  the gates, as it does in mapped mode, so a ranged read can never go unchecked.
- D5 (2026-10-03): every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g12/`
  and `PATH=/cache/opt/dynhdr-tools/bin:$PATH`, goal shells use `rg` or `git grep`, and commits
  carry no trailer (T38, `CLAUDE.md`).
- D6 (2026-10-03): PR #156 (`speed/gate`) and the dependency PRs #108 to #110 are not this
  goal's and were open at its start; this goal touches none of their branches (§0.13).

- D7 (2026-10-03): PR #159's own decisions, in its body and `docs/design/nodes.md`: no schema
  change - a lease's mode and streamed digest live in the hub's memory, so after a restart a
  recovered lease's source is refused rather than served on a guess (the job is then proven by the
  server's own hash, or the server encodes it); the comparison with the streamed digest is taken
  at `complete`, inside the lease's transaction; the source is served only while the lease is
  `granted`; only a `200` is media for the worker, never a `206` (a departure from the letter of
  P4 rule 6, which also names `206`, following D4: the worker sends no `Range`); the
  `Repr-Digest` trailer goes out only where the protocol carries one and is never load-bearing;
  new lease endings that charge the file nothing: `source_download_failed`, `work_dir_full`,
  `source_withdrawn`; `ASSUMED` values: 3 download attempts, a 60 s idle cut on the worker, the
  source write deadline of 30 s plus 1 s per 64 KiB (the upload's figures); `holdfast validate`
  accepts an http worker's file with no `library_roots` and runs the worker's own start refusals
  on it.
- D8 (2026-10-03): line C's proof that both comparisons bite is the builder's, on the branch
  before the review: with the streamed comparison removed the test fails "the server did not
  compare the node's source digest with the digest of what it streamed"; with the upload's
  `Content-Digest` comparison removed, "the server did not hold the upload to its Content-Digest";
  the engine's own-hash comparison is held by
  `TestWorkerFixture_HTTPMode_ARangedDownloadIsStillProvenByTheServersOwnHash`. The report's
  reviewer repeats them.
- D9 (2026-10-03): the branch review's findings, all fixed in `7ae9c7e`. MED: a `503` for a full
  transfer cap counted as a download attempt, so good http leases failed and good nodes cooled
  off at the shipped caps; a `503` is now waited out. LOW: a link swapped in at the leased path
  was followed (now opened with `O_NOFOLLOW`; the engine already skips linked sources);
  conditional request headers drew undeclared `304` and `412` (now ignored); a source the server
  withdrew cooled the node off (now `source_withdrawn`, which does not); the room check was per
  lease (now reserved per worker, and a full disk while writing is `work_dir_full`); `validate`
  passed a file `worker` refuses (both now run `workerPreflight`).
- D10 (2026-10-03): the 15 `*_test.go` lines PR #159 removed against the goal-start tree, each
  replaced: six in `internal/node/scenarios_test.go` (two pipe writes now go through a helper
  that fails by name instead of blocking, goal 11's D21); five where
  `TestNodeFixture_HTTPModeIsRefusedNamingMappedMode` became
  `TestNodeFixture_AnUnknownModeIsRefusedNamingBothModes` (http is served now; `"HTTP"`, `""` and
  an unknown word are still refused); four in `internal/server/node_token_test.go` (the lease
  routes are six, not five). No assertion is weaker.
- D11 (2026-10-03): of goal 11's D21, one fixture is fixed
  (`TestNodeFixture_UploadOnAnExpiredLeaseIs410AndLeavesNoFile` now reds by named assertion); the
  two in `internal/engine/nodes_test.go` are unchanged and stay recorded there.

- D12 (2026-10-03): PR #160 was built beside #159 on the names of D2 and merged second, after its
  statements were checked against #159 as merged and three sentences were added for what the
  review changed (what an http-mode worker leaves behind, re-sends and refuses for room). Its
  gate ran on the branch merged up to `f48d4e0`; `main` then moved only by a ledger commit, so the
  result stood (brief §0.3).
- D13 (2026-10-03): no minor release is cut by this goal (T37 optional): the release is goal 15's,
  and the owner's queue item #219 is worded for "the next release".

- D14 (2026-10-03): what the report's adversarial review left, all LOW and none acted on: the
  line C test shows "the worker has no mount" by its configuration and by the node's own ffmpeg
  log, not by OS isolation (one host, one uid) - queue #219 is the run on a real second host; the
  hub's streamed-digest comparison does not apply after a ranged read or a restart, where the
  engine's own hash is the one source check (documented in `docs/design/nodes.md#http-mode`, held
  by its own fixture); `CLAUDE.md` is at 199 of its 200 lines; one test file uses a private-range
  example address where the documents use RFC 5737 ones. The session died once during this
  review and was relaunched by the supervisor ("Supervisor actions"); the review was resumed and
  nothing was lost.

## Proposals awaiting the owner

Carried from goal 11, unchanged: hardware encoders on a node; which lease endings count against a
file's `max_failures`; the park at the retry bound; no maximum lease lifetime; files queued
outside a pass are encoded by the server.

New in this goal:

- No server key forbids http mode (D3): any holder of `node_token` can read the source of a job
  leased to it. A key that keeps a server mapped-only would be an addition.
- The worker takes only a `200` as the source, never a `206` (D7), where P4 rule 6 names both.
- A lease's mode is not durable (D7): a server restart mid-download costs that node the
  download, never the file a failure.

## Supervisor actions

- 2026-10-03T13:49:59Z relaunched dead session 742ef52d-c2be-48cb-92bd-62d2ad74f9de in window holdfast-g12 with --resume (try 1 of 2)
- 2026-10-03T13:52:46Z re-issued the goal in the relaunched session

## Resume here

Both PRs are merged (#159 at `f48d4e0`, #160 at `e679d0a`); no branch, worktree or open PR of
this goal remains (#156 and #108 to #110 are not this goal's). Queue #219 is filed. The
adversarial review found none false. Goal complete.

COMPLETE (goal 12): 2026-10-03
