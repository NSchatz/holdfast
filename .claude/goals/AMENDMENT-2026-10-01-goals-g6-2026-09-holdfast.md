# Amendment to the holdfast program: spec v1.0 (goals program, goal 6, 2026-10-01)

Written by the goals program (its brief, sections 10 and 0.13, goal 6) and merged into holdfast between goals, during a
drain of this program (no holdfast goal running). It applies to every holdfast goal whose ledger is created after
it merged, and **never against a line of that goal's goal file**: where a goal file names a file, command,
gate, label or rule, that line stays in force for that goal (the list below, and the list of `AMENDMENT-2026-10-01-goals-g5-2026-09-holdfast.md`). A goal
already running when it merged keeps the contract it started with.

- **Spec version:** 1.0
- **The spec:** spec v1.0 of the goals program (its release v1.0.0; sessions load it as the `/goals:spec` skill of the goals plugin). Its files: `contract.md` (the standing contract), `brief.md`, `goal-file.md`,
  `ledger.md`, `lanes-manifest.md`, `checkpoint.md`, `amendment.md`, `report.md`, `needs-noah.md`,
  `requests.md`, `locks.md`, `gates.md`.
- **What it supersedes:** the brief's rule "Where they disagree, §0-§4 win" reads "where §0-§4 and the spec
  disagree, the spec wins". The brief, its goal files, `AMENDMENT-2026-10-01-goals-g5-2026-09-holdfast.md` and every checkpoint file are otherwise unchanged.

## The owner's words it carries out (the goals interview, 2026-10-01)

| Row | The owner's decision, as recorded |
|---|---|
| W1 | Outcomes: **Hands-off reliability + One shared standard + Visibility from anywhere** |
| W7 | **Between goals, self-merged**: no goal sees its contract change mid-run; no per-amendment approval |
| W35 | Versions of the standard: **Semver releases, pinned per program**: each brief names the spec version it runs; upgrades arrive only as between-goal amendments that say what changed |
| W52 | Success: **one standard everywhere**: every program with goals left on the same pinned spec version; the linter passes on every goal file in every repo |
| W63 | Merge invariant of the goals program: **No program breaks**: the linter and the supervisor accept every program at the spec version it pins |
| W64 | Delegated checkpoint review: **Fresh independent reviewer** for checkpoints after goal 1; goal-1 checkpoints stay the owner's alone |

These carry out the reversals R5 (delegated review, now in the spec's `checkpoint.md`) and R6 of the goals
brief; its inferred reading "running programs adopt spec v1's rules by amendment while their goal files keep
reading their own section 0" was shown at its Checkpoint W and approved by the owner. As the owner's later words
they beat earlier lines where they conflict, except where a goal file wins.

## What changes

1. **The pin.** holdfast runs spec 1.0. A later spec version reaches it only by another amendment that says
   what changed (W35). `goals lint holdfast 2026-09-holdfast` checks this program at 1.0 (the "adopted" profile: the
   brief and goal files keep the form they were planned in), and the goals program's gate runs it over every
   program (W63).
2. **Precedence.** Where the brief's §0-§4 and the spec disagree, the spec wins; a goal file wins over both;
   holdfast's own rules in the brief's tables (gate, locks, commit rules) are the parameters the spec leaves to
   the program and stay as written.
3. **What a goal reads.** Each goal still reads what its goal file names (§0-§4 and its own section); it also
   loads the spec's `contract.md`, `ledger.md` and `report.md` (the `/goals:spec` skill) at its start and
   after a compaction.
4. **Ledgers** created after this merged follow the spec's `ledger.md`: the states `TODO`, `DOING`, `DONE`,
   `NEEDS-NOAH`, `NEEDS-OWNER`, `PROPOSED`, `DROPPED` in every `| # | Item | State |` row (every holdfast
   ledger already does), and no `TODO` or `DOING` row once the COMPLETE line is in.
5. **Fan-out.** Before a fan-out a goal asks `goals admit --agents <N>` and runs at most what it allows and
   at most the brief's own cap, whichever is lower (W17, W42).
6. **Checkpoints, locks, reports** follow the spec's `checkpoint.md` (W64, as `AMENDMENT-2026-10-01-goals-g5-2026-09-holdfast.md` item 5 already says),
   `locks.md` (the names and order `AMENDMENT-2026-10-01-goals-g5-2026-09-holdfast.md` gave) and `report.md` (the GOAL REPORT, BLOCKED and INCOMPLETE
   headings the brief already uses).

## Left in force (goal files win)

Every line of the table "Left in force" in `AMENDMENT-2026-10-01-goals-g5-2026-09-holdfast.md` stays in force, unchanged, until holdfast finishes or the
later amendment it names. In particular the spec's issues for human-only steps and requests, and its gate tiers, do not
replace the lists, rows and gates those goal-file lines name; the goals program mirrors or adds beside them.
Also left in force: each goal file's first line, which reads the brief's §0-§4 in full.
