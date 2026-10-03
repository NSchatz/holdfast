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
| `make check` under `goals-heavy` then `flock -o` `holdfast-heavy` | exit 0 in 2021 s | `gate-baseline.log` |
| `internal/engine` under `go test -race` (from that run) | ok, 89.0% coverage, 1869.2 s (51.9% of `TEST_TIMEOUT` 60m) | `gate-baseline.log` |
| `cmd/holdfast`, `internal/server` (from that run) | ok, 731.5 s; 325.8 s | `gate-baseline.log` |
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
| 1.3 | Baseline gate with timings | DONE (`d636e7f`): `make check` exit 0 in 2021 s; the table above (D4: the first run lacked ffmpeg on `PATH`) |

## Phase 2 - Triage rows (line B)

| # | Item | State |
|---|---|---|
| 2.1 | S0175 `server-warning-scope`: the two read-surface statements are emitted by `serve`, hedged by `validate`, and not at all by `run`; the set-token statement says what `/` serves once the UI is embedded (track `holdfast-g13/notices`) | DONE (PR #165, `ff3f4f5`): `Config.ReadSurfaceNotices(scope)`; `cmd/holdfast/server_notice_scope_test.go` grades AC-1 to AC-8 on the real CLI (`TestS0175_AC1_RunSaysNothingAboutAReadSurfaceItDoesNotOpen` to `TestS0175_AC8_AMalformedServerAddrStillRefusesRunAndValidate`), each shown red by a named assertion under nine mutations of the product code (D14); gate exit 0 in 2286 s on `9048c56` (`internal/engine` 1991.8 s, 55.3% of `TEST_TIMEOUT`; `cmd/holdfast` 806.2 s), AC-9; CI green (`build`, `package`, `mutation`); mutation-diff 100.00% (killed 31, lived 0); 1 fix round |

## Phase 3 - Toolchain, gate, pin check, embed, image (track `holdfast-g13/ui`)

| # | Item | State |
|---|---|---|
| 3.1 | `web/`: Svelte 5, Vite, TypeScript 6.x, vitest, svelte-check, eslint; exact versions, a lockfile, pnpm through `packageManager`, Node LTS in `web/.node-version`; `ignoreScripts: true`, `minimumReleaseAge: 1440` and `fsevents` in `allowBuilds`, all in `pnpm-workspace.yaml`; versions re-checked on the registry today | DONE (PR #161, `0c3b25d`): `web/` with `svelte` 5.57.1, `vite` 8.3.2, `typescript` 6.0.3, `vitest` 5.0.3, `svelte-check` 4.7.6, `eslint` 10.11.0 (D5); Node 24.21.0 in `web/.node-version`; pnpm 12.8.1 with its sha512 in `packageManager`; `web/pnpm-lock.yaml`; `ignoreScripts: true`, `minimumReleaseAge: 1440` and `allowBuilds: fsevents: false` in `web/pnpm-workspace.yaml` |
| 3.2 | `check-pins.sh` section 8: `ignoreScripts: true` in `pnpm-workspace.yaml` for a pnpm project, a `minimumReleaseAgeExclude` list refused; the selftest fails an `.npmrc`-only setup (line D) | DONE (PR #161, `0c3b25d`): section 8 requires a committed `pnpm-workspace.yaml` with `ignoreScripts: true` for a pnpm project and refuses a `minimumReleaseAgeExclude` list; selftest 88/88, case 58 "a pnpm project that sets ignore-scripts ONLY in .npmrc is caught (pnpm does not read it)" and case 59 the same for a new project; section 12 holds Node, pnpm, the packages, the install line and the lockfile |
| 3.3 | `make check` runs UI lint, typecheck, unit tests and build on the pinned toolchain; `go build` and `go vet` pass without a built UI (line C, foundation) | DONE (PR #161, `0c3b25d`): `check:` runs `ui-lint ui-typecheck ui-test ui-build` between `vet` and `build`; gate exit 0 in 2210 s on `b485e60` (`ea2be7e` merged up to `origin/main`), `internal/engine` 1944.9 s (54.0% of `TEST_TIMEOUT`), `cmd/holdfast` 776.9 s; "ui: Node 24.21.0 and pnpm 12.8.1, as pinned", eslint clean, svelte-check 0 errors 0 warnings, vitest 21 of 21, vite build; `TestUnbuiltTree_BuildsAndVetsBecauseOfThePlaceholder`; CI green (`build`, `package`, `mutation`); mutation-diff 100.00% against the 70% floor (killed 22, lived 0); 0 fix rounds |
| 3.4 | `internal/ui`: `go:embed` with a committed placeholder; the UI served at `/` with the source offer in the page; the offer still served and tested where no UI is built (line E) | DONE (PR #161, `0c3b25d`): `internal/ui` (`//go:embed all:dist`, placeholder `internal/ui/dist/.gitkeep`); `GET /` answers a request for HTML with the page and the offer written into it, every other request with the plain-text page unchanged (D6); `GET /assets/*`; `TestUI_RootServesThePageWithTheSourceOfferToARequestForHTML`, `TestUI_ARequestThatDoesNotAskForHTMLGetsThePlainTextPageUnchanged`, `TestUI_AssetsAreServedByNameAndNothingElseIs`, `TestWireUI_ServesAWholeBuildAndNoPartOfABrokenOne`, the existing `internal/server/sourceoffer_test.go` unchanged and green; `make api-schema-diff` 21 additions, 0 breaks |
| 3.5 | The image: a UI build stage on a Node image pinned by tag and digest; the final image stays distroless; CI `package` green (line E) | DONE (PR #161, `0c3b25d`): the `ui` stage is `FROM node:24.21.0-trixie-slim@sha256:8ec5d755...`; the runtime stage is still `gcr.io/distroless/cc-debian13:nonroot@sha256:54df941e...`; CI `package` green on `ea2be7e` (run 37130784349), its smoke test: "serve answers a request for HTML at / with the web UI's page, carrying the Corresponding Source offer" |
| 3.6 | CI: the `build` job runs the gate with the pinned Node and pnpm | DONE (PR #161, `0c3b25d`): `actions/setup-node` (the commit of `v7`) with `node-version-file: web/.node-version` in `ci.yml` and `release.yml`; CI `build` ran "ui: Node 24.21.0 and pnpm 12.8.1, as pinned" through corepack and passed in 23m22s |
| 3.7 | Adversarial review of the branch before its gate | DONE (PR #161, `0c3b25d`, commit `ea2be7e` on the branch): a fresh agent found no HIGH, 5 MED, 11 LOW; fixed with a selftest case or a test each (D11) |

## Phase 4 - A gate fix found on the way (track `holdfast-g13/node-flake`)

| # | Item | State |
|---|---|---|
| 4.0 | `main`'s CI went red twice (the runs for `25e0c9c` and `fe5c7bf`) on a race in a goal-12 test of `internal/node`; the three reads that expect a streamed digest wait for its record (D15) | DONE (PR #166, `2f1f4ed`): gate exit 0 in 2211 s on `76786b5` (`internal/engine` 1983.0 s, 55.1% of `TEST_TIMEOUT`; `cmd/holdfast` 803.9 s); CI green (`build`, `package`, `mutation`); mutation-diff: no mutant in scope (a test file only); 40 runs of the three tests under `-race` green; with the record delayed 30 ms the tests as on `main` fail by CI's message; 0 fix rounds |

## Phase 5 - Report

| # | Item | State |
|---|---|---|
| 5.1 | Gate integrity counted from the goal-start SHA | DONE (counted at `2f1f4ed`): `func Test` 2004 -> 2033, no package fell (`cmd/holdfast` 269 -> 280, `internal/config` 174 -> 176, `internal/server` 129 -> 137, `internal/sourceoffer` 9 -> 10, `internal/ui` 0 -> 7, every other package unchanged); `git diff --numstat d636e7f origin/main -- docs/design/swap.md docs/design/quality-gate.md` empty (66 and 80 lines); `*_test.go` +1456 -10, the 10 deleted lines in D16; zero `co-authored-by` in `git log d636e7f..origin/main --format=%B` |
| 5.2 | Adversarial review of the report | DOING |

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

- D11 (2026-10-03): the branch review of `holdfast-g13/ui` (a fresh agent, before the gate).
  Fixed: the smoke test searched `docker logs` through a pipe that could fail on a match under
  pipefail; the pin check stayed green on `minimumReleaseAgeStrict: false`, on an install line
  carrying `--ignore-scripts=false` or a `pnpm_config_*` variable (each measured to run a root
  `postinstall` past the workspace file under pnpm 12.8.1), on a pnpmfile, on an inline
  `allowBuilds` map, on a `ui` stage that was not the node image, on a lockfile entry with no
  registry sha512 and on an `overrides` block (selftest cases 76 to 87); `ui.Load` accepted a
  page naming an asset the build did not hold; `scripts/ui.sh` left a temporary directory; the
  comments claimed the `packageManager` hash pins pnpm's binary, and for pnpm 12 it pins a
  launcher that fetches the native binary and checks it against npm's registry signatures (the
  text now says so). Left as they are, with the reason: `make fmt` walks `web/node_modules`
  (one gofmt-clean Go file there today; an unformatted one would be a loud red naming its
  path); the surface document describes `GET /` 200 as `text/plain` only (its format keys a
  response by status; the second representation is in `docs/design/web-ui.md#root` and the API
  reference); the arm64 smoke never reaches the UI step (D9); the statements that holdfast
  ships no frontend in `README.md` and `docs/docker.md` (D2, goal 14's).
- D12 (2026-10-03): a duplicate of the gate job was queued by mistake behind the mutation run
  and merged `origin/main` (two ledger-only commits) into the local branch before running, so
  the gate that counts ran on `b485e60`, the PR head `ea2be7e` merged up to `origin/main`. The
  merge commit was never pushed (main had moved by ledger commits only, §0.3) and went with the
  worktree. The other copy was stopped while it waited for the lock and ran nothing.
- D13 (2026-10-03): Dependabot opened #162 (the Node image), #163 (the distroless base) and
  #164 (the `web/` packages) after #161 merged. They are the bot's proposals under S0151
  ("opens PRs that are never merged" by an agent), not PRs of this goal, and are left open.

- D14 (2026-10-03): S0175, as built and reviewed. The builder's mutations, each restored after
  its red: `run` logging the read surface again (AC-1), `cmdServe` without `logReadSurface`
  (AC-2, AC-6, AC-7), `validate` in the serving wording (AC-3), the loopback rule dropped
  (AC-5), `validate` ignoring a token it can see (AC-4), `server_addr` validation skipped
  (AC-8), a present-but-empty variable counted as set (AC-7), every notice suppressed (AC-1's
  undo-window half), the old no-frontend wording (AC-2, AC-3, AC-6). A fresh adversarial review
  of the branch found no HIGH: the only listener is `runServer`, its only caller `cmdServe`.
  Fixed from it: the documents that said the read surface is stated "at startup" name `serve`
  (`docs/docker.md`, `docs/secrets.md`, `docs/design/nodes.md`, `config.example.yaml`), and the
  root statement says the page goes to a request that asks for HTML and its static files to any
  client. The fix round: `7685d85` reworded that statement and dropped the phrase the tests
  hold it to, red locally and in CI's `mutation` job; `9048c56` restores it. Left: `serve` with
  an absent `server_addr` is graded at the unit and on `validate`, not on a live `serve`, which
  would bind port 8080 on a shared host; the tests' child environment clears the two variables
  they are about and not every `HOLDFAST_*` (a false red at worst).
- D15 (2026-10-03): a flaky goal-12 test. `main`'s CI run for the ledger commit `25e0c9c` was
  red in `build`: the mutation gate selftest's throwaway suite failed on
  `TestNodeFixture_AConditionalSourceRequestIsAnsweredWithTheWholeSource` ("the whole source
  was sent and "" recorded for it"). The source handler records the streamed digest after
  `http.ServeContent` returns, so a test that reads the record straight after the response
  races it. Outside §17, and fixed here because a gate that flakes costs every later goal a fix
  round: the three reads that expect a digest wait for the record (at most 5 s); with the
  record delayed 30 ms the tests as on `main` fail by CI's message and the fixed ones pass. No
  product change. For the owner's eye, not built on: a completion that arrives before the
  record exists is not held to the transport comparison (`decideCompleteStreamed` skips an
  empty streamed digest); the engine's own hash of the source still is, and a real worker
  encodes between the two.
- D16 (2026-10-03): the deleted `*_test.go` lines since the goal-start SHA, with their reasons.
  In `internal/config/read_token_test.go` (PR #165), 8 lines: the 5-line comment of the
  `readTokenNotices` helper and its `for _, n := range c.Notices()` line, because the two
  statements moved from `Notices()` to `ReadSurfaceNotices` and the helper now reads the
  serving scope (same three directions graded); two `t.Fatalf` lines whose message named
  `Notices()`, reworded to name `ReadSurfaceNotices(serving)` with the same condition. In
  `internal/node/source_test.go` (the flake fix), 2 lines: `f.streamed(...)` became
  `f.streamedOnceRecorded(...)` in two comparisons, same expected value and message. Outside
  `*_test.go`: `scripts/check-pins-selftest.sh` case 55 (D10).
- D17 (2026-10-03): no release at this goal's end (T37 makes it optional for goals 5 to 14).
  What merged is the UI's shell with no views; goal 14 builds them, and a release that
  announces a web UI is better cut with one.

## Requests

None filed, none addressed to holdfast (T6, §0.13). The owner's queue: no new item so far; nothing in this goal needs the owner's hands (no hardware, no live service).

## Resume here

All three PRs of this goal are merged: #161 (`0c3b25d`), #165 (`ff3f4f5`), #166 (`2f1f4ed`).
No branch or worktree of this goal is left, and no PR of it is open (the open ones are
Dependabot's and another program's `speed/gate`, D13). The owner's queue has no new item.

Left: `main`'s CI run for `2f1f4ed` (watched; log `main-ci.log`), the fresh adversarial review
of the report's lines A to G against the repos (row 5.2), then the COMPLETE line,
`goals check /workspace/holdfast 2026-09-holdfast 13` and the GOAL REPORT. Evidence files are
under `/cache/tmp/holdfast-g13/` (`gate-ui-1.log`, `gate-notices-2.log`, `gate-flake-1.log`,
`evidence/`, `pr161-smoke.txt`); the directory is deleted at the goal's end.
