# Goal 13 ledger - web UI: toolchain, gate, embed and image

- Brief: `.claude/goals/2026-09-holdfast.md` §0, §4 and §17 (spec v1.1, the `/goals:spec` skill);
  goal file `.claude/goals/2026-09-holdfast-g13.goal.txt`. Amended by
  `.claude/goals/CHECKPOINT-T.approved`: "P1 proposal-triage.md: approved as proposed."
- Started: 2026-10-03, from `/workspace/holdfast` in the maker container, by the goals supervisor's
  `/goal` condition.
- Goal-start SHA: `d636e7f77c68916c53e1e1e6411991ebb8fcdad4` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g12.status.md` (ends
  `COMPLETE (goal 12): 2026-10-03`). The owner's queue at start (`goals needs list`): eleven open
  holdfast items, queue #50 to #56, #202 to #204 and #219.
- Precondition, as checked:

```
$ git fetch origin && git rev-parse HEAD origin/main
d636e7f77c68916c53e1e1e6411991ebb8fcdad4
d636e7f77c68916c53e1e1e6411991ebb8fcdad4
$ git show origin/main:.claude/goals/2026-09-holdfast-g12.status.md | grep -n 'COMPLETE (goal 12)'
211:COMPLETE (goal 12): 2026-10-03
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
$ goals admit --agents 4
decision: admit a goal
agents: 4 of 4 allowed (requested)
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md`, line 66, "Per goal", last
changed in `d4d70a6`): "**Goal 13** (web UI: toolchain, gate, embed): S0175." Its row (line 43):
"`server-warning-scope`: limit the read-API exposure notices to the modes that can actually know
them | keep | goal 13 (web UI serving)". The spec's full text was read in goal 1's read-only
umbrella clone (push URL disabled), at its commit of 2026-09-24.

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g13-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g13/tmp` and the pinned dynamic-HDR tools on PATH. Logs under
`/cache/tmp/holdfast-g13/`.

| What | Value | Source |
|---|---|---|
| `make check` under `goals-heavy` then `flock -o` `holdfast-heavy` | running at the time of this commit; the figure lands with the next ledger commit | `gate-baseline.log` |
| `func Test` count, all packages | 2004 | `rg -c '^func Test' -g '*_test.go'`, `functest-start.txt` |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines | 66, 80 | `wc -l` |
| `CLAUDE.md` lines | 199 | `wc -l` |
| Node and pnpm on PATH in the container | Node 24.21.0, pnpm 12.8.1 | `node --version`, `pnpm --version` |
| Usage headroom | five_hour 8%, seven_day 54%; 4 of 4 agents allowed | `goals admit --agents 4` |
| Host load at start | load average 26 on 56 CPUs (quota 28) | `uptime` |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`d636e7f`): goal 12's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (the commit that adds this file): fast docs checks passed first |
| 1.3 | Baseline gate with timings | DOING |

## Phase 2 - Triage rows (line B)

| # | Item | State |
|---|---|---|
| 2.1 | S0175 `server-warning-scope`: the two read-surface statements are emitted by `serve`, hedged by `validate`, and not at all by `run`; the set-token statement says what `/` serves once the UI is embedded (track `holdfast-g13/notices`) | TODO |

## Phase 3 - Toolchain, gate, pin check, embed, image (track `holdfast-g13/ui`)

| # | Item | State |
|---|---|---|
| 3.1 | `web/`: Svelte 5, Vite, TypeScript 6.x, vitest, svelte-check, eslint; exact versions, a lockfile, pnpm through `packageManager`, Node LTS in `web/.node-version`; `ignoreScripts: true`, `minimumReleaseAge: 1440` and `fsevents` in `allowBuilds`, all in `pnpm-workspace.yaml`; versions re-checked on the registry today | TODO |
| 3.2 | `check-pins.sh` section 8: `ignoreScripts: true` in `pnpm-workspace.yaml` for a pnpm project, a `minimumReleaseAgeExclude` list refused; the selftest fails an `.npmrc`-only setup (line D) | TODO |
| 3.3 | `make check` runs UI lint, typecheck, unit tests and build on the pinned toolchain; `go build` and `go vet` pass without a built UI (line C, foundation) | TODO |
| 3.4 | `internal/ui`: `go:embed` with a committed placeholder; the UI served at `/` with the source offer in the page; the offer still served and tested where no UI is built (line E) | TODO |
| 3.5 | The image: a UI build stage on a Node image pinned by tag and digest; the final image stays distroless; CI `package` green (line E) | TODO |
| 3.6 | CI: the `build` job runs the gate with the pinned Node and pnpm | TODO |
| 3.7 | Adversarial review of the branch before its gate | TODO |

## Phase 4 - Report

| # | Item | State |
|---|---|---|
| 4.1 | Gate integrity counted from the goal-start SHA | TODO |
| 4.2 | Adversarial review of the report | TODO |

## Decisions taken

- D1 (2026-10-03): tracks. `holdfast-g13/ui` carries §17 items 2 to 6 in one PR, because the
  gate targets, the pin check, the embed and the image stage each need the others to be green
  (T52: few larger PRs). `holdfast-g13/notices` carries S0175, built beside it in its own
  worktree and merged second, so its statement about what `/` serves is checked against the
  merged serving code.
- D2 (2026-10-03): the statement that holdfast ships no frontend stays as written in `README.md`,
  `docs/api-reference.md` and `docs/docker.md`: §18 item 4 gives its rewrite to goal 14, with the
  views. This goal changes only what it must: the `CLAUDE.md` Layout line for the new package and
  the notice S0175 rewords.
- D3 (2026-10-03): every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g13/` and
  `PATH=/cache/opt/dynhdr-tools/bin:$PATH` (goal 9's finding), goal shells use `rg` or `git grep`,
  and commits carry no trailer (T38).

## Requests

None filed, none addressed to holdfast (T6, §0.13). The owner's queue: no new item so far.

## Resume here

The baseline gate is running in `/cache/wt/holdfast/g13-baseline` (log
`/cache/tmp/holdfast-g13/gate-baseline.log`). Next: record its figures in the Baselines table,
then build the two tracks of D1 in worktrees under `/cache/wt/holdfast/`.
