# Dynamic HDR

What holdfast does with a Dolby Vision or HDR10+ source, and the argument behind the rule
`CLAUDE.md` states about it. This document is that argument's single home: `CLAUDE.md` names
the rule and links here rather than restating it. The code is `internal/dynhdr` (the decision,
the tools, the pre-pass and the gate arithmetic) and the engine's guard, pre-pass and gate
(`internal/engine/dynamichdr.go`).

## The rule

<a id="dynamic-hdr"></a>

**Dynamic HDR metadata is carried only through libx265, and an output carrying it replaces its
source only when its DOVI configuration record names the planned profile and compatibility id
and every one of its frames carries the RPU and the HDR10+ metadata its plan declares.** Every
other Dolby Vision or HDR10+ source - another encoder, a remux-only root, a profile this build
does not carry, metadata it cannot read, a tool it cannot find - is skipped by name, and never
encoded flat.

Why the skip existed, and why it is lifted only this far. A generic re-encode strips a Dolby
Vision RPU and HDR10+ SMPTE ST 2094-40 metadata, and the loss is invisible until somebody
watches the file on a display that would have used it. None of holdfast's other gates can see
it: the perceptual gate compares pixels, and an RPU is not pixels; the fidelity gate checks the
HDR10 static blocks, which survive. So carrying dynamic metadata is worth exactly as much as the
gate that proves it was carried, and the rule lifts the skip for the cases where both the
carriage and the proof were measured on this build, and nowhere else.

## What is carried

| Source | Encoder `cpu` (libx265) | Every other encoder, and a remux-only root |
|---|---|---|
| Dolby Vision profile 8.1 (compatibility id 1, an HDR10 base layer) | carried, by default (brief I6) | skipped, `dolby-vision` |
| Dolby Vision profile 7 | `dolby_vision_p7: skip` (default): skipped, `dolby-vision-profile-7`; `convert`: converted to 8.1 and carried | skipped, `dolby-vision` |
| Dolby Vision profile 5, 8.2, 8.4, 4, 9, 10, or a record that cannot be read | skipped, `dolby-vision` | skipped, `dolby-vision` |
| HDR10+ | carried, by default (brief I6) | skipped, `hdr10-plus` |
| Dolby Vision 8.1 (or converted 7) and HDR10+ together | both carried, both gated | skipped, `dolby-vision` |

<a id="profile-8"></a>

**Profile 8.1.** libx265 codes the RPU through ffmpeg's own Dolby Vision support (FFmpeg commit
39ca87ed1e, "avcodec/libx265: implement dolby vision coding",
https://github.com/FFmpeg/FFmpeg/commit/39ca87ed1ef876af9622a5aa331e18167fdfdf27 , read
2026-10-01): the decoder exports each frame's RPU as side data and the encoder re-codes it, one
per frame. Three things are on the command line, each because the encode is otherwise wrong:

- `-dolbyvision 1`, explicitly. The option's default, `auto`, never codes an RPU from the ffmpeg
  command line: the wrapper reads the metadata only from the encoder's global side data, which
  the command line fills only from side data flagged global, and the RPU is not
  (`libavcodec/dovi_rpuenc.c` line 81, `fftools/ffmpeg_filter.c` about lines 2261-2272 and
  `libavutil/side_data.c` line 45 at the pinned 5d4d3bdc61; verify-streams-hdr.md claim 3,
  measured: `auto` gave 0 of 24 frames an RPU). The x265 CLI's own `dolby-vision-rpu` is never
  passed: ffmpeg's wrapper ignores it with a warning and exit 0 (claim 4), so it would be a
  carriage nobody performed. The gate, not the option, is what proves the carry.
