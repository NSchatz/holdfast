# The playability gate

The argument behind the rule the [design index](README.md) states for it, and its one home.

<a id="playability-gate"></a>

**A replacement replaces nothing until it decodes when a player starts it cold at a keyframe
other than the first, and when its video is remuxed to fragmented MP4 and each sampled fragment
is decoded on its own.** It is the last gate before the swap, after VMAF, and it holds every
encode; a remux (`remux_only`) is not held to it, since its video is the source's bytes,
established identical before the perceptual gate is declined.

## Why faithfulness is not enough

Every other gate measures whether the output is faithful to its source. The decode-integrity gate
decodes the output once, linearly, from its first frame, so each picture is decoded with every
picture before it already in the decoder. Players do not only do that. A player that seeks, a
media server that cuts a stream into segments at keyframes, and a client that joins a stream
mid-way all start decoding at some keyframe with nothing before it decoded.

An HEVC keyframe can be followed, in decode order, by pictures that are shown before it: leading
pictures. libx265's default open GOP makes most keyframes CRA pictures, and the leading pictures
after a CRA may reference the picture before it (RASL pictures). A decoder that starts at a CRA
must drop them; a RADL leading picture references only the keyframe and what follows it and is
decoded. A stream whose leading pictures do not decode from their keyframe decodes cleanly from
its start and fails from anywhere else, and every faithfulness gate passes it. Issue #200 is a
swap that passed every gate and stalls most players at the same point of the film, on a path
where a server stream-copies the video into fragments a browser decodes.

## What it runs

The gate reads the first moving video stream, which is the one a player starts.

1. **The keyframes.** One ffprobe pass lists the stream's packets in decode order, each with its
   presentation time and flags. It reads the container and decodes nothing. A keyframe is
   followed by leading pictures when a packet after it, before the next keyframe, has an earlier
   presentation time.
2. **The sample.** 16 keyframes spread evenly over every keyframe after the first, and up to 16
   more spread over the keyframes after the first that are followed by leading pictures and were
   not already chosen. Those are the ones a cold start can fail at, and an even spread would
   mostly miss them: in #200, 7% of 2,236 keyframes carried RASL pictures. The first keyframe
   is not sampled, because the decode-integrity gate already started there cold.
3. **Cold starts.** For each sampled keyframe, ffmpeg seeks to it (`-ss` before `-i`,
   `-noaccurate_seek`, `-copyts`) and decodes 48 frames under `-xerror -err_detect +explode`,
   as the decode-integrity gate decodes. Any error refuses the output.
4. **Fragments.** The stream is remuxed (`-c copy`) to fragmented MP4 with
   `frag_keyframe+empty_moov+default_base_moof`. That is the shape HLS and DASH serve: a new
   fragment at every keyframe, and every fragment readable from its own moof. The remux is read
   through a pipe, and each sampled fragment, with the init segment and nothing else before it,
   is streamed into a decoder of its own and decoded for up to 250 frames under the same flags.
   This is the path #200 failed on, so it is a peer of the cold starts, not a fallback: a
   container-level fault in the fragments, or a fragment that needs one before it, is refused
   here even where a seek in the original container decodes. The remux must produce one
   fragment per keyframe, or the fragments cannot be matched to the keyframes and the gate is
   unmeasured.
5. **Hardware.** On a job that decodes on hardware (`hw_decode: hardware`), the gate first
   decodes the output's opening 48 frames on that decoder (`-hwaccel cuda`, or VAAPI on the
   job's decode node), then repeats every cold start on it. The decode options are the decode
   half of the job's own, run through the job's encoder binary. A root that does not decode on
   hardware skips this step.

## Where a seek lands

A cold start that lands on an earlier keyframe than the one it was aimed at decodes through that
keyframe with every reference in place, and would report a pass for a keyframe it never started
at. So each seek is computed to land exactly, and then checked.

ffmpeg's input seek finds the last keyframe at or before its target. On a file with a video
stream that has a reorder delay (`has_b_frames`), fftools first moves the target 3/23 s earlier
(the `dts_heuristic` in `fftools/ffmpeg_demux.c`), and it adds the file's start time. The gate
aims at the keyframe's timestamp relative to the start, plus 1 ms, plus 3/23 s where there is a
delay. That lands on the keyframe whenever the next keyframe is more than a millisecond later.
`-noaccurate_seek` stops anything after the landing point from being dropped.

