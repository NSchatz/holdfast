# Audio

What holdfast does to a source's audio tracks when a root asks it to, and what an output's audio
must be before it may replace the source. This document is that argument's single home:
the [design index](README.md) names each rule and links here rather than restating it. The keys and their values
are in [`docs/profiles.md`](../profiles.md#audio).

Nothing here relaxes a gate. The audio gates are added to every gate an encode already meets
(`docs/design/swap.md`, `docs/design/quality-gate.md`), on the jobs that transform audio and on no
other; the whole file must still be strictly smaller than its source.

## The rules

<a id="reencode"></a>

**A track is re-encoded only where its root asks, only from a lossless codec, and only into a
layout the configured codec carries as declared.** Every audio key is off by default, and a
configuration that sets none of them derives an audio plan that copies every track: the command
line, the decision and the row are what they were before the keys existed, and no probe or pass
runs. With `audio_reencode: on`, the tracks that qualify are a closed set - TrueHD, DTS-HD Master
Audio (ffprobe's `dts` with a profile beginning `DTS-HD MA`), any `pcm_*`, FLAC - and every other
track is copied with the reason on the row. The layout is named on the command line for every
track (`-ch_layout:a:N`), from an explicit matrix (`internal/audio`, `OutputLayout`); a layout the
codec cannot carry is copied with `layout-not-carried`, never negotiated. The sample rate is
declared too: the source's where the codec takes it, and 48 kHz otherwise.

<a id="loudness"></a>

**Loudness is normalised in two passes to EBU R 128, always resampled to the declared rate, and
the mode that ran is read from the encoder's own report.** The first pass measures the source
track (through the downmix, for a downmix) during the plan's derivation; the second is loudnorm
told those figures and asked for linear mode, followed by `aresample` to the declared rate. The
second pass's report is written to a descriptor of its own per track and read back after the
encode; the row records `linear`, `dynamic`, or `not-recorded` where no report came back. It is
never inferred from the first pass's figures.

<a id="audio-gates"></a>

**A transformed track replaces nothing until its length, channels, layout, sample rate and
loudness are what its plan declares, and every output audio stream decodes in full.** The gates
run on a plan that transforms audio, after the video's decode-integrity check and before the
perceptual gate, and are in the gate vocabulary as `audio` and `loudness` (a decode failure is
`decode`, as the video's is).

## Why only lossless tracks, and why replace by default

A lossless track is the bulk of a film's audio bytes - a TrueHD 7.1 track can be several
gigabytes - and re-encoding it is a transcode from a perfect copy, the one case where the result's
fidelity is bounded by the encoder alone. Re-encoding a lossy track (AC-3, DTS core, AAC) is a
second generation of loss for a fraction of the bytes, so it is not offered. Replace is the
default because a kept original reclaims nothing; `keep_original_audio: true` is the escape hatch
for a library that wants both, and the re-encode is then added after every carried track and is
never the default track.

## Why every layout is named, and 7.1 into AC-3 is copied

The pinned ffmpeg's AC-3 and E-AC-3 encoders list layouts up to 5.1 only
(`ff_ac3_ch_layouts`, libavcodec/ac3enc.c:152-190 at
https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-10-01). Handed a 7.1 source with no
`-ch_layout`, ffmpeg negotiates a fold to 5.1(side) and exits 0 with no warning; told `-ch_layout
7.1`, the encoder refuses (verify-streams-hdr.md claim 8; `TestAudioFFmpeg_SevenOneIntoEAC3FoldsSilentlyUnlessTheLayoutIsNamed`).
A silent fold is a track that lost two channels while every structural gate passed, so the matrix
never asks for it: 7.1 into AC-3 or E-AC-3 copies the track. The layout each codec writes is the
one the pinned ffprobe reads back from both Matroska and MP4 - `5.1` for AAC and Opus (a 5.1(side)
source's side pair is copied to the back pair at unity gain, libswresample/rematrix.c:236-245),
`5.1(side)` for AC-3 and E-AC-3 (a back pair to the side pair likewise, rematrix.c:206-212) - measured
for every cell (`TestAudioFFmpeg_EveryMatrixCellReadsBackAsDeclared`). Only Matroska and MP4
outputs are transformed: they are the two whose read-back was measured, and any other container
copies its audio with `container-not-supported`.

## Bitrates, and where each number comes from

- Opus: stereo 128, 5.1 256, 7.1 450 kb/s, the top of the Xiph music-storage ranges ("96 - 128",
  "128 - 256", "256 - 450"; "Opus at 128 KB/s (VBR) is pretty much transparent"),
  https://wiki.xiph.org/Opus_Recommended_Settings , read 2026-10-01. Mono 64: ASSUMED.
- AC-3: mono 96 and stereo 192 are the pinned encoder's own defaults (libavcodec/ac3enc.c:2242-2246);
  5.1 640, AC-3's ceiling: ASSUMED as the default for a lossless source. AC-3 carries only the
  bitrates in `ff_ac3_bitrate_tab` (libavcodec/ac3tab.c:99-102), 640 the largest, and the encoder
  snaps any other request to the nearest without a word (ac3enc.c:2300-2315), so a configured AC-3
  bitrate outside the table is refused at start.
- E-AC-3: ASSUMED, the AC-3 figures.
- AAC: ASSUMED, 64 kb/s per channel (the FFmpeg wiki's AAC page was unreachable on 2026-10-01).
  The native encoder clamps a frame to 6144 bits per channel (libavcodec/aacenc.c:1364-1365), so a
  configured bitrate past six bits per sample per channel copies the track rather than writing one
  at a bitrate the row would misstate; libopus likewise refuses more than 256 kb/s per channel
  (libavcodec/libopusenc.c:399-402).

All ffmpeg sources at https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-10-01.

## Why the downmix is the default matrix, once per language

The stereo track is folded by swresample's default matrix (`aformat=channel_layouts=stereo`):
centre and surrounds at -3 dB into the fronts, LFE left out, the whole scaled so it cannot clip
(research-streams-hdr.md section 2.5, measured). An explicit `pan` that mixes the LFE in risks
clipping, and the default matrix matches the conventional Lo/Ro fold (ASSUMED: the ATSC A/52
downmix leaves LFE out). Normalising its loudness afterwards recovers the level the matrix gives
up. One downmix is added per language, from the first carried surround track that is not
commentary, and none where a stereo track of that language is already carried: a second stereo
track of the same programme would cost bytes for nothing.

## Why two passes, and why the mode is read back

The targets are EBU R 128's own (EBU R 128-2023, https://tech.ebu.ch/docs/r/r128.pdf , read
2026-10-01): "the Programme Loudness Level shall be normalised to a Target Level of -23.0 LUFS",
and "the True Peak Level of a programme shall not exceed -1 dBTP". R 128 sets no loudness-range
target; loudnorm needs one, and keeps its linear (dynamics-preserving) mode only where the
measured range is at or below it, so it is set at 20 LU (ASSUMED: wide enough for ordinary film).

A single pass runs loudnorm's dynamic mode, which compresses the programme's dynamics and
upsamples to 192 kHz. The second pass is told the first pass's figures and asked for linear mode,
one gain over the whole track, and loudnorm reverts to dynamic on its own where the measured range
is above the target, where the gain would push the true peak past -1 dBTP, or where the measured
range is exactly 0 (libavfilter/af_loudnorm.c:820-822; verify-streams-hdr.md claim 10). Nothing
on the command line can stop that, so `aresample` to the declared rate always follows - a
dynamic pass would otherwise write 192 kHz, and the sample-rate gate would refuse it - and the
mode is read from loudnorm's own report: each normalised track's report goes to its own
inherited descriptor (`stats_file=/proc/self/fd/N`), so it is matched to its track by
construction, nothing is written to disk, no path needs quoting, and the encoder's own log level
and error text are untouched. A report that did not come back is `not-recorded`, never a guess. A
track whose first pass cannot be handed to a second (silence measures -inf) is re-encoded without
normalisation, with `loudness-unmeasurable` on the row.

## The gates, and their tolerances

- **Stream parity learns the additions.** The intended-map gate counts each added track - a kept
  original's re-encode, a downmix - as an audio stream in its source track's language, carrying
  the commentary disposition only where it is a commentary track's re-encode.
- **Codec, channel count and layout equal the plan** (`audio`, deterministic). The count catches
  a fold the layout name alone might not; the layout a re-ordering the count cannot see.
- **Sample rate equal to the plan** (`audio`, deterministic): catches a dynamic loudness pass
  written at 192 kHz.
- **Decoded duration within two frames of the output codec of the source track's**
  (`audio`, deterministic). Both are measured by decoding to the end. Two frames: one for the
  last frame, which an encoder pads to its frame size, and one for priming, which each encoder
  states and which is at most a frame - AAC 1024 samples per frame and 1024 of priming
  (aacenc.c:1574-1575), AC-3 and E-AC-3 1536 and 256 (ac3enc.c:2502-2503), libopus 20 ms frames
  (libopusenc.c:278, :548) and its lookahead, 312 samples here (ASSUMED at most a frame for any
  build). At 48 kHz that is 43 ms for AAC, 64 ms for AC-3 and E-AC-3, 40 ms for Opus; every
  faithful encode measured was out by none, since the demuxers trim the priming, and a truncated
  one is out by far more. Packet counts are not compared: they are a function of each codec's
  frame size (verify-streams-hdr.md claim 9).
- **A full decode of every output audio stream** (`decode`, transient, as the video's decode
  check is): `-xerror`, one stream at a time, copied tracks included on a plan that transforms
  any. A plan that copies all its audio is not held to it - those are the source's bytes, and a
  new decode there would change decisions on configurations that never asked for audio.
- **Integrated loudness within 1.0 LU of -23 LUFS** where the track was normalised (`loudness`,
  deterministic), measured on the output track by the same meter the first pass used. R 128
  permits +-1.0 LU "where attaining the Target Level is not achievable practically"; a lossy
  encode after the normalisation is that case.

A figure that could not be established (a source track that would not decode for its length, a
measurement that returned no report) refuses the output as transient: the source is kept, and
the next attempt measures again.

<a id="which-files"></a>

## Which files the audio keys reach

**The audio keys act only inside a job that already encodes or remuxes the file.** They add
operations to that job's plan; they never start a job of their own. A file the guards skip -
already in the target codec, under `min_bitrate_kbps`, or any other skip - keeps its audio as it
is, whatever these keys say. That includes a `remux_only` root: its remux is a job only for a
file the guards let through, and the codec guard skips a file already in the target codec before
any remux is considered. So in this build a library already in the target codec gets no audio
rework at all; an audio-only job for such files would be a new kind of job, not a reading of
these keys.

No terminal row records these keys (`docs/requeue.md`): a `done` row records the video settings the
encode was taken under, and its file is now in the target codec, so turning a key on later does
not re-open it, and `holdfast requeue` would only bring it back to the codec guard that skips it.
Turning the keys on changes what the next jobs do, not what the past ones did.

A downmix and a kept re-encode carry their source track's title tag unchanged (holdfast writes no
title); a player names them by language, layout and codec. A title that named the source's
layout now describes the source track, not the added one.
