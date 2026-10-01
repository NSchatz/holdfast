# holdfast - the Tdarr replacement at feature parity, without trading away the gate (brief v2, 2026-09-29)

This program is fifteen `/goal` runs, in order, goal 1 to goal 15. It runs beside the shopkit, 3d,
devices and home programs, which live in other containers on the same host and share nothing with
it but CPU (T6). Goal 1 ends at Checkpoint T; goals 2 to 15 then chain without further review
(T3).

**Amended 2026-10-01 by the goals program** (`AMENDMENT-2026-10-01-goals-g5-2026-09-holdfast.md`, carrying out W4, W7, W19, W20, W26, W30, W31 and W64): the goals supervisor replaces `claude-goal-chain` as holdfast's runner, with its checker, BLOCKED and INCOMPLETE as end states, later checkpoints to a fresh independent reviewer and the lock tool's names and order; it applies to goals whose ledger is created after it merged and never against a line of their goal file.

**Amended 2026-10-01 by the goals program** (`AMENDMENT-2026-10-01-goals-g6-2026-09-holdfast.md`, carrying out W1, W7, W35, W52, W63 and W64): this program pins spec v1.0; where §0-§4 and the spec disagree, the spec wins; it applies to goals whose ledger is created after it merged and never against a line of their goal file.

Every goal reads §0-§4 in full plus its own section. §0-§4 are the contract; a goal section says
*what* to build and *when it is done*. Where they disagree, §0-§4 win, except where
`CHECKPOINT-T.approved` amends them: the owner's amendments beat this file.

**Where it comes from.** A planning interview with the owner on 2026-09-29: 14 rounds, 55
questions, 55 decisions (T1-T55), plus planner inferences I1-I20 that Checkpoint T shows for
amendment. The owner's emphasis, in the option they chose: **"Tdarr feature parity"** (T1), with
the program replacing the spec-driven (SDD) pipeline as holdfast's plan of record (T2), sized "as
big as needed" (T4) and run under the owner's standing contract unchanged (T7). The shape is
foundations first (T44): a start-up goal that hardens the base, triages the old specs and writes
every research-and-propose decision for the checkpoint (T48); then the encode plan and fidelity
gate everything else hangs off; then hardware encode, audio and subtitles, dynamic HDR and crop,
queue and health, media-server clients, distributed nodes, and the web UI last, so it can show
everything; then a finale that releases it.

**v2.** This version answers the adversarial review of v1 (`review-brief-v1.md`: 5 HIGH, 7 MED,
14 LOW) and the three research verifications (`verify-*.md`); §23 answers every finding. The
largest changes: the owner's name is kept out of the public repo from the first push (I1), PR
branches are updated by merge rather than force-push (I11), five goals were split so no goal is
more than a day (I13), and a dropped track has a legal state (I14).

The invariant in `CLAUDE.md` governs every goal and is never weakened: **no source is mutated
until a replacement passed every gate**, and the swap is an atomic same-filesystem `rename()` from
the source's own directory. Every capability below is additive to the gate set.

| Goal | Title | § |
|---|---|---|
| 1 | Start-up, triage and proposals (ends at Checkpoint T) | §5 |
| 2 | Carried specs and the debian13 base | §6 |
| 3 | The encode plan | §7 |
| 4 | The fidelity gate and per-encoder quality | §8 |
| 5 | Hardware runtime, argv and detection | §9 |
| 6 | Hardware decode, new encoders and hardware reports | §10 |
| 7 | Audio and subtitles | §11 |
| 8 | Dynamic HDR and crop | §12 |
| 9 | Queue priority and the health sweep | §13 |
| 10 | Media-server clients | §14 |
| 11 | Worker nodes: protocol, shared mount, server re-gate | §15 |
| 12 | Worker nodes: HTTP streaming, TLS and deployment | §16 |
| 13 | Web UI: toolchain, gate, embed and image | §17 |
| 14 | Web UI: views and controls | §18 |
| 15 | Finale: release, docs and the program report | §19 |

---

## §0 How to run (every goal)

### 0.1 Where to start

- **Launch directory:** the `NSchatz/holdfast` checkout: `/workspace/holdfast` in the maker container
  (one session per repo, since 2026-09-30), or `/workspace` in a standalone holdfast container (clone
  it there if missing). The session must have been started after `git pull` brought in
  `.claude/settings.json`, so its `env` is loaded. Sibling repos and access:
  - `NSchatz/holdfast` - read and write (branches, PRs, self-merge, the T35 deletions, T37 minor
    releases).
  - `NSchatz/super` (private umbrella; the SDD specs) - **read only** (T32). Clone to
    `/cache/tmp/holdfast-super-ro` and immediately run
    `git -C /cache/tmp/holdfast-super-ro remote set-url --push origin DISABLED`, so a push cannot
    happen by accident. Never branch, PR or push there.
  - `NSchatz/homelab` (private; the deployment and the Bash predecessor) - **PR, never merge**
    (T32). Clone to `/cache/wt/homelab/holdfast-g<n>` only when a goal opens its PR.
  - Every other repo: never touched.
