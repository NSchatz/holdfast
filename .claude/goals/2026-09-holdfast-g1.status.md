# Goal 1 ledger - start-up, triage and proposals

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §5; goal file
  `.claude/goals/2026-09-holdfast-g1.goal.txt`. Ends at Checkpoint T (§20); this goal never writes
  `CHECKPOINT-T.approved`.
- Started: 2026-09-29.
- Goal-start SHA: `4ac983a24061d76baf8699a6f49b689341098ef0` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: none (this is goal 1). `NEEDS-OWNER.md` at start: header only, no rows.
- Precondition, as checked:

```
$ git ls-tree --name-only origin/main .claude/goals/
.claude/goals/2026-09-holdfast-g1.goal.txt
.claude/goals/2026-09-holdfast-g10.goal.txt
.claude/goals/2026-09-holdfast-g11.goal.txt
.claude/goals/2026-09-holdfast-g12.goal.txt
.claude/goals/2026-09-holdfast-g13.goal.txt
.claude/goals/2026-09-holdfast-g14.goal.txt
.claude/goals/2026-09-holdfast-g15.goal.txt
.claude/goals/2026-09-holdfast-g2.goal.txt
.claude/goals/2026-09-holdfast-g3.goal.txt
.claude/goals/2026-09-holdfast-g4.goal.txt
.claude/goals/2026-09-holdfast-g5.goal.txt
.claude/goals/2026-09-holdfast-g6.goal.txt
.claude/goals/2026-09-holdfast-g7.goal.txt
.claude/goals/2026-09-holdfast-g8.goal.txt
.claude/goals/2026-09-holdfast-g9.goal.txt
.claude/goals/2026-09-holdfast-research
.claude/goals/2026-09-holdfast.md
.claude/goals/NEEDS-OWNER.md
$ echo "$GOFLAGS $GOMAXPROCS"
-p=2 2
```

## Baselines