The check: each cold start copies its first packet to `framecrc` on stdout as a second output.
With `-copyts` that is where the seek landed, in the stream's own time base. A landing more than
half a millisecond from the keyframe is unmeasured, never a pass. The fixtures prove landing in
Matroska and MP4, with and without B-frames, and prove the check is not vacuous: a seek computed
without the delay compensation lands one keyframe early and is refused.

<a id="classes"></a>

## Classes

A failed decode, in any stage, is **deterministic**: the output's bytes are a pure function of
the source, the settings and the pinned ffmpeg, so the same settings fail the same way. The row is
parked at its first failure, and a settings change is what re-opens it. Everything that only failed to
*measure* is **transient**, the fail-safe class, and the output is refused all the same:
the packets could not be listed, a seek landed elsewhere, the remux did not complete or did not
produce one fragment per keyframe, or the hardware decoder did not decode the opening frames.
That last case is a device or driver condition, since the same frames already decoded in
software.

The gate label is `playability`. The row's `reason` names the stage, the keyframe (ordinal,
count, time, and whether leading pictures follow it), and the decoder's first error line, for
example: `playability gate: a decode started cold at keyframe 1 of 10 (at 1.000s, followed by
leading pictures) failed (... Could not find ref with POC 21): a player that seeks there or
starts a stream there cannot decode it. The source is kept`.

There is no key to turn the gate off. Like the decode-integrity and fidelity gates, it has no
threshold to tune, and every output must pass it.

<a id="cost"></a>

## Cost

The decoding is bounded by constants, independent of the output's length:

- cold starts: at most 32 x 48 = 1,536 frames;
- fragments: at most 32 x 250 = 8,000 frames;
- hardware, where configured: at most 48 + 1,536 frames more.

That is at most about 11,100 decoded frames on any file. A 2.5 h film at 23.976 fps is about
216,000 frames, so the gate decodes at most about 5% of what the decode-integrity gate does, and
much less of what VMAF does. On top of that come two sequential reads of the file that decode
nothing: the packet listing, and the `-c copy` remux. The remux writes nothing to disk and holds
one box, and the init segment, in memory at a time. A sampled fragment is streamed into its
decoder as it is read, so a long GOP costs no memory. The number of processes is also bounded:
at most 32 cold starts, 32 fragment decoders, 33 hardware decodes and three probes or remuxes.

## What it does not close

- **A sample, not every keyframe.** A cold-start fault at a keyframe that is neither in the
  spread nor among the 16 leading-picture keyframes sampled is not seen. Every keyframe followed
  by leading pictures is sampled when there are 16 or fewer. Decoding every fragment would cost
  a second full decode.
- **ffmpeg is the decoder.** The gate proves the output decodes from a cold start in ffmpeg's
  decoders, which handle RASL pictures as the standard requires. A player whose decoder does
  not, such as one that fails on any RASL picture, is not modelled. A valid open-GOP output
  passes this gate, and its compatibility with such players is a GOP setting, not something
  this gate decides.
- **A hardware decode can fall back.** Without `-hwaccel_output_format`, ffmpeg decodes in
  software a stream its hardware cannot. That keeps the hardware step from refusing a valid
  file over a missing profile, and it also means the step proves nothing on such a stream.
- **Only the first moving video stream** is gated. A second angle is decoded linearly by the
  decode-integrity gate only.

## The proof

`internal/playability` and `internal/engine` (`TestPlayability_*`) build the fixture with real
encodes. The fixture is libx265 with a keyframe every second, open GOPs and four B-frames, so
every keyframe after the first is followed by RASL pictures. Then the RASL NAL units are
relabelled RADL in place (`playabilitytest.Mislabel`). Every picture keeps its bytes and its
references, so the linear decode yields the same frames and passes `-xerror -err_detect
+explode`. A decoder starting at a keyframe then decodes pictures that reference one it never
saw ("Could not find ref"). The engine case runs the job's own encode under every gate: the
valid open-GOP output swaps; the relabelled one is refused by this gate alone, deterministically,
with its passing VMAF score on the failed row and the source byte-identical. Valid open-GOP,
closed-GOP and no-B-frame encodes, in Matroska and MP4, pass.
