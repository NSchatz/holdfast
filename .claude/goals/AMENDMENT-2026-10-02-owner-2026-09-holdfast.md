# Amendment to the 2026-09-holdfast program: onto spec v1.1, the brief compacted, the goal files off NEEDS-OWNER.md (the owner, 2026-10-02)

The owner decided this on 2026-10-02, in the goals session's own chat. After an audit of every repo
against the goal-process spec (spec v1.1), the owner wrote **"Everything needs to get ported over"**.
Asked how far to go for the programs with goals left, the owner chose **"Swap + compact briefs"**:
the swap ("change only the lines that name [the old lists] to the spec's issue wording; every other
line word for word; recorded as an amendment [...] pinning spec 1.1")
"plus rewrite each running brief onto the spec: drop its copied contract and keep only its own
parameters (lint's 'planned' profile)". That session wrote this change, and it merges into holdfast during a
drain of this program. The owner holds the program; goal 9 is complete and no goal is running. These
are the owner's later words, so they beat the brief and the earlier amendments where they disagree
(spec amendment.md, "Precedence"). Unlike those amendments, this one changes the remaining goal files
themselves, by the owner's choice.

- **Spec version:** 1.1. The pin is the new manifest `2026-09-holdfast.lanes.toml`, whose only key is
  `spec = "1.1"`; `goals lint` reports "spec 1.1 planned".
  - The manifest has no `[[goal]]` entries, so the goals still run in order, goal n after goal n-1.
  - It has no `[checkpoints]`: Checkpoint T and its approval file are unchanged.
- **In force for:** goals 10 to 15, whose ledgers are created after this merged.

## What changes

1. **The brief references the spec instead of copying the standing contract.**
   - It has a `**Spec:**` line, and the introduction carries the spec's must-read paragraph.
   - §0 is "Program parameters". It keeps this program's own rules, with every `### 0.x` number and
     every T and I citation, among them:
     - the gate (`make check` under `holdfast-heavy`, plus CI green);
     - branch updates by merging main;
     - the 3 fix rounds and DROPPED;
     - Conventional Commits with no trailer;
     - the agent budget and GOFLAGS/GOMAXPROCS;
     - the hardware and outward-facing limits;
     - the identity rules;
     - scope;
     - the environment table.
   - §21, the report formats, points at spec report.md and keeps this program's own words.
   - §1-§13, §20, §22 and §23 are byte for byte unchanged.
2. **The state word stays this program's own.** NEEDS-OWNER is this program's word for the spec's
   state for a step physically impossible for an agent. holdfast files no requests between programs. "The owner's queue" is the
   issues in the goals program's private repository (`goals needs add --repo holdfast`,
   `/goals:needs`). Its label is never written here.
3. **§0 and the remaining goal files name no private repository.** Spec contract §6 wins over the v1
   review's L9 since the spec pin; §1-§3, §5 and §23 keep their wording byte for byte. Access stays
   exactly as T32 decided:
   - the owner's private umbrella repository: read only, never branched, PR'd or pushed. A goal that
     needs a spec's full text reads goal 1's read-only clone, and a fresh clone gets its push URL
     disabled first;
   - the owner's private homelab repository: PRs only, never merged by an agent.
4. **Rules the owner's port retires, or the spec settles, are gone from §0:**
   - "§0-§4 win";
   - reading NEEDS-OWNER.md at a goal's start, which is now `goals needs list`;
   - NEEDS-OWNER.md as the live list (T42, I1), and T6's own list where W21's queue supersedes it;
   - one heavy job at a time across the container. Inside holdfast it still holds, because
     `holdfast-heavy` is exclusive; a heavy job now holds `goals-heavy`, then `holdfast-heavy`;
   - the copied report formats.
   I20 is narrowed to "no issue in this repo": items in the owner's queue are the only issues a goal
   files.
5. **Goal files 10 to 15.**
   - Each reads "spec v1.1 (the /goals:spec skill), `.claude/goals/2026-09-holdfast.md` §0, §4 and
     §<k>" instead of §0-§4.
   - Physical steps go on the owner's queue.
   - The private repositories are named by role, as above.
   - Every other line keeps its meaning. Goals 1 to 9 are unchanged.
6. **NEEDS-OWNER.md** stays as it is, with its open rows (each already an item in the owner's queue),
   until this change merges. Then no remaining goal file names it as the place to write, it leaves
   `named_by` in the goals program's `oldlists.toml`, and `goals needs pointer` allows it to become a
   pointer to the owner's queue. §0.3's straight-to-main permission covers that pointer and its
   generated mirror.

## What it supersedes

The "Left in force (goal files win)" lists of the goals-g5, g6, g8, g9 and g10 amendments, where they
quote goal-file wording this change replaced, among them g6's "each goal file's first line, which
reads the brief's §0-§4 in full", and "Physical steps go on NEEDS-OWNER.md", "NEEDS-OWNER.md
is current", g10's live-check commands in NEEDS-OWNER.md, and g15's NEEDS-OWNER entry. The
NEEDS-OWNER state word they left in force stays. Their other rules stand, and their top notes stay in
the brief.

## The lettered lines changed (goal, letter)

- **g10 E:** "the owner's queue has live-check commands for Plex, Sonarr and Radarr".
- **g10-g14 G and g15 I:** "the owner's queue is current".
- **g15 E:** "its item in the owner's queue".

## Measured

- `goals lint` on the branch: "spec 1.1 planned, manifest ... 0 failed".
- Must-read per goal (`goals lint --must-read`): goals 10-15 went from 58,236-59,231 bytes to
  44,563-45,558 bytes; the after figure includes the spec's 12,253 bytes.
- The largest goal file is goal 15, at 3,518 characters.
- `identity-scan`: clean.
