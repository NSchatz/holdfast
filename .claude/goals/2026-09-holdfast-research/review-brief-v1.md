# Adversarial review of the holdfast brief v1 (2026-09-29)

Reviewer: a fresh read-only session. Nothing in any repo was changed. Scope: the brief
`.claude/goals/2026-09-holdfast.md`, the 9 goal files, the Needs file, `.claude/settings.json`, the
research copies, `decisions.md`, `interview.md` and the repo at 3c229da plus the staged program
files. Commands ran under `timeout`; `make check` was not run. Web claims went to a separate
verifier, and every claim it checked came back TRUE.

This file never spells out the owner's name. It writes the name-bearing identifier as
`NEEDS-<first-name>`, so that copying it into the repo cannot repeat finding H1.

## Summary

The brief is careful and mostly correct. Every decision row has a home, the lettered lines
match byte for byte, the environment facts re-run true, and every load-bearing web claim held.
Five things would still make goals fail, loop or do damage:

1. The owner's first name reaches the public repo before Checkpoint T can stop it.
2. The merge rule cannot be met without a force-push, which the brief forbids.
3. A track dropped after 3 fix rounds has no legal done-when state, so the goal can never finish.
4. The engine test package will hit the 30-minute per-package timeout on this 2-CPU container.
5. Several goals are multi-day.

## Findings

### HIGH

**H1. The owner's name enters public history before the owner can amend I1, and older name
leaks are left in place.**

Evidence:
- The program files put the first name in a file name (`.claude/goals/NEEDS-<first-name>.md`), in a state token, and in about 35 prose uses:
  - brief lines 135, 255, 275, 278, 304, 306, 311, 316, 318, 841, 866, 880 and 883;
  - every goal file, on line 1 ("Physical steps go on Needs <first-name> (§0.6)") and on the last line;
  - the Needs file, line 1;
  - the interview copy, lines 11, 54, 58, 65 and 77.
- I1 (brief:329) defers the question to Checkpoint T. But goal 1's precondition (brief:205-207, 488-490) needs these files on public `origin/main` first, and §0.3 (brief:78-79) forbids rewriting history. So the leak cannot be undone.
- The owner's own words: identity must not appear in the public repo beyond git author fields.
- Existing tracked files already break T41. Nothing in the program touches them:
  - `CLAUDE.md:115` carries the full name and personal email;
  - `internal/server/server_test.go:1541-1546` uses the owner's name as fixture header values.
- `NOTICE:2` is the only line T41 allows.

Fix:
- Before the first push, rename to a neutral identifier everywhere: `.claude/goals/NEEDS-OWNER.md`, state `NEEDS-OWNER`, prose "Needs owner". This covers the §15 formats and the interview copy.
- Regenerate the goal files with `gen/goals.py` and re-check that the lettered lines are identical.
- Record the rename as an I-row that the owner can reverse. Renaming later is cheap; unpublishing is impossible.
- Add to goal 1's T34/T41 cleanup:
  - `CLAUDE.md:115` becomes "commit as the repository's configured git identity";
  - the server fixture uses a synthetic name.
- Add a mechanical identity check to the fast docs checks run before a ledger commit, or as a check-pins section: `git grep -niE` for the owner's first name, last name and mail user must be empty outside `NOTICE`.

**H2. The merge rule needs a force-push that the brief forbids.**

Evidence:
- brief:78-79 forbids force-pushing.
- brief:80-86 merges only "on the branch rebased onto origin/main".
- Main keeps moving while a PR waits:
  - ledger-only commits go straight to `main` (brief:91-92, 131-132);
  - up to 2 parallel tracks each merge (brief:103);
  - each PR spends about 25 minutes in the local gate and 10-13 minutes in CI.
- Once main moves after a PR branch is pushed, the branch can only be "rebased" again by force-pushing.
- §6.1 (brief:542-543) says "PR #94 if kept (rebase, gate, merge)". #94's head is `sdd/S0159-holdfast-bounded-run-temp-sweep` (`gh pr list`), which an earlier session pushed. Rebasing it means rewriting another session's branch.

Fix:
- Allow `git push --force-with-lease` only on the goal's own unmerged `holdfast-g<n>/*` branches. `main` is never rewritten, and the squash merge keeps main linear anyway.
- Alternatively, count `git merge origin/main` into the branch as "rebased".
- State that a base that moved only by ledger-only commits needs no re-gate, because the fast docs checks cover those commits. Any code change on main does need a re-gate.
- For #94: cherry-pick it onto `holdfast-g2/s0159-temp-sweep`, open a new PR, and close #94 with a link. Never push to the old branch.

**H3. A DROPPED track has no exit in the done-when lines, so the goal loops forever or the report lies.**

