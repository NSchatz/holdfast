# Amendment to the holdfast program: the new goal supervisor (goals program, goal 5, 2026-10-01)

Written by the owner's goals program (its brief, section 9 and 0.13, goal 5) and merged into holdfast
between goals, during a drain of this program (no holdfast goal running). It applies to every holdfast
goal whose ledger is created after it merged, and **never against a line of that goal's goal file**:
where a goal file names a file, command, gate, label or rule, that line stays in force for that goal
(the list below). A goal already running when it merged, such as goal 9, keeps the contract it started
with.

- **Spec version:** none yet. The goals program releases spec v1 in its goal 6; it reaches holdfast only
  by a later amendment that says what changed.
- **What it supersedes:** "goals 2 to 15 then chain without further review" (brief L5, T3) and every other
  place that means the old runner, `claude-goal-chain`, read as below. Nothing else in the brief or
  `CHECKPOINT-T.approved` changes.

## The owner's words it carries out (the goals interview, 2026-10-01)

| Row | The owner's decision, as recorded |
|---|---|
| W4 | Running programs: **"Migrate as pieces land"**: as each new piece merges, running programs are amended to use it |
| W7 | **Between goals, self-merged**: the program PRs an AMENDMENT file into each affected repo, self-merges under that repo's merge lock, and the supervisor holds that program's next goal until it lands; no goal sees its contract change mid-run |
| W19 | Supervisor self-healing: **All four** (relaunch dead sessions, stop hung work, re-set goals cleared by errors, clean up disk) |
| W20 | Judging "goal done": **Checker, then evaluator** |
| W26 | Locks: **Supervisor-managed**: one lock tool, fixed names, logged wait and hold times |
| W30, W31 | The runner: cosyte/claude-containers PR #67 **ported to the goals program and closed**; supervisor cut-over **"Shadow, then swap"** |
| W64 | Delegated checkpoint review: **Fresh independent reviewer** for checkpoints after goal 1; goal-1 checkpoints stay the owner's alone |

These are the reversals R4 (the runner) and R5 (delegated review) of the goals brief. As the owner's later
words (2026-10-01) they beat earlier lines where they conflict, except where a goal file wins.

## What changes

1. **The runner (R4).** Since 2026-10-01 18:18:17Z the goals supervisor (`goals supervise`, in tmux
   window `goals-supervisor`) runs holdfast; `claude-goal-chain` is stopped and kept inert, with a
   documented fallback. It starts holdfast goal n+1 only when goal n's COMPLETE line is on origin and its
   session ended with a met verdict (or a printed GOAL REPORT) whose origin check passed; it never starts
   a goal 1 and never starts a goal twice. Each goal runs in its own interactive session with Remote
   Control, in window `holdfast-g<n>`, registered as a goal session; the session ends once its goal is
   met and origin-checked. "Chain" in this brief reads "the supervisor starts the next goal". The lanes
   check is `goals lanes holdfast` (the CLI is linked at `/cache/goals/<container>/bin/goals`). holdfast
   has no lanes manifest, so it stays a strict n-1 chain and finishes at goal 15's COMPLETE line.
2. **Self-healing (W19).** A dead goal session is relaunched with its goal re-issued (a push after two
   failures); a goal cleared by an error, such as the auth failure that cleared goal 3, is re-set once the
   cause clears; a session paused at a usage limit gets one "Continue." after the reset; Claude Code is
   pinned and changes only between goals. Every action is logged, and one that touches a goal also goes
   in that goal's ledger under `## Supervisor actions`, which the goal commits with its next ledger
   commit.
3. **The checker (W20).** Before the evaluator judges a goal, `goals check` verifies it on origin: the
   `COMPLETE (goal n)` line, no `TODO` or `DOING` row, the report's lettered lines, merged PRs and clean
   trees. It runs in the plugin's Stop hook (at most 3 blocks) and again in the supervisor after a met
   verdict (at most 2 re-issues, then the goal is held and the owner is pushed). A goal may run it itself
   before its final report: `goals check /workspace/holdfast 2026-09-holdfast <n>` (or the
   `/goals:check` skill). It adds to the goal files' own checks (zero `co-authored-by`, gate integrity,
   CI green); it replaces none.
4. **End states (W20).** BLOCKED and INCOMPLETE (this brief's section 0.10 and 21 formats, unchanged) are
   end states: a BLOCKED goal is held once and gets no note and no new turn; an INCOMPLETE goal holds the
   program and pushes the owner. The old runner's not-met notes ("Chain stopped...") no longer arrive.
5. **Checkpoints (R5, W64).** holdfast's one checkpoint, T after goal 1, is approved. A later checkpoint,
   if a future amendment adds one, goes to a **fresh independent reviewer**: a separate new session on a
   model at least as strong as the goal's, which sees only the packet and the repos and writes its
   verdict and reasons in the checkpoint's issue. A goal-1 checkpoint is the owner's alone. The
   same-session delegated review (`CLAUDE_GOAL_CHAIN_REVIEW`) is never used again. No remaining holdfast
   goal file names a new checkpoint, so no goal-file line changes.
6. **Locks (W26).** `goals lock <name> -w <seconds> -- <cmd>` takes flock(2) on the same
   `/cache/locks/<name>.lock` files, so it and `flock -o` exclude each other; it logs waits and holds and
   never leaks the lock to the child as an open fd. The name stays `holdfast-heavy`. **Order:** a release
   lock, then at most one merge lock, then `goals-heavy`, then the gated repo's own heavy lock; never two
   programs' heavy locks at once; never a shared lock while holding a heavy one. A holder that shows no
   progress for 1800 s (a computing gate is progress) is stopped by the supervisor, ledgered, and its
   goal retries. `timeout 10800 flock -o /cache/locks/holdfast-heavy.lock mise exec go@1.25.14 -- make
   check` keeps working unchanged.
7. **Merges from the goals program.** The goals program merges into holdfast only between holdfast goals,
   in a drain: the supervisor starts no holdfast goal while a goals PR is queued and merges it once none
   is mid-run, with main merged into the branch (never a rebase), `make check` under `holdfast-heavy`, CI
   green (`build`, `package`, `mutation`), and no `Co-Authored-By` or AI trailer.

**Unchanged:** holdfast is under a program hold since the swap, on the owner's words of 2026-10-01
16:03Z that stopped holdfast's goal chain. No holdfast goal starts until the owner releases it in a
session's own chat. This amendment does not release it.

## Left in force (goal files win; goal 1's seam inventory of the goals program)

None of these changes until holdfast finishes or a later amendment says so:

| Goal-file lines | What stays |
|---|---|
| g10-g15 L1 ("Physical steps go on NEEDS-OWNER.md (section 0.6)"); g10-g14 L10, g15 L12; g10 L8; g15 L8, L9, L10 | `.claude/goals/NEEDS-OWNER.md` is holdfast's live list (I1, T42); the goals program mirrors it, never replaces it |
| g10-g14 L13, g15 L15 | the states `NEEDS-OWNER` and `DROPPED` (after the 3 fix rounds) |
| g10-g15 L1 (the gate); g13 L6, L8; g15 L6 | `make check` under `flock -o` on the branch merged up to origin/main, plus green PR CI |
| g10-g14 L9, g15 L11 | gate integrity since each goal's start sha (no deleted test without a reason, no fall in `func Test` counts, no removed line of `docs/design/swap.md` or `quality-gate.md`) |
| g10-g15 L1, g10-g14 L10, g15 L12 | Conventional Commits, no AI trailer, zero `co-authored-by` in `git log <goal-start>..origin/main` |
| g11 L6 | holdfast's own server gate re-run |