Measured at the goal-start SHA in this container (2 CPUs by cgroup quota). Logs under
`/cache/tmp/holdfast-g1/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (detached worktree at `4ac983a`; log `gate-baseline-main.log`) | exit 0 | 1472 s (24m32s) |
| `internal/engine` under `go test -race` (from that run) | ok, 87.1% coverage | 1246.7 s (69% of `TEST_TIMEOUT` 30m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 88.1% coverage | 338.1 s |
| `go test -race ./internal/engine -run FailurePathIsByteIdentical -count=3` under plain `flock` (no `-o`) | FAIL 3 of 3: the no-progress error gains `cat: write error: Bad file descriptor` | 3.2 s |
| `scripts/test-mass.sh -check` | exit 1: `docs/test-mass.md` records commit `e09a819`, which this repository does not have | under 1 s |
| `go test ./internal/docscheck/ ./internal/corpus/` | ok | 2.3 s |
| `scripts/check-pins.sh` | exit 0, "pins agree" | under 10 s |
| `make secret-scan` | exit 0, clean | under 10 s |
| `func Test` count, all packages | 1200 in 25 packages (table below) | - |
| `CLAUDE.md` lines (`wc -l`) | 138 | - |
| tracked `scripts/regress_0057_*` (`git ls-files`) | 4 | - |

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
| `internal/engine` | 422 | `internal/sourceoffer` | 9 |
| `internal/fsclass` | 6 | `internal/startup` | 70 |
| `internal/hdr` | 9 | `internal/store` | 149 |
| `internal/heapmeasure` | 5 | `internal/vmaf` | 30 |
| `internal/logging` | 1 | `scripts/mutation-gate` | 5 |
| `internal/memlimit` | 3 | | |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`4ac983a`): both checks pass |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`30d245f`) |
| 1.3 | Baselines with timings | DONE (`4ac983a`): the table above; `make check` exit 0 in 24m32s |

## Phase 2 - Identity (T41, I1; line B, foundation)

| # | Item | State |
|---|---|---|
| 2.1 | `CLAUDE.md` commit-identity line becomes "commit as the repository's configured git identity", keeping the no-trailer rule | DONE (PR #96, `33c80fc`): "Commit as the repository's configured git identity; no `Co-Authored-By` and no AI co-author trailer" |
| 2.2 | The owner's name in the `internal/server/server_test.go` fixture becomes a synthetic one | DONE (PR #96, `33c80fc`): the proxy-header fixture uses a synthetic user, name and email |
| 2.3 | Identity scan in `make check`: tokens derived at run time, whole-word match, `LICENSE` and `NOTICE` exempt | DONE (PR #96, `33c80fc`): `scripts/identity-scan.sh`, `make identity-scan` in `check:` |
| 2.4 | Its selftest proves it bites on a synthetic identity | DONE (PR #96, `33c80fc`): `make identity-scan-selftest` in `check:`, 18/18 cases bite locally and in CI |
| 2.5 | In CI it derives the identity or fails loudly, never passes vacuously | DONE (PR #96, `33c80fc`): CI run 36557800799 derived the first commit `bfd3a01`, fingerprint `ac8ffad005da`, the same as locally; a shallow clone exits 4 (selftest case) |

## Phase 3 - The fd-3 fixture (line C)

| # | Item | State |
|---|---|---|
| 3.1 | Investigation: is production affected by an inherited read-only fd 3? | DONE: no. The real ffmpeg writes to fd 3 only when told `-progress pipe:3`, and `runFFmpeg` passes that only with `ExtraFiles[0]` (the pipe's write end), which `os/exec` places at fd 3 over anything inherited (`internal/engine/encode.go:629-661`) |
| 3.2 | `progressFake` probe true only when fd 3 is actually writable, with a test that bites on the old probe | DONE (PR #97, `26d88b1`): the probe reads the access mode from `/proc/<pid>/fdinfo/3`; `TestProgressFake_OnlyAWritableFd3IsAProgressChannel` reds on the old probe with `cat: write error`; local gate exit 0 in 24m25s on `98690d7` (engine 1232.8 s), CI green |
| 3.3 | Plain `flock` over the `FailurePathIsByteIdentical` run passes after the merge | DONE (`26d88b1`): 3 of 3 PASS under `flock` without `-o` on `main` (baseline: 3 of 3 FAIL) |

## Phase 4 - Reversals R1-R6 and the T34 cleanup (lines D, E)

| # | Item | State |
|---|---|---|
| 4.1 | R1 audio (`README.md`, `docs/migration.md`) | DOING (PR #98) |
| 4.2 | R2 worker nodes (`README.md`, `docs/migration.md`) | DOING (PR #98) |
| 4.3 | R3 frontend (`README.md`, `CLAUDE.md`, `docs/api-reference.md`, `docs/docker.md`) | DOING (PR #98) |
| 4.4 | R4 Dolby Vision / HDR10+ note under the `README.md` anchor | DOING (PR #98) |
| 4.5 | R5 release acts (`docs/release.md`, `README.md`) | DOING (PR #98) |
| 4.6 | R6 plan of record (`CLAUDE.md`, `README.md`, `cmd/holdfast/main.go`) | DOING (PR #98) |
| 4.7 | `README.md` status line: no stale date, points at the releases page | DOING (PR #98) |
| 4.8 | `CLAUDE.md` Layout: every `internal/` package, `analyze` and `plan`, no "API and UI", the TRANSCODE label range the code uses (and the `check-pins.sh` comment) | DOING (PR #98) |
| 4.9 | Web-UI and dashboard wording in `cmd/` and `internal/` | DOING (PR #98) |
| 4.10 | `docs/test-mass.md` and `scripts/test-mass.sh -check` agree | DOING: prose in PR #98; the measurement block is re-recorded in the proposals PR, after the last `*.go` change is on `main` |
| 4.11 | `scripts/regress_0057_*.js` deleted | DOING (PR #98) |
| 4.12 | `docs/comparison.md`, `docs/migration.md`, `docs/api-reference.md` match the code | DOING (PR #98) |

## Phase 5 - GitHub deletions (T35; line F)

| # | Item | State |
|---|---|---|
| 5.1 | Ref names before (`git ls-remote --heads --tags origin`) | DONE: 18 names (`/cache/tmp/holdfast-g1/t35-refs-before.txt`) |
| 5.2 | Delete the 6 branches and 1 tag by full refname | DONE: one `git push origin --delete` of the 7 full refnames, exit 0; their SHAs for the record: `remove-comment-density` `9025a49`, `remove-dashboard` `4ad6f03`, `sdd/S0022-holdfast-ffmpeg-rot` `1528735`, `sdd/S0035-holdfast-dashboard-ui` `3cd2adf`, `sdd/S0126-holdfast-depth-state-matrix-graders` `024e349`, `webui-e2e-playwright-graders` `6cdac0f`, tag `abandoned/S0098-pre-d911314` `c89a4c0` |
| 5.3 | Ref names after, and the diff | DONE: 11 names; `diff` before/after lists exactly the 6 branches and the tag, nothing else |

## Phase 6 - Proposals (T48; line G)

| # | Item | State |
|---|---|---|
| P1 | `proposal-triage.md` (T30, T33): a row per S0151, S0162-S0180 and PR #94 | DOING: 21 rows (20 keep, 1 merged), adversarially verified and corrected; goes in the proposals PR |
| P2 | `proposal-hw-gates.md` (T10) | DOING: claims re-verified, adversarially verified and corrected; goes in the proposals PR |
| P3 | `proposal-amd-image.md` (T45, I8, I9) | DOING: claims re-verified, adversarially verified and corrected; goes in the proposals PR |
| P4 | `proposal-node-protocol.md` (T46) | DOING: claims re-verified, adversarially verified and corrected; goes in the proposals PR |
| P5 | `proposal-crop-dv.md` (I7) | DOING: claims re-verified and lab-tested on synthetic material, adversarially verified and corrected; goes in the proposals PR |
| P6 | `proposal-docs-corpus.md` (I16) | DOING: adversarially verified and corrected; goes in the proposals PR |

## Phase 7 - Fresh-clone gate (line B)

| # | Item | State |
|---|---|---|
| 7.1 | Clone `origin/main` to `/cache/tmp/holdfast-g1-fresh` and run the full gate there | TODO |

## Phase 8 - Checkpoint T packet and the report (lines H-K)

| # | Item | State |
|---|---|---|
| 8.1 | Checkpoint T packet printed | TODO |
| 8.2 | Gate integrity counted from the goal-start SHA | TODO |
| 8.3 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-29: the goal-start baseline gate runs in a detached worktree
  (`/cache/wt/holdfast/g1-baseline`) so edits in `/workspace` cannot touch what it measures.
  Reasoning: §0.1 step 5 and §0.4 (worktrees for anything that writes).
- 2026-09-29: one branch and PR per track: `holdfast-g1/identity-scan`,
  `holdfast-g1/fd3-fixture`, `holdfast-g1/reversals-cleanup`, `holdfast-g1/proposals`. The
  proposals live under `.claude/goals/` but are not ledger files, so they go through a PR and the
  gate (§0.3 lets only the ledger and `NEEDS-OWNER.md` go straight to `main`).

- 2026-09-29: the identity scan reads the owner's identity from the author of the repository's
  first commit (every root commit), never from a literal, the configured identity or HEAD; a
  shallow clone exits 4, so the `build` jobs of `ci.yml` and `release.yml` check out with
  `fetch-depth: 0`. Findings print path, line and which part matched, never the text, because a
  public repository's CI log is public. Reasoning: `scripts/identity-scan.sh` header, PR #96.
- 2026-09-29: the fd-3 probe reads the access mode from `/proc/<pid>/fdinfo/3`. It is
  Linux-only, as the suite already is (it needs the pinned BtbN ffmpeg, a Linux build); with no
  `/proc` the fake writes no progress and the failure-path test reds on "no progress was
  collected", never a false green. Reasoning: `progressFake` doc comment, PR #97.
- 2026-09-29: `docs/test-mass.md`'s measurement block is re-recorded in the proposals PR, the
  last PR of the goal and a docs-only one, because `-check` needs the recorded commit on `main`
  with `*.go` identical to HEAD, and a squash merge leaves a branch commit unreachable.
  Reasoning: `scripts/test-mass.sh` `-check`, and the lost `e09a819`.
- 2026-09-29: P1's S0151 row. Installing a hosted update-bot app is neither a NEEDS-OWNER kind
  (§0.6 names four) nor something a goal may do (§0.7), so it is a Checkpoint T decision in P1,
  not a `NEEDS-OWNER.md` row.
- 2026-09-29: the read-only umbrella clone is `/cache/tmp/holdfast-super-ro` with its push URL
  `DISABLED` (§0.1).

- 2026-09-29: deviations recorded, all from one agent. The adversarial verifier of the proposals
  (a general-purpose agent) started four forks of its own at once, one per proposal P2-P5, so up
  to five agents ran together against T40's cap of 2; its report says it had not read the cap.
  The goal saw two of them still running at a check-in, found one already finished, and messaged
  the verifier to spawn nothing further. Its P2 fork also ran two sub-second CPU test encodes (a
  2-frame 128x128 lavfi clip) without the heavy lock (§0.4), and a fork's `go` command added 22
  `/go.mod` hash lines to `go.sum` in `/workspace`; the goal restored `go.sum` (additions only,
  nothing committed). Every later agent prompt forbids spawning agents, running encodes and
  running `go` in `/workspace`.

- 2026-09-29: the proposals' adversarial verification (§0.4) checked 54 claims across P1-P6 and
  refuted none outright; 16 needed a correction (P5's L5 output gate, P2's VAAPI key range, P4's
  retry-bound wording among them), and every recommendation stands. All 16 were applied before
  commit; the report is `.claude/goals/2026-09-holdfast-research/verify-proposals.md`.

## Resume here

PRs #96 (`33c80fc`) and #97 (`26d88b1`) are merged; line C holds on `main`. Next: #98
(reversals and cleanup) after merging `origin/main` and a fresh gate, then the proposals PR
(P1-P6, `verify-proposals.md`, and the test-mass record of #98's squash commit), the fresh-clone
gate, the packet and the report. Then the proposals PR (P1-P6 plus the test-mass
record), the plain-`flock` run for line C, the fresh-clone gate, and the packet.
