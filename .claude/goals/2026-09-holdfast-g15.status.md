# Goal 15 ledger - finale: release, docs and the program report

- Brief: `.claude/goals/2026-09-holdfast.md` §0, §4 and §19 (spec v1.1, the `/goals:spec` skill,
  read at 1.1.6 from the plugin's 1.5.0 directory); goal file
  `.claude/goals/2026-09-holdfast-g15.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`
  and by `.claude/goals/AMENDMENT-2026-10-03-owner-2026-09-holdfast.md` (CI is the whole gate: no
  gate, tier, whole-package or `-race` suite or mutation run on this host; line C reads CI on
  `origin/main`'s head).
- Started: 2026-10-03 23:59Z, from `/workspace/holdfast` in the maker container, by the `/goal`
  condition.
- Goal-start SHA: `1507a2b428c0f29f8e5d249e483d13bb43e89b6e` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g14.status.md` (ends
  `COMPLETE (goal 14): 2026-10-03`). The owner's queue at start (`goals needs list`): the open
  holdfast items are queue #50 to #56, #202 to #204 and #219. No request is addressed to holdfast
  (`goals request list`).
- Precondition, as checked:

```
$ git fetch origin && git rev-parse origin/main
1507a2b428c0f29f8e5d249e483d13bb43e89b6e
$ git grep -n "COMPLETE (goal 14)" origin/main -- .claude/goals/2026-09-holdfast-g14.status.md
origin/main:.claude/goals/2026-09-holdfast-g14.status.md:240:COMPLETE (goal 14): 2026-10-03
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
$ goals admit --agents 4
usage: five_hour 20% ... ok
usage: seven_day 83% ... ok
decision: admit a goal
agents: 4 of 4 allowed (requested)
```

## Baselines

No gate runs on this host (the owner, 2026-10-03), so the gate's baseline is CI's own last green
run on `main`.

| What | Value | Source |
|---|---|---|
| CI on `main`, last green run at start | run 37162014945 on `d688105`: `build` and `package` green, 23:30:51Z to 23:55:18Z (24m27s) | `gh run list --branch main` |
| CI on the goal-start SHA `1507a2b` | run 37163421432, in progress at start | `gh run list --branch main` |
| `func Test` count, all packages | 2145 | `rg -c '^func Test' -g '*_test.go'` |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines | 66, 80 | `wc -l` |
| `CLAUDE.md` lines | 208 (over the 200 of T51 since #170; row 3.3) | `wc -l` |
| Tags | `v0.3.0`, `v0.2.0`, `v0.1.0`; the next minor is `v0.4.0` | `git tag --sort=-v:refname` |
| `testdata/hw-reports/` | `README.md` only: no report has arrived | `ls testdata/hw-reports/` |
| Usage headroom | five_hour 20%, seven_day 83%; 4 of 4 agents allowed | `goals admit --agents 4` |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`1507a2b`): goal 14's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (the commit that adds this file) |
| 1.3 | Baselines | DONE (`1507a2b`): the table above; CI's last green run stands in for a local gate (the 2026-10-03 amendment) |

## Phase 2 - Triage rows (line B)

| # | Item | State |
|---|---|---|
| 2.1 | Every P1 row (20 specs and PR #94) is DONE or a listed follow-up, with the count | DONE (read at `b720162`): 21 of 21 rows DONE in the earlier ledgers, 0 follow-ups: PR #94 (S0159) as #100, S0151 #104, S0162 #121, S0163 #111, S0164 #143, S0165 #128, S0166 #102, S0167 and S0169 to S0172 #167, S0168 and S0180 #112, S0173 #107, S0174 #142, S0175 #165, S0176 #103, S0177 #105, S0178 #152 (the statement half, as approved), S0179 #153 (merged into goal 10); each PR is MERGED on GitHub (`gh pr list --state merged`) |

## Phase 3 - Docs (track `holdfast-g15/docs`)

| # | Item | State |
|---|---|---|
| 3.1 | `README.md` describes every shipped area and its default | DONE (PR #171, `c95dbb9`; CI run 37165455933 green on `5135101` (`build` 24m19s, `package` 4m25s), `mutation` green (run 37165455936); 1 fix round (D6)): two new sections (encoders and hardware; how much runs at once) and the defaults added to the gate, health sweep, audio, sidecar, node and priority paragraphs, each read from `internal/config` (`defaultLayer()` in `config.go`, `hardware.go`, `workers.go`, `audio.go`, `nodes.go`, `priority.go`, `watch.go`); corrected: six read endpoints, four credentials |
| 3.2 | `docs/comparison.md` updated to what ships | DONE (PR #171, `c95dbb9`): holdfast's side only (priority queue, per-library profiles, hardware selection, audio rules, tokens, web UI, nodes); no claim about another product added or changed; `docs/migration.md`'s NVENC quality row follows |
| 3.3 | `CLAUDE.md` Layout complete and under 200 lines | DONE (PR #171, `c95dbb9`): 198 lines (`wc -l`); every package under `internal/` and `cmd/` has its line, plus one for the remaining scripts; the CI rule stated without a date; no rule or rationale bullet removed |
| 3.4 | Every `docs/design/` anchor linked | DONE (PR #171, `c95dbb9`): 68 explicit anchors in `docs/design/*.md`, each linked from `README.md`, `CLAUDE.md` or a file under `docs/`; 29 had no inbound link before |
| 3.5 | Goal 14's D15 note on `docs/docker.md` (what the UI shows before a read token is typed) | DONE (PR #171, `c95dbb9`): no library datum until a token is typed; the version and endpoint count from the ungated `/api/schema` and each view's `401` are shown; the read-gated list names all six endpoints |

## Phase 4 - The gate on CI (line C)

| # | Item | State |
|---|---|---|
| 4.1 | `build` and `package` green on `origin/main`'s head: run link, result, wall-clock | TODO |

## Phase 5 - Release (line D, foundation)

| # | Item | State |
|---|---|---|
| 5.1 | The dry-run dispatch on `main`, green, `publish` skipped, artifacts read | DONE (run 37166751433 on `c95dbb9`, `workflow_dispatch`, 01:01:43Z to 01:27:34Z, green): the plan line reads `publish=false  version=0.0.0-dev-c95dbb9`; the gate, both image builds and both smokes ran; the `publish` job is `skipped`; artifact `release-dist-0.0.0-dev-c95dbb9` holds both tarballs and `SHA256SUMS` (`sha256sum -c` OK), each tarball `holdfast`, `LICENSE`, `NOTICE`. CI on the same commit: run 37166745058 green (`build`, `package`; 01:01:36Z to 01:26:11Z) |
| 5.2 | The tag `v0.4.0` pushed and its release run green | DOING: annotated `v0.4.0` on `c95dbb9` pushed 01:28Z; release run 37168110898 in progress |
| 5.3 | The pull confirmed with `crane digest` (crane installed rootless through mise) | TODO |
| 5.4 | The compose pin, the API schema baseline and `docs/release.md`'s record committed (track `holdfast-g15/release-record`) | TODO |

## Phase 6 - homelab (line E)

| # | Item | State |
|---|---|---|
| 6.1 | A PR in the owner's private homelab repository, never merged, bringing its holdfast deployment and env example up to the release and the new keys | TODO |
| 6.2 | Its item in the owner's queue | TODO |

## Phase 7 - Hardware reports (line G)

| # | Item | State |
|---|---|---|
| 7.1 | `testdata/hw-reports/` re-checked: what arrived, what is still with the owner | TODO |

## Phase 8 - The program report (line F)

| # | Item | State |
|---|---|---|
| 8.1 | `docs/program-report-2026-09-holdfast.md` with its sections | TODO |

## Phase 9 - Report

| # | Item | State |
|---|---|---|
| 9.1 | Gate integrity counted from the goal-start SHA (line H) | TODO |
| 9.2 | Hygiene: repos clean and pushed, no open PR of this goal, the queue current (line I) | TODO |
| 9.3 | Adversarial review of the report (line J) | TODO |

## Decisions taken

- D1 (2026-10-04): tracks. `holdfast-g15/docs` carries §19 item 2 and merges before the release,
  so the release ships the documents that describe it. `holdfast-g15/release-record` carries what
  can only be written after the tag (the compose pin, the API schema baseline, the record in
  `docs/release.md`) and the program report, which cites the release. The homelab PR follows the
  release, since it pins the released digest. T52: two PRs here, not one per item.
- D2 (2026-10-04): §19 item 3 (a fresh-clone gate on this host) is replaced by line C as the
  2026-10-03 amendment rewrote it: the gate is CI on `origin/main`'s head. Nothing larger than one
  focused test runs here in this goal.
- D3 (2026-10-04): a push to `main` cancels the CI run in progress on the older head (seen on
  `7bcfbcd`, `d51c9a2`). Ledger commits are therefore batched at phase boundaries, and none is
  pushed while a run this goal needs (the pre-release run, the last run for line C) is in flight.
- D4 (2026-10-04): commits carry no trailer (T38, `CLAUDE.md`), which wins over the session's
  attribution reminder; a PR body ends with the session's "Generated with Claude Code" line (§0.3).
- D5 (2026-10-04): usage is at 83% of the seven-day window (the bound is 95%), so this goal runs
  at most two build agents beside the session and one reviewer at the end, below the 4 allowed.

- D6 (2026-10-04): PR #171's first CI run (37164247250) was red in `build` on one test,
  `TestView_ARecoveredLeaseNamesANodeWhoseModeIsNotKnown` in `internal/node` ("Recover: <nil>, 0
  kept"), which goal 14 added and a prose change cannot reach. Cause, shown here by widening the
  window with a 200 ms sleep (red 3 of 3): the fixture stopped the first hub and then closed the
  ledger, and a stopping hub ends its lease as cancelled in the engine call's goroutine, so that
  write raced the close. The fix is on the PR's branch (fix round 1 of 3, §0.3): the restart is
  the killed process the sibling fixtures use (`f.srv.Close()`, no `f.stop()`); no assertion
  changes; green at `-race -count=200` and with the same window widened. It rides the docs PR
  and not one of its own because a release run executes the same suite, so the release needs it
  on `main` first, and a second PR is a second 21-minute run. The one deleted `*_test.go` line
  of this goal is that `f.stop()`.
- D7 (2026-10-04): `crane` is installed rootless as `mise use -g aqua:google/go-containerregistry`
  (0.22.1; Apache-2.0, https://github.com/google/go-containerregistry/blob/main/LICENSE, read
  2026-10-04). It is a tool of this session, not a dependency of the repository. It answers
  `ghcr.io/nschatz/holdfast:v0.3.0` with the digest `docs/release.md` records.
- D8 (2026-10-04): the homelab PR changes the image pin, comments and the runbook only. Every
  key `v0.4.0` adds stays unset there, each as a commented block or a named refusal: turning one
  on in the one delete-capable deployment is the owner's decision, not a release bump's.

## Requests

Filed: none. Addressed to holdfast: none open at start.
The owner's queue: open holdfast items at start are queue #50 to #56, #202 to #204 and #219.

## Resume here

Phases 1 to 3 are done (PR #171 merged as `c95dbb9`). The tag `v0.4.0` is pushed on `c95dbb9`
and its release run 37168110898 is in flight: it is irreversible, never re-push or move the tag.
Next, when it is green: `crane digest ghcr.io/nschatz/holdfast:v0.4.0`; then on the worktree
`/cache/wt/holdfast/g15-release` (branch `holdfast-g15/release-record`, which already holds the
re-recorded `docs/test-mass.md`, the `v0.4.0` `docs/api-schema.json` taken on the tag, and the
program report with `<<...>>` placeholders) set the compose pin, record the release in
`docs/release.md`, fill the placeholders, PR, CI, merge. Then the homelab PR from
`/cache/wt/homelab/holdfast-g15` (branch `holdfast-g15/release-v0.4.0`, edits made, the digest is
the placeholder `@@DIGEST@@`) and its queue item. The report draft is
`/cache/tmp/holdfast-g15/program-report-draft.md`.
