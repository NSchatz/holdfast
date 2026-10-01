# Crop

What `crop: auto` cuts away from a picture, how it decides, the gate a cropped replacement
passes, and why a crop that cannot be decided confidently is not made. This document is that
argument's single home: `CLAUDE.md` names the rule and links here rather than restating it. The
key is described in [`docs/profiles.md`](../profiles.md#crop); the code is `internal/crop`, and
the engine's half is `internal/engine/crop.go`.

Nothing here relaxes a gate. A cropped replacement is held to every gate a replacement always
was ([`docs/design/swap.md`](swap.md)), and to one more.

## The rule

<a id="crop"></a>

**Under `crop: auto`, a source's black bars are cut away only where spread samples agree on
them, the area removed is black on every frame of the source, and the source is not Dolby
Vision; anything else encodes the whole frame and the row says why.** With the key at `off` (the
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

**A Dolby Vision source is never cropped in this build.** Its RPU names the active area of the
picture (level 5); ffmpeg's crop does not touch it, so a cropped replacement would carry metadata
describing bars that are no longer there (proposal P5, finding 2, re-run on the pinned build).
The decision reads the source's own Dolby Vision class from its probe (`crop.DolbyVisionOf`,
the same reading as the engine's Dolby Vision guard) and refuses (`dolby-vision`) before it
reads a sample, whatever the guards in front of it did; the file is encoded uncropped wherever
it is encoded at all. Approved P5, option (c), is the next phase: crop a DV source only to the
rectangle its own RPU names, with L5 zeroed by `dovi_tool -m 0 -c convert` and a gate proving
0/0/0/0 on every output frame. The decision's `DolbyVision.L5` input is where that rectangle
arrives.

## Which files the key reaches

<a id="which-files"></a>

The key changes what the next jobs do and re-opens nothing. No guard reads it and no row records
it as a decision input, so turning it on does not offer back a file a terminal row already
answered: re-encoding a finished replacement to crop it would be a second generation of loss for
a few kilobytes of black. `holdfast requeue` is the lever for a file you want cropped
([docs/requeue.md](../requeue.md)). What a job did about its crop is recorded on its row as
proof: the rectangle kept and the frame it was kept from, or the refusal token and its words
(`crop` in [docs/api-reference.md](../api-reference.md)).