- **First actions of every goal**, in order:
  1. `git pull --rebase` in the launch directory (§0.1) (and in any sibling clone this goal uses).
  2. Check the precondition (the goal's own section). If it fails, print only the BLOCKED
     report (§21) and stop.
  3. Read the earlier goals' ledgers (`.claude/goals/2026-09-holdfast-g*.status.md`), the
     `CHECKPOINT-T.approved` amendments (goals 2-15), proposal P1's rows for this goal, and
     `.claude/goals/NEEDS-OWNER.md`.
  4. Create this goal's ledger (§0.5) as the goal's first commit (straight to `main`); its header
     records the **goal-start SHA** (`git rev-parse origin/main` before any work), which the
     gate-integrity line counts from.
  5. Re-baseline the gates the goal will change, with timings (§0.11 gives the last known).
- **After a compaction:** re-read §0-§4, the goal's own section and its ledger before the next
  action.

### 0.2 Autonomy and fallbacks

- Fully autonomous: never stop to ask. The owner is not watching the run.
- An unsettled choice takes the option that keeps everything that exists building and valid (for
  holdfast: an existing config produces the same decisions and argv as before; I5). It is
  recorded in the ledger's "Decisions taken" with its reasoning, and the goal carries on.
- A research-and-propose decision (T10, T30/T33, T45, T46, and the questions of I7 and I16) is
  written as a proposal in goal 1 (options, costs, recommendation) and decided at Checkpoint T
  (T48). Later goals build on the approved option, or on the recommendation where the approval
  is silent. Nothing proposed is ever silently final.
- A finding after goal 1 that needs a decision only the owner can make does not stop the run:
  the goal takes the fallback above, records it under "Decisions taken", and lists it under
  "Proposals awaiting the owner" in its GOAL REPORT and in the finale report.
- A missing tool is no blocker until a rootless install was tried (`mise use`, `uv`, a pinned
  tarball) and its failure shown. There is no `sudo`, and `apt` needs root.
- NEEDS-OWNER is only for steps physically impossible for an agent (§0.6).

### 0.3 Repos, branches, PRs

- **Branches:** `holdfast-g<n>/<topic>`, one per track, from up-to-date `origin/main`.
- **Commits:** Conventional Commits (`feat(engine): ...`, `fix(encoder): ...`, `docs: ...`,
  `chore(goals): ...` for ledger commits), authored as the repo's configured git user. **No
  `Co-Authored-By` and no AI trailer in commits**: `CLAUDE.md` forbids them and wins over the
  session's attribution reminder (T38); `.claude/settings.json` sets `attribution.commit` to ""
  so the CLI adds none (I18). A PR body may end with the session's "Generated with Claude Code"
  line and session link, which are not co-author trailers.
- `git pull --rebase` on `main` before every ledger commit. **Never force-push, never rewrite
  history** (T7), never discard another session's commits.
- **Updating a PR branch (I11):** when `origin/main` moves, merge it into the branch
  (`git merge origin/main`), never rebase a pushed branch. The squash merge keeps `main` linear.
  If `origin/main` moved only by ledger-only commits (paths under `.claude/goals/` only), the
  last gate result still stands; any other change on `main` means the gate runs again.
- **The merge rule (T36, T52):** a PR is self-merged with `gh pr merge --squash --delete-branch`
  only when BOTH hold on the branch up to date with `origin/main`:
  1. the local gate passes: `timeout 10800 flock -o /cache/locks/holdfast-heavy.lock mise exec
     go@1.25.14 -- make check`, with its last 20 lines, its wall-clock and the `internal/engine`
     seconds pasted in the PR body;
  2. the PR's CI is green (`build`, `package`, `mutation`), waited for with
     `timeout 3600 gh pr checks <n> --watch`. CI is the only place the image builds and smokes,
     because this container has no Docker daemon.
- **Bounded fixing:** a red gate is fixed on the branch, at most 3 fix rounds per PR. At the limit
  the track is `DROPPED (why)` in the ledger, the PR is closed with the reason and the three red
  tails, and the goal carries on, unless the track carries a line marked (foundation), in which
  case the goal ends INCOMPLETE (§0.10, I14).
- **Batch the work (T52):** the local gate takes about 26 minutes here (§0.11), so a goal groups
  its work into a few larger PRs per track, not one per commit.
- **Ledger-only commits** (the goal's `.status.md`, `NEEDS-OWNER.md`) go straight to `main` with
  `chore(goals): ...`, after the fast docs checks of §4 pass on them; nothing else does.
- **Releases (T37):** goals 5-14 may cut a minor release at their end when `main` is green
  (optional; the ledger says whether and why); goal 15 must.
- **CI as it really behaves:** `ci.yml` (push and PR: `make check`, selftests, the config
  self-test, amd64 image smoke with a real encode, arm64 smoke with `--no-encode`; 10-13 minutes),
  `mutation.yml` (diff-scoped on PRs; full run Saturdays, which opens an issue when it fails),
  `pin-health.yml` (Mondays), `release.yml` (`v*` tags). GitGuardian also checks PRs.
- The program runs alone in this repo, so no merge lock is needed beyond the heavy lock.

### 0.4 Multi-agent orchestration

- The owner opted in to Workflows and subagents for parallel tracks, research and adversarial
  review (this brief and every goal file restate it).
- **At most 4 agents at once** (T40, raised 2026-09-30). The maker container has 28 CPUs, shared by its
  9 Claude sessions, and the host is shared.
- **One heavy job at a time** under `flock -o /cache/locks/holdfast-heavy.lock` (`make check`,
  `make mutation-diff`, any real encode loop, `pnpm build`). **Always `-o`** (I10): plain
  `flock <lock> <cmd>` hands the lock file to the child as a read-only fd 3.
- Agents that write files work in worktrees under `/cache/wt/holdfast/<branch-name>`, removed
  when their PR merges or closes.
- Research claims a decision rests on get an adversarial verifier prompted to refute them; a
  claim most verifiers refute is dropped.
- Every GOAL REPORT is checked by a fresh adversarial subagent against the repos before it is
  printed (§21).
- Every agent prompt states a time budget (research 30 min, review 40 min, a build track at most
  4 hours); a hung agent is stopped and its track re-run once, then recorded `DROPPED (why)`.
  Every long shell command runs under `timeout`.

### 0.5 The ledger

`.claude/goals/2026-09-holdfast-g<n>.status.md`, created by the goal's first commit.

- A header: the brief sections it runs, the start date, the goal-start SHA, the precondition as
  checked (command and output).
- A **Baselines** table: each gate the goal touches, its value and its wall-clock, including the
  `internal/engine` seconds.
- **One table per phase:** `| # | Item | State |`. States:
  - `TODO`, `DOING`
  - `DONE (repo@sha or PR#): evidence`
  - `NEEDS-OWNER (why)`
  - `PROPOSED (where)`
  - `DROPPED (why)`
- **Decisions taken:** dated entries, each saying where its reasoning lives.
- **Resume here:** rewritten at every phase boundary and before any long wait.
- Committed at every phase boundary. At the end no row is `TODO` or `DOING`, and the last commit
  adds `COMPLETE (goal <n>): <date>`. An INCOMPLETE goal writes no such line.
- Ledgers are docscheck input (§4): write identifiers in code spans.

### 0.6 Human-only steps (NEEDS-OWNER)

- **What counts:** only a step physically impossible for an agent here: a run on real GPU
  hardware (T9), a live check against the owner's Plex, Sonarr or Radarr (T49), merging the
  homelab PR (T32), committing a hardware report (T43). Nothing else.
- **Where:** `.claude/goals/NEEDS-OWNER.md` in this repo (T42, I1). Search it before adding; ask
  once.
- **Each item says:** what to do, with what tool and the exact command, the expected result, and
  which file or parameter changes with the answer.
- **Speed:** the owner acts within days (T42), so no goal waits. Hardware and live-service work
  proceeds on fakes; the goal that consumes a result re-checks for it and re-runs when it is
  there.
- **Results reach the repo** through `scripts/hw-report.sh` (goal 6): the owner runs it on the
  host, it writes a redacted `testdata/hw-reports/<encoder>-<date>.json`, and the owner commits it
  to `main` (T43). Live-service checks use the same pattern (goal 10).
- **The owner's hardware (T5):** NVIDIA (NVENC), Intel iGPU (QSV/VAAPI), AMD (AMF/VAAPI). What
  else they have was not asked; a step needing anything beyond a shell on those hosts is written
  as a question in the item, not assumed.

### 0.7 Hardware, outward-facing limits and identity

- **GPUs:** no goal runs a real GPU (T9). Hardware paths are proven on fakes and golden argv
  tests. Guard in code: the hardware integration tests are behind a build tag no agent sets
  (`hwlive`), and `rg -n hwlive .github Makefile` must print nothing (goal 5 shows it).
- **Live services:** no goal calls a real Plex, Sonarr, Radarr or worker node (T49). Clients are
  tested against `httptest` fakes.
- **GitHub:** allowed are branches and PRs in `NSchatz/holdfast` (including closing PR #94 per
  I12); PRs (never merged) in `NSchatz/homelab`; deleting exactly the 6 branches and 1 tag of
  T35, by full refname; minor `v*` tags, the release dry-run dispatch, and the GitHub releases and
  ghcr images `release.yml` publishes (T37). Goals never open issues; an issue a workflow opens is
  fixed, then commented and closed with a link (I20). Nothing else is created, deleted, renamed or
  made public.
- **Never:** flash, order, spend, or create accounts, tokens or API keys.
- **Identity and private data (T41, I1):** the repo is public (AGPL-3.0). Never in any tracked
  file: LAN addresses, hostnames, media paths or service URLs of the owner's hosts; real library
  titles or filenames; account IDs, tokens or API keys; the owner's name or email beyond git
  author fields and the existing LICENSE/NOTICE lines. Program prose says "the owner". Fixtures
  are synthetic (lavfi-generated). From goal 1 on, an identity scan in `make check` enforces the
  name and email part mechanically.
- **Secrets:** every new credential is reached by reference and joins
  `config.SecretBearingKeys` (§4). `make secret-scan` runs in the gate; the container's global
  `core.hooksPath` means the repo's pre-commit hook is not active here, so the gate is the check.

### 0.8 Scope guards

- **In:** the areas of T12-T15 and T19-T31 as refined by T16-T29, the cleanup of T34-T35, the
  triage of T30/T33, and the release of T37/T47.
- **Out (declined; never re-opened):** plugin/flow system and the Tdarr migration importer
  (T14, T15); Jellyfin/Emby clients (T28); VP9 and VideoToolbox (T27); OCR of image subtitles,
  stripping subs, and converting mov_text (T24, I17); quarantine or repair in the health sweep
  (T29); a UI/API priority bump (T31); Playwright or a separate UI repo (T22); user login or mTLS
  (T23); repo-wide dash replacement and mutation-domain widening (T34); a Claude Code skill
  (T53).
- Nothing measured or unknown is invented: a missing input (a hardware result, a calibration
  value, a live API shape) is refused by name, never replaced with a typical value. A default
  value not measured here is marked `ASSUMED` in code comments and docs until a report replaces
  it.

### 0.9 Research, citations and pins

- Primary sources first (T54): ffmpeg documentation and source, Intel/AMD/NVIDIA/Mesa docs, the
  dovi_tool and hdr10plus_tool repos, the Plex and Sonarr/Radarr API docs and source. Forums and
  secondary guides only as leads, marked `LEAD`.
- Cite the URL and the date read for anything a version, number, price, code section, licence
  or API rests on, in the doc or code comment that relies on it. A claim from memory is marked
  `ASSUMED`.
- Pins: every new tool, image, binary or package is pinned (version and digest or sha256) and
  covered by `scripts/check-pins.sh`. Pinned versions are never upgraded silently: an upgrade is
  its own commit saying why.
- The planning research lives in `.claude/goals/2026-09-holdfast-research/` (§22); a goal
  re-verifies any claim it builds on before building on it.

### 0.10 Preconditions, BLOCKED, INCOMPLETE, and evidence for the evaluator

- **Preconditions:** goal 1 needs this brief and its goal files on up-to-date `origin/main`,
  plus the environment check that `echo "$GOFLAGS $GOMAXPROCS"` prints `-p=4 12` in the session.
  Goal 1 never commits the brief or the settings itself. Goal 2 needs the goal-1 ledger's
  `COMPLETE (goal 1)` line and `.claude/goals/CHECKPOINT-T.approved` on `origin/main`, and prints
  the approval commit's verification (I19). Goals 3-15 need the previous ledger's `COMPLETE`
  line. No goal ever writes a checkpoint file.
- **BLOCKED:** a failed precondition means no other work; the final turn prints only the BLOCKED
  report of §21.
- **INCOMPLETE (I14):** a line marked (foundation) that cannot be met within the bound of §0.3
  ends the goal: the final turn prints the INCOMPLETE report of §21, the ledger gets no COMPLETE
  line, and the next goal is BLOCKED until the owner acts.
- **Evidence:** the `/goal` evaluator sees only the transcript; it runs nothing and reads no
  files. Every done-when line is shown by output printed in the final turn: a command and its
  5-20 line tail, a path, a PR URL, a SHA, or a count with the command that counted it. Counts are
  computed when the report is written. A line that names a proposal or a section restates, in the
  report, the one-line content it relies on (for example the P1 rows for the goal). "Works" or "is
  complete" is never written without the check that shows it.
- **Done-when states:** DONE; NEEDS-OWNER only when the step is physically impossible for an
  agent (why, and where the entry is); DROPPED only after the 3 fix rounds of §0.3 (PR URL and
  the red tails), never for a (foundation) line; PROPOSED only where this brief marks the
  decision research-and-propose.

### 0.11 Environment facts (verified 2026-09-29; container rows updated 2026-09-30 for the move into maker)

| Fact | Consequence |
|---|---|
| CPU quota 28 in the maker container, shared by its 9 Claude sessions (2 in the standalone container); `nproc` says 56 | `.claude/settings.json` commits `GOFLAGS=-p=4` and `GOMAXPROCS=12` (T40); at most 4 agents; one heavy job under the lock |
| Memory limit 48 GiB in the maker container, shared by its 9 Claude sessions (16 GiB standalone) | the encode memory watchdog (85% of the limit) applies to test encodes; the limit is shared, so keep test encodes far below it |
| The maker container has the host's GPU (a Quadro P2200 shared with Plex, through `claude-gpu`); no `/dev/dri` | hardware only on fakes and golden argv (T9), unchanged: every real hardware run stays a NEEDS-OWNER item |
| `/cache` and `/workspace` on one filesystem, about 640G free; in the maker container `/scratch` (`TMPDIR`) is an 8 GiB RAM tmpfs counted against memory | fixtures and worktrees fit; keep large temporary files out of `TMPDIR` |
| No global Go; mise has go 1.23.12, 1.25.12 and 1.25.14 | run Go as `mise exec go@1.25.14 -- ...` |
| `ffmpeg` on PATH is the pinned BtbN `N-125875-g5d4d3bdc61` (autobuild-2026-07-31) with libx264, libx265, libsvtav1, libvpl, vaapi, amf, nvenc; hwaccels cuda/vaapi/qsv/drm/opencl/vulkan/amf; filters loudnorm, cropdetect | every encoder name T27 needs exists in the pinned build; only vendor runtimes are missing from the image |
| A `docker` CLI is on PATH but there is no daemon or socket; no `sudo`; `apt` needs root | image builds and smokes happen only in CI (T36); rootless installs only |
| `make check` on `main` (3c229da) under `flock -o`: exit 0 in 25m35.7s; `internal/engine` 1339.8 s, `cmd/holdfast` 398.9 s | `timeout 10800` on the gate; batch PRs (T52); the engine is at 74% of `TEST_TIMEOUT` (30m, `Makefile:82`), see §4 |
| `flock <lock> <cmd>` leaks the lock as a read-only fd 3; `TestEncodeWithProgress_FailurePathIsByteIdentical` then fails 6/6 | always `flock -o` (I10); goal 1 hardens the fixture |
| `gh` is authenticated with admin on holdfast, super and homelab; `main` has no branch protection; `delete_branch_on_merge` is on | ledger commits to `main` work; super's clone gets its push URL disabled (§0.1) |
| GitHub Actions: the last 25 runs green apart from 2 superseded cancels; PR CI 10-13 min | wait for CI before merging (T36) |
| `git config --global core.hooksPath` = `/home/claude/.claude-hooks` | the repo's pre-commit secret scan is inactive here; the gate's `secret-scan` is the check |
| `/cache/locks/` exists and holds other programs' locks (shopkit, 3d, home, dev, inventory) | this program's lock is `/cache/locks/holdfast-heavy.lock`; never touch the others |
| pnpm 11 and later ignore `ignore-scripts` in `.npmrc`, including the container's `~/.npmrc` (verified with pnpm 11.27.1, 12.6.0, 12.8.1) | the UI goals set `ignoreScripts: true` in `pnpm-workspace.yaml` (§17) |
| The Claude shell may alias `grep` through the CLI binary, which breaks while the CLI updates | use `command grep`, `rg` or `git grep` in goal shells |
| `claude --version` = 2.1.284 | - |

### 0.12 Long-run hygiene

- The worker variables live in the committed `.claude/settings.json` `env` (`GOFLAGS=-p=4`,
  `GOMAXPROCS=12`) with `autoMemoryEnabled: false` and `attribution.commit` "" (I18). They stay
  after the finale (I4); the finale report lists them for the owner to remove.
- `timeout` on everything that can hang (`make check` 10800 s, `gh pr checks --watch` 3600 s,
  a single encode test 1800 s).
- Big output goes to files under `/cache/tmp/holdfast-g<n>/`, never into the context; paste
  tails.
- Keep the context small: agents return conclusions, not file dumps.

### 0.13 Running beside other programs

This program runs alone in `NSchatz/holdfast` and shares nothing with the shopkit, 3d, devices
and home programs except host CPU (T6). There is no requests file and no shared list; the
`NEEDS-OWNER` state here means only a step for the owner, never a request to another program. If
another program starts working in this repo, the goal records it under "Decisions taken", touches
none of that program's branches or files, and lists it in its GOAL REPORT.

---

## §1 The owner's decisions, 2026-09-29

Source: the planning interview of 2026-09-29 (14 rounds, 55 questions; in
`.claude/goals/2026-09-holdfast-research/interview-2026-09-29.md`, verbatim except identity
scrubbed per I1). These rows override anything older in this repo or in `NSchatz/super`. "Not
chosen" options are never re-opened. Research-and-propose rows: T10, T30, T33, T45, T46 (and the
questions of I7 and I16).

**Decisions** (letter T; letters in use by other programs: D F H P):
| # | Topic | Decision |
|---|---|---|
| T1 | Direction | **Tdarr feature parity**: close capability gaps vs Tdarr. Not chosen: public v1.0 release focus, running on the owner's library, research-and-propose direction. |
| T2 | SDD workflow | **The program replaces it**: the /goal program is the plan of record; umbrella roadmap superseded for holdfast. Not chosen: program feeds SDD, both side by side. |
| T3 | Autonomy | **One checkpoint after goal 1**, then goals chain. Not chosen: review after every goal, checkpoints at engine changes. |
| T4 | Size | **As big as needed**. Not chosen: ~4 goals, ~8 goals. |
| T5 | Accelerators the owner has | **NVIDIA (NVENC), Intel iGPU (QSV/VAAPI), AMD (AMF/VAAPI)**: all three. Not chosen: none/CPU only. |
| T6 | Other programs | **Independent, runs beside** shopkit/3d/devices/home; owns only NSchatz/holdfast; shares host CPU only; no requests file, own NEEDS-OWNER list. Not chosen: shared NEEDS-OWNER list, run after them. |
| T7 | Contract | **Standing contract inherited unchanged** as §0. Not chosen: inherit with changes, walk through. |
| T8 | Old roadmap and naming | **Mark superseded, keep S-names in history**: program commits use Conventional Commits; CLAUDE.md's umbrella-roadmap pointer is rewritten to point at the brief; the umbrella repo is never touched (the owner annotates it). Not chosen: program edits umbrella, continue S-numbers. |
| T9 | Hardware testing | **No live GPU in goals**: every hardware path is proven on fakes and argv golden tests; every real hardware run (NVENC, QSV, VAAPI, AMF) is a NEEDS-OWNER item with an exact command. Not chosen: NVENC live via --gpu, all three reachable. |
| T10 | HW encodes vs gates | **Research and propose**: a goal measures/researches HW vs SW against the gates and proposes; the owner decides at Checkpoint T. Until decided, the existing gates apply unchanged to HW output (the invariant is never weakened by default). Not chosen: same gates no exceptions (as a final decision), per-encoder VMAF targets (as a final decision). |
| T11 | HW unavailable/fails | **Configurable per library**: a YAML key chooses software fallback or skip; the program picks and documents the default. Not chosen: always fall back, always skip. |
| T12 | Encode scope | **All four**: QSV/VAAPI/AMF working in the shipped image; hardware decode (-hwaccel); automatic HW detection with per-library fallback (T11); H.264 and more output codecs. |
| T13 | Stream scope | **All four**: audio transcode/downmix (REVERSES the audio re-encode non-goal); subtitle extraction to sidecars; crop/black-bar detection; Dolby Vision/HDR10+ transcode (reverses the DV/HDR10+ skip). |
| T14 | Platform scope | **Web UI (REVERSES the no-frontend decision, 4ad6f03), distributed worker nodes (REVERSES the multi-node non-goal, docs/migration.md:120-126), native Plex/Jellyfin/*arr clients (covers S0179)**. Not chosen: plugin/flow system. |
| T15 | Ops scope | **Priority/savings-rate queue (S0164) and library health-check mode**. Not chosen: Tdarr migration importer, none. |
| T16 | Node swap | **Server gates and swaps**: workers only encode; the server owning the library re-runs every gate and does the same-filesystem rename itself. The invariant text is unchanged. Not chosen: workers gate/server swaps, research and propose. |
| T17 | Node media access | **Both, configurable per node**: shared mount (path mapping) or the server streams source and output over HTTP. Not chosen: shared mount only, streaming only. |
| T18 | Web UI form | **Full SPA** with a Node build (npm/pnpm enters the gate). Not chosen: server-rendered no-build UI, research and propose. |
| T19 | Audio | **All four**: re-encode lossless/bulky tracks to a configured codec; add a stereo downmix track; keep the original track alongside unless config says otherwise; EBU R128 loudness normalisation on added/re-encoded tracks. |
| T20 | Audio default | **Replace by default, keep opt-in**: a re-encoded track replaces its source track; `keep_original_audio: true` keeps both; the downmix is always an added track; the whole file must still pass strictly-smaller. Refines T19 "keep original alongside" to "available by opt-in". Not chosen: keep by default, per-codec rule. |
| T21 | SPA stack | **Svelte + Vite, pnpm, go:embed into the binary** (one static distroless image; built output not committed; `make check` gains UI lint, typecheck, unit tests and build). Not chosen: React, research and propose. |
| T22 | SPA home/tests | **`web/` in this repo, unit tests only** (no browser in the gate). Not chosen: Playwright e2e, separate repo. |
| T23 | Auth | **Existing read/control tokens for the UI + a new `node_token`** joining `config.SecretBearingKeys`, by reference; no user login, no mTLS; 127.0.0.1 default bind kept. Not chosen: user login, research and propose. |
| T24 | Subtitles | **Text subs (SRT/ASS/WebVTT) extracted to sidecars `<name>.<lang>[.forced].<ext>` beside the file, embedded subs kept**; never overwrite an existing sidecar; image subs (PGS/VobSub) untouched. Not chosen: extract and strip, OCR of image subs. |
| T25 | DV/HDR10+ | **HDR10+ and DV profile 8 carried through libx265, plus DV P7 to P8.1 conversion (opt-in only, drops the enhancement layer)**; P5 stays skipped (ASSUMED: P5 not named; the fail-safe rule keeps it skipped). dovi_tool/hdr10plus_tool pinned into the image. Not chosen: P8/HDR10+ only, research and propose. |
| T26 | Crop | **Opt-in, consensus-sampled**: off by default; cropdetect over spread samples must agree, else no crop; VMAF against the identically cropped source plus a gate that the removed area was black. Not chosen: on by default, research and propose. |
| T27 | New codecs | **H.264 (libx264 + h264_nvenc/qsv/vaapi/amf) and AV1 hardware (av1_qsv/av1_vaapi/av1_amf)**. Not chosen: VP9, VideoToolbox. |
| T28 | Media clients | **Plex (rescan after swap + hold a file being played) and Sonarr/Radarr (rescan after swap + native webhook intake replacing the custom script in docs/docker.md)**; new API keys join `config.SecretBearingKeys` by reference. Not chosen: Jellyfin/Emby. |
| T29 | Health check | **Scheduled, run-window-respecting, resumable full-decode sweep; report only** (ledger, API, UI, notification); never moves or deletes a file. Not chosen: quarantine, repair re-encode. |
| T30 | Active SDD specs | **Goal 1 triages** each of the 20 approved specs in super/pipeline/active (S0151, S0162-S0180) and proposes keep/drop/merge-into-goal per spec; the owner decides at Checkpoint T. Not chosen: fold all, fold p2 only, ignore. |
| T31 | Priority | **Per-path/rule `priority` in YAML and a savings-per-encode-hour `queue_order` (S0164)**. Not chosen: bump from UI/API. |
| T32 | Other repos | **NSchatz/super read-only** (clone under /cache, never branch/PR/push); **NSchatz/homelab PR-never-merge** (may open a PR updating the holdfast deployment/env example, e.g. S0181; the owner merges). Not chosen: both read-only, both PR-never-merge, never touch. |
| T33 | PR #94 (S0159) | **Part of the goal-1 triage** (proposed at Checkpoint T with the specs); not merged before the owner decides. Not chosen: goal 1 merges it first, the owner merges before goal 1. |
| T34 | Cleanup | **Docs/code drift** (roadmap pointer, README status, CLAUDE.md layout and 'API and UI', TRANSCODE-16/17 vs 1..15, stale comparison/migration/api-reference claims, test-mass -check) **and dead code** (4 regress_0057_*.js, web-UI leftover comments). Not chosen: repo-wide dash replacement + check (existing dashes stay; new text still uses plain hyphens per CLAUDE.md), mutation-domain widening. |
| T35 | GitHub deletions | **Delete exactly** remote branches remove-comment-density, remove-dashboard, sdd/S0022-holdfast-ffmpeg-rot, sdd/S0035-holdfast-dashboard-ui, sdd/S0126-holdfast-depth-state-matrix-graders, webui-e2e-playwright-graders and tag abandoned/S0098-pre-d911314; nothing else is deleted on GitHub. Not chosen: only the 2 merged, NEEDS-OWNER. |
| T36 | Merge gate | **Local `make check` on the rebased branch AND green PR CI (build, package, mutation) before `gh pr merge --squash --delete-branch`**. Amends the standing contract's "local gate only". Not chosen: local gate only, CI only on engine changes. |
| T37 | Releases | **The program may cut minor releases** (v0.4.0, v0.5.0, ...) per docs/release.md after a goal's work is merged and CI is green; tags + GitHub releases + the ghcr images release.yml publishes are allowed. Amends the contract's "nothing created on GitHub beyond branches and PRs" for `v*` minor tags only. Not chosen: never (NEEDS-OWNER), pre-releases only. |
| T38 | Attribution | **CLAUDE.md wins**: Conventional Commits authored as the owner, no Co-Authored-By or AI trailer in commits; PR bodies may carry the session link. Not chosen: harness trailer. |
| T39 | Gate budget | **No ceiling; timings recorded** in every ledger baseline and PR. Not chosen: <=20 min, <=35 min. |
| T40 | Machine | **At most 4 agents at once; `.claude/settings.json` env commits `GOFLAGS=-p=4` and `GOMAXPROCS=12` (raised 2026-09-30 from 2 agents, `-p=2` and 2, when holdfast moved into the 28-CPU maker container; `-p` stays low because one package dominates `make check` and encode tests are memory-heavy); one heavy job (`make check`, real encodes, SPA build) at a time under `flock /cache/locks/holdfast-heavy.lock`**. (Inferred: the option named HOLDFAST_GATE_JOBS, but HOLDFAST_* is the config env prefix, internal/config/config.go:36, so the variable was renamed; shown at Checkpoint T.) Not chosen: 3 agents, 1 agent. |
| T41 | Private data (public AGPL repo) | **Never in the repo: LAN addresses/hostnames/media paths/service URLs; real library titles or filenames; account IDs/tokens/API keys; the owner's name/email beyond git author fields and existing LICENSE/NOTICE lines.** Fixtures are synthetic. |
| T42 | NEEDS-OWNER | **`.claude/goals/NEEDS-OWNER.md` in holdfast**; the owner acts within days, so the chain never waits: hardware/live-service work proceeds on fakes and a later goal re-checks for results. Not chosen: GitHub issues, same-day. |
| T43 | Real-hardware results | **The program ships a script (e.g. `scripts/hw-report.sh`) that runs the check and writes a redacted `testdata/hw-reports/<encoder>-<date>.json`; the owner commits it to main.** Not chosen: paste into a session, research and propose. |
| T44 | Ordering | **Foundations first**: the encode-pipeline refactor that hardware, audio, subtitles, crop and DV hang off comes first, then encode parity, streams, queue/health, clients, nodes, and the UI last. Not chosen: value first, risk first. |
| T45 | AMD/AMF | **Research and propose**: licence and packaging of the AMF runtime vs AMD via Mesa VAAPI; decided at Checkpoint T. Not chosen: VAAPI-only (as a final decision), separate AMF image variant (as a final decision). |
| T46 | Node protocol | **Research and propose**: transport (HTTP+JSON on the existing server vs gRPC vs other), leasing, streaming; decided at Checkpoint T. |
| T47 | Success | **A tagged minor release whose image carries every chosen area, with docs; and committed real-hardware reports (NEEDS-OWNER) for the hardware encoders.** Not chosen as success measures: an updated comparison.md, a diff check that the invariant text is untouched (the invariant still governs per CLAUDE.md). |
| T48 | Where proposals go | **All research-and-propose items (T10 HW gates, T45 AMF, T46 node protocol, T30/T33 triage) are researched and written in goal 1**, decided at Checkpoint T before anything is built on them. Not chosen: just-in-time + finale list, a second checkpoint. |
| T49 | Live services | **Never live**: Plex, Sonarr/Radarr and node tests use fakes/recorded fixtures; a live check is a NEEDS-OWNER command producing a redacted report. Not chosen: local containers. |
| T50 | Design records | **`docs/design/<topic>.md` with anchors**, CLAUDE.md links each by its rule only ("one argument, one home"). Not chosen: ADRs, brief/ledgers only. |
| T51 | CLAUDE.md length | **Under 200 lines**; no dates or history in it. Not chosen: ~150, no limit. |
| T52 | Slow local gate | **Keep full local `make check` (under `timeout 10800` and the heavy lock) + green CI as the merge rule; batch work into fewer, larger PRs per track** so the gate runs a few times per goal. Not chosen: local fast subset with CI authoritative, full local only on engine PRs. |
| T53 | Daily interfaces | **The web UI and the JSON API/Prometheus**; CLI and YAML remain but are not the daily surface. Not chosen: a Claude Code skill. |
| T54 | Research sources | **Primary sources first** (ffmpeg docs/source, Intel/AMD/NVIDIA docs, dovi_tool/hdr10plus_tool repos, Plex/Sonarr/Radarr API docs); forums only as leads, marked as such. Not chosen: anything cited. |
| T55 | Anything else | **Nothing else.** |

**Inferred after the interview** (the planner's; shown at Checkpoint T, open to amendment):
| # | Topic | Resolution |
|---|---|---|
| I1 | T41 vs T42 (name in a public repo) | Revised in v2 (review H1): the human-step list is published as `.claude/goals/NEEDS-OWNER.md` with state `NEEDS-OWNER` (T42's substance - a file in the repo, the owner acts in days - kept; its label carried the owner's name, which T41 keeps out). Prose says "the owner". Goal 1 scrubs the two older leaks (`CLAUDE.md` commit-identity line, a server test fixture) and adds a mechanical identity scan to the gate. Survey and interview copies are scrubbed. The owner may rename at Checkpoint T. |
| I2 | T37 vs docs/release.md:12-13 ("pushing a tag was never a machine act ... Those four are yours") | T37 reverses it for minor `v*` tags and the dry-run dispatch only; renames and visibility flips stay the owner's. Recorded in docs/release.md. |
| I3 | T24 vs README library-manager non-goal | Sidecar creation is not renaming/moving/deleting, so no reversal; but README says no un-gated mutation, so sidecars get their own gate (extracted text parses and its cue count equals the source stream's packet count) and are never overwritten. |
| I4 | Worker variable end-of-program | Decisions are silent; `.claude/settings.json` env stays after the finale (harmless); the finale report lists it for the owner to remove. |
| I5 | Defaults of new transformations | Every new transformation (audio re-encode/downmix/loudnorm, subtitle sidecars, crop, HW decode, `encoder: auto`, P7 conversion, priority ordering, health sweep, clients, nodes) is OFF until configured, so an existing config yields the same decisions and argv as before (the contract's "keep what exists valid" rule). |
| I6 | DV P8 / HDR10+ carry default | Read from T25 ("P7 opt-in only" contrasted with P8/HDR10+): P8 and HDR10+ sources are carried (no longer skipped) by default on the `cpu` encoder once the metadata verification gates pass; any other encoder keeps skipping them. |
| I7 | Crop on DV sources | Research found crop leaves stale RPU L5 offsets; until Checkpoint T decides proposal P5, crop is refused on DV sources (the file still encodes uncropped). |
| I8 | Image variant | Whether the VAAPI/QSV runtime goes in the default image or a `-hw` tag is part of proposal P3 (T45); goal 3 builds on the recommendation. |
| I9 | AMF in the image (T12) | Blocked by AMD's EULA (no redistribution); proposal P3 recommends AMD via Mesa VAAPI with `amf` kept for host installs and refused in the image with a named reason. |
| I10 | Heavy lock form | `flock -o` is mandatory: `flock <lock> <cmd>` leaks the lock as a read-only fd 3 into children, which turns TestEncodeWithProgress_FailurePathIsByteIdentical red (verified 2026-09-29). |
| I11 | Updating a PR branch (review H2) | The branch is brought up to date by merging `origin/main` into it, never by rebase + force-push (T7 keeps "never force-push"); the squash merge keeps `main` linear. A base that moved only by ledger-only commits needs no re-gate. |
| I12 | PR #94 if P1 keeps it | Cherry-picked onto a new `holdfast-g2/...` branch and PR; #94 closed with a link; its old branch is left (not in T35's list) and listed as a follow-up. |
| I13 | Goal size (review H5) | 15 goals instead of 9 (T4: as big as needed): foundations, hardware, clients, nodes and UI are each split so one goal is a day at most. |
| I14 | Dropped tracks (review H3) | A feature line may read DROPPED after the 3 fix rounds, with the PR URL and red tails; a line marked (foundation) cannot, and its failure ends the goal INCOMPLETE with no COMPLETE line, stopping the chain for the owner. |
| I15 | Engine test clock (review H4) | Record `internal/engine` seconds in every PR; new real-encode fixtures live in the package that owns the code; raise `TEST_TIMEOUT` in the Makefile in its own commit with the measurement once the engine passes 80% of it; never delete a proof to fit the clock. |
| I16 | docscheck corpus (review M4) | The corpus includes `.claude/**`. Until the owner decides proposal P6 (exclude `.claude` or not), program Markdown avoids docscheck vocabulary and every new statement or metric is proven present in README.md or docs/ by `git grep`. |
| I17 | T24 exactness (review L7) | Sidecar names are exactly `<name>.<lang>[.forced].<ext>`; mov_text is skipped with a reason, not converted (T24 named SRT, ASS, WebVTT only). |
| I18 | T38 mechanically (review L10) | `.claude/settings.json` sets `attribution.commit` to "" so the CLI adds no commit trailer; the PR attribution stays default (T38 allows it). |
| I19 | Checkpoint approval form (review M7) | Preferred: the owner adds `CHECKPOINT-T.approved` in the GitHub web UI (a GitHub-verified commit); goal 2 prints the adding commit's verification and committer. A commit from a session the owner told to, in its own chat, is also accepted (the command allows it), so the check is shown, not enforced. |
| I20 | Issues (review L5) | Goals never open issues; an issue a workflow opens (the scheduled mutation run does on failure) is fixed, then commented and closed with a link, and recorded in the ledger. |

### 1.1 Reversals to record

Goal 1 writes each reversal into the file whose rule it changes, marked
`decided 2026-09-29 by the owner (T<ids>)` (in `CLAUDE.md` and in Go comments, which carry no
dates, the marker is `decided by the owner (T<ids>)`). Each is neither broader nor narrower than
its decision; the statement itself is rewritten by the goal that ships the feature.

| # | Rule reversed | Where | Decisions | Rewritten by |
|---|---|---|---|---|
| R1 | "Audio transcoding is a non-goal" | `README.md` (Non-goals), `docs/migration.md` ("no audio/subtitle mangling") | T13, T19, T20 | goal 7 |
| R2 | "Distributed or remote processing is a non-goal by design" | `README.md` (Non-goals), `docs/migration.md` (worker nodes are a non-goal) | T14, T16, T17 | goal 12 |
| R3 | "holdfast ships no frontend: the JSON API is the interface" | `README.md`, `CLAUDE.md` (the `internal/server` line), `docs/api-reference.md`, `docs/docker.md` | T14, T18 | goal 14 |
| R4 | The Dolby Vision / HDR10+ skip is deferred | `README.md` (anchor `dynamic-hdr-deferred`; a note is ADDED under it, the docscheck-enforced clauses stay until goal 8) | T13, T25 | goal 8 |
| R5 | Cutting a tag and dispatching a workflow are the owner's acts | `docs/release.md` (opening section), `README.md` ("Cutting a tag is a deliberate human act") | T37 (inferred scope I2: minor `v*` tags and the dry-run dispatch only; renames and visibility flips stay the owner's) | goal 1 (record) |
| R6 | The umbrella roadmap and the SDD pipeline are the plan of record | `CLAUDE.md` (Conventions, the "phased plan lives in the umbrella" line), `README.md` ("the roadmap names each phase"; "The phased plan and its research live in the umbrella"), `cmd/holdfast/main.go` (package comment pointing at the umbrella's roadmap) | T2, T8 | goal 1 |

R5 is inferred (I2) and is shown at Checkpoint T. T24's sidecars do not reverse the
library-manager non-goal (they rename, move and delete nothing); they get their own gate (I3).

---

## §2 Where things stand (2026-09-29; `review-holdfast.md`)

- `main` = `3c229da` (S0158, PR #93). 99 commits, tags `v0.1.0`, `v0.2.0`, `v0.3.0` (Latest,
  2026-09-19); S0155-S0158, S0160 and S0161 are unreleased. The compose file pins
  `ghcr.io/nschatz/holdfast:v0.3.0` by digest.
- Size (`scripts/test-mass.sh`): 23,474 production and 56,225 test lines, ratio 2.40, 336 `.go`
  files. Largest packages (prod/test): engine 11.4k/30.8k, store 5.1k/8.7k, config 4.8k/4.6k,
  cmd 4.2k/10.9k, server 3.3k/7.1k.
- The gate `make check` (`Makefile:277`) runs check-pins, its selftest, install-ffmpeg-selftest,
  secret-scan and its selftest (33 cases), api-schema-diff, mutation-shape, fmt, vet, build,
  `go test -race` (30-minute per-package timeout), staticcheck, govulncheck and its selftest. It
  passed on `main` in 25m35.7s here (§0.11). The last full mutation run (36230570966) scored 100%
  (1233 killed, 0 lived, 672 not covered); the floor is 70% (`.gremlins.yaml`) over the module
  minus six excluded packages (engine, vmaf, cmd, store, server, metrics), so every NEW package is
  inside the domain.
- Encoders (`internal/encoder/encoder.go`): `cpu` (libx265, default), `svtav1`, `nvenc`,
  `av1_nvenc`, `qsv`, `vaapi`, `amf`. Only NVENC is usable in the image, and on hosts on the
  legacy (non-CDI) NVIDIA runtime path it lacks the `video` driver capability. No `-hwaccel`
  anywhere; a failing encoder stops the run. Every VAAPI job is encoded 8-bit (`nv12`,
  `internal/engine/encode.go:815,927`) and the VAAPI `Available()` probe cannot pass on a working
  host (`internal/encoder/encoder.go:127-131`); the CRF value is reused as `-cq`,
  `-global_quality` and `-qp` (`encode.go:804-822`).
- Absent against Tdarr and its peers: web UI, distributed nodes, audio transcode, subtitle
  extraction, H.264 output, hardware decode, QSV/VAAPI/AMF in the image, automatic hardware
  selection or fallback, DV/HDR10+ transcode, native Plex/*arr clients, priority queue, crop.
- Open work: PR #94 (S0159 bounded-run temp sweep; green, mergeable, head branch pushed by an
  earlier session) and 20 approved specs in `NSchatz/super` `pipeline/active/` (S0151,
  S0162-S0180); no issues; no to-do markers in code.
- Rough edges (T34 fixes these): the roadmap pointers in `CLAUDE.md`, `README.md` and
  `cmd/holdfast/main.go` name a file that does not exist; `README.md:10` says v0.1.0;
  `CLAUDE.md`'s layout omits 9 packages and the `analyze`/`plan` commands and line 51 says "API
  and UI"; TRANSCODE-16/17 exist in code while `CLAUDE.md` and `check-pins.sh` say 1..15; web-UI
  wording in `cmd/holdfast/main.go:59,893,994` and about 8 other files; `scripts/test-mass.sh
  -check` fails and `docs/test-mass.md` calls the live `internal/docscheck` retired; 4 dead
  `scripts/regress_0057_*.js` (479 lines); `docs/comparison.md`, `docs/migration.md` and
  `docs/api-reference.md` contradict the code. Identity (T41, I1): `CLAUDE.md`'s commit-identity
  line and a fixture in `internal/server/server_test.go` carry the owner's name. Not fixed (T34):
  613 en/em dashes in 52 files, and the mutation domain.
- Stale refs (T35): `remove-comment-density`, `remove-dashboard` (both merged),
  `sdd/S0022-holdfast-ffmpeg-rot`, `sdd/S0035-holdfast-dashboard-ui`,
  `sdd/S0126-holdfast-depth-state-matrix-graders`, `webui-e2e-playwright-graders`, and the tag
  `abandoned/S0098-pre-d911314`. None is an open PR's head.

---

## §3 Target

### 3.1 The repo after the program

A tagged minor release (T47) whose image carries every chosen area: hardware encode and decode
for NVIDIA, Intel and AMD with automatic selection and a per-library fallback; H.264 and AV1
hardware output; audio re-encode, downmix and loudness; text-subtitle sidecars; Dolby Vision P8,
HDR10+ and opt-in P7 conversion; opt-in crop; rule priority and a savings-rate queue; a
report-only health sweep; Plex and Sonarr/Radarr clients; distributed worker nodes; and an
embedded Svelte web UI. Every one of them sits behind the same gate set, extended, never relaxed.
Real-hardware reports for the hardware encoders are committed by the owner (T43, T47). The
SDD pipeline is superseded for holdfast by this program (T2, T8); S-numbers stay in history.

### 3.2 New or changed components (names are proposals the goals may refine)

- `internal/engine`: the **encode plan** (goal 3), one declared description of a job's
  transformations that both the argv builder and the gates read; new gates: output fidelity
  (goal 4), audio and sidecar (goal 7), dynamic HDR and crop blackness (goal 8).
- `internal/encoder`: codec families (HEVC, AV1, H.264), per-encoder quality scales (goal 4),
  device discovery and a real-argv `Available()` (goal 5), new encoders (goal 6).
- New packages, as the goals find them needed: `internal/audio`, `internal/subtitle`,
  `internal/crop`, `internal/dynhdr` (dovi_tool and hdr10plus_tool wrappers), `internal/health`,
  `internal/mediaclient` (Plex, Sonarr, Radarr), `internal/node` (the lease protocol),
  `internal/webui` (the embed). Each is inside the mutation domain (§4).
- `cmd/holdfast`: `worker` (goal 11).
- `web/`: the Svelte SPA (goals 13-14).
- `scripts/hw-report.sh` (goal 6); an identity scan in the gate (goal 1).
- `docs/design/`: one file per new rule, each with an anchor that `CLAUDE.md` links by its rule
  only (T50).

### 3.3 Interfaces

- **Daily (T53):** the web UI and the JSON API/Prometheus. The CLI and YAML stay the
  configuration surface.
- **Config keys** (names proposed; each goal fixes the final spelling in `docs/profiles.md` and
  `config.example.yaml`): `encoder: auto`, `hw_fallback: software|skip`, `hw_decode`, per-encoder
  quality keys, audio keys including `keep_original_audio`, `subtitle_sidecars`, `crop`, a DV P7
  opt-in, `priority`, `queue_order: savings_rate`, a health schedule, Plex/Sonarr/Radarr URLs and
  credentials by reference, `node_token` and worker settings, optional TLS files. Every new key
  is OFF or inert by default (I5), except DV P8/HDR10+ carry (I6).
- **HTTP:** additions only; `api-schema-diff` stays green with `.api-schema-breaks.yaml` `[]`.
  New groups: node lease endpoints (goals 11-12), webhook intake (goal 10), health results
  (goal 9), the UI at `/` (goal 13, keeping the AGPL section 13 source offer served).
- `restore` and `requeue` stay CLI-only and never appear in the UI or the API.

---

## §4 Engineering rules

- **Tests.** Fixtures are synthetic and generated (lavfi) or tiny committed files; never real
  library content (T41). Hardware and services are faked (`httptest`, fake ffmpeg scripts,
  golden argv). Every engine change extends the fixture suite so it reds on that specific
  regression (`CLAUDE.md`). Every calculator (loudness targets, bitrate tables, crop geometry,
  savings rate) is checked against a published worked example or a cited primary figure.
- **The gate** is `make check` plus green PR CI (T36), no wall-clock ceiling, timings recorded in
  every ledger and PR (T39). A new gate target (UI lint, typecheck, tests, build; new pin
  sections; the identity scan) is added to the `check:` target itself, so CI runs the same thing.
- **The engine clock (I15).** `internal/engine` takes 1339.8 s of its 30-minute package timeout
  here. Every PR and ledger records its seconds; new real-encode fixtures live in the package that
  owns the code (audio, crop, dynhdr, node), so `-p=2` spreads the load; once the engine passes
  80% of `TEST_TIMEOUT`, raise `TEST_TIMEOUT` in the Makefile in its own commit with the
  measurement (never by environment override, so CI runs the same thing). A proof is never
  deleted or shortened to fit the clock (`Makefile:66-81`).
- **The mutation floor.** Every new package, and every change in a package outside the six
  excluded ones, is inside the mutation domain at the 70% floor; the PR's `mutation` job must be
  green. Run `make mutation-diff REF=origin/main` under the heavy lock before pushing such a PR.
  Never add an exclusion or lower the floor.
- **The invariant at every merge.** No PR relaxes or removes an existing gate, moves the swap off
  the same-directory rename, or removes a line of `docs/design/swap.md` or
  `docs/design/quality-gate.md` except as an approved proposal at Checkpoint T licenses (only
  P2/T10 can). New gates are additive. Fail-safe: ambiguous, malformed or unsupported input SKIPS
  with a logged reason or returns a typed error. Every GOAL REPORT shows gate integrity counted
  from the goal-start SHA: each deleted line in `*_test.go` files with its reason, `func Test`
  counts per package before and after (none falls), and the count of removed lines in those two
  design documents.
- **docscheck statements.** When a feature changes a statement `internal/docscheck` enforces
  (the `dynamichdr`, `interlacing`, `downscale`, `quickstart` and `enumeration` checks), the
  statement and its check change in the same PR and the check's bite test still proves it fails
  on the old wording. A new statement or metric is proven present in `README.md` or `docs/` by
  `git grep -- README.md docs/` (a ledger mention does not count, I16).
- **Program Markdown is scanned too.** `internal/corpus` walks every `.md` in the repo, so the
  brief, ledgers, proposals and `NEEDS-OWNER.md` are docscheck input until P6 decides otherwise:
  never restate a statement docscheck owns, and write the vocabulary its absence checks count
  (the word stem for scaling a picture down, and the phrase for a maximum output height) only
  inside code spans. Before any ledger-only commit, `go test ./internal/docscheck/`,
  `scripts/check-pins.sh` and `make secret-scan` (and, from goal 1 on, the identity scan) pass.
- **The rename guard** (`scripts/check-pins.sh` section 4) scans every tracked file: never write
  the pre-rename underscore identifiers, including in program files.
- **Conventions.** `CLAUDE.md` rules apply, including plain hyphens in all new text (existing
  dashes stay, T34), Conventional Commits, no AI trailer (T38). `CLAUDE.md` stays under 200 lines
  (T51) with no dates; each new package gets one Layout line; rationale goes to
  `docs/design/<topic>.md` with an anchor (T50).
- **Secrets.** Every new credential key (Plex token, Sonarr and Radarr API keys, `node_token`,
  TLS key file) is reached by reference (`file:` or `cmd:`), joins `config.SecretBearingKeys`,
  refuses a literal at start, and resolves to a `secret.Value`; a test proves each.
- **Security.** The default bind stays `127.0.0.1:8080`; control endpoints stay 403 without a
  control token; `node_token` can lease and upload, never control; webhook intake is
  authenticated; `restore` and `requeue` never become HTTP.
- **Provenance.** Citations (URL, date read) live in the doc or comment that relies on them.
- **Claude Code integration.** `.claude/settings.json` holds only the `env` worker variables,
  `autoMemoryEnabled: false` and `attribution.commit` "" (I18); no hooks or skills are added (T53
  declined a skill).

---

## §5 Goal 1 - Start-up, triage and proposals (ends at Checkpoint T)

**Precondition:** this brief and `2026-09-holdfast-g1.goal.txt` through `-g15.goal.txt` are on
up-to-date `origin/main` (`git ls-tree --name-only origin/main .claude/goals/`), and
`echo "$GOFLAGS $GOMAXPROCS"` prints `-p=4 12`. Research: all of §22.

1. **Start-up.** The first actions of §0.1; the ledger; the Baselines table.
2. **Identity (T41, I1), first.** Replace `CLAUDE.md`'s commit-identity line with "commit as the
   repository's configured git identity" (keeping the no-trailer rule) and the owner's name in
   `internal/server/server_test.go` (around lines 1541-1546) with a synthetic one. Add an
   identity scan to `make check`: it derives the owner's name and email tokens at run time (never
   from a literal in the repo; for example from the configured git identity or the commit
   history), fails on any whole-word match in a tracked file other than `LICENSE` and `NOTICE`,
   has a selftest that proves it bites with a synthetic identity, and in CI either derives the
   identity or fails loudly - it never passes vacuously. The GitHub account name in URLs and the
   module path is not a name match (whole words only).
3. **Harden the fd-3 fixture.** `progressFake` (`internal/engine/encode_test.go:551-569`)
   decides "is fd 3 open for writing?" with `( : >&3 )`, which passes on an inherited read-only
   fd (the plain-`flock` case, §0.11). Make the probe true only when fd 3 is actually writable.
   Production code is unchanged unless the investigation shows it is affected too (then fix it
   and add a fixture).
4. **One PR for the reversals R1-R6 (§1.1) and the T34 cleanup:** the roadmap pointers (R6);
   `README.md:10` status (no stale date; point at the releases page); `CLAUDE.md` Layout lists
   every `internal/` package and the `analyze` and `plan` commands, drops "API and UI", and
   states the TRANSCODE label range the code uses (and `check-pins.sh:245`'s comment with it);
   web-UI and dashboard wording in `cmd/` and `internal/` that describes something that does not
   exist; `docs/test-mass.md` and `scripts/test-mass.sh -check` agree again; delete
   `scripts/regress_0057_*.js`; `docs/comparison.md`, `docs/migration.md` and
   `docs/api-reference.md` match the code.
5. **Deletions (T35):** record `git ls-remote --heads --tags origin | awk '{print $2}' | sort`
   before; delete by full refname (`git push origin --delete refs/heads/<b>` for the 6 branches,
   `refs/tags/abandoned/S0098-pre-d911314`); record after, back to back.
6. **Proposals (T48)**, each at `.claude/goals/2026-09-holdfast-research/proposal-<topic>.md`
   with options, costs, a `## Recommendation` heading and citations, built from §22 and
   re-verified:
   - P1 `proposal-triage.md` (T30, T33): one row per spec S0151 and S0162-S0180 and PR #94: keep
     (and which of goals 2-14 carries it, by topic), drop, or merged into a decided area, with the
     reason. PR #94, if kept, is carried by cherry-pick (I12). Read the specs in the read-only
     super clone (§0.1).
   - P2 `proposal-hw-gates.md` (T10): recommendation from research: the same gates for every
     encoder, plus per-encoder quality scales and an output fidelity gate. It flags that "same
     gates, no exceptions" was not chosen as a final answer in T10, so approving it is a
     deliberate choice.
   - P3 `proposal-amd-image.md` (T45, I8, I9): AMF redistribution, AMD via Mesa VAAPI, and
     whether the VAAPI/QSV runtime goes in the default image or a `-hw` tag. It flags that
     VAAPI-only was not chosen as a final answer in T45.
   - P4 `proposal-node-protocol.md` (T46): HTTP+JSON leases on the existing server with a
     `holdfast worker` subcommand, fencing, digests, TLS stance, backpressure.
   - P5 `proposal-crop-dv.md` (I7): crop on Dolby Vision sources (refuse, or rewrite the RPU's
     active area with dovi_tool), with a test plan.
   - P6 `proposal-docs-corpus.md` (I16): whether `internal/corpus` should skip `.claude/`, so
     program files are neither docscheck input nor able to satisfy a presence check.
7. **One fresh-clone gate** at the end: clone `origin/main` to `/cache/tmp/holdfast-g1-fresh` and
   run the full gate there; it serves line B and the packet.
8. **Checkpoint packet** (§20), printed in the final turn.

**Done when:** the lettered lines of `2026-09-holdfast-g1.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. (foundation) The identity scan runs in `make check` with a biting selftest, CLAUDE.md and internal/server/server_test.go no longer name the owner, and `make check` passes on a fresh clone of origin/main (tails, wall-clock)
C. `flock` (no -o) over `go test -race ./internal/engine -run FailurePathIsByteIdentical -count=3` passes (tail, PR URL)
D. `git grep -n 'by the owner (T' origin/main` shows the six reversals (audio, nodes, frontend, DV/HDR10+ note, release acts, plan of record)
E. `scripts/test-mass.sh -check` exits 0, no `scripts/regress_0057_*` is tracked, and CLAUDE.md is under 200 lines naming every `internal/` package (commands printed)
F. Branch/tag names from `git ls-remote --heads --tags origin` before vs after differ by exactly remove-comment-density, remove-dashboard, sdd/S0022-*, sdd/S0035-*, sdd/S0126-*, webui-e2e-playwright-graders and tag abandoned/S0098-*
G. P1-P6 (triage, HW gates, AMD/image, node protocol, crop on DV, docs corpus) exist, each with `## Recommendation` (paths); P1 has a row per S0151, S0162-S0180 and PR #94 (count printed)
H. The Checkpoint T packet (proposals, reversal diffs, I1-I20, the fresh-clone gate, ledger@sha, NEEDS-OWNER list) is printed; `git ls-tree origin/main .claude/goals/CHECKPOINT-T.approved` is empty
I. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
J. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
K. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §6 Goal 2 - Carried specs and the debian13 base

**Precondition:** the goal-1 ledger's `COMPLETE (goal 1)` line and
`.claude/goals/CHECKPOINT-T.approved` on `origin/main`; print the approval commit's
`gh api repos/NSchatz/holdfast/commits/<sha> --jq '.commit.verification.verified,.commit.committer.name'`
(I19). Research: `research-hw-encode.md` (packaging note), the approved P1.

1. **Triage rows.** Serve every spec the approved P1 assigns to goal 2. PR #94, if kept, is
   cherry-picked onto `holdfast-g2/s0159-temp-sweep`, gated and merged as a new PR; #94 is closed
   with a link to it; its old branch is left and listed as a follow-up (I12).
2. **Base image.** Move the runtime to `gcr.io/distroless/cc-debian13` (nonroot), pinned by tag
   and digest; keep `check-pins.sh` sections 3 and 7 green; the CI `package` job proves the
   image. This goes first among the image changes because trixie's vendor libraries need glibc
   2.38 or later (`research-hw-encode.md`).
3. Docs follow the image (`docs/docker.md`).

**Done when:** the lettered lines of `2026-09-holdfast-g2.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 2 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. (foundation) The runtime base is gcr.io/distroless/cc-debian13 pinned by tag and digest: the Dockerfile line, `scripts/check-pins.sh` tail, and the merged PR's green CI `package` job
D. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
E. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
F. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §7 Goal 3 - The encode plan

**Precondition:** the goal-2 ledger's `COMPLETE (goal 2)` line.

1. **Triage rows** the approved P1 assigns to goal 3.
2. **The encode plan.** Refactor `internal/engine` so each job carries one declared plan: the
   video encoder and its device, the decode path, pixel format and quality value; audio
   operations; subtitle operations; picture operations (`deinterlace`, `max_height`, crop);
   metadata carriers (HDR10, HDR10+, DV). The argv builder and every gate read the plan, so a gate
   can never check something other than what was encoded.
3. **Behaviour-preserving:** golden argv tests for every registry encoder and every option
   combination the existing fixtures use are written BEFORE the refactor and pass unchanged after
   it; no existing engine test assertion is removed or relaxed.
4. `docs/design/encode-plan.md` with an anchor for the plan; `CLAUDE.md` links it by rule.

**Done when:** the lettered lines of `2026-09-holdfast-g3.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 3 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. (foundation) The encode plan drives argv and every gate: golden argv tests written before the refactor cover every registry encoder and every existing option combination and pass unchanged after it (test tail and the golden-file diff since goal start, empty)
D. docs/design/encode-plan.md carries the plan's anchor and CLAUDE.md links it: `git grep` hits in both
E. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
F. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
G. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §8 Goal 4 - The fidelity gate and per-encoder quality

**Precondition:** the goal-3 ledger's `COMPLETE (goal 3)` line. Research:
`research-hw-encode.md` Q3-Q4, the approved P2.

1. **Triage rows** the approved P1 assigns to goal 4.
2. **Output fidelity gate** (P2 as approved; default: build it): for every encoder, the output's
   bit depth, chroma subsampling, colour primaries, transfer, matrix, and HDR10 mastering and
   content-light side data equal the source's, or equal what the plan declares it changes. One
   fixture per field reds when that field is lost.
3. **Explicit pixel formats** in every argv, so ffmpeg never auto-selects a format silently
   (`fftools/ffmpeg_mux_init.c` logs that only as a warning, and holdfast runs at
   `-loglevel error`).
4. **Per-encoder quality scales** (P2): replace the single CRF reused as `-cq`,
   `-global_quality` and `-qp` with a per-encoder quality key and default. Defaults not measured
   on hardware are marked `ASSUMED` until a hardware report calibrates them (§0.6).
5. `docs/design/encode-plan.md` gains the fidelity gate's anchor; `CLAUDE.md` links it.

**Done when:** the lettered lines of `2026-09-holdfast-g4.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 4 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. (foundation) The fidelity gate reds one fixture per field - bit depth, chroma subsampling, primaries, transfer, matrix, HDR10 mastering and light-level side data: `go test -run` tail naming each
D. Every argv sets its pixel format explicitly (golden test tail), and per-encoder quality keys replace the CRF reused as -cq/-global_quality/-qp, unmeasured defaults marked ASSUMED (`git grep -n ASSUMED internal/encoder`)
E. docs/design/encode-plan.md carries the fidelity anchor linked from CLAUDE.md (`git grep` hits)
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §9 Goal 5 - Hardware runtime, argv and detection

**Precondition:** the goal-4 ledger's `COMPLETE (goal 4)` line. Research:
`research-hw-encode.md`, `verify-hw-encode.md`, the approved P3.

1. **Triage rows** the approved P1 assigns to goal 5 (S0165, per-rule encoder selection, if kept).
2. **Runtime in the image** per the approved P3 (default, its recommendation): libva, libva-drm
   and libdrm (the pinned ffmpeg aborts without them); the Intel iHD driver and libmfx-gen on
   amd64; Mesa's radeonsi VA driver. Debian package versions pinned; `NOTICE` names each copied
   package and its licence. An image smoke step proves the libraries resolve (a VAAPI init on a
   missing device prints a device error, not an abort).
3. **`amf` in the image** is refused at start, in `Available()`, with a named reason (AMD's EULA
   bars redistributing the runtime; use `vaapi`), and kept for host installs; it is never aliased
   to `vaapi`. `holdfast validate` still accepts the key.
4. **NVIDIA:** `ENV NVIDIA_DRIVER_CAPABILITIES=compute,video,utility` and the compose/docs snippet
   fixed for hosts on the legacy runtime path (on CDI hosts, Docker 29.2+ with NVIDIA Container
   Toolkit 1.18, the encoder library is already included; `verify-hw-encode.md` claim 9).
5. **Argv correctness:** every VAAPI device opens with `connection_type=drm` (without it a missing
   render node reaches the X11 shim and aborts; `verify-hw-encode.md` claim 1); VAAPI uploads
   10-bit as `p010` with `main10`; NVENC and QSV get explicit pixel formats; a source whose chroma
   an encoder cannot carry is skipped with a reason (the fidelity gate is the backstop).
6. **Detection and fallback (T11, T12):** device discovery (`/dev/dri/renderD*`, sysfs vendor, a
   permission error that names `group_add`), `Available()` probing through the real argv builder
   including a 10-bit probe, `encoder: auto` choosing per job, and a per-library
   `hw_fallback: software|skip`. The goal picks the default, documents why, and records it in
   the ledger (T11).
7. `docs/docker.md` hardware section and `docs/design/hardware.md`.

**Done when:** the lettered lines of `2026-09-holdfast-g5.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 5 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. The image carries libva, libva-drm, libdrm, Intel iHD and libmfx-gen (amd64) and Mesa radeonsi VA, or what the approved P3 names instead, each in NOTICE, and NVIDIA_DRIVER_CAPABILITIES includes video: Dockerfile and NOTICE lines and the merged PR's green CI `package` job
D. VAAPI opens with connection_type=drm and uploads 10-bit as p010 with main10, NVENC/QSV pixel formats are explicit, and `Available()` probes through the real argv builder: test tails; `rg -n hwlive .github Makefile` prints nothing
E. `encoder: auto` and per-library `hw_fallback` (default and reason stated) choose and fall back on fakes, and `amf` in the image is refused at start with a named reason: test tail
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §10 Goal 6 - Hardware decode, new encoders and hardware reports

**Precondition:** the goal-5 ledger's `COMPLETE (goal 5)` line. Research:
`research-hw-encode.md` Q3.

1. **Triage rows** the approved P1 assigns to goal 6.
2. **Hardware decode** (`hw_decode`, off by default): per-vendor `-hwaccel` pipelines that keep
   10-bit and HDR, with frames downloaded to system memory wherever a gate needs them; the
   fidelity gate covers them.
3. **New encoders (T27):** `libx264`, `h264_nvenc`, `h264_qsv`, `h264_vaapi`, `h264_amf`,
   `av1_qsv`, `av1_vaapi`, `av1_amf`, with golden argv, quality scales and the codec-family skip
   rules (a source already in an equal-or-better family is not re-encoded into a worse one).
   `NOTICE` names libx264 (GPL-2.0-or-later).
4. **`scripts/hw-report.sh` (T43):** runs a fixed synthetic clip through holdfast's real path on
   each available encoder and writes `testdata/hw-reports/<encoder>-<date>.json` (encoder,
   driver, ffmpeg pin, gate figures, timing; no hostname, path, serial or username - a test proves
   the redaction). `NEEDS-OWNER.md` gets one entry per path: NVENC, QSV, VAAPI on Intel, VAAPI on
   AMD, and AMF on a host install.

**Done when:** the lettered lines of `2026-09-holdfast-g6.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 6 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. `hw_decode` pipelines per vendor keep 10-bit and HDR (golden argv and the fidelity gate on fakes): test tail
D. libx264, h264_nvenc, h264_qsv, h264_vaapi, h264_amf, av1_qsv, av1_vaapi and av1_amf are in the registry with golden argv and quality scales, and `holdfast validate` accepts a config naming each: tails
E. `scripts/hw-report.sh` writes a redacted report (a test proves no hostname, path, serial or user survives), and NEEDS-OWNER.md has one entry each for NVENC, QSV, VAAPI on Intel, VAAPI on AMD and AMF on a host (count printed)
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §11 Goal 7 - Audio and subtitles

**Precondition:** the goal-6 ledger's `COMPLETE (goal 6)` line. Research:
`research-streams-hdr.md` sections 2-3, `verify-streams-hdr.md`.

1. **Triage rows** the approved P1 assigns to goal 7.
2. **Audio (T13, T19, T20), all off until configured (I5):** re-encode lossless or bulky tracks
   (TrueHD, DTS-HD MA, PCM, FLAC) to a configured codec and bitrate per layout, replacing the
   source track by default and keeping both with `keep_original_audio: true`; an explicit codec
   and layout matrix with `-ch_layout` set (E-AC-3 and AC-3 carry at most 6 channels, and the
   encoder maps 7.1 to 5.1 silently with exit 0, so a 7.1 source goes to a codec that carries it
   or skips); an added stereo downmix track; two-pass EBU R128 loudness on added and re-encoded
   tracks, always followed by `aresample` to the source rate; pass 2 can silently fall back to
   dynamic mode, so the job row records which mode ran and the loudness gate judges the result.
   `libfdk_aac` is never used. Bitrates not taken from a primary source are marked `ASSUMED`.
3. **Audio gates:** decoded duration parity within a stated tolerance, channel count and layout
   equal the plan, sample rate equal the plan, a full decode of every output audio stream, a
   post-encode loudness check within tolerance of the target; stream-count parity learns the
   declared additions and replacements. Packet counts are not compared across codecs. Strictly
   smaller still applies to the whole file. One fixture per gate reds.
4. **Subtitles (T24, I17):** SubRip, ASS and WebVTT copied to sidecars
   `<name>.<lang>[.forced].<ext>` beside the file, embedded subs kept; bitmap subs and mov_text
   skipped with a reason; an existing sidecar is never overwritten (exclusive create); an ASS
   track with font attachments is logged as losing its fonts. Sidecar gate (I3): the written file
   parses back with ffprobe and its event count equals the source stream's.
5. R1's statement rewritten in `README.md` and `docs/migration.md`; `docs/design/audio.md` and
   `docs/design/subtitles.md` with anchors; `docs/profiles.md` and `config.example.yaml` gain
   the keys.

**Done when:** the lettered lines of `2026-09-holdfast-g7.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 7 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. Audio re-encode (replace by default, `keep_original_audio` keeps both), the stereo downmix and two-pass loudness pass synthetic fixtures, and each audio gate - duration, channel layout, sample rate, full decode, loudness - reds its own fixture: test tail
D. With the new keys unset, argv and decisions equal the pre-goal ones on the existing fixtures: golden test tail
E. SRT, ASS and WebVTT go to `<name>.<lang>[.forced].<ext>` sidecars, never overwriting; bitmap and mov_text subs skip with a reason; the parse-back count gate reds on a truncated sidecar: test tail
F. The audio non-goal statement is rewritten in README and docs/migration.md, and docs/design/audio.md and subtitles.md carry anchored rules: `git grep` hits
G. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
H. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
I. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §12 Goal 8 - Dynamic HDR and crop

**Precondition:** the goal-7 ledger's `COMPLETE (goal 7)` line. Research:
`research-streams-hdr.md` sections 1 and 4, `verify-streams-hdr.md`, the approved P5.

1. **Triage rows** the approved P1 assigns to goal 8.
2. **Tools:** `dovi_tool` and `hdr10plus_tool` static musl binaries pinned per arch by version and
   sha256 in the Dockerfile and in a CI install script modelled on `scripts/install-ffmpeg.sh`,
   covered by `check-pins.sh` and its selftest, named in `NOTICE` (MIT).
3. **Carry (T13, T25, I6):** DV profile 8 through libx265 with `-dolbyvision 1` set explicitly
   (never `auto`, which drops the RPU), with the VBV settings and mastering-display metadata x265
   requires for DV; ffmpeg ignores `x265-params dolby-vision-rpu` with only a warning and exit 0,
   so it is never used and the RPU gate is what proves the carry. HDR10+ extracted with
   hdr10plus_tool and passed as `dhdr10-info`; an HDR10+ source whose metadata cannot be extracted
   and validated skips. P7 to P8.1 only when opted in: `dovi_tool -m 2 convert --discard` before
   the encode, refusing variable frame rate, logging FEL or MEL (mode 2 per the dovi_tool
   README). P5 stays skipped. Only the `cpu` encoder carries dynamic HDR; others keep skipping such
   sources. Raw HEVC cannot be stream-copied into mkv with this ffmpeg, so every design injects
   metadata during the encode, never after it.
4. **Dynamic-HDR gates:** the output's DOVI record profile and compatibility id match the plan
   and its RPU count equals the frame count; HDR10+ side data is present with a count equal to the
   frame count. One fixture per gate reds when the metadata is dropped.
5. **The statement:** R4's section of `README.md` and `internal/docscheck/dynamichdr.go` are
   rewritten in the same PR to describe the lifted skip and the gates that license it; the
   check's bite tests still fail on the old wording.
6. **Crop (T26), off by default:** cropdetect over spread samples (skipping the first and last
   few percent) with a fractional limit (an absolute limit fails on 10-bit, where black is 64);
   negative and implausible results discarded; the loose consensus; too few valid samples or
   disagreeing samples means no crop (the file still encodes uncropped). A blackness gate over the
   removed area of the source; VMAF against the identically cropped source; even dimensions for
   4:2:0; the crop rectangle recorded on the job row. Crop on a DV source follows the approved P5
   (default I7: refused, because the RPU's active-area offsets would go stale).
7. `docs/design/dynamic-hdr.md` and `docs/design/crop.md` with anchors.

**Done when:** the lettered lines of `2026-09-holdfast-g8.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 8 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. dovi_tool and hdr10plus_tool are pinned per arch by sha256 in the Dockerfile and the CI installer, covered by check-pins and its selftest, and named in NOTICE: tails
D. DV profile 8 and HDR10+ carry and opt-in P7-to-P8.1 pass fixtures, and each gate (DOVI profile, RPU count, HDR10+ count) reds when its metadata is dropped: test tail
E. README's dynamic-HDR statement and internal/docscheck/dynamichdr.go are rewritten in one PR and the bite tests pass: tail
F. Opt-in crop passes fixtures (letterbox, text in the bar, mixed aspect refused, 10-bit) with the blackness gate and a cropped VMAF reference, and crop on a DV source follows the approved P5 (refused by default): test tail
G. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
H. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
I. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §13 Goal 9 - Queue priority and the health sweep

**Precondition:** the goal-8 ledger's `COMPLETE (goal 8)` line.

1. **Triage rows** the approved P1 assigns to goal 9 (S0164 savings per hour, if kept).
2. **Priority (T31):** a `priority` on rules and profiles, and `queue_order: savings_rate`
   (estimated bytes saved per encode-hour); the estimator's inputs documented and checked against
   a worked example.
3. **Health sweep (T29):** a scheduled, run-window-respecting, resumable full-decode sweep that
   records results in the store, exposes them over the read API, notifies on corruption and
   exports a metric; it never moves, renames or deletes a file (a test proves it).
4. Keys and the metric documented in `docs/profiles.md`, `config.example.yaml` and the metrics
   documentation that docscheck reads.

**Done when:** the lettered lines of `2026-09-holdfast-g9.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 9 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. `priority` on rules and profiles and `queue_order: savings_rate` (bytes saved per encode-hour) order a fixture queue as specified, the estimator checked against a worked example: test tail
D. The health sweep is scheduled, honours the run window, resumes after a restart and is report-only (a test proves no file moved, renamed or deleted): test tail and a read-API sample
E. Its keys and metric are documented outside .claude: `git grep` hits in README.md, docs/ or config.example.yaml
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §14 Goal 10 - Media-server clients

**Precondition:** the goal-9 ledger's `COMPLETE (goal 9)` line. Research:
`research-nodes-clients-spa.md` section 2, `verify-nodes-clients-spa.md`.

1. **Triage rows** the approved P1 assigns to goal 10 (S0179 post-swap rescan, if kept).
2. **Plex (T28):** a partial refresh of the section path after a swap
   (`POST /library/sections/{id}/refresh?path=`); a hold on a file currently being played
   (`GET /status/sessions` mapped to file paths), in addition to the Tautulli hold. Both need an
   admin-scoped token.
3. **Sonarr and Radarr (T28):** `RescanSeries`/`RescanMovie` through `POST /api/v3/command` after a
   swap (API v3 on Sonarr v4 and Radarr v6); native webhook intake that accepts both Sonarr
   Download payload shapes and Radarr's (an upgrade arrives as Download with `isUpgrade`), queues
   the file, and replaces the custom script in `docs/docker.md`; the intake is authenticated.
4. **Credentials** for Plex, Sonarr and Radarr are by reference in `config.SecretBearingKeys`
   (§4).
5. **Live checks** are NEEDS-OWNER commands that write a redacted report (T49).

**Done when:** the lettered lines of `2026-09-holdfast-g10.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 10 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. After a swap Plex gets a partial refresh of the section path and a file being played is held, and Sonarr/Radarr get RescanSeries/RescanMovie: tests against httptest fakes (tail)
D. Authenticated webhook intake accepts both Sonarr Download shapes and Radarr's and queues the file (test tail), and `make api-schema-diff` passes with .api-schema-breaks.yaml still []
E. Each new credential key is in config.SecretBearingKeys and refuses a literal (test tail), and NEEDS-OWNER.md has live-check commands for Plex, Sonarr and Radarr
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §15 Goal 11 - Worker nodes: protocol, shared mount, server re-gate

**Precondition:** the goal-10 ledger's `COMPLETE (goal 10)` line. Research:
`research-nodes-clients-spa.md` section 1, the approved P4.

1. **Triage rows** the approved P1 assigns to goal 11.
2. **The protocol** per the approved P4 (default: its recommendation): `holdfast worker`; leases
   with TTL, heartbeat, epoch fencing and a bounded retry count that ends in a SKIP; an expired
   lease's calls get 410 and their bytes are discarded; idempotent, digest-checked uploads;
   versioned endpoints; per-node slots and a global transfer cap.
3. **Shared-mount mode (T17):** an ordered path-prefix map per node; the node reads the source
   through the mount and uploads the output, which lands as a server-named temp in the source's
   own directory, reserved by the free-space guard (the server stays the only writer into the
   library unless P4 as approved says otherwise).
4. **The server decides (T16):** the server re-runs every gate on the output and performs the
   rename itself; a worker verdict never licenses a swap. The swap invariant text is unchanged.
5. **Auth (T23):** `node_token` by reference, distinct from the read and control tokens; it can
   lease and upload, never control.
6. **Fixtures:** an end-to-end loopback run with a real tiny encode, and one fixture each for: an
   upload on an expired lease, a duplicate upload, a digest mismatch, a 404 body offered as media,
   a free-space reservation refusal, and a server restart with live leases.

**Done when:** the lettered lines of `2026-09-holdfast-g11.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 11 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. (foundation) `holdfast worker` encodes a fixture end to end on loopback over a shared mount with a path map, and the server re-runs every gate and does the rename: test tail
D. Fixtures red for an expired-lease upload, a duplicate upload, a digest mismatch, a 404 body as media, a free-space reservation refusal and a server restart with live leases: test tail naming each
E. `node_token` is by reference in config.SecretBearingKeys and cannot call a control endpoint: test tail
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §16 Goal 12 - Worker nodes: HTTP streaming, TLS and deployment

**Precondition:** the goal-11 ledger's `COMPLETE (goal 11)` line. Research: the approved P4.

1. **Triage rows** the approved P1 assigns to goal 12.
2. **HTTP-streaming mode (T17):** the server streams the source and receives the output, with
   sha256 digests checked in both directions; the output lands as in goal 11.
3. **Transport (T23, P4):** the TLS stance of the approved P4; the worker refuses plain `http://`
   to a non-loopback server unless explicitly allowed (logged loudly).
4. R2's statement rewritten in `README.md` and `docs/migration.md`; `docs/design/nodes.md` with
   anchors; `docs/docker.md` gains a worker deployment.

**Done when:** the lettered lines of `2026-09-holdfast-g12.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 12 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. HTTP-streaming mode encodes a fixture end to end on loopback with sha256 digests checked both ways: test tail
D. The worker refuses plain http to a non-loopback server unless explicitly allowed, and the approved P4's TLS stance is tested: test tail
E. The distributed non-goal statement is rewritten in README and docs/migration.md, docs/design/nodes.md carries the rule, and docs/docker.md shows a worker deployment: `git grep` hits
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §17 Goal 13 - Web UI: toolchain, gate, embed and image

**Precondition:** the goal-12 ledger's `COMPLETE (goal 12)` line. Research:
`research-nodes-clients-spa.md` section 3, `verify-nodes-clients-spa.md`.

1. **Triage rows** the approved P1 assigns to goal 13.
2. **Toolchain (T21):** `web/` with Svelte 5, Vite, TypeScript (6.x until svelte-check and
   typescript-eslint support 7), vitest, svelte-check and eslint; pnpm pinned through
   `packageManager` and a lockfile; Node LTS pinned (`.node-version` or `mise.toml`);
   `ignoreScripts: true` and `minimumReleaseAge: 1440` set explicitly in `pnpm-workspace.yaml`;
   `fsevents` (macOS-only, runs `node-gyp`) handled explicitly in `allowBuilds`; versions
   re-checked on the registry the day the goal runs.
3. **The pin check:** `check-pins.sh` section 8 requires `ignoreScripts: true` in
   `pnpm-workspace.yaml` for a pnpm project and refuses a `minimumReleaseAgeExclude` list (pnpm
   writes one on its own); its selftest proves it fails on a setup that sets `ignore-scripts`
   only in `.npmrc`.
4. **Gate:** `make check` gains UI lint, typecheck, unit tests and build (T21, T22; no browser).
   `go build` and `go vet` pass without a built UI (a committed placeholder in the embed
   directory; Vite builds elsewhere and the result is copied, because `emptyOutDir` would delete
   the placeholder).
5. **Serving:** the UI is embedded with `go:embed` and served at `/`; the AGPL section 13 source
   offer stays served and tested.
6. **Image:** a UI build stage on a Node image pinned by tag and digest; the final image stays
   distroless.

**Done when:** the lettered lines of `2026-09-holdfast-g13.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 13 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. (foundation) `make check` runs UI lint, typecheck, unit tests and build with Node, pnpm, Svelte and Vite pinned: the gate tail showing those targets
D. check-pins section 8 requires `ignoreScripts: true` in pnpm-workspace.yaml and refuses a minimumReleaseAgeExclude list, and its selftest fails an .npmrc-only setup: tail
E. The UI is embedded and served at /, the AGPL source offer is still served, and the image builds the UI in a digest-pinned Node stage and stays distroless: sourceoffer test tail and the green CI `package` job
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §18 Goal 14 - Web UI: views and controls

**Precondition:** the goal-13 ledger's `COMPLETE (goal 13)` line.

1. **Triage rows** the approved P1 assigns to goal 14 (S0169-S0172 API facts and paging, if kept
   and not landed earlier).
2. **Views (T18, T53):** summary and savings, queue (with priority), history (filters and
   paging), health results, nodes.
3. **Controls:** the ones the API already offers (pause, resume, scan, exclusions) with the
   control token, held for the browser session only. `restore` and `requeue` never appear. The
   default bind stays loopback.
4. R3's statement rewritten in `README.md`, `CLAUDE.md`, `docs/api-reference.md` and
   `docs/docker.md`; `docs/design/web-ui.md`.

**Done when:** the lettered lines of `2026-09-holdfast-g14.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every row the approved P1 (path@sha printed) assigns to goal 14 is listed with DONE (PR URL) or DROPPED (why), or P1 assigns none
C. The UI shows summary and savings, queue with priority, history with filters and paging, health results and nodes: unit test tail
D. The UI drives pause, resume, scan and exclusions with the control token held for the session only, and `git grep` finds no restore or requeue route in web/ or internal/server: tails
E. The no-frontend statement is rewritten in README, CLAUDE.md, docs/api-reference.md and docs/docker.md, and docs/design/web-ui.md exists: `git grep` hits
F. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
G. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
H. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

## §19 Goal 15 - Finale: release, docs and the program report

**Precondition:** the goal-14 ledger's `COMPLETE (goal 14)` line.

1. **Triage rows** still open are DONE or listed as follow-ups with the reason.
2. **Docs:** `README.md` describes every shipped area and its default; `docs/comparison.md`
   updated to what ships; `CLAUDE.md` Layout complete and under 200 lines; every `docs/design/`
   anchor linked.
3. **Fresh-clone gate:** clone `origin/main` to `/cache/tmp/holdfast-g15-fresh` and run the full
   gate there.
4. **Release (T37, T47):** per `docs/release.md`: the dry-run dispatch, a version higher than
   every existing tag (a minor bump), the tag push, the release run green, the pull confirmed
   (with `crane` installed rootless through mise, since there is no Docker daemon), the compose pin
   and the API schema baseline committed.
5. **homelab (T32):** a PR, never merged, bringing its holdfast deployment and env example up to
   the release and the new keys; a NEEDS-OWNER entry to review and merge it.
6. **Hardware reports:** re-check `testdata/hw-reports/`; list what arrived and what is still
   NEEDS-OWNER.
7. **The program report:** `docs/program-report-2026-09-holdfast.md` - what was built and where,
   test counts and gate runtimes, NEEDS-OWNER items (safety first), proposals awaiting the owner,
   follow-ups (including #94's old branch, I12), and the worker variables to remove (I4).

**Done when:** the lettered lines of `2026-09-holdfast-g15.goal.txt`, verbatim (§21):

A. The precondition checks, printed with their output
B. Every P1 row is DONE or a listed follow-up (count printed)
C. The full gate passes on a fresh clone of origin/main: tail and wall-clock
D. (foundation) A minor release is out per docs/release.md: dry-run run URL, tag, green release run, `crane digest` of the pulled image, and the compose pin commit
E. The homelab PR (never merged) is open with its NEEDS-OWNER entry: PR URL
F. docs/program-report-2026-09-holdfast.md exists with sections for what was built, tests and runtimes, NEEDS-OWNER, proposals, follow-ups and the worker variables: path and its `## ` headings
G. `ls testdata/hw-reports/` lists a report per hardware path, or the line reads NEEDS-OWNER naming the missing ones
H. Gate integrity since the ledger's goal-start SHA: every deleted `*_test.go` line has a reason, no package's `func Test` count fell, no line of docs/design/swap.md or quality-gate.md was removed (counts printed)
I. All repos are clean and pushed, no open PR of this goal, zero `co-authored-by` in `git log <goal-start>..origin/main --format=%B`; NEEDS-OWNER.md is current; the ledger ends with its COMPLETE line
J. A fresh adversarial subagent checked every line above against the repos and found none false (its verdict pasted)

---

## §20 Checkpoint T (the owner, after goal 1)

Goal 1's final turn prints the packet:

- the proposals P1-P6 with their paths and one-line recommendations, flagging that P2's and P3's
  recommendations are options T10 and T45 did not choose as final answers;
- the reversals R1-R6 as worded (the diff lines), and the inferred rows I1-I20;
- the fresh-clone gate (tail and wall-clock);
- the goal-1 ledger path and SHA;
- the `NEEDS-OWNER.md` list.

**Approval** is a commit on `main` adding `.claude/goals/CHECKPOINT-T.approved` with the owner's
own words ("Approved by the owner, <date>" plus any amendments, for example "P3: use a -hw tag").
Preferred: the owner adds it in the GitHub web UI, which makes a GitHub-verified commit that goal 2
prints (I19); a session the owner tells to, in its own chat, may also write it. No goal ever writes
it. Goals 2-15 then chain without further review (T3). `T` is not used by any other program's
checkpoint (D, F, H and P are).

---

## §21 GOAL REPORT, BLOCKED and INCOMPLETE formats

```
GOAL REPORT (goal <n>): <title>
A. <line text> - DONE | NEEDS-OWNER (why; entry) | DROPPED (why; PR; red tails) | PROPOSED (where)
   evidence: <command> -> <5-20 line output tail>; PR <url>; <repo>@<sha>
B. ...
NEEDS-OWNER (this goal): <list, safety first, then what unblocks the most>
Proposals awaiting the owner: <list with paths>
Ledger: <path>@<sha> - <n> DONE, <n> NEEDS-OWNER, <n> DROPPED (reasons listed); COMPLETE line <sha>
Adversarial review of this report: <verdict and what it checked>
```

```
BLOCKED (goal <n>): precondition not met
check: <command> -> <output showing the failure>
origin/main: <git log -1 --oneline origin/main>
No other work was done in this goal.
```

```
INCOMPLETE (goal <n>): a foundation line could not be met
line: <letter and text>
attempts: <PR url> - <the three red gate tails, 5-20 lines each>
state left: <branches, open PRs, main@sha>; ledger <path>@<sha>, no COMPLETE line
The next goal will print BLOCKED until the owner acts.
```

---

## §22 Research appendix

All in `.claude/goals/2026-09-holdfast-research/`:

| File | Holds |
|---|---|
| `review-holdfast.md` | the pre-interview survey: layout, gate, CI, docs rules, open work, rough edges, feature state (identity scrubbed, I1) |
| `interview-2026-09-29.md` | every round, verbatim except identity scrubbed (I1) |
| `research-hw-encode.md` | vendor runtimes per arch, AMF licence, 10-bit/HDR pipelines, HW vs x265 efficiency, detection, licences; draft proposals for T10 and T45 |
| `research-streams-hdr.md` | dovi_tool/hdr10plus_tool, DV/HDR10+ through libx265 (tested), audio layout and loudness (tested), subtitle extraction and naming, cropdetect (tested) |
| `research-nodes-clients-spa.md` | Tdarr/FileFlows/Unmanic node models and the T46 draft proposal; Plex and Sonarr/Radarr APIs; the Svelte toolchain probe |
| `verify-hw-encode.md`, `verify-streams-hdr.md`, `verify-nodes-clients-spa.md` | the adversarial verification of each research file; their corrections are folded into §9-§17 |
| `review-brief-v1.md` | the adversarial review of v1, answered in §23 |

---

## §23 The v1 reviews, finding by finding

Review: `review-brief-v1.md` (5 HIGH, 7 MED, 14 LOW). Verifications: `verify-*.md` (claims
refuted or corrected are marked V).

| # | Finding | v2 |
|---|---|---|
| H1 | The owner's first name reaches public history (needs-file name, state token, about 35 prose uses) before Checkpoint T can stop it; `CLAUDE.md` and a server test fixture already leak it | Fixed before the first push: the list is `NEEDS-OWNER.md`, the state `NEEDS-OWNER`, prose "the owner" in every program file, survey and interview copies scrubbed (I1). Goal 1 scrubs the two old leaks and adds a mechanical identity scan to the gate (§5.2, line B). |
| H2 | The merge rule needs a rebase, and so a force-push, which the brief forbids; PR #94's branch belongs to another session | Fixed without touching T7: a PR branch is updated by merging `origin/main` into it (I11, §0.3); a base that moved only by ledger commits needs no re-gate. #94 is carried by cherry-pick to a new PR and closed with a link (I12, §6.1). |
| H3 | A DROPPED track has no legal done-when state, so a goal loops or lies | Fixed: DROPPED is a legal state for feature lines after the 3 fix rounds (§0.10, the goal files' closing sentence); lines marked (foundation) cannot be dropped and end the goal INCOMPLETE with its own report (§21, I14). |
| H4 | `internal/engine` will outgrow the 30-minute package timeout | Fixed: the engine clock rule (§4, I15), engine seconds in every PR and ledger (§0.3, §0.5). Measured: 1339.8 s, 74% (§0.11). |
| H5 | Goals 2, 3, 6, 7 and 8 are multi-day | Fixed: split into 15 goals (I13): foundations into goals 2-4, hardware into 5-6, ops and clients into 9-10, nodes into 11-12, UI into 13-14. |
| M1 | New packages fall under the 70% mutation floor, unmentioned | Fixed: §4 mutation-floor rule, `make mutation-diff` under the heavy lock before pushing, never an exclusion or a lower floor; §2 states the domain. |
| M2 | G1's ref diff would be red from `refs/pull/*` noise | Fixed: names only from `--heads --tags`, taken back to back, deletion by full refname (§5.5, line F). |
| M3 | Done-when lines cite sections and proposals the evaluator cannot see | Fixed: lines restate the content (the reversal files, the ref names, the packet contents, the gate list); §0.10 requires a report to restate any proposal rows a line relies on (the B lines print P1's rows for the goal). |
| M4 | Program Markdown can satisfy docscheck presence checks falsely | Fixed for now: new statements and metrics are proven in `README.md`/`docs/` by `git grep` (§4, I16); whether to exclude `.claude/` from the corpus is proposal P6. |
| M5 | Gate integrity is checked mechanically only once, and impractically | Fixed: every goal carries a gate-integrity line counted from the ledger's goal-start SHA (test deletions with reasons, `func Test` counts, removed lines in the two design documents; §4). |
| M6 | R5 and R6 locations incomplete or wrong | Fixed: R5 adds `README.md` ("Cutting a tag is a deliberate human act"); R6 names the `CLAUDE.md` Conventions line, both `README.md` sentences and `cmd/holdfast/main.go`'s package comment (§1.1). |
| M7 | Only prose stops a goal writing the checkpoint file | Partly: the web-UI path gives a GitHub-verified commit that goal 2 prints (I19, §6); enforcing it would forbid the session path the program's contract allows, so it is shown, not enforced, and the adversarial report review checks no goal created it. |
| L1 | §0.11 stale in places (gate row, docker CLI, go 1.23.12, free space, inventory lock) | Fixed: all updated with the measured gate run (§0.11). |
| L2 | The interview copy is not verbatim | Fixed: "verbatim except identity scrubbed (I1)" (§1, §22). |
| L3 | S0163 hinted to the nodes goal, but it is the local worker pool | Fixed: the hint is gone; P1 routes it. |
| L4 | Goal 1 runs the gate six or more times | Fixed: reversals and cleanup in one PR; one fresh-clone run serves line B and the packet (§5.4, §5.7). |
| L5 | The scheduled mutation workflow opens issues on failure | Fixed: goals open none; a workflow's issue is fixed, commented and closed (I20, §0.7). |
| L6 | P2 and P3 recommend options the owner did not choose as final | Fixed: both proposals and the packet flag it (§5.6, §20). |
| L7 | §8.4 went beyond T24 (`[.sdh]`, mov_text conversion) | Fixed: exactly T24's naming; mov_text skipped with a reason (I17, §11.4). |
| L8 | The settings only apply to a session started after the pull | Fixed: §0.1 says so, and the hand-off command pulls first. |
| L9 | Private repo names appear in public program files | Declined: goals need the names to clone them, T41's list does not include repo names, and the public repo already names both (`docs/migration.md` names the homelab path; `CLAUDE.md` names the umbrella). |
| L10 | T38 could be enforced mechanically | Fixed: `attribution.commit` "" in `.claude/settings.json` (I18, confirmed against the settings reference, 2026-09-29), and every goal's hygiene line counts `co-authored-by` in its commits. |
| L11 | The `hwlive` rule is in no done-when line | Fixed: goal 5 line D prints the `rg` result. |
| L12 | Goal files said "~2 agents" | Fixed: "at most 2 agents". |
| L13 | `validate` acceptance clashed with the in-image `amf` refusal | Fixed: the refusal lives in `Available()` at start; `validate` accepts the key (§9.3). |
| L14 | A scrub left "the host's an NVIDIA workstation card" | Fixed in `review-holdfast.md`. |
| V1 | `-vaapi_device` alone still aborts without libX11 (verify-hw-encode claim 1) | Folded: `connection_type=drm` on every VAAPI device (§9.5). |
| V2 | NVIDIA caps finding is stale on CDI hosts (claim 9) | Folded: the ENV is for the legacy runtime path (§2, §9.4). |
| V3 | HW efficiency was said to match x265 medium (claim 10) | Folded: the source supports only real-time presets such as x265 faster; P2 cites it that way. |
| V4 | `auto` drops the RPU for a different reason; x265 needs VBV and mastering display for DV; `dolby-vision-rpu` fails silently (streams claims 3-4) | Folded (§12.3). |
| V5 | "Never mode 5" has no primary source (claim 5) | Folded: mode 2 per the README; the mode-5 claim is dropped. |
| V6 | loudnorm pass 2 can fall back to dynamic silently (claim 10) | Folded (§11.2). |
| V7 | pnpm's release-age default is not strict and writes its own exemptions; `fsevents` runs `node-gyp` (spa claims 4-5) | Folded: explicit `minimumReleaseAge`, check-pins refuses the exemption list, `allowBuilds` for `fsevents` (§17.2-3). |
