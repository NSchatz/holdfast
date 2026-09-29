# Goal 2 ledger - carried specs and the debian13 base

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §6; goal file
  `.claude/goals/2026-09-holdfast-g2.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`.
- Started: 2026-09-29.
- Goal-start SHA: `3bfd42337585ce47e0da2727b43192585677d744` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md`, ending `COMPLETE (goal 1): 2026-09-29`.
  `NEEDS-OWNER.md` at start: header only, no rows.
- Precondition, as checked:

```
$ git show origin/main:.claude/goals/2026-09-holdfast-g1.status.md | grep -n 'COMPLETE (goal 1)'
223:COMPLETE (goal 1): 2026-09-29
$ git ls-tree origin/main .claude/goals/CHECKPOINT-T.approved
100644 blob f24d5520b0aa31f3130b1566c5235da605129ebd	.claude/goals/CHECKPOINT-T.approved
$ git log --diff-filter=A --format='%H %s' origin/main -- .claude/goals/CHECKPOINT-T.approved
3bfd42337585ce47e0da2727b43192585677d744 chore(goals): Checkpoint T approved
$ gh api repos/NSchatz/holdfast/commits/3bfd42337585ce47e0da2727b43192585677d744 --jq '.commit.verification.verified,.commit.committer.name'
false
<the owner's name>
$ gh api repos/NSchatz/holdfast/commits/3bfd423... --jq '.commit.verification.reason'
unsigned
$ echo "$GOFLAGS $GOMAXPROCS"
-p=2 2
```

The approval commit is not GitHub-verified (unsigned), so it was not added in the web UI. I19
accepts a commit from a session the owner told to, and says the check is shown, not enforced; no
goal wrote the file (its only commit is `3bfd423`, authored and committed as the owner).

## What the approval assigns to this goal

From `CHECKPOINT-T.approved` and P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md`
at `3bfd423`, "Per goal"): PR #94 (S0159) first, by cherry-pick; S0151, S0163, S0166, S0168,
S0173, S0176, S0177, S0180; and P6 option (a) (`internal/corpus` skips `.claude/`). The
approval's amendments for this goal: S0151 is a committed `.github/dependabot.yml` (GitHub
Actions, the Docker base images, Go modules; PRs never merged; the ffmpeg pin stays with
`pin-health.yml`; no app); S0176 builds the holdfast half here and opens a never-merged PR in
`NSchatz/homelab` for the `TZ` half, whose merge is a `NEEDS-OWNER.md` row.

## Baselines

