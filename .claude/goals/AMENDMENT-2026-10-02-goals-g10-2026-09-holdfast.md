# Amendment to the 2026-09-holdfast program: shared packages and releases (goals program, goal 10, 2026-10-02)

Written by the goals program (its brief, sections 14 and 0.13, goal 10, R1 and R2) and merged into holdfast between goals, during a drain of this program (no 2026-09-holdfast goal running). It applies to every 2026-09-holdfast goal whose ledger is created after it merged, and **never against a line of that goal's goal file**: where a goal file names a file, command, gate, label or rule, that line stays in force for that goal (the list below). A goal already running when it merged keeps the contract it started with.

- **Spec version:** 1.0
- **What it supersedes:** nothing in practice for goals 10-15. This program runs alone in its repository (T6): it owns no package of a shared library, pins none, and cuts no release of one. The amendment records the owner's rule for shared libraries here because every program with goals left carries it.
- **In force from:** the day every package-owning program with goals left carries its own goal-10 amendment on its main branch (`goals ship <library> --in-force` prints that day). holdfast owns no shared package, so it is not one of them.

## The owner's words it carries out (the goals interview, 2026-10-01)

| Row | Decision, as recorded |
|---|---|
| W22 | **"Goals should be able to work across shared repos."** |
| W23 | Ownership in shared repos: **Any goal, guarded**: any goal may change any shared package in its own PR if the shared repo's gate passes and every consuming repo's tests pass against the change; owners remain the record of who knows the code |
| W24 | Shipping a shared-library change: **One goal, ordered PRs**: the same goal does library PR -> release tag -> bump and test each consumer, under one release lock; no cap on releases per goal; pins stay reproducible |
| W7 | **Between goals, self-merged**: no goal sees its contract change mid-run; no per-amendment approval |

As later words of the owner these beat earlier lines where they conflict, except where a goal file wins.

## What changes

1. **Nothing for goals 10-15 as planned.** No remaining goal file names a shared library, and holdfast is no consumer of one: `goals consumers-test` finds consumers from the pins in each repository's manifest, and holdfast has none.
2. **The rule, should a holdfast goal ever need a shared library:** any goal may change a shared package in its own PR when the shared repository's gate passes and `goals consumers-test <library> --ref <branch>` passes (every consuming repository's fast tier run against the change, each printing the path of the library it imported); the library's ownership table stays the record of who knows the code; one goal ships the library PR, the release tag and every consumer's pin bump with `goals ship <library>`, holding the release lock throughout, with no cap on releases per goal; a pin bump into a repository whose program has goals left waits for a drain, so it never lands in the middle of a goal.
3. **Into holdfast:** unchanged. Another program's PR lands here only in a drain, under `make check` and green PR CI, with no trailer.

## Left in force (goal files win)

- g10-g15, the git line: "a branch + PR per track, merged only when `make check` (under `flock -o`) passes on the branch merged up to origin/main, tail in the PR, and PR CI is green": the merge gate, as written.
- Every goal file's "zero `co-authored-by` in `git log <goal-start>..origin/main`" line: no trailer, as written.
- Every goal file's "Physical steps go on NEEDS-OWNER.md" line: as written.
