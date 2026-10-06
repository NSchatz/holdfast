# Crop

What `crop: auto` cuts away from a picture, how it decides, the gate a cropped replacement
passes, and why a crop that cannot be decided confidently is not made. This document is that
argument's single home: the [design index](README.md) names the rule and links here rather than restating it. The
key is described in [`docs/profiles.md`](../profiles.md#crop); the code is `internal/crop`, and
the engine's half is `internal/engine/crop.go`.

Nothing here relaxes a gate. A cropped replacement is held to every gate a replacement always
was ([`docs/design/swap.md`](swap.md)), and to one more.

## The rule

<a id="crop"></a>

**Under `crop: auto`, a source's black bars are cut away only where spread samples agree on
them, the area removed is black on every frame of the source, and - for a Dolby Vision source -
its RPU names that same rectangle as its active area, zeroed and gated in the replacement;
anything else encodes the whole frame and the row says why.** With the key at `off` (the
default) none of this runs: nothing is sampled, the command line and every gate are what they
were, the row records nothing about a crop, and the profile digest is the one a build without
the key computed.

A crop is a transformation on request, like a deinterlace: **the replacement is no longer the
same content as the source**. The rows and columns it removes are not in the replacement, and
the swap deletes the original. What makes it safe to ask for is that it is only ever made where
the removed area provably held nothing but black.

It is the one picture transformation whose input is guessed rather than read: nothing in a file
says "these rows are bars". So every step below errs the same way. A crop that is missed costs
the few kilobytes black bars cost an encoder; a crop that is wrong cuts picture out of a file
whose original is then deleted. Every step that cannot answer confidently answers **no crop**,
never a smaller one, and the file is then encoded exactly as it would have been with the key off.

## How a crop is decided

<a id="detection"></a>

1. **Sampling.** The source is read by ffmpeg's `cropdetect` at 10 points spread evenly over the
   file, skipping its first and last 5% (opening logos and end credits are the frames most
   often black or boxed differently), four consecutive frames at each, with the box cumulative
   over them (`reset=0`). The filter is
   `cropdetect=limit=0.0941176:round=2:skip=0:reset=0`.
   - **The limit is fractional.** cropdetect scales a limit below 1 by the frame's bit depth
     ("You can also specify a value between 0.0 and 1.0 which will be scaled depending on the
     bitdepth", filters.texi, https://ffmpeg.org/ffmpeg-filters.html#cropdetect, read
     2026-10-01). `0.0941176` is 24/255 and cropdetect's own default (`ffmpeg -h
     filter=cropdetect` on the pinned build). An absolute limit of 24 sits below 10-bit
     limited-range black, which is 64, and finds no bars at all on a 10-bit source; the
     fractional one finds them. Both are measured on the pinned build by
     `TestDetect_TenBitPQ_FractionalLimitCropsAndAbsoluteDoesNot`, as the research measured them
     first (`research-streams-hdr.md` section 4.2, `verify-streams-hdr.md` section 12).
     On a libx265 10-bit fixture the compression ringing in the row touching the picture
     crosses the limit on some frames, so the loose consensus keeps that row and the crop
     leaves one or two near-black rows of each bar: the conservative direction.
   - `round=2` asks for even dimensions ("Use 2 to get only even dimensions", filters.texi as
     above); `skip=0` because the default skips each sample's first two frames.
   - The sample count and the frames per sample are `ASSUMED` (HandBrake's scan takes 10
     previews by default, `LEAD`); the 5% is `ASSUMED` ("the first and last few percent").
2. **Invalid samples are discarded.** A sample whose box is empty - the NEGATIVE crop an
   all-black frame produces (`crop=-1918:-1078:1920:1080` on a 1920x1080 frame, research
   section 4.2) - or outside the frame, or removing **more than a quarter of the frame on any
   side**, is not a sample. The quarter is HandBrake's rule: it discards a preview whose crop
   exceeds a quarter of the frame on a side, because it was "fooled by frames with a lot of black
   like titles, credits & fade-thru-black" (`libhb/scan.c`,
   https://raw.githubusercontent.com/HandBrake/HandBrake/master/libhb/scan.c, as read for the
   research on 2026-09-29).
3. **Too few valid samples is no crop** (`too-few-samples`): a crop rests on at least 3, the
   smallest count satisfying HandBrake's "more than two previews" (`libhb/scan.c`, as above).
4. **The loose consensus.** The crop is the **minimum** each side was removed by over the valid
   samples, so no pixel any valid sample found non-black is ever cut. This is HandBrake's
   "loose" crop rather than its default median: a median crops through the picture of the
   samples above it.
5. **Disagreement is no crop** (`samples-disagree`). Any valid sample removing more than 9 px
   beyond the consensus on a side means the samples do not describe one picture: a mixed aspect
   ratio (an IMAX sequence, a pillarboxed insert) or a dark scene read as bars. The loose
   consensus would still be safe, but a file whose samples disagree is ambiguous input, and
   ambiguous input is not acted on. 9 px is the distance HandBrake counts as a different aspect
   ratio (`libhb/scan.c`, as above); that it suits every resolution is `ASSUMED`.
6. **Alignment.** The rectangle is aligned to the coarsest chroma subsampling of the formats the
   picture passes through - the source's (the crop runs on its decoded frames) and the one the
   encoder is handed: 4:2:0 needs width, height and both offsets even, 4:2:2 the width and the
   x offset, 4:4:4 nothing. The edges are rounded **outward**, so aligning can only keep more.
   A frame that cannot be aligned that way (an odd frame width) is `unaligned`; bars that round
   away to nothing are `no-bars`. The encoder registry declares no dimension constraint of its
   own beyond its pixel formats' chroma (`ASSUMED` sufficient for every registry encoder: none
   of them refuses an even size in the fixtures).
7. **The blackness check** (below) runs on the rectangle **before** the encode. A crop whose
   bars are not black is refused there (`bars-not-black`), and the file is encoded uncropped in
   the same attempt.

The decision is `crop.Decide`, a pure function of the consensus, the frame, the pixel formats
and the source's Dolby Vision class, and it returns a rectangle or a named refusal. The engine
samples once per job (`Engine.cropConsensus`); the encode plan decides once
(`cropApplied`, from `deriveEncodePlan`), and the command line, the perceptual gate, the crop
gate and the row all read the plan's `Picture.Crop`
([encode-plan](encode-plan.md#encode-plan)).

## The order of the picture operations

<a id="order"></a>

**Deinterlace, then crop, then scale.** The deinterlacer interpolates from the fields where the
source has them, so it runs first, on the whole frame, exactly as it always did. The crop then
cuts the deinterlaced frame. The `max_height` scale runs last and is resolved against the
CROPPED picture: a 1920x1080 source with 140 px bars under `max_height: 720` keeps 1920x800 and
scales that to 1728x720, holding the picture's own aspect ratio, where scaling first would
resample the bars and then cut a rectangle that no longer lands on whole source rows. A job
that crops and scales records both; the `max_height` skip that guards an unacknowledged final
swap is still decided on the source's own height, which is never smaller than the cropped one,
so it can only refuse more.

## The perceptual gate's reference

<a id="reference"></a>

**The reference is the source put through the same deinterlace and the same crop, in the same
order, and nothing else.** The encode removed bars the source carried, so a score against the
uncropped source would compare pictures of two sizes, and resampled to meet they are two
different pictures: the number would measure the crop, not the encode. A crop is not a
resampling: it keeps the source's own pixels, so cropping the reference loses nothing the
encode did not also lose, which is the property that keeps a scale out of the reference
([quality-gate](quality-gate.md#vmaf-pooling)). On a job that also scales, the output is scaled
back up to the cropped size and scored there. The row's `deinterlace_filter` stays the
deinterlace alone; the crop is on the row's own `crop` record.

## The crop gate

<a id="crop-gate"></a>

**A cropped output replaces its source only when it is the size its plan declares and the area
the crop removed from the source is black on every frame.** The gate (`Engine.cropGate`, gate 6c
of `verifyAgainst`, `GateCrop`, gate label `crop`) runs on every job whose plan crops and on no
other, after the decode-integrity check and before the perceptual gate. It is additive: no
existing gate moves.

- **Size.** The output's dimensions must equal the crop's rectangle, or the scale's target where
  the cropped picture was then scaled. A mismatch is deterministic: the same plan builds the
  same command line.
- **Blackness.** The removed area of the SOURCE - a full-width band above and below the kept
  picture and a band left and right of it - is measured on every frame with `signalstats`, in
  one decode. Each band's **mean** luma must be within 24 (of 255, scaled by the bit depth: 96
  at 10 bits), which is cropdetect's own threshold, so the area removed is black by the measure
  that found it; and each band's **brightest pixel**, leaving out the 4 rows (or columns) next
  to the picture, must be within 48 (192 at 10 bits). Calibration, on this package's 10-bit PQ
  fixture encoded by libx265: the bars' mean stays at 64-67 against black at 64, while
  compression ringing next to the picture reaches 224 in the rows touching it and 149 four rows
  away (`TestBlackness_CalibrationOnTheTenBitFixture`; the research measured 73-138 on its own
  10-bit fixture, section 4.4); 8-bit bars read 16, and text burned into a bar reads 235 (940 at
  10 bits). The two bounds and the 4 guard rows are `ASSUMED` beyond that calibration. Text
  that hugs the picture within those 4 rows is held only by the mean; that is the residual.
- **Why twice.** The same check runs before the encode (detection, step 7) so a crop through
  content costs no encode, and again here so the replacement is never accepted on a check that
  belongs to a different stage: the gate measures the rectangle the plan declares. A failure
  here is transient, because the check before the encode passed on the same bytes: the next
  attempt checks again and, failing there, encodes uncropped.

## Dolby Vision

<a id="dolby-vision"></a>

**A Dolby Vision source is cropped only to the active area its own RPU names, only where the
picture agrees, and with that metadata zeroed and gated; every other Dolby Vision source is
encoded uncropped with its Dolby Vision carried.** This is approved proposal P5, option (c). An
RPU's level 5 (L5) states the active area as offsets from each edge; ffmpeg's crop does not
touch it, so a cropped replacement that kept it would describe bars that are no longer there
(P5 finding 2, re-run on the pinned build). So two independent witnesses must agree before a
row of a Dolby Vision picture is cut: the mastering-side metadata and the pixels.

- **Reading L5.** Only for a source whose Dolby Vision this job carries (the `cpu` encoder,
  [dynamic-hdr](dynamic-hdr.md)), before the dynamic-HDR pre-pass: the video piped as Annex B
  HEVC from ffmpeg into `dovi_tool extract-rpu - -o <rpu>` (dovi_tool picks its demuxer by the
  file's extension, and reads the engine's `.holdfast-part` working file as raw HEVC and fails),
  then `dovi_tool export -i <rpu> -l level5=<json> -f json`, which writes one record per frame
  that HAS an L5 block. `export -d level5` is never used: it writes a frame with
  no L5 as 0/0/0/0, exactly like a zeroed one (dovi_tool 2.3.4 `src/dovi/exporter.rs` lines 154
  and 165, read for P5 on 2026-09-29; the difference re-run by
  `TestRealFixture_ADroppedL5IsRefusedAlthoughExportDReadsZero`). The source's frame count and
  frame rate are read with it.
- **The decision** (`crop.Decide`, its Dolby Vision half). A crop is made only where every
  frame carries one identical, non-zero L5, its offsets are aligned to the chroma subsampling,
  and the cropdetect consensus (taken exactly as for any source) agrees with it within 2 px on
  every side (`L5TolerancePx`, `ASSUMED` from P5 and calibrated on the 10-bit fixture, whose
  libx265 ringing keeps one row of each 40-row bar above cropdetect's limit: the consensus reads
  39 where L5 says 40). The rectangle is then **L5's own**, not the consensus'. The refusals,
  each encoding the file uncropped with its Dolby Vision carried and the token on its row:
  - `dolby-vision` - no L5 was read (the source's Dolby Vision is not carried by this job);
  - `dolby-vision-l5-unreadable` - dovi_tool failed, or its export did not parse;
  - `dolby-vision-variable-frame-rate` - the frame rate is not one constant rate starting at
    zero, which the raw stream the zeroing writes is read at;
  - `dolby-vision-l5-zero-or-absent` - some frame carries no L5, or every frame's L5 is zero
    (the RPU names no bars, while the pixels may show some: ambiguous, so not acted on);
  - `dolby-vision-l5-varies` - frames carry different rectangles (a shot-varying active area);
  - `dolby-vision-l5-odd-offset` - an offset the chroma subsampling cannot cut at;
  - `dolby-vision-l5-disagrees` - L5 and the picture differ by more than 2 px on a side, or L5
    names no picture inside the frame;
  - the consensus' own refusals (`samples-disagree` and the rest), and `bars-not-black` from the
    blackness check, which runs on the L5 rectangle before the pre-pass;
  - `dolby-vision-l5-zeroing-failed` - the pre-pass that zeroes L5 did not complete; the source
    is prepared again as it is;
  - `dolby-vision-l5-gate-failed` - an earlier attempt's cropped output failed the L5 gate.
- **The pre-pass.** A crop asks the dynamic-HDR pre-pass to rewrite the source's video into a
  raw stream with L5 zeroed, through dovi_tool's global `-c` ("Set active area offsets to 0
  (meaning no letterbox bars)", `dovi_tool --help` on the pinned 2.3.4): `dovi_tool -m 0 -c
  convert - -o <raw>` for profile 8.1, and `dovi_tool -m 2 -c convert --discard - -o <raw>` for
  an opted-in profile 7 source, the conversion that path already runs plus the one flag. The
  encode then reads the raw stream exactly as a converted profile 7 encode does (`-f hevc
  -framerate <source rate>`, the audio and subtitles mapped from the original, variable frame
  rate refused, the working file counted by the room check and removed on every way out of the
  job; [dynamic-hdr](dynamic-hdr.md#profile-7)). `--edit-config` is never paired with `-m` or
  `-c`: any edit config switches them off (`src/main.rs` lines 84-88 at 2.3.4, P5 finding 4).
  The derivation takes the crop decision again from the same inputs and refuses a plan where a
  crop and the zeroing do not go together: a crop without it would carry stale L5, the zeroing
  without the crop would strip an L5 that is true.

<a id="l5-gate"></a>

**The L5 gate.** A Dolby Vision output cropped this way replaces its source only when it carries
exactly one L5 record per decoded frame and every one is 0/0/0/0 (`dynhdr.CheckZeroL5`, gate
label `dolby-vision-l5`), read with the same `extract-rpu` and `export -l level5` on the output.
The count is what keeps a DROPPED L5 from passing as a zeroed one: a frame without L5 has no
record. It runs beside the dynamic-HDR gates, which prove the DOVI record and an RPU on every
frame but cannot see what an RPU says, and the crop gate and the perceptual gate still run. A
failure is transient: the source is kept, the source is remembered by the process, and its next
attempt in that process is encoded uncropped with its Dolby Vision carried
(`dolby-vision-l5-gate-failed`). A restart forgets it; the next attempt then crops and is gated
again, bounded by `max_failures`. Without `-c` the cropped output's L5 stays at the source's
40/40, and the gate refuses it (`TestRealFixture_TheL5ZeroingCropsAndItsGateRedsWithoutTheFlag`,
`TestCropDV_TheL5GateRefusesAStaleL5AndTheNextAttemptIsUncropped`).

**The same limit as Dolby Vision carriage.** Every Dolby Vision profile 7 and 8 source is HEVC,
and the `cpu` encoder writes HEVC, so under this build's guards such a source is skipped
`already-at-target-codec` before the HDR guard and never reaches the crop at all
([dynamic-hdr](dynamic-hdr.md#reach)). What is described here is what a Dolby Vision source that
DOES reach the carriage gets; the engine fixtures reach it as the dynamic-HDR ones do, by
answering the guards' snapshot probe with an h264 codec and every other probe with the real
ffprobe.

## Which files the key reaches

<a id="which-files"></a>

The key changes what the next jobs do and re-opens nothing. No guard reads it and no row records
it as a decision input, so turning it on does not offer back a file a terminal row already
answered: re-encoding a finished replacement to crop it would be a second generation of loss for
a few kilobytes of black. `holdfast requeue` is the lever for a file you want cropped
([docs/requeue.md](../requeue.md)). What a job did about its crop is recorded on its row as
proof: the rectangle kept and the frame it was kept from, or the refusal token and its words
(`crop` in [docs/api-reference.md](../api-reference.md)).