- <a id="vbv"></a>A VBV ceiling (`vbv-maxrate`, `vbv-bufsize`). x265 refuses to open a Dolby
  Vision encode without one ("Dolby Vision requires VBV settings to enable HRD.", exit 183).
  holdfast's encodes are quality-targeted, so the ceiling is DERIVED, never chosen: the maximum
  bit rate and CPB size of the lowest HEVC level whose luma picture size and luma sample rate
  admit the source (ITU-T H.265 Annex A, Table A.8), high tier from level 4 up and main tier
  below, as x265 carries the table in `source/encoder/level.cpp`
  (https://bitbucket.org/multicoreware/x265_git/raw/master/source/encoder/level.cpp , read
  2026-10-01). That is the largest ceiling a decoder conforming to the level must accept, so it
  constrains peaks only where a conforming stream is constrained. 1080p24 gets level 4's 30000
  kbit/s; 2160p24 level 5's 100000. A source past level 6.2 (beyond any Dolby Vision level) or
  whose rate cannot be read is skipped, `dolby-vision-frame-rate`. The worked examples are
  `TestVBVFor_TakesTheLowestAdmittingLevelsCeiling`.
- The mastering display, which x265 refuses profile 8.1 without ("Dolby Vision profile - 8.1
  requires Mastering display color volume information", exit 183). The encode already writes the
  source's HDR10 block (`hdr.Color.X265Params`); a source carrying none is skipped,
  `dolby-vision-no-mastering-display`, because there is nothing true to write.

The RPU's compatibility id is coded from the colour the encode writes (`dovi_rpuenc.c` lines
140-153): BT.2020 primaries and matrix with PQ is id 1. A profile 8.1 source whose colour would
code anything else is skipped rather than relabelled.

<a id="profile-7"></a>

**Profile 7, on request.** A profile 7 stream carries an enhancement layer libx265 cannot code
("Coding of Dolby Vision enhancement layers is currently unsupported", `dovi_rpuenc.c`), so it is
converted to 8.1 first, and only where the root says `dolby_vision_p7: convert` (brief T25, I5:
off until configured), because the conversion discards the enhancement layer: what the
replacement carries is the base layer and a rewritten RPU. The pre-pass is dovi_tool's own
documented pipe, `ffmpeg -i <source> -map 0:v:0 -c copy -bsf:v hevc_mp4toannexb -f hevc - |
dovi_tool -m 2 convert --discard - -o <working>.dynhdr1`
(https://github.com/quietvoid/dovi_tool/blob/2.3.4/README.md , read 2026-10-01). Mode 2 "Converts
the RPU to be profile 8.1 compatible. Removes luma/chroma mapping for profile 7 FEL"; mode 5,
which keeps the mapping, is never used: a mapping made for base plus enhancement layer applied to
the base layer alone is the reasoning (LEAD, community consensus; the verifier found nothing
primary calling mode 5 wrong, verify-streams-hdr.md claim 5), and mode 2 is the tool author's
default. Whether the layer was FEL or MEL is logged, read off `dovi_tool info -s` on the RPUs of
the source's first 48 frames (`Profile: 7 (FEL)`; dovi_tool 2.3.4 `src/dovi/rpu_info.rs` lines
240-254 and `dolby_vision/src/rpu/rpu_data_nlq.rs` lines 16-17 and 188-194).

The encode then reads the converted stream as a second input, `-f hevc -framerate <the source's
exact rate> -i <working>.dynhdr1`, mapped in the source's video's place (`-map 1:v:0`), with every
other stream mapped from the source as the intended map says. Raw HEVC carries no timestamps, so a
source whose `r_frame_rate` and `avg_frame_rate` differ (variable rate, or a rate ffprobe could not
tell), or whose video does not start at 0, is skipped, `dolby-vision-frame-rate`: it would come out
retimed or shifted against its audio. Metadata is always injected during the encode and never
after it: raw HEVC cannot be stream-copied into Matroska with this ffmpeg ("Can't write packet with
unknown timestamp", verify-streams-hdr.md claim 6), and the mp4 hop that seems to work corrupts
B-frame timing.

The converted stream is a working file beside the job's working file (`<working>.dynhdr1`, under
the temp marker), and the per-job space check counts it: a converting job reserves twice its
source's size. It is removed on every way out of the job. A conversion that fails is skipped,
`dolby-vision-conversion-failed`.

