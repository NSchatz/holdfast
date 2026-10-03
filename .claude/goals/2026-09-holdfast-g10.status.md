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
| `make check` under `goals-heavy` then `flock -o` `holdfast-heavy` | exit 0 in 2488 s | `gate-baseline.log` |
| `internal/engine` under `go test -race` (from that run) | ok, 89.0% coverage, 2283.9 s (84.6% of `TEST_TIMEOUT` 45m) | `gate-baseline.log` |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 89.2% coverage, 773.7 s | `gate-baseline.log` |
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
| 1.3 | Baseline gate with timings | DONE (`6e1058f`): `make check` exit 0 in 2488 s; the table above |
| 1.4 | `TEST_TIMEOUT` raised in its own commit (the engine passed 80%, I15) | DONE (PR #151, `b139fcb`): 45m to 60m with the measurement in `Makefile`; gate exit 0 (`internal/engine` 2430.5 s, `cmd/holdfast` 914.0 s); CI green. A first gate run was stopped by the session's 2-hour background limit while waiting for the lock and re-run detached |

## Phase 2 - Triage rows (line B)

| # | Item | State |
|---|---|---|
| 2.1 | S0178 statement half: `validate` and startup state the effective `preserve_mtime` choice | DONE (PR #152, `2787ea6`): one `preserve_mtime` notice (value, default or explicit, its consequence) printed by `validate` and logged at start; `TestNotices_S0178_AC5_StatesTheEffectivePreserveMtimeChoice`, `TestValidate_S0178_AC5_PrintsExactlyOnePreserveMtimeNote`, `TestStartup_S0178_AC5_LogsThePreserveMtimeChoice`, AC-6 and restore tests; default unchanged; mutation-diff 100%; gate exit 0 (`internal/engine` 2446.3 s); CI green; 1 fix round (an empty key still loads as the default) |
| 2.2 | S0179 post-swap rescan (merged into phase 3) | DONE (PR #153, `138c0cc`): AC-1 to AC-16 tested in `internal/mediaclient`, `internal/config`, `cmd/holdfast/post_swap_hook_test.go`; AC-17 the gate; AC-18 is the owner's live check (phase 5) |

## Phase 3 - Plex, Sonarr and Radarr after a swap (line C)

| # | Item | State |
|---|---|---|
| 3.1 | Plex partial refresh of the section path after a swap | DONE (PR #153): `TestMediaClients_AfterASwapPlexGetsAPartialRefreshOfTheSectionPath`, `TestHook_AC4_*`; a file directly in a section location is never refreshed (review fix) |
| 3.2 | Plex hold on a file currently being played, beside the Tautulli hold | DONE (PR #153): held at the door (no row) and before the swap's rename (ahead of its re-checks); `TestMediaClients_AFileBeingPlayedInPlexIsHeld`, `TestMediaClients_PlexUnreachableDoesNotHoldAndWarnsOnce`, `TestPlayHold_*` incl. `TestPlayHold_TheWaitSitsAheadOfTheSwapsOwnChecks` (red when the wait is moved below them) |
| 3.3 | Sonarr `RescanSeries` and Radarr `RescanMovie` after a swap | DONE (PR #153): `TestMediaClients_AfterASwapSonarrGetsRescanSeriesAndRadarrGetsRescanMovie`, `TestArr_NoCommandIsEverSentWithoutAnOwnerID`; mutation-diff 100%; gate exit 0 (`internal/engine` 2447.4 s, `cmd/holdfast` 882.2 s); CI green; 1 fix round (an adversarial review: 4 MED, 4 LOW, none HIGH, all fixed or recorded) |

## Phase 4 - Webhook intake (line D)

| # | Item | State |
|---|---|---|
| 4.1 | Authenticated intake: both Sonarr Download shapes and Radarr's, queues the file | DONE (PR #155, `c727f7a`): `POST`/`PUT` `/api/webhook/sonarr` and `/api/webhook/radarr`, `webhook_token` as bearer or Basic password; `TestWebhook_SonarrDownloadPerFileShapeQueuesTheFile`, `TestWebhook_SonarrDownloadImportCompleteShapeQueuesEveryFile`, `TestWebhook_RadarrDownloadQueuesTheFile`, `TestWebhook_UpgradeArrivesAsDownloadWithIsUpgradeAndIsQueued`, `TestWebhook_IsAuthenticated`; gate exit 0 (`internal/engine` 2701.8 s, `internal/server` 325.7 s); CI green; 1 fix round (an adversarial review: 2 MED, 5 LOW, none HIGH, fixed) |
| 4.2 | `docs/docker.md`: the native intake replaces the custom script | DONE (PR #155): the Custom Script and the shim are gone; `Connect > Webhook` is the documented route |
| 4.3 | `make api-schema-diff` passes, `.api-schema-breaks.yaml` still `[]` | DONE (PR #155): four `endpoint-added` additions, 0 breaks |

## Phase 5 - Credentials and live checks (line E)

| # | Item | State |
|---|---|---|
| 5.1 | Each new credential key in `config.SecretBearingKeys`, a literal refused | DONE (PRs #153, #155): `plex_token`, `sonarr_api_key`, `radarr_api_key`, `webhook_token`; `TestSecretKeys_PlexSonarrRadarrAreSecretBearingAndRefuseALiteral`, `TestSecretKeys_WebhookTokenIsSecretBearingAndRefusesALiteral` |
| 5.2 | Live-check commands that write a redacted report, filed on the owner's queue for Plex, Sonarr and Radarr | DONE (PR #154, `21ee6fc`; queue #202 Plex, #203 Sonarr, #204 Radarr): `scripts/client-report.sh` + `scripts/clientreport` write `testdata/client-reports/<service>-<date>.json` through the shipped clients; `TestClientReport_*CarriesNoIdentity`, `TestClientReport_RefusesToWriteAReportCarryingTheCredentialOrHost`; mutation-diff 100%; gate exit 0 on the re-run (`internal/engine` 2295.9 s, `cmd/holdfast` 758.4 s); CI green; 1 fix round: the first gate went red from host load only (the engine's 60m clock and two readiness waits; red log commented on the PR) and was re-run unchanged |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | DONE (counted at `21ee6fc`): `func Test` 1724 -> 1834, no package fell (`cmd/holdfast` 244 -> 259, `internal/config` 150 -> 163, `internal/engine` 569 -> 574, `internal/mediaclient` 0 -> 44, `internal/server` 107 -> 125, `scripts/clientreport` 0 -> 15, every other package unchanged); `git diff --numstat 6e1058f origin/main -- docs/design/swap.md docs/design/quality-gate.md` empty (66 and 80 lines); `*_test.go` +7439 -1, the one deleted line below; zero `co-authored-by` in `git log 6e1058f..origin/main --format=%B` |
| 6.2 | Adversarial review of the report | TODO |

### The one deleted `*_test.go` line and why

- `internal/server/scan_test.go`: `if !strings.HasPrefix(r, "GET /api/") && !strings.HasPrefix(r, "POST /api/") {`
  became a check on the route under any method (PR #155, review finding M2), so the guard that
  keeps `restore` and `requeue` off HTTP also sees a `PUT` or any other method; the two `PUT`
  webhook routes joined its list. The guard is stronger, nothing it asserted is gone.

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

- D5 (2026-10-02): the baseline put `internal/engine` at 84.6% of `TEST_TIMEOUT`, past the 80%
  line of §4 (I15), so track `holdfast-g10/test-timeout` (PR #151) raises it from 45m to 60m in
  its own commit with the measurement, and merges first.
- D6 (2026-10-02): the webhook intake's credential is a new key `webhook_token`, by reference,
  that can only queue a file. The arr Webhook connection sends HTTP Basic (`Username`,
  `Password`) and custom `Headers` (`WebhookSettings.cs` and `WebhookProxy.cs` at Sonarr
  v4.0.20.3014 and Radarr v6.4.4.10685, read through `gh api` 2026-10-02), so the token is
  accepted as a bearer header or as the Basic password, never in the URL.
- D7 (2026-10-02): S0178 AC-6 as built (PR #152): a `preserve_mtime` value that is not a boolean
  refuses to start naming the key. Two readings tighten: a number in the file used to read as
  true, and an empty `HOLDFAST_PRESERVE_MTIME` used to read as false; both now refuse (fail-safe:
  never a confident wrong reading). An empty key in the file still loads as the default (I5).
- D8 (2026-10-02): the clients track's own decisions (PR #153) are in its PR body and
  `docs/design/media-clients.md`: package `internal/mediaclient`; the Plex section list read from
  `GET /library/sections/all` as the official reference documents it; JSON only; path maps have
  no environment form; no command without an id and no unscoped refresh can be built; the hold at
  the door writes no row, and the wait before the swap has no upper bound.

- D9 (2026-10-03): PR #155's own decisions are in its PR body and
  `docs/design/media-clients.md#webhook-intake`: the control and read tokens are not accepted on
  the intake; a `webhook_token` that resolves to a server token refuses to start; an event
  holdfast does not consume answers 200 and queues nothing; a pause answers 200; a path not in
  clean form is refused before the path map; Rename queues the new path only.
- D10 (2026-10-03): merging `origin/main` into a branch stacked on a squash-merged one conflicts
  on the same content. The branch first merged the stacked branch's final commit, then
  `origin/main` keeping the branch side, and the merge was checked: `git diff origin/main HEAD`
  equals the branch's own diff over the stacked commit (same `sha1sum`).

- D11 (2026-10-03): another session opened PR #156 (`speed/gate`, a faster local gate) in this
  repo during this goal. It is not this goal's; this goal touched none of its branch or files
  (§0.13).
- D12 (2026-10-03): no minor release is cut (T37 optional): every new key is off by default and
  the live checks are still on the owner's queue; a later goal can release them.

## Proposals awaiting the owner

- Fail-open against fail-closed for the Plex play hold (D3).
- A bound on the wait before the swap while a file is played: today it is unbounded (it only
  delays) and says so every 10 minutes; a paused session pins a worker
  (`docs/post-swap-hook.md`).
- `TestServeSmoke` waits 3 s for the listener, and two runs under host load average 40-60 missed
  it; a longer wait in that test would remove a flake that is not about the code.

## Resume here

All PRs merged: #151, #152, #153, #155, #154; the live checks are queue #202-#204. No branch,
worktree or open PR of this goal remains (#156 is another session's). Left: the adversarial
review of the GOAL REPORT (6.2), then the COMPLETE line, `goals check`, and the report.
