# Goal 10 ledger - media-server clients

- Brief: `.claude/goals/2026-09-holdfast.md` §0, §4 and §14 (spec v1.1, the `/goals:spec` skill);
  goal file `.claude/goals/2026-09-holdfast-g10.goal.txt`. Amended by
  `.claude/goals/CHECKPOINT-T.approved`: P1 approved as proposed, "S0179 merged into goal 10", and
  "S0178: preserve_mtime stays on by default. Build only the statement half: validate and startup
  state the effective choice. The default does not flip."
- Started: 2026-10-02, from `/workspace/holdfast` in the maker container, by the goals supervisor's
  `/goal` condition.
- Goal-start SHA: `6e1058f089f554887416e9b7b82d23385294d04b` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g9.status.md` (ends
  `COMPLETE (goal 9): 2026-10-01`). The owner's queue at start (`goals needs list`): seven open
  holdfast items, queue #50 to #56 (the former `NEEDS-OWNER.md` rows 1-7).
- Precondition, as checked:

```
$ git fetch origin && git rev-parse origin/main
6e1058f089f554887416e9b7b82d23385294d04b
$ git show origin/main:.claude/goals/2026-09-holdfast-g9.status.md | grep -n "COMPLETE (goal 9)"
157:COMPLETE (goal 9): 2026-10-01
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
$ goals admit --agents 4
decision: admit a goal
agents: 4 of 4 allowed (requested)
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `d4d70a6`, line 63, "Per
goal"): "**Goal 10** (media-server clients): S0178, S0179 (merged)."

- S0178 (`swap-mtime-choice`): the approval builds "only the statement half": `validate` and
  startup state the effective `preserve_mtime` choice; the default stays on.
- S0179 (`post-swap-rescan-hook`): merged into this goal's clients; the goal takes over the spec's
  safety criteria (off by default, a failed request never touches the job, the swap, the undo
  window or the exit code, one attempt per target off the workers, a bounded queue and drain, the
  owner found by directory through a path map, no resolved credential in any log line, the
  deployment note on the arr custom-format re-download risk).

The specs are read from goal 1's read-only umbrella clone (push URL disabled).

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g10-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g10/tmp` and the pinned dynamic-HDR tools on PATH. Logs under
`/cache/tmp/holdfast-g10/`.

| What | Value | Source |
|---|---|---|
| `make check` under `goals-heavy` then `flock -o` `holdfast-heavy` | running at ledger creation; filled in at the next phase boundary | `gate-baseline.log` |
| `func Test` count, all packages | 1724 in 34 directories | `rg -c '^func Test' -g '*_test.go'`, `functest-start.txt` |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines | 66, 80 | `wc -l` |
| `CLAUDE.md` lines | 199 | `wc -l` |
| `TEST_TIMEOUT` | 45m | `Makefile:89` |
| `config.SecretBearingKeys` | 4 keys | `internal/config/config.go:762` |
| Usage headroom | seven_day 7%; 4 of 4 agents allowed | `goals admit --agents 4` |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`6e1058f`): goal 9's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (the commit that adds this file): fast docs checks passed first |
| 1.3 | Baseline gate with timings | DOING |

## Phase 2 - Triage rows (line B)

| # | Item | State |
|---|---|---|
| 2.1 | S0178 statement half: `validate` and startup state the effective `preserve_mtime` choice | TODO |
| 2.2 | S0179 post-swap rescan (merged into phase 3) | TODO |

## Phase 3 - Plex, Sonarr and Radarr after a swap (line C)

| # | Item | State |
|---|---|---|
| 3.1 | Plex partial refresh of the section path after a swap | TODO |
| 3.2 | Plex hold on a file currently being played, beside the Tautulli hold | TODO |
| 3.3 | Sonarr `RescanSeries` and Radarr `RescanMovie` after a swap | TODO |

## Phase 4 - Webhook intake (line D)

| # | Item | State |
|---|---|---|
| 4.1 | Authenticated intake: both Sonarr Download shapes and Radarr's, queues the file | TODO |
| 4.2 | `docs/docker.md`: the native intake replaces the custom script | TODO |
| 4.3 | `make api-schema-diff` passes, `.api-schema-breaks.yaml` still `[]` | TODO |

## Phase 5 - Credentials and live checks (line E)

| # | Item | State |
|---|---|---|
| 5.1 | Each new credential key in `config.SecretBearingKeys`, a literal refused | TODO |
| 5.2 | Live-check commands that write a redacted report, filed on the owner's queue for Plex, Sonarr and Radarr | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- D1 (2026-10-02): tracks. `holdfast-g10/clients` (S0179: the post-swap rescan of Plex, Sonarr and
  Radarr, the Plex play hold, the credentials), `holdfast-g10/mtime-statement` (S0178's statement
  half), then `holdfast-g10/webhook` (the intake, built on the clients' path maps) and
  `holdfast-g10/live-check` (the owner's redacted live-check report). Each is built by one agent
  in its own worktree and merged by the rule of §0.3.
- D2 (2026-10-02): configuration follows S0179's ruling: flat top-level keys beside
  `tautulli_url`: `radarr_url`, `radarr_api_key`, `radarr_path_map`, `sonarr_url`,
  `sonarr_api_key`, `sonarr_path_map`, `plex_url`, `plex_token`, `plex_path_map`. Everything is off
  until a target's address and credential are both set (I5: an existing config decides as before).
- D3 (2026-10-02): the Plex play hold fails open, as the Tautulli hold does: when Plex cannot be
  asked, the file is not held and one `warn` record says so. The research
  (`research-nodes-clients-spa.md` section 2) asks the owner to ratify fail-open against
  fail-closed; it is listed under "Proposals awaiting the owner" (§0.2 fallback: an unreachable
  Plex must not stop every job).
- D4 (2026-10-02): every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g10/`
  and `PATH=/cache/opt/dynhdr-tools/bin:$PATH` (goal 9's finding), goal shells use `rg` or
  `git grep`, and commits carry no trailer (T38).

## Proposals awaiting the owner

- Fail-open against fail-closed for the Plex play hold (D3).

## Resume here

Phase 1: the ledger is being committed; the baseline gate runs in the background
(`/cache/tmp/holdfast-g10/gate-baseline.log`). Next: launch the `clients` and `mtime-statement`
agents in worktrees under `/cache/wt/holdfast/`, then `webhook` and `live-check` on top of the
clients branch.
