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
| 2.1 | S0175 `server-warning-scope`: the two read-surface statements are emitted by `serve`, hedged by `validate`, and not at all by `run`; the set-token statement says what `/` serves once the UI is embedded (track `holdfast-g13/notices`) | DOING |

## Phase 3 - Toolchain, gate, pin check, embed, image (track `holdfast-g13/ui`)

| # | Item | State |
|---|---|---|
| 3.1 | `web/`: Svelte 5, Vite, TypeScript 6.x, vitest, svelte-check, eslint; exact versions, a lockfile, pnpm through `packageManager`, Node LTS in `web/.node-version`; `ignoreScripts: true`, `minimumReleaseAge: 1440` and `fsevents` in `allowBuilds`, all in `pnpm-workspace.yaml`; versions re-checked on the registry today | DOING |
| 3.2 | `check-pins.sh` section 8: `ignoreScripts: true` in `pnpm-workspace.yaml` for a pnpm project, a `minimumReleaseAgeExclude` list refused; the selftest fails an `.npmrc`-only setup (line D) | DOING |
| 3.3 | `make check` runs UI lint, typecheck, unit tests and build on the pinned toolchain; `go build` and `go vet` pass without a built UI (line C, foundation) | DOING |
| 3.4 | `internal/ui`: `go:embed` with a committed placeholder; the UI served at `/` with the source offer in the page; the offer still served and tested where no UI is built (line E) | DOING |
| 3.5 | The image: a UI build stage on a Node image pinned by tag and digest; the final image stays distroless; CI `package` green (line E) | DOING |
| 3.6 | CI: the `build` job runs the gate with the pinned Node and pnpm | DOING |
| 3.7 | Adversarial review of the branch before its gate | DOING |

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

- D4 (2026-10-03): the container was recreated before this goal started and `ffmpeg` is no
  longer on its `PATH` (§0.11 says it is). The pinned build (`N-125875-g5d4d3bdc61`, the
  Dockerfile's pin) is at `/cache/ffmpeg/bin`, so every gate and test of this goal runs with
  `PATH=/cache/ffmpeg/bin:/cache/opt/dynhdr-tools/bin:$PATH`. The first baseline run, started
  without it, went red on "ffmpeg is required" in every package that drives ffmpeg and was
  stopped at 746 s (`gate-baseline-noffmpeg.log`); the baseline in the table is the second run.
- D5 (2026-10-03): versions, read from the npm registry (`npm view <package> dist-tags time`) and
  https://nodejs.org/dist/index.json on 2026-10-03: Node 24.21.0 (the newest LTS release; Node 26
  becomes LTS on 2026-10-28 by https://raw.githubusercontent.com/nodejs/Release/main/schedule.json,
  read 2026-09-29, and a bump to it is left to a later goal), pnpm 12.8.1, `svelte` 5.57.1,
  `vite` 8.3.2, `@sveltejs/vite-plugin-svelte` 7.3.1, `svelte-check` 4.7.6, `vitest` 5.0.3,
  `typescript` 6.0.3 (the newest 6.x; `svelte-check` and `typescript-eslint` do not accept 7),
  `typescript-eslint` 8.71.0, `eslint` 10.11.0 (10.12.0 was published 2026-10-02T20:08Z, under a
  day old, and `minimumReleaseAge: 1440` refuses it), `@eslint/js` 10.0.1, `eslint-plugin-svelte`
  3.23.0, `globals` 17.13.0, `jsdom` 30.1.1, `@testing-library/svelte` 5.4.2, `@types/node`
  24.19.1. Licences (`npm view <package> license`): TypeScript is Apache-2.0, every other direct
  dependency MIT; across the lockfile (`pnpm licenses list`) MIT 166, Apache-2.0 17, ISC 8,
  BSD-2-Clause 8, BSD-3-Clause 3, MIT-0 2, MPL-2.0 2, BlueOak-1.0.0 2, CC0-1.0 1. Only the
  Svelte runtime's code is in the built bundle (the module list of a production build); `NOTICE`
  carries its MIT notice. The Node image is `node:24.21.0-trixie-slim`, digest read from
  registry-1.docker.io with `crane digest` on 2026-10-03; `actions/setup-node` is pinned to the
  commit its `v7` tag names (`gh api`, 2026-10-03).
- D6 (2026-10-03): what `/` answers. A request whose `Accept` names HTML gets the embedded UI's
  page with the source offer written into it; every other request gets the plain-text page,
  byte for byte what it was. Reasons: the surface gate (`make api-schema-diff`) records
  `GET /` 200 as `text/plain`, and changing that for every client is a break of the released
  surface (I5, and spec contract §9, add or deprecate, never break); a browser always names
  `text/html`. The reasoning's home is `docs/design/web-ui.md#root`.
- D7 (2026-10-03): no `web/.npmrc`. The research recommended one for anyone running npm; the
  container's commit hook and the selftests' throwaway commits refuse a file of that name, pnpm
  does not read it, and section 8 now says so. The selftest writes one itself to prove an
  `.npmrc`-only pnpm project fails.
- D8 (2026-10-03): `scripts/ui.sh` finds the pinned Node and pnpm on `PATH`, through corepack, or
  through mise, and refuses otherwise. So the supervisor's nightly `make tier-full`, which sets
  up only Go, keeps working in this container (mise is there), and CI needs only `setup-node`.
- D9 (2026-10-03): the image smoke test's UI step runs in the native (amd64) smoke only. The
  arm64 smoke is exec-only under QEMU by its own design; the arm64 binary embeds the same files
  from the same `ui` stage, and the build stage checks they arrived.
- D10 (2026-10-03): `scripts/check-pins-selftest.sh` case 55 counted one MIT permission notice
  left in `NOTICE` after dropping one tool's; `NOTICE` now carries Svelte's too, so it counts
  two. A changed precondition of a case, not a weakened assertion: the case still asserts the
  same refusal by the same message.

## Requests

None filed, none addressed to holdfast (T6, §0.13). The owner's queue: no new item so far.

## Resume here

Both tracks are built and pushed or committed, neither is merged, no PR is open yet.

- `holdfast-g13/ui` in `/cache/wt/holdfast/g13-ui` (5 commits, head `25fcade`, not pushed yet):
  targeted tests green; a fresh adversarial review of the branch is running; its
  `make mutation-diff` is queued behind the baseline (log `mutation-ui-1.log`). Next: apply the
  review, run the gate under the heavy locks with D4's `PATH`, push, open the PR with the gate
  tail, wait for CI (`build`, `package`, `mutation`), merge.
- `holdfast-g13/notices` in `/cache/wt/holdfast/g13-notices` (pushed, head `90a1d01`): S0175,
  AC-1 to AC-8 graded by `cmd/holdfast/server_notice_scope_test.go`. Next, after the UI PR
  merges: merge `origin/main` into it, gate, PR, CI, merge.
- The baseline gate is running in `/cache/wt/holdfast/g13-baseline` (log `gate-baseline.log`);
  its figures go in the Baselines table.
