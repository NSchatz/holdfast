# Amendment to the 2026-09-holdfast program: CI is the whole gate (the owner, 2026-10-03)

The owner decided this on 2026-10-03, in a session's chat, after the repository load on the shared
host was measured (the goals program's ADR 0036: holdfast held 27.0 of 74.5 heavy-lock hours from
2026-10-01 17:00Z to 10-03). The owner wrote **"Holdfast and chorus NEED to use the public CI and
nothing local"**. holdfast is public, so its CI runs cost no minutes. These are the owner's later
words, so they beat the brief and the earlier amendments where they disagree (spec amendment.md,
"Precedence"). Like the 2026-10-02 owner amendment, this one changes the remaining goal files
themselves, by the owner's choice, and it applies at once: goal 14, in flight, included.

- **Spec version:** 1.1 (spec 1.1.7, gates.md "CI-only repos", holds for every pin).
- **In force for:** goal 14 from this merge on, and goal 15.

## What changes

1. **The merge rule (§0.3).** A PR merges when its CI is green (`build`, `package`, `mutation`) on
   the branch up to date with `origin/main`, with the run's link and wall-clock in the PR body. The
   local `make check` under `goals-heavy` and `holdfast-heavy` is gone from the rule.
2. **Nothing runs here.** No gate, tier, whole-package or `-race` suite, mutation run or `pnpm build`
   on the development host. One focused test (one package, one `-run` filter) is the inner loop, not
   a gate; one with a real encode loop still takes `goals-heavy`, then `holdfast-heavy` (§0.4).
3. **The goals supervisor.** Its drain runs nothing for holdfast and takes no lock: GitHub merges
   main into the branch, the drain waits for the three checks on that head and squashes exactly
   that head. Its nightly no longer runs holdfast's full tier here: `ci.yml` runs nightly on main.
4. **Ledger-only commits** still go straight to main; CI's push run scans them.
5. **Goal files.** Goals 14 and 15: "merged only when `make check` (under `flock -o`) passes on the
   branch merged up to origin/main, tail in the PR, and PR CI is green" now reads "merged only when
   PR CI is green (`build`, `package`, `mutation`) on the branch merged up to origin/main, the run's
   link in the PR, and no gate, tier or whole suite runs on this host (owner amendment 2026-10-03)".
   Goal 15's line C, "The full gate passes on a fresh clone of origin/main: tail and wall-clock",
   now reads "The full gate passes on CI on origin/main's head (`build`, `package`): the run's link,
   result and wall-clock".

## Left in force

Every other line of goals 14 and 15, the 3 fix rounds and DROPPED, merging main into a branch
(never rebasing), Conventional Commits with no trailer, the identity rules, the agent budget,
GOFLAGS/GOMAXPROCS, and every earlier amendment's top note.
