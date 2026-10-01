# Goal 7 ledger - audio and subtitles

- Brief: `.claude/goals/2026-09-holdfast.md` §0-§4 and §11; goal file
  `.claude/goals/2026-09-holdfast-g7.goal.txt`. Amended by `.claude/goals/CHECKPOINT-T.approved`
  (no amendment names goal 7; P1 approved as proposed).
- Started: 2026-10-01, from `/workspace/holdfast` in the maker container.
- Goal-start SHA: `3df61626c42cfa19f5faabdf0c794f5a2aa1c55e` (`git rev-parse origin/main` before
  any work).
- Earlier ledgers: `2026-09-holdfast-g1.status.md` to `2026-09-holdfast-g6.status.md` (ends
  `COMPLETE (goal 6): 2026-10-01`). `NEEDS-OWNER.md` at start: rows 1-7, all `OPEN`.
- Precondition, as checked:

```
$ git fetch -q origin && git log -1 --format='%H %s' origin/main
3df61626c42cfa19f5faabdf0c794f5a2aa1c55e chore(goals): goal 6 ledger - the final review's corrections, main CI green on 400ff42
$ git show origin/main:.claude/goals/2026-09-holdfast-g6.status.md | command grep -n 'COMPLETE (goal 6)'
261:COMPLETE (goal 6): 2026-10-01
$ echo "$GOFLAGS $GOMAXPROCS"
-p=4 12
```

## What the approval assigns to this goal

P1 (`.claude/goals/2026-09-holdfast-research/proposal-triage.md` at `d4d70a6`, line 60, "Per
goal"): "**Goal 7** (audio and subtitles): none." `CHECKPOINT-T.approved` approves P1 as proposed
and amends no goal-7 row.

## Baselines

Measured at the goal-start SHA in a detached worktree (`/cache/wt/holdfast/g7-baseline`), with
`TMPDIR=/cache/tmp/holdfast-g7/tmp`. Logs under `/cache/tmp/holdfast-g7/`.

| Gate | Value at goal start | Wall-clock |
|---|---|---|
| `make check` under `flock -o` (log `gate-baseline.log`) | running | - |
| `func Test` count, all packages | 1463 in 28 packages (`functest-start.txt`) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 62, 76 | - |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`3df6162`): goal 6's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`ec02306`) |
| 1.3 | Baselines with timings | DOING |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | P1 rows for goal 7 | DONE (`d4d70a6`): P1 assigns none |

## Phase 3 - Audio (lines C, D)

| # | Item | State |
|---|---|---|
| 3.1 | Audio keys, off until configured; re-encode lossless or bulky tracks per an explicit codec and layout matrix, replace by default, `keep_original_audio` keeps both | TODO |
| 3.2 | Added stereo downmix track | TODO |
| 3.3 | Two-pass EBU R128 loudness, `aresample` to the source rate, the mode that ran recorded | TODO |
| 3.4 | Audio gates (duration, channel layout, sample rate, full decode, loudness), one red fixture each; stream-count parity learns the declared changes | TODO |
| 3.5 | Keys unset: argv and decisions equal the pre-goal ones (golden) | TODO |

## Phase 4 - Subtitles (line E)

| # | Item | State |
|---|---|---|
| 4.1 | SubRip, ASS, WebVTT to `<name>.<lang>[.forced].<ext>` sidecars, exclusive create; bitmap and mov_text skipped with a reason; ASS fonts logged | TODO |
| 4.2 | Sidecar parse-back event-count gate, red on a truncated sidecar | TODO |

## Phase 5 - Docs (line F)

| # | Item | State |
|---|---|---|
| 5.1 | R1 rewritten in `README.md` and `docs/migration.md`; `docs/design/audio.md`, `docs/design/subtitles.md` anchored; `docs/profiles.md`, `config.example.yaml` keys | TODO |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | TODO |
| 6.2 | Adversarial review of the report | TODO |

## Decisions taken