A synthetic profile 7 source cannot be built: dovi_tool 2.3.4 generates profile 5, 8.1 and 8.4
RPUs and never 7 (proposal-crop-dv.md, "Profile 7 limit"). The fixtures therefore drive the
conversion with a source whose configuration record PROBES as profile 7 (enhancement layer
flagged, compatibility id 6) over 8.1 RPUs, through the real dovi_tool, ffmpeg and gates
(`TestDynamicHDR_Profile7IsSkippedByDefaultAndConvertedWhenOptedIn`,
`TestRealFixture_Profile7IsConvertedTo81`). What a real dual-layer profile 7 stream's enhancement
layer does to the conversion is dovi_tool's, and is not measured here (ASSUMED from its README).

<a id="hdr10-plus"></a>

**HDR10+.** ffmpeg does not carry 2094-40 through libx265 on its own (verify-streams-hdr.md claim
2), so it is extracted first with hdr10plus_tool's documented pipe, `ffmpeg ... -c copy -bsf:v
hevc_mp4toannexb -f hevc - | hdr10plus_tool extract -o <working>.dynhdr0.json -`
(https://github.com/quietvoid/hdr10plus_tool/blob/1.7.2/README.md , read 2026-10-01), validated,
and handed to x265 as `dhdr10-info` (https://x265.readthedocs.io/en/master/cli.html#cmdoption-dhdr10-info
, read 2026-10-01). Validation is that the file parses as the tool's JSON and carries exactly one
`SceneInfo` entry per source frame (the source's packet count, counted by demuxing): fewer would
leave frames without metadata, more would shift every entry after the first extra one. A source
whose metadata cannot be extracted or does not validate is skipped, `hdr10-plus-unreadable`.

The file name ends in `.json` because x265 reads `dhdr10-info` from no other: on any other
extension it prints "Fail open file, extension not valid!" and the encoder process ABORTS
(`source/dynamicHDR10/JsonHelper.cpp` lines 124-131,
https://bitbucket.org/multicoreware/x265_git/src/master/source/dynamicHDR10/JsonHelper.cpp , read
2026-10-01; the abort measured on the pinned build). Its path is escaped for `-x265-params`, which
ffmpeg splits on `:` and `=` with `av_get_token`'s rules, so a library directory named with a colon
stays one path (`TestRealFixture_HDR10PlusIsCarriedAndItsGateRedsWithoutIt` writes it under
`work: a=b`).

<a id="both"></a>

**Both.** A source carrying Dolby Vision and HDR10+ is carried with both: x265 codes the RPU and
the 2094-40 SEI on the same frames, and both gates pass on the same output (measured,
`TestRealFixture_DolbyVisionAndHDR10PlusTogetherAreBothCarried`). Skipping it would have been the
fail-safe answer had either gate failed; neither does.

## The gates

<a id="gates"></a>

**An output carrying dynamic HDR replaces its source only when:**

1. its DOVI configuration record is present and readable, names profile 8 and compatibility id 1,
   and flags an RPU and a base layer and no enhancement layer (`dolby-vision-record`);
2. every one of its decoded frames carries a Dolby Vision RPU (`dolby-vision-rpu`);
3. every one of its decoded frames carries HDR10+ metadata, where the plan carries HDR10+
   (`hdr10-plus`).

They join the gate list by addition, after the decode-integrity check and before the audio and
perceptual gates (`verifyAgainst`, 6a), and only on a plan that carries dynamic HDR: every other
job is held to exactly the gates it was before. A record or a count that differs is a
deterministic rejection; counts that could not be taken are transient, as the decode gate's are.
The source is kept either way.

How the frames are counted, and why not with the tools. `ffprobe -show_frames` on the output's
video, counting each frame once that carries `Dolby Vision RPU Data` or `HDR Dynamic Metadata
SMPTE2094-40 (HDR10+)` side data. That side data is what libavcodec's HEVC decoder exports for an
RPU NAL and a 2094-40 SEI it parsed, so the count is the metadata a player's decoder sees,
measured by a second implementation rather than by the tool whose output it checks; and the total
comes from the same decode, so "every frame" is one measurement. It costs one decode of the video,
as the decode-integrity gate does. The record is read from the output's stream side data by the
same probe the fidelity gate takes.

One fixture per gate reds when its metadata is dropped, with the source byte-identical:
an encode without `-dolbyvision 1` has no record
(`TestRealFixture_Profile81IsCarriedAndItsRecordGateRedsWithoutIt`, and through the engine
`TestDynamicHDR_AnEncodeThatDropsTheRPUIsRefusedAndTheSourceKept`); an output whose record names
compatibility id 4 (a profile 8.4 HLG encode) is refused by the record gate
(`TestRealFixture_AnOutputOfAnotherCompatibilityIDIsRefusedByTheRecordGate`); an output with a
profile 8.1 record and an RPU on only half its frames (stripped by the `dovi_rpu` bitstream filter
and concatenated) is refused by the RPU gate
(`TestRealFixture_AnOutputMissingRPUsIsRefusedByTheRPUGate`); an encode without `dhdr10-info` is
refused by the HDR10+ gate (`TestRealFixture_HDR10PlusIsCarriedAndItsGateRedsWithoutIt`).

## The plan

The carriage is part of the encode plan (docs/design/encode-plan.md): the guards decide what is
carried (`dynhdr.Decide`), the pre-pass establishes the ceiling, the metadata file and the
converted stream after the claim, the derivation declares `MetadataPlan.DolbyVision` and
`MetadataPlan.HDR10Plus` from that pre-pass and nothing else, the command-line builder honours
them, and the gate reads the same declaration. A plan declaring either without the pre-pass behind
it, or on a copy, or on any encoder but libx265, is refused before any subprocess runs
(`EncodePlan.buildable`).

## The tools

<a id="tools"></a>

`dovi_tool` and `hdr10plus_tool` are found as ffmpeg is: `HOLDFAST_DOVI_TOOL` and
`HOLDFAST_HDR10PLUS_TOOL` where set, else the binary's own name on `PATH`. Neither is required
to start: profile 8.1 needs neither, HDR10+ needs hdr10plus_tool, and a profile 7 conversion needs
dovi_tool. A job that needs one that cannot be found is skipped, `dynamic-hdr-tool-missing`. That
skip is a condition of the host, not a verdict about the file, so it is mutable: the first pass
that finds the tool takes the file again.

## The rows

<a id="rows"></a>

The skips are part of the skip vocabulary ([docs/api-reference.md](../api-reference.md)). The
profile 7 skip records `dolby_vision_p7` and `encoder`, so turning conversion on offers the file
back; the others record `encoder` where the verdict was the cpu encoder's own, and nothing where no
configuration change moves it (profile 5). What a done row carried is logged per job (`dynamic HDR
carried`, with the profile, the VBV level, whether it was converted and the enhancement-layer
type); the row itself gains no column in this build. Rows an earlier build wrote under
`dolby-vision` and `hdr10-plus` recorded nothing read, so no configuration change re-opens them:
`holdfast requeue --guard dolby-vision` (or `hdr10-plus`) is the lever ([docs/requeue.md](../requeue.md)).

## Where it reaches

<a id="reach"></a>

**One limit is stated rather than hidden.** The already-at-target-codec guard runs before the HDR
guard and skips a source already in the codec the job's encoder writes. The cpu encoder writes
HEVC, and every Dolby Vision profile 7 and 8 source is HEVC, so under this build's guards no such
source reaches the carriage: it is skipped `already-at-target-codec`, exactly as it was. An AV1
source is skipped `better-codec-family` first too. HDR10+ reaches it only in another codec (H.264,
VP9), and hdr10plus_tool reads only HEVC, so such a source is skipped `hdr10-plus-unreadable`. What this build carries is therefore what a source that DOES
reach the HDR guard on the cpu encoder gets; whether an HEVC Dolby Vision or HDR10+ source should
be re-encoded to HEVC at all is a decision about the already-at-target guard (brief I5: a change
to existing decisions), not one this document makes. The engine fixtures reach the carriage by
answering the guards' snapshot probe with an h264 codec and every other probe with the real
ffprobe (`codecMaskingFFprobe` in `internal/engine/dynamichdr_test.go`).