Evidence:
- brief:87-88 drops a track after 3 fix rounds.
- brief:217-219 allows only DONE, `NEEDS-<first-name>` and PROPOSED as done-when states.
- Only the B lines accept DROPPED. These lines can never be met once their PR is dropped: G2 C-F, G3 C-G, G4 C-F, G5 C-F, G6 C-F, G7 C-F, G8 C-F and G9 C-D.
- The `/goal` evaluator then never passes, which is an unbounded loop. Otherwise the agent overstates, or quietly breaks the 3-round bound.

Fix:
- Allow `DROPPED (why; PR url; the three red tails)` for feature lines. List those lines under "Proposals awaiting the owner" and in the finale.
- Lines that later goals build on cannot be DROPPED: G1 B, G2 D and G2 E. If one of them fails, the goal ends with an INCOMPLETE report and writes no COMPLETE line, so the chain stops for the owner.

**H4. The engine tests will outgrow the 30-minute per-package timeout.**

Evidence:
- `Makefile:82` sets `TEST_TIMEOUT ?= 30m`, applied at `Makefile:85`.
- In `check-full.log:142`, `internal/engine` took 1397.4 s on this container. That is 78% of the budget.
- Goals 2, 4, 5 and 7 add real-encode engine fixtures:
  - one per fidelity field;
  - audio gates;
  - 10-bit x265 DV/HDR10+;
  - crop VMAF;
  - the node end-to-end.
- The result will be "panic: test timed out", which looks like a code failure. It burns fix rounds and tempts the agent to trim fixtures.
- `Makefile:66-81` already says the answer is the clock, never deleting a proof. The brief says nothing about it.

Fix: add a §4 rule.
- Every PR and ledger records the `internal/engine` seconds.
- New real-encode fixtures live in the new package that owns the code (audio, crop, dynhdr, node), so `-p=2` spreads the load.
- Once the engine passes 80% of `TEST_TIMEOUT`, raise it in the Makefile in its own commit with the measurement. Never override it by environment variable, so CI runs the same thing.

**H5. Goals 2, 3, 6, 7 and 8 are each several days of work on 2 CPUs.**

Each PR round costs about 25 minutes of local gate plus 10-13 minutes of CI, and there are 3-6 PRs per goal plus fix rounds.

- G2: an engine refactor (11.4k production and 30.8k test lines) with golden argv for every encoder, plus the fidelity gate, the quality scales, the base image and the triage rows.
- G3: the image runtime, argv fixes, hardware decode for 3 vendors, detection, fallback, 8 encoders and hw-report.
- G7: leases, fencing, uploads, 2 media modes, TLS and 6 failure fixtures.
- G8: the toolchain, the pin check, gate targets, 5 views and the image stage.

Fix: T4 ("as big as needed") allows more goals. Suggested splits:

| Goal | First half | Second half |
|---|---|---|
| G2 | 2a: triage rows, #94, the debian13 base | 2b: the encode plan; 2c: the fidelity gate and quality scales |
| G3 | 3a: runtime, NVIDIA caps, p010/pix_fmt argv, detection, `Available()`, auto, fallback | 3b: hardware decode, the 8 T27 encoders, hw-report, docs |
| G6 | 6a: priority, savings rate, health sweep | 6b: Plex, Sonarr, Radarr and webhook intake |
| G7 | 7a: protocol, shared-mount mode, server re-gate, failure fixtures | 7b: HTTP streaming mode, TLS, docs |
| G8 | 8a: toolchain, section 8, gate targets, embed, image stage | 8b: views, controls, docs |

Each new goal gets its own precondition line.

### MED

**M1. New packages fall under the mutation floor, and the brief never says so.**

Evidence:
- `.gremlins.yaml` sets `efficacy: 70`.
- `docs/mutation-testing.md:56`: "The domain is the module MINUS the list below". The exclusions (:76-86) cover only engine, vmaf, cmd, store, server and metrics.
- `mutation.yml:92-112` runs a diff-scoped mutation run on every PR, and T36 requires it green.
- So audio, subtitle, crop, dynhdr, health, mediaclient, node and webui, plus every encoder and config change, must reach 70%.
- `docs/mutation-testing.md:29-31` forbids a wider exclusion list.

Fix:
- State this in §4.
- Run `make mutation-diff REF=origin/main` under the heavy lock before pushing an in-domain PR.
- Budget the time for it.
- Never add an exclusion or lower the floor.

**M2. G1-E will be red from ref noise.**

Evidence:
- `git ls-remote origin` returns 95 `refs/pull/*` refs.
- `refs/pull/94/merge` (4e89bc5) is recomputed whenever main moves, and goal 1 moves main with PRs and ledger commits.

Fix:
- Diff only the names: `git ls-remote --heads --tags origin | awk '{print $2}' | sort`, taken back to back around the deletions.
- Delete by full refname (`refs/heads/...`, `refs/tags/abandoned/S0098-pre-d911314`).