Measured at the goal-start SHA in this container (2 CPUs by cgroup quota). Logs under
`/cache/tmp/holdfast-g2/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (detached worktree at `3bfd423`; log `gate-baseline.log`) | exit 0 | 1565 s (26m5s) |
| `internal/engine` under `go test -race` (from that run) | ok, 87.1% coverage | 1314.6 s (73% of `TEST_TIMEOUT` 30m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 88.1% coverage | 387.8 s |
| `scripts/check-pins.sh`, `make check-pins-selftest` | exit 0, "pins agree"; 31/31 cases bite | under 1 min |
| `func Test` count, all packages | 1201 in 25 packages (table below) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 58, 76 | - |

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
| `internal/engine` | 423 | `internal/sourceoffer` | 9 |
| `internal/fsclass` | 6 | `internal/startup` | 70 |
| `internal/hdr` | 9 | `internal/store` | 149 |
| `internal/heapmeasure` | 5 | `internal/vmaf` | 30 |
| `internal/logging` | 1 | `scripts/mutation-gate` | 5 |
| `internal/memlimit` | 3 | | |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`3bfd423`): both files on `origin/main`; verification printed |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`3c57336`) |
| 1.3 | Baselines with timings | DONE (`3bfd423`): the table above; `make check` exit 0 in 26m5s |

## Phase 2 - PR #94 (S0159), by cherry-pick (I12)

| # | Item | State |
|---|---|---|
| 2.1 | Cherry-pick #94's four commits onto `holdfast-g2/s0159-temp-sweep`, gate, merge as a new PR | DONE (PR #100, `ca4968b`): four commits cherry-picked with `-x`, applied cleanly, 10 files +1673/-40 as in #94; local gate exit 0 in 28m20s on `0a8efb8` (`internal/engine` 1431.1 s, 79.5% of `TEST_TIMEOUT`); CI build, mutation, package green |
| 2.2 | Close #94 with a link to the new PR; its old branch is left and listed as a follow-up | DONE: #94 closed with a comment linking #100 and `ca4968b`; branch `sdd/S0159-holdfast-bounded-run-temp-sweep` left at `9b016d4` (follow-up: the owner may delete it; T35's list does not name it) |

## Phase 3 - The debian13 base (line C, foundation)

| # | Item | State |
|---|---|---|
| 3.1 | `RUNTIME_IMAGE` is `gcr.io/distroless/cc-debian13:nonroot`, pinned by tag and digest | DONE (PR #101, `cab897a`): `sha256:54df941ed0d06a1bd95ef5e0ce391fd8d9f94b64782dc9a60062727849ee3f97`; build and fetch stages `golang:1.25.14-trixie`, `debian:trixie-slim` |
| 3.2 | `scripts/check-pins.sh` sections 3 and 7 green | DONE (PR #101): green, selftest 31/31; local gate exit 0 in 27m7s on `1d79a78` (`internal/engine` 1391.0 s) |
| 3.3 | The merged PR's CI `package` job green | DONE (PR #101): `package` pass (3m50s) on `1d79a78`, with `build` and `mutation` |
| 3.4 | `docs/docker.md` follows the image | DONE (PR #101) |

## Phase 4 - Carried specs (line B)

| # | Item | State |
|---|---|---|
| S0177 | working-file extensions | DOING (PR #105): AC-1 to AC-16 pass under the lock, AC-14 bites; CI's first run failed two S0155 regressions (long names past NAME_MAX/PATH_MAX with the suffix), fixed in `0ca25c0` (fallback to the earlier name where the suffix cannot fit) |
| S0163 | `workers: auto` from the CPU quota; several jobs in flight safe on one drive | DOING (build agent, branch `holdfast-g2/s0163-workers` on #105's head) |
| S0173 | `run` progress | DOING (PR #107): 23 tests for AC-1 to AC-13 and AC-15 pass; AC-16 and AC-17 pass with no edit; no existing test line changed |
| S0168 | prune excluded directories from the walk | DOING (build agent, branch `holdfast-g2/census-scope`) |
| S0180 | census scope parity | DOING (build agent, same branch, after S0168) |
| S0166 | restart survey overcount | DONE (PR #102, `02aa553`): AC-1 to AC-10 pass, and AC-1 to AC-5 red on the old resolution (33 band-decided rows counted as moved); 22 existing test lines changed call shape only; local gate exit 0 in 31m20s on `52f96ed` (`internal/engine` 1627.4 s); CI green |
| S0176 | log time offset (holdfast half), and the homelab PR for the `TZ` half | DOING (PR #103, with P6): AC-H1 to AC-H7 pass, AC-H9 grep empty. Homelab half: NSchatz/homelab#208 opened (never merged by a session), `NEEDS-OWNER.md` row 1 |
| S0151 | `.github/dependabot.yml` | DOING (PR #104, `99ca634`): selftest 35/35; CI `package` green on the literal-`FROM` Dockerfile |

## Phase 5 - P6 (approved option (a))

| # | Item | State |
|---|---|---|
| P6 | `internal/corpus` skips `.claude/`, with a test | DOING (PR #103, commit `790c54d`): both new tests red with the skip removed |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-09-29: the goal-start baseline gate runs in a detached worktree
  (`/cache/wt/holdfast/g2-baseline`) so edits in `/workspace` cannot touch what it measures.
  Reasoning: §0.1 step 5 and §0.4 (worktrees for anything that writes).
- 2026-09-29: tracks and PRs. PR #100 carries #94 (S0159); PR #101 is the debian13 base
  (line C); `holdfast-g2/corpus-log-offset` carries P6 and S0176 together (both small, both
  in the mutation domain); `holdfast-g2/s0151-dependabot` carries S0151 after #101, because
  it rewrites the same Dockerfile lines; build agents take S0177 (on #100's head, since it
  renames the temps #94's sweep guards) and S0173; S0163 follows S0177 (both change the
  working-path construction); S0168, S0180 and S0166 follow as the census track.
  Reasoning: T52 batching against a 26-minute gate, and P1's suggested order.
- 2026-09-29: every image stage moves to Debian 13, not only the runtime. The zone database
  the runtime copies from the build stage then comes from the same release as the base, and
  the fetch stage is the one later goals extend with trixie packages. The Go toolchain and
  the ffmpeg pin are unchanged. Reasoning: PR #101's body and the Dockerfile comment.
- 2026-09-29: the zoneinfo `COPY` from the build stage replaces the base's own zone files:
  at these pins the golang image has tzdata 2026b (every layer checked) and the distroless
  base 2026c. That predates this goal (the bookworm golang image also carries 2026b), so it
  is documented in the Dockerfile rather than changed. Reasoning: PR #101.
- 2026-09-29: S0151 as amended (Dependabot). Dependabot's Docker parser reads only an image
  written on a `FROM` line and never resolves an `ARG` (dependabot-core
  `docker/lib/dependabot/docker/file_parser.rb`, `FROM_LINE`, at `78005a8`, read
  2026-09-29; a Python port of the regex matched none of the three `ARG`-form lines and all
  three literal ones), so the approval's "covers the Docker base images" needs the base
  images on literal `FROM` lines. `ARG GO_IMAGE` stays as the copy the in-image toolchain
  check reads; `scripts/check-pins.sh` holds the two equal (section 3), refuses a base image
  behind an `ARG` (section 7) and refuses a configuration that stops watching a class
  (section 9). The spec's Renovate-era "no edit to the Dockerfile or check-pins" applied to
  a mechanism the owner replaced.
- 2026-09-29: S0176 in this public repository tests the daylight-saving offset with
  `Europe/Berlin`, not the zone the spec names (the owner's; P1 treats it as private). `New`
  installs the process-wide default logger (it writes to the process's own stderr); `To`
  over a caller's writer does not, so a test buffer never becomes the process default.
- 2026-09-29: S0176's homelab half. The runbook changed after the spec was written: its four
  inline `docker run` blocks became one `IMG`/`HF` shorthand every step uses, and the check
  script its AC-L2 names is gone (the merge gate is `make ci`). So the flag goes on `HF`
  (covering all four invocations), the spec's AC-L1 awk, which now matches no block, is shown
  beside an adapted check (1 block, 0 without the flag), and `make ci` is left to the owner:
  every validator in it is a container and this container has no Docker daemon. PR
  NSchatz/homelab#208, never merged by a session (T32); `NEEDS-OWNER.md` row 1.
- 2026-09-29: S0168, S0180 and S0166 quote the owner's library paths and ledger counts;
  their tests use synthetic paths under `t.TempDir()` and synthetic counts of the same shape
  (T41, as P1 did).
- 2026-09-29: `TEST_TIMEOUT` raised from 30m to 45m in a PR of its own (#106), because the
  gate of #102 measured `internal/engine` at 1627.4 s, 90.4% of 30m, past I15's 80% line. A
  commit inside a feature PR would be squashed into it, so it was not "its own commit" on
  `main`. It is gated before any other PR, since each later gate risked a false timeout. Much
  of the jump is contention: the build agents compile and run the non-encoding packages
  outside the heavy lock, and that shares the container's 2 CPUs with the gate.
- 2026-09-29: S0177's names are 14 bytes longer, so a source whose earlier working or
  retained name fitted NAME_MAX or PATH_MAX within 14 bytes would fail. Those are the long
  names S0155 made swap, and CI's two S0155 regressions caught it. The constructors fall
  back to the earlier name only where the suffixed one cannot fit, rather than truncating
  the stem, because the record-free hold-back and the source-beside lookup read the full
  stem back from the name, and every reader already accepts the earlier name.
- 2026-09-29: two test fixtures carried a real film title from the operator's report (T41).
  They were replaced with a synthetic name of the same byte length and shape, in PR #105
  (the PR that had to touch one of them). A search of `main` for title-shaped fixture names
  finds no other.

## Resume here

Merged: #100 (`ca4968b`), #101 (`cab897a`, line C), #102 (`02aa553`), #106 (`3996107`,
`TEST_TIMEOUT` 45m; its first gate failed on a startup-scan race in
`TestServe_AC1_TheResolvedTautulliKeyReachesItsOwnRequest`, re-run green). Open: #103 (P6 +
S0176 + that test's hardening; gate running), #104 (S0151), #105 (S0177 + fix `0ca25c0`), #107
(S0173). Gate order: #103, #104, #105, #107, then census, S0163. Build agents: S0163 (on
#105's head before `0ca25c0`), census (S0168 + S0180). Audit of deleted test lines so far:
`/cache/tmp/holdfast-g2/audit-deleted-test-lines.md`.
