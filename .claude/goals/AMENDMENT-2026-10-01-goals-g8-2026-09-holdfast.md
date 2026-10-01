# Amendment to the 2026-09-holdfast program: the owner's queue and requests as issues (goals program, goal 8, 2026-10-01)

Written by the goals program (its brief, sections 12 and 0.13, goal 8, R3) and merged into holdfast between goals, during a drain of this program (no 2026-09-holdfast goal running). It applies to every 2026-09-holdfast goal whose ledger is created after it merged, and **never against a line of that goal's goal file**: where a goal file names a file, section, label or rule, that line stays in force for that goal (the list below). A goal already running when it merged keeps the contract it started with.

- **Spec version:** 1.0
- **What it supersedes:** nothing is removed. The lists this program writes (`.claude/goals/NEEDS-OWNER.md` in this repo) stay live; from the next goal each item and request is also an issue in the owner's queue: issues in the goals program's private repository, made from the list by one command, and the requests addressed to holdfast are served from those issues as well as from the rows.

## The owner's words it carries out (the goals interview, 2026-10-01)

| Row | Decision, as recorded |
|---|---|
| W21 | the owner's queue: **One GitHub issue per item in the goals program's repository**, labelled by repo and kind (physical, purchase, credential, decision), safety first; the owner answers/closes from the GitHub app; a push on each new one (W9) |
| W25 | Program-to-program requests: **Issues in the goals program's repository** (labelled from/to, state, acceptance check); every goal serves the open issues addressed to its program |
| W37 | Existing the owner items and requests: **Migrate all, de-duplicated**: every open the owner item and request row becomes one issue in the goals program's repository, linked to its source; old files become pointers |
| W7 | **Between goals, self-merged**: no goal sees its contract change mid-run; no per-amendment approval |

As later words of the owner these beat earlier lines where they conflict, except where a goal file wins.

## What changes

`G="${GOALS_STATE_DIR:-/cache/goals/${CLAUDE_PROJECT_NAME:-$(hostname)}}/bin/goals"` (the goals command the supervisor runs).

1. **Filing a human-only step.** Write it in the list the goal file names, as before, and push it. Then run `"$G" needs sync --repo holdfast` (or, before the push, `"$G" needs sync --repo holdfast --tree <your worktree>`): each open item of the list that has no issue becomes one, labelled the Needs label, the repo and the kind, with a link to its line, and the owner gets a push. The command asks once (an item already filed is never filed again); the supervisor runs the same sync every 15 minutes. A step no list of this program holds is filed with `"$G" needs add --repo holdfast --kind <kind> --title ... --do ... --tool ... --expect ... --changes ...`.
2. **Ending a step.** The owner says it in a session's chat; that session marks the item done in the list as the list's own rule says; the next sync closes the issue. Closing an issue alone records nothing.
3. **Requests.** holdfast files and receives no request rows today; if one is needed, `"$G" request add --from holdfast --to <owner> --title ... --what ... --acceptance ...` files it, and `"$G" request list --to holdfast` lists what is addressed to holdfast (the REST list, never search). Only the addressee changes a state (`"$G" request state`).
4. **Ownerless requests.** A request still open when its owner's program has finished is served by the requester, under the owner's rules and gate, citing it.
5. **The mirror.** Each list carries a generated block at its end, `## Issues (mirror)`, rewritten by the sync from the issues' states. Never edit it by hand; edit the list above it. The list becomes a pointer only after this program (and every other program that names it) has finished.

## Left in force (goal files win)

- Each remaining goal file's first line (g10-g15): "Physical steps go on NEEDS-OWNER.md (§0.6); carry on.": the file stays the live list; the issue is made from it.
- The done-when lines "...NEEDS-OWNER.md is current..." (g10-g15), g10 line 8 (live-check commands in NEEDS-OWNER.md), g15 lines 8-10 (the homelab PR's NEEDS-OWNER entry, the report's NEEDS-OWNER section, the hardware reports): the file, as written.
- Only the owner, or a session they tell to in its own chat, marks a row DONE (the file's own rule): unchanged; closing an issue alone records nothing.
- The NEEDS-OWNER state word of the goal files' last lines: unchanged.
