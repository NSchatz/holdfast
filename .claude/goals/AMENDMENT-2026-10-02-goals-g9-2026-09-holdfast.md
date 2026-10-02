# Amendment to the 2026-09-holdfast program: gate tiers (goals program, goal 9, 2026-10-02)

Written by the goals program (its brief, sections 13 and 0.13, goal 9, R8) and merged into holdfast between goals, during a drain of this program (no 2026-09-holdfast goal running). It applies to every 2026-09-holdfast goal whose ledger is created after it merged, and **never against a line of that goal's goal file**: where a goal file names a file, command, gate, label or rule, that line stays in force for that goal (the list below). A goal already running when it merged keeps the contract it started with.

- **Spec version:** 1.0
- **What it supersedes:** nothing in practice for goals 10-15. Every remaining goal file names its merge gate (`make check` under `flock -o`, on the branch merged up to origin/main, and PR CI green), so that rule stays in force for each of them. What this adds: the two tier targets, the full tier at a goal's end, and a nightly run of the full tier on main.

## The owner's words it carries out (the goals interview, 2026-10-01)

| Row | Decision, as recorded |
|---|---|
| W15 | Per-goal costs the new standard may cut: **Compact context + Tiered gates (fast gate per PR, full gate at goal end and nightly on main)** |
| W36 | Scope extras: **Tiered gates per repo** (the goals program adds fast and full tiers to each program repository's Makefile, holdfast's among them) |
| W7 | **Between goals, self-merged**: no goal sees its contract change mid-run; no per-amendment approval |

As later words of the owner these beat earlier lines where they conflict, except where a goal file wins.

## What changes

1. **The tiers** (in the `Makefile`, both printing their elapsed time):
   - `make tier-full`: `make check`, unchanged;
   - `make tier-fast`: `make check` with every package tested but `internal/engine` (the suite of real encodes; 2033 s of the 2066 s test step on a full run), through `TEST_PKGS`, which is `./...` by default;
   `check` and every other target keep their meaning; CI is unchanged.
2. **Every PR:** the goal file's rule, unchanged: `make check` (under `flock -o`) on the branch merged up to origin/main, its tail in the PR, and PR CI green. `make tier-fast` is a quicker check to run while working; it is never the merge gate of goals 10-15.
3. **A goal's last merge:** `make tier-full` (= `make check`), which the merge rule already runs.
4. **Nightly:** the goals supervisor runs `make tier-full` on origin/main every night from 08:00 UTC in its own worktree, with TMPDIR on disk, the pinned dynamic-HDR tools and the pinned Go, under the goals heavy lock and `holdfast-heavy`, and skips the night if those locks stay held 30 minutes. A goal reads the latest result in the owner's pinned status issue or with `goals nightly schedule`.

## Left in force (goal files win)

- g10-g15, the git line: "a branch + PR per track, merged only when `make check` (under `flock -o`) passes on the branch merged up to origin/main, tail in the PR, and PR CI is green": the merge gate, as written.
- g10 D: "`make api-schema-diff` passes with .api-schema-breaks.yaml still []": as written.
- g13 C: "`make check` runs UI lint, typecheck, unit tests and build with Node, pnpm, Svelte and Vite pinned": `make check` (and so `make tier-full`), as written.
- g15 C: "The full gate passes on a fresh clone of origin/main: tail and wall-clock": the full gate is `make check` (= `make tier-full`).
- Every goal file's "zero `co-authored-by` in `git log <goal-start>..origin/main`" line: no trailer, as written.