- 2026-10-01: every gate in this goal runs with `TMPDIR` under `/cache/tmp/holdfast-g7/` (a tmpfs
  `TMPDIR` is refused by `fsclass`), and goal shells use `command grep`, `rg` or `git grep`.
- 2026-10-01: slip, recorded. The ledger's first commit `ec02306` carries a `Claude-Session:`
  session-link line (the session's attribution reminder), against T38 (no AI trailer in commits;
  `CLAUDE.md` wins). It is not a `Co-Authored-By` line. History is never rewritten (T7), so it
  stays; no later commit of this goal carries one.
- 2026-10-01: tracks. `holdfast-g7/audio` (re-encode, downmix, loudness, the audio gates, R1's
  rewrite, `docs/design/audio.md`) and `holdfast-g7/subtitles` (sidecars, their gate,
  `docs/design/subtitles.md`), each built by one agent in its own worktree, reviewed here, and
  merged by the rule of §0.3; the second to land merges `origin/main` and re-gates. Reasoning: T52
  batching; the two touch the same config, store and engine seams but different stages (the
  encode's argv and gates; a post-swap step).
- 2026-10-01: audio design (reasoning to live in `docs/design/audio.md`). Every key is per root
  with a top-level default, digest-silent at its default, and off until configured (I5):
  `audio_reencode` (`off`|`on`), `audio_codec` (`aac`, `ac3`, `eac3`, `opus` = libopus; required
  by a re-encode or a downmix), per-layout bitrates (Opus from the Xiph recommendation, the rest
  `ASSUMED`), `keep_original_audio` (false), `audio_downmix` (`off`|`stereo`), `audio_loudness`
  (`off`|`ebu_r128`, targets fixed to EBU R128). The re-encoded source codecs are a closed set:
  TrueHD, DTS-HD MA (by ffprobe profile), PCM, FLAC. A layout the configured codec cannot carry
  (7.1 into AC-3/E-AC-3) leaves that track copied with a recorded reason, never auto-negotiated;
  `-ch_layout` and the sample rate are set explicitly per output stream. The downmix is added per
  carried non-commentary track of more than two channels, unless a two-channel track of the same
  language is already carried. Two-pass loudness on added and re-encoded tracks only, `aresample`
  after it; the mode that ran is read from the encoder's own report, never inferred, and an
  unobserved mode is recorded as not recorded. With every key unset the plan's audio is copy and
  no new probe runs, so argv and decisions are unchanged (line D).
- 2026-10-01: audio gates run only on a plan that transforms audio: per transformed or added
  output stream, decoded duration within a stated tolerance of its source track, channel count and
  layout and sample rate equal to the plan, and post-encode integrated loudness within tolerance of
  the target; a full decode of every output audio stream. Stream-count parity learns the declared
  additions. A full decode of every audio stream is not added to plans that copy audio (that would
  change decisions on existing configurations, against I5).
- 2026-10-01: subtitle design (reasoning to live in `docs/design/subtitles.md`). One key per root,
  `subtitle_sidecars` (`off`|`text`), off by default. Only the carried (intended) subtitle streams
  are considered; SubRip, ASS and WebVTT are stream-copied into their native format; `<name>` is
  the replacement's own stem, `<lang>` the stream's language tag as the source wrote it (lowercased;
  untagged or `und` writes `und`), `.forced` from the forced disposition. A name already on disk
  or already chosen for an earlier stream of the same job is skipped with a reason; never
  overwritten (exclusive create or link). Bitmap codecs and mov_text skip with a reason. Sidecars
  are published only after the swap commits; a sidecar that fails its parse-back count gate, or
  cannot be written, is not published and is recorded, and does not fail the job or block the
  swap: it is an added convenience copy of a stream the replacement still carries, and the swap's
  gates are about the replacement.

## Resume here

Ledger created; baseline gate running. Next: read the encode plan, the argv builder and the gates,
then plan the audio and subtitle tracks.