**M3. Some done-when lines depend on text the evaluator never sees.**

These lines point at the brief or at proposals the evaluator does not read:
- G1-C ("R1-R6 of §1.1 in their files");
- G1-G ("the packet of §14");
- G6-C ("as §10.2 specifies");
- G9-F ("the §13.7 sections");
- G3-C ("the approved P3 runtime");
- G5-F ("P5's DV rule");
- every B line ("assigned to goal N").

Fix:
- Put the content in the line itself. For G1-C, for example: R1 README+migration, R2 README+migration, R3 README+CLAUDE+api-reference+docker, R4 README, R5 release.md, R6 CLAUDE+README, one hit printed per R-id.
- Or add to §15: a line that cites a section or proposal first restates it in one line, and B lines print the P1 path@sha and that goal's rows.

**M4. Program Markdown can make docscheck presence checks pass falsely.**

Evidence:
- `internal/corpus/corpus.go:41-49` walks the filesystem and skips only `.git`, `vendor` and `node_modules`. So `.claude/**` and untracked `.md` files are corpus input.
- `docscheck.go` (Undocumented) is a presence check.
- `dynamichdr.go:40-43` says the statement "is satisfied by the anchor wherever in the shipped corpus it appears".
- So a new metric name, or anchor plus clauses, written only in a ledger or proposal turns the "documented" check green.

Fix, either of:
- Goal 1 skips `.claude` in `corpus.Markdown` with a test, shown as an I-row. That also removes the §4 vocabulary hazard.
- Or each goal shows new metrics and statements with `git grep -- README.md docs/`.

**M5. Gate integrity is checked mechanically only in G2-D.**

- Other goals also change gates:
  - G4 teaches stream-count parity about added streams;
  - G5 changes the VMAF reference;
  - G7 re-gates worker output.
- G2-D's "each listed with its replacement" is impractical for a refactor of a 30.8k-line suite.

Fix: every goal's report prints:
- `git diff --numstat <goal-start>..origin/main -- '*_test.go'` deletions, with a reason for each;
- per-package counts of `func Test` and of `t.Error`/`t.Fatal`, before and after;
- the diff of `docs/design/swap.md` and `docs/design/quality-gate.md`, which must be empty unless P2 is approved.

**M6. The R5 and R6 locations are incomplete or wrong.**

- R5 misses `README.md:16-17`: "Cutting a tag is a deliberate human act".
- R6 says "CLAUDE.md (last line)", but the pointer is at `CLAUDE.md:119-120` in Conventions. The last lines are References.
- R6 also misses:
  - `README.md:329` ("The phased plan and its research live in the umbrella");
  - `cmd/holdfast/main.go:8-10` ("see operations/roadmaps/holdfast.md in the umbrella").

Fix: list all of them.

**M7. Only prose stops a goal from writing the checkpoint approval.**

Evidence:
- brief:868-871 relies on prose.
- `main` has no protection (API 404), and `gh` has admin.

Fix:
- The owner creates the approval file in the GitHub web UI.
- Goal 2's precondition prints `gh api repos/<repo>/commits/<sha> --jq '.commit.verification.verified,.commit.committer.name'`, where the sha comes from `git log --diff-filter=A -- .claude/goals/CHECKPOINT-T.approved`.
- It requires `true` / `GitHub`, and requires that commit to come after the goal-1 COMPLETE commit.

### LOW

1. **§0.11 is stale in places.**
   - The gate row points at a §17 row that does not exist yet. The measured figures are 24m19.6s and engine 1397 s (`check-full.log:142,170`).
   - A docker CLI exists at `/cache/go/bin/docker`, but there is no daemon or socket. Say so.
   - mise also has go 1.23.12.
   - Free space is now 642G.
   - `/cache/locks` also holds `inventory-merge.lock`.
2. **The interview copy is not verbatim** (brief:263-264). Line 16 was edited. Say "verbatim except identity scrubbed (I1)".
3. **S0163 is hinted to the wrong goal.** The hint sends it to G7 (distributed nodes), but S0163 is `workers-scale-cpu-quota`, about the local worker pool. Drop the hint.
4. **Goal 1 runs the gate 6 or more times.** Batch the reversals and the cleanup into one PR (T52), and let one fresh-clone run serve both G1-B and §14.
5. **Workflows can open issues.** `mutation.yml:137-138` files an issue when the scheduled run fails, but §0.7 says "Issues are not opened". Say that goals never open issues, and fix and log any issue a workflow opens.
6. **P2 and P3 recommend options the owner declined as final** (T10, T45). The packet should flag this, so that a bare approval is a knowing choice.
7. **§8.4 is broader than T24.** It adds `[.sdh]` and mov_text-to-srt conversion. Record them as an I-row or drop them.
8. **The settings only take effect in a new session.** `/workspace` has no `.claude/` today, and the env is read at session start. The owner must pull, then start a new session, before `/goal`. Otherwise G1-A is BLOCKED.
9. **The private repo names appear about 30 times** in public program files, while the public repo so far only says "a private homelab repo" and "the umbrella". Consider neutral names.
10. **T38 could be enforced mechanically.**
    - The installed CLI knows `attribution`; `includeCoAuthoredBy` is marked deprecated.
    - Set the commit attribution to empty in `.claude/settings.json`. That means amending §4's "only env and autoMemoryEnabled".
    - Have the H lines count `co-authored-by` in `git log <goal-start>..origin/main --format=%B`, which must be 0.
