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
| `make check` under `flock -o` (log `gate-baseline.log`) | exit 0 | 2002 s (33m22s) |
| `internal/engine` under `go test -race` (from that run) | ok, 88.1% coverage | 1887.9 s (70% of `TEST_TIMEOUT` 45m) |
| `cmd/holdfast` under `go test -race` (from that run) | ok, 89.2% coverage | 667.0 s |
| `func Test` count, all packages | 1463 in 28 packages (`functest-start.txt`) | - |
| `docs/design/swap.md`, `docs/design/quality-gate.md` lines (`wc -l`) | 62, 76 | - |

## Phase 1 - Start-up

| # | Item | State |
|---|---|---|
| 1.1 | Precondition checked (header above) | DONE (`3df6162`): goal 6's COMPLETE line is on `origin/main` |
| 1.2 | Ledger created as the goal's first commit, straight to `main` | DONE (`ec02306`) |
| 1.3 | Baselines with timings | DONE (`3df6162`): the table above; `make check` exit 0 in 2002 s |

## Phase 2 - Triage (line B)

| # | Item | State |
|---|---|---|
| 2.1 | P1 rows for goal 7 | DONE (`d4d70a6`): P1 assigns none |

## Phase 3 - Audio (lines C, D)

| # | Item | State |
|---|---|---|
| 3.1 | Audio keys, off until configured; re-encode lossless or bulky tracks per an explicit codec and layout matrix, replace by default, `keep_original_audio` keeps both | DONE (PR #136, `a530a56`): `audio_reencode`, `audio_codec`, `audio_mono_kbps`/`audio_stereo_kbps`/`audio_51_kbps`/`audio_71_kbps`, `keep_original_audio`, `audio_downmix`, `audio_loudness`; `internal/audio`; `TestAudio_ReencodeReplacesTheTrackByDefault`, `TestAudio_KeepOriginalKeepsBothTracks`; gate exit 0 on the branch with `eb424d5` merged (run 1966 s, `internal/engine` 1853.8 s, 69%); mutation-diff 100% (191 killed, 0 lived); CI green |
| 3.2 | Added stereo downmix track | DONE (PR #136): `TestAudio_StereoDownmixIsAdded` |
| 3.3 | Two-pass EBU R128 loudness, `aresample` to the source rate, the mode that ran recorded | DONE (PR #136): `TestAudio_TwoPassLoudnessRunsLinearAndIsRecorded`, `TestAudioFFmpeg_TwoPassLoudnessRunsLinearAndMeetsR128`, `TestAudioFFmpeg_ADynamicFallbackIsReportedAndKeepsTheDeclaredRate`; the mode is read from each loudnorm's own report on an inherited descriptor (`stats_file`), the log level unchanged |
| 3.4 | Audio gates (duration, channel layout, sample rate, full decode, loudness), one red fixture each; stream-count parity learns the declared changes | DONE (PR #136): `TestAudioGate_DurationReds`, `TestAudioGate_ChannelLayoutReds`, `TestAudioGate_SampleRateReds`, `TestAudioGate_FullDecodeReds`, `TestAudioGate_LoudnessReds` (each with the source byte-identical), control `TestAudioGate_LossyControlPasses`; `StreamPlan.CheckOutputAdding`; gate members `audio`, `loudness` |
| 3.5 | Keys unset: argv and decisions equal the pre-goal ones (golden) | DONE (PR #136): `TestAudio_KeysUnsetChangeNoArgvNoDecisionAndRunNoProbe`, `TestGoldenArgv` (every existing golden file byte-identical; `engine-cpu.txt` gains 8 appended `audio/*` cases) |

## Phase 4 - Subtitles (line E)

| # | Item | State |
|---|---|---|
| 4.1 | SubRip, ASS, WebVTT to `<name>.<lang>[.forced].<ext>` sidecars, exclusive create; bitmap and mov_text skipped with a reason; ASS fonts logged | DONE (PR #135, `eb424d5`): `subtitle_sidecars: off|text`, `internal/subtitle`; extracted from the source after the gates to `<working file>.subtitle<n>` temps beside it, published by `link(2)` only after the swap commits; `TestSidecar_SRTASSWebVTTNamedByLanguageAndForced`, `TestSidecar_EngineWritesSidecarsBesideTheReplacement`, `TestSidecar_ExistingSidecarIsNeverOverwritten`, `TestSidecar_ANameTakenByAnEarlierStreamIsSkipped`, `TestSidecar_BitmapSkipsWithItsReason` (stream-list entries: the pinned ffmpeg cannot make a bitmap subtitle), `TestSidecar_MovTextSkipsWithItsReason` (a real MP4), `TestSidecar_EngineKeyUnsetWritesNothing`; gate round 1 exit 0 (run 1887 s, `internal/engine` 1773.3 s, 66%); mutation-diff 100% (43 killed, 0 lived); CI green |
| 4.2 | Sidecar parse-back event-count gate, red on a truncated sidecar | DONE (PR #135): `TestSidecar_TruncatedSidecarFailsTheCountGate`, `TestSidecar_EngineTruncatedSidecarIsRefusedAndTheSwapStands` (nothing published, no temp left, the swap stands) |

## Phase 5 - Docs (line F)

| # | Item | State |
|---|---|---|
| 5.1 | R1 rewritten in `README.md` and `docs/migration.md`; `docs/design/audio.md`, `docs/design/subtitles.md` anchored; `docs/profiles.md`, `config.example.yaml` keys | DONE (PRs #136, #135, #137 `df90fbb`): `README.md:116` "Audio is re-encoded on request, and copied otherwise.", `docs/migration.md` "Filters as a pipeline" lists the audio keys as on-request transformations; anchors `audio.md#reencode`, `#loudness`, `#audio-gates`, `#which-files`, `subtitles.md#sidecars`, `#sidecar-gate`, `#which-files`; CLAUDE.md links each rule (189 lines); the stream-selection docs check changed with its bite case (`internal/startup/stream_docs_test.go`) |

## Phase 6 - Report

| # | Item | State |
|---|---|---|
| 6.1 | Gate integrity counted from the goal-start SHA | DONE (counted at `df90fbb`): `func Test` 1463 -> 1542, no package fell (`internal/audio` 0 -> 30, `internal/subtitle` 0 -> 19, `internal/config` 125 -> 131, `internal/engine` 504 -> 521, `internal/store` 149 -> 154, every other package unchanged); `docs/design/swap.md` 62 and `docs/design/quality-gate.md` 76 lines, `git diff --numstat 3df6162 df90fbb` empty for both; 16 lines deleted in `*_test.go` (+2974 -16), each with its reason below |

### The 16 deleted `*_test.go` lines and why

Each tracks the newest store migration, which this goal moved twice (v21 `subtitle_sidecars`, #135;
v22 `audio_tracks`, #136); the wind-back fixtures exist to undo exactly the newest step.

- `cmd/holdfast/export_test.go` (6): the comment naming the newest step, `olderSchemaVersion`
  19 (now 21), and the four `DROP COLUMN` lines of v20, replaced by the one of v22.
- `internal/store/readonly_test.go` (5): the same comment and the same four `DROP COLUMN` lines.
- `internal/store/migrate_test.go` (1): `newestStepColumn` `downscaled` -> `audio_tracks`.
- `internal/store/resolution_migrate_test.go` (3): the forced half-applied step pre-adds the newest
  column (`audio_tracks`, not `downscaled`); the "`downscale_scaler` left behind" check named a
  second column of v20, which the one-column v22 has none of. It is replaced by a check reading
  every further column off the newest step's own SQL (none today), plus "the pre-added column
  survives"; the unmoved stamp and row count still prove the rollback.
- `internal/startup/stream_docs_test.go` (1): a doc comment that described the stream-selection
  statement as calling audio transcoding a non-goal; that statement and its check changed together
  (R1), with a bite case proving the old wording now fails. |
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

- 2026-10-01: PR #135 (subtitles) merged as `eb424d5`. Its own decisions: a language tag must match
  `^[a-z]{2,3}(-[a-z0-9]{1,8})*$` or the stream is skipped `language-tag-not-a-name` (no tag
  becomes a path segment); a name is taken once computed even if the earlier sidecar later fails;
  sidecars are recorded on `done` rows only; a filesystem without hard links records
  `sidecar-publish-failed`; store schema v21. Its agent once ran `go test ./cmd/holdfast/` outside
  the heavy lock, then re-ran it under the lock (a slip, recorded).

- 2026-10-01: PR #136 (audio) merged as `a530a56`. Its own decisions: `EncodePlan.Audio` stays the
  blanket `copy` and a new `AudioTracks` declares each track's operation; bitrates are four flat
  keys (a `5.1` map key collides with koanf's delimiter); an AC-3 bitrate must be one of AC-3's
  own rates, and one past a codec's ceiling copies the track `bitrate-beyond-codec`; only Matroska
  and MP4 outputs transform audio (others copy, `container-not-supported`); one downmix per
  language from its first carried non-commentary surround track; -23 LUFS and -1 dBTP cited from
  EBU R 128-2023 with the gate at its +-1.0 LU, the LRA target 20 LU `ASSUMED`; a silent track is
  re-encoded without normalisation (`loudness-unmeasurable`); a failed first pass refuses the
  plan; duration tolerance two codec frames, cited from the ffmpeg sources; Opus and AC-3 mono and
  stereo bitrates cited, the rest `ASSUMED` (the FFmpeg wiki's AAC page returned 403). Store
  schema v22, after #135's v21.
- 2026-10-01: review finding on #136, fixed in a follow-up (`holdfast-g7/docs-scope`): the audio
  keys and `subtitle_sidecars` act only inside a job the guards let through and are recorded on no
  row, so a file already in the target codec gets no audio rework and no sidecar - `remux_only`
  included, since the codec guard runs before any remux. The docs now say so (`#which-files` in
  both design files, `docs/requeue.md`). Listed for the owner under proposals.

- 2026-10-01: PR #137 (docs-scope) merged as `df90fbb`. Gate round 1 exit 2: `TestServeSmoke`
  missed its 3 s readiness wait under the gate's parallel load and then passed 5/5 alone under the
  lock; round 2 unchanged exit 0 (762 s; `internal/engine` from Go's test cache, identical inputs to
  round 1 where it passed in 1924.7 s, 71%); CI green. Not fixed: the flake is in an existing test
  outside this goal's scope; recorded for the owner.
- 2026-10-01: `NEEDS-OWNER.md` unchanged by this goal: audio and sidecars need no real hardware and
  no live service, so no step here is physically impossible for an agent. No minor release is cut
  (T37 optional): the features are new and off by default, and a later goal can release them.

## Proposals awaiting the owner

- A file already in the target codec gets no audio rework and no sidecar, `remux_only` included
  (the codec guard runs first). An audio- or sidecar-only job for such files would be a new kind of
  job; it is not built (`docs/design/audio.md#which-files`).
- The downmix carries its source track's title; holdfast writes no titles.
- `TestServeSmoke`'s 3 s readiness wait can miss under gate load (seen once in this goal).

## Resume here

All PRs merged: #135, #136, #137. No branch, worktree or open PR of this goal remains but the
ledger worktree. Next: the fresh adversarial review of the GOAL REPORT (row 6.2), then the COMPLETE
line.