11. **The `hwlive` rule (brief:156-158) is in no done-when line.** Add `rg -n hwlive .github Makefile`, which must print nothing, to G3-D.
12. **The goal files are looser than T40.** They say "at most ~2 agents"; T40 says 2. Drop the tilde.
13. **G3-F conflicts with the amf refusal.** "`holdfast validate` accepts ... each" can clash with refusing `amf` inside the image. Say the refusal lives in `Available()` or at runtime, with a named reason.
14. **The scrub left a grammar slip.** `review-holdfast.md:308,402,550` reads "the host's NVIDIA workstation card".

## What checked out

- **Goal files:** all 9 are under 4,000 characters (2,845-3,230). The lettered lines are byte-identical to the brief (checked by script). No program file, staged or untracked, contains an en or em dash; the only non-ASCII character is §.
- **Fast gates:** on the worktree with the program files, `go test ./internal/docscheck/ ./internal/corpus/` is ok, `scripts/check-pins.sh` prints "pins agree", and `make secret-scan` is clean.
- **Decisions:**
  - T1-T55 and I1-I10 each have a home. No declined area gets built.
  - The order matches T44.
  - The debian13 base traces to `research-hw-encode.md:116-119`: trixie libs need glibc 2.38.
- **§0.11 re-run:**
  - `cpu.max` is 200000 100000, `nproc` is 56, the memory limit is 16 GiB;
  - no `/dev/dri`, and the GPU is off;
  - one filesystem;
  - no global go (the mise shim errors);
  - the ffmpeg pin and every encoder, hwaccel and filter named are present;
  - `core.hooksPath`, `claude` 2.1.284;
  - `gh` has admin on all three repos, `main` is unprotected and delete_branch_on_merge is on;
  - the last 25 runs were 23 success and 2 cancelled, and PR CI took 10.3-13.0 minutes;
  - `GOFLAGS` is not overridden in the Makefile, so the settings env reaches `go test`, and the CLI knows `autoMemoryEnabled`.
- **Web checks, all TRUE:**
  - `distroless/cc-debian13:nonroot` exists;
  - pnpm 11+ reads only auth and registry settings from `.npmrc`, `minimumReleaseAge` defaults to 1440 since v11, `allowBuilds` exists, and pnpm 12.6.0 is the latest;
  - dovi_tool 2.3.4 and hdr10plus_tool 1.7.2 are MIT and ship x86_64 and aarch64 musl builds;
  - `-m 2 convert --discard` matches the README (note: the meaning of mode 2 has changed before);
  - libx265 `-dolbyvision` has existed since FFmpeg 7.1;
  - Sonarr v4 and Radarr v6 still serve API v3 RescanSeries/RescanMovie;
  - crane is in the mise registry;
  - the AMD GPU PRO EULA forbids distribution;
  - NVIDIA Container Toolkit 1.18 CDI includes the encode library;
  - x264 is GPL-2.0-or-later.
- **Remote state:**
  - The 6 T35 branches and the tag exist, and none of them is an open PR's head.
  - super `pipeline/active` holds 21 holdfast specs: S0151, S0159 (= #94) and S0162-S0180. That equals P1's 21 rows.
- **Repo claims:**
  - The R1-R4 texts exist: `README.md:107,113,120,265`, `docs/migration.md:120-128`, `CLAUDE.md:67`, `docs/api-reference.md:12`, `docs/docker.md:177`.
  - `dynamichdr` is a presence check, so the R4 note is safe.
  - R5 is at `docs/release.md:12-13`.
  - The probe is at `encode_test.go:557-560`.
  - check-pins section 4 is at :235 and section 8 at :567.
  - The 4 regress files total 479 lines and nothing references them.
  - `CLAUDE.md` is 138 lines.
- **First gate run:** it failed only on the fd-3 test (`check-full.log:136-142`). The second run was still in the engine package at 02:19 and was not seen green.

## Verdict

**Not ready to run as v1.** Fix H1 before anything is pushed, because it cannot be undone. Fix
H2-H5 before goal 1 starts. M1-M7 are cheap and should go into v2. With those changes the brief
is sound: the decisions are complete, the facts are true and the lettered lines are
consistent.
