# The swap

The one irreversible thing holdfast does, and the argument behind the rule
the [design index](README.md) states. This document is that argument's single home: the [design index](README.md)
names the invariant and links here rather than restating it.

## The invariant

<a id="swap-invariant"></a>

**No source is mutated until a replacement has passed every gate.** The gates are
correct codec, duration and packet parity, strictly-smaller output, per-type
stream-count parity, full decode-integrity, and VMAF. The encode goes to a
same-directory temp, and the swap itself is the only filesystem mutation there
is: an atomic same-filesystem rename(2) out of that temp onto the source. This is
the exact fix for Tdarr's documented replace-before-verify data loss.

The output fidelity gate joins them by addition: the replacement must carry the bit depth,
chroma subsampling, colour tags and HDR10 static metadata its encode plan declares
([encode-plan](encode-plan.md#fidelity)).

The crop gate joins them by addition on a job that crops: the replacement must be the size its
plan declares, and the area the crop removed from the source must be black on every frame
([crop](crop.md#crop-gate)).

The chapter-parity gate joins them by addition: the replacement must carry the source's
chapter table ([chapter parity](#chapter-parity)).

The playability gate joins them by addition on every encode: the replacement must decode from a
cold start at a sample of its keyframes, and segment by segment, the way players and media
servers start it ([playability](#playability)).

Any gate failure discards the temp and leaves the source byte-for-byte intact.
There is no half-written state left behind to clean up, and no moment at which the
source is gone and the replacement is not yet in place.

The rename is made durable rather than merely atomic. A rename is atomic for a
concurrent reader, but POSIX does not make it persistent until the containing
directory has been flushed, so a power loss an instant after the call returns can
still lose it, and where the source had already been removed that leaves a
directory entry pointing at nothing. holdfast therefore flushes the encode before
the rename and fsyncs the parent directory after it, which is the POSIX
durable-rename recipe; when that directory fsync fails the source is kept, never
removed under a rename nothing has proved. True power-loss survival is filesystem-
and hardware-dependent and is untestable in CI without a power-cut harness, so
this is the portable discipline, documented as such, and not an absolute
guarantee.

## Chapter parity

<a id="chapter-parity"></a>

Chapters are container metadata: not a stream, so the intended-stream check cannot count them,
and not pictures, so the decode and perceptual gates cannot score them. An output whose chapters
were dropped, moved or pushed past its end passes every other gate. The chapter-parity gate
(`internal/chapters`, gate label `chapters`) runs after the intended-stream check, on every job
including a remux, and refuses the output unless:

- it carries the same number of chapters as the source, with the same titles, in the same order;
- each chapter starts and ends within **100 ms** of the source's, both read as positions into the
  file (the chapter time less the file's `start_time`, the position a player shows, and the one
  ffmpeg keeps when it rebases a copy to start at 0);
- every chapter the source places inside its own duration starts before the output's end and
  ends no later than the output's end plus 100 ms plus `duration_tolerance_sec` (the drift the
  length gate already accepted between the two files).

The tolerance is 100 ms because an encode copies chapters and never re-times them. The only
differences a faithful copy carries are the rounding of a rescale between time bases (Matroska
stores nanoseconds, ffmpeg's MP4 muxer milliseconds: at most 1 ms) and a move of the first
presented timestamp by an encoder delay or a frame reordering (two frames at 23.976 fps is
83 ms). A crop, a scale or a deinterlace changes no timestamp. 100 ms is still far below what a
viewer can see: a chapter seek lands on the keyframe before it, usually seconds away.

A chapter table either file reports that cannot be read is refused: an unknown table is not an
empty one. A refusal is deterministic, since the same source, configuration and ffmpeg build
write the same table, and the source is kept.

**A source whose own table is malformed** (a chapter past its own end, ending before it starts,
or overlapping or out of order with the one before it) is not refused for it, and is not
skipped. The gate's question is whether the encode damaged what was there, and an output that
reproduces the source's table exactly has not; refusing it would keep a file whose table is just
as broken and make it unencodable for a defect the swap neither causes nor changes. Parity is
still enforced chapter by chapter, and only the in-duration check is waived, and only for a
chapter the source itself places outside its duration; each defect is logged as a warning naming
the file. Where the source's duration is unknown nothing is waived. This is the case
[issue #200](https://github.com/NSchatz/holdfast/issues/200) met: the swapped file's table had
three chapters starting after its end, and
[the re-acquired source carries the same table](https://github.com/NSchatz/holdfast/issues/200#issuecomment-6730994015),
copied faithfully. That swap passes this gate.

The encode takes chapters from the source. A single-input command line has nowhere else to take
them from; the one command line with a second input (a converted Dolby Vision profile 7 stream,
`1:v:0`) names the source explicitly with `-map_chapters 0`.

## Playability

<a id="playability"></a>

**An output replaces its source only when it decodes the way players and media servers start it:
from a cold start at a sample of its keyframes, at the keyframes followed by leading pictures,
and segment by segment. That proves one decoder starts it cleanly everywhere it was started; it
does not prove every client plays it.**

### Why playability is not faithfulness

Every gate before this one asks whether the output is faithful to its source: the codec, the
length, the streams, the chapters, the declared metadata, every frame decoding from the first,
and the pictures looking like the source's. None asks whether the output starts where a player
starts it. The decode-integrity gate is one linear decode from frame 0. A player that seeks or
resumes, a media server cutting HLS or DASH segments for a browser, a transcoder seeking with
`-ss`, a hardware decoder handed one segment: each starts decoding at a keyframe in the middle of
the stream with nothing decoded before it, a path a linear decode never takes. An output can be
faithful frame for frame and not start cleanly there.

[Issue #200](https://github.com/NSchatz/holdfast/issues/200) is the case that made it a gate: a
libx265 output coded with open GOPs (CRA keyframes, some followed by RASL pictures that reference
the group before them) passed every gate and stalled several Plex clients about 34 minutes in,
while the source it replaced played through. The defect in that file has not been isolated: the
pinned ffmpeg decodes it, and the other open-GOP outputs audited beside it, cleanly in software
and through NVDEC, linearly and from cold starts at its RASL-bearing CRAs. So this gate is not a
fix for that one stall. It closes the class of path the gates never exercised, and it is
deliberately honest about what it can and cannot see (below).

### What the gate does

The gate (`internal/playability`, gate label `playability`) runs on every encode after the
decode-integrity check and the gates that follow it, and before the perceptual gate. On the
output's first video stream (an attached picture is never it):

1. It lists the stream packet by packet (ffprobe): each packet's presentation time and the
   container's keyframe flag. A cold start is aimed by packet order, never by time, so a
   container that records no presentation time at all (AVI, ASF) is checked as any other.
2. It remuxes the stream, copied and never re-encoded, into fragmented MP4, the shape HLS and
   DASH stream it in, with a fragment starting at every keyframe the container flags (and the
   pictures ahead of the first keyframe kept, `-copyinkf`). The remux
   reads the file from its first byte to its last and seeks nothing, so a container with no
   keyframe index (MPEG-TS) is checked exactly as Matroska and MP4 are. Nothing is written to
   disk: the remux streams into holdfast, and each fragment streams on into the decoders it is
   routed to.
3. **Cold starts** (`playability_cold_starts`, default 16): that many keyframes spread evenly
   across the stream, and up to four times as many of the keyframes followed by leading pictures
   (a picture after the keyframe in decode order but before it in presentation order: in HEVC a
   RASL or RADL picture, in an open-GOP H.264 stream a B picture after a recovery point), spread
   evenly across every such keyframe. Each is a fresh decoder handed the remux's initialisation
   segment and the fragments from that keyframe on, three seconds of decode time, with nothing
   before it: the decoder's start a player makes after a seek.
4. **Segments** (`playability_segments`, default on): the fragments cut into segments of at least
   six seconds, each starting at a keyframe (the first keyframe decoded at least six seconds past the
   last segment's start, as an HLS or DASH segmenter cuts a stream it copies), and each decoded on
   its own by a fresh decoder. Every picture of the output is decoded this way once more, and
   every segment boundary is a cold start of its own.

Every decode is ffmpeg with `-xerror` and `-err_detect +explode`, and it passes only where the
decoder exits 0, prints no error at all at `-v error`, and produces at least one picture. An error
printed beside a clean exit fails it: a decoder starting at a picture whose references it never
decoded prints `Could not find ref with POC` and conceals, and that is exactly the failure the
gate is for. ffprobe lists packets and is never the decoder a verdict is read from: it swallows
decode errors and exits 0.

**The gate judges whether the pictures decode, not whether their timestamps are right.** Each
decoder numbers its pictures in the order it decodes them (`-vf setpts=N`) and keeps those numbers
in the stream's own time base (`-enc_time_base:v demux`), so no container timestamp ever reaches a
verdict. Without that, a stream that decodes cleanly is refused for a timestamp ffmpeg invented:
re-timed onto the frame rate it guesses from the pipe, a variable frame rate stream (24, then 60,
then 30 fps) puts two pictures on one tick, and an AVI or ASF stream with B pictures and no
presentation times repeats one, and the null muxer prints "non monotonically increasing dts".
Whether an output's timestamps match its source's is a timing question for a gate of its own. HDR10
and Dolby Vision profile 8.1 outputs print nothing at any cold start, so every error line the
decoder prints still fails it.

A failure keeps the source. A decode that ran and failed, a stream that cannot be listed, and a
stream that cannot be remuxed into fragmented MP4 are **deterministic**: the same output fails the
same way again, so the file parks at once. Everything that is not evidence about the file is
**transient**: a check that could not be run at all (a binary that did not start, a process killed
by a signal, a cancelled run, an answer this build could not read); a process that exited non-zero
printing no error (ffmpeg stopped by SIGTERM exits 255 and says nothing at `-v error`), since only
an error a decoder printed is a verdict; and a remux that did not carry the stream the listing
describes - a picture for every listed packet, in one fragment per keyframe - which leaves part of
the stream undecoded by the checks and proves nothing about it. The reason names the check, where it
failed ("the keyframe at 2069.069 s", "segment 12 (decode time 66.000 s)") and the decoder's first
error line.

A remux is not held to the gate: its video is the source's own, bit for bit (the video-identity
check), so it starts exactly as the source does, and refusing it would keep a source with the same
property.

The fixture that reds it is an output every gate before it passes, and the decode-integrity gate's
linear decode in particular: an MP4 whose sync-sample table flags a trailing picture as a keyframe
in place of the IDR before it. Every picture decodes from the first frame; a cold start at that
keyframe, and the segment that starts there, decode a picture whose references they never had
(`TestColdStart_AKeyframeFlaggedOnATrailingPictureFails`,
`TestSegments_AFragmentStartingAtATrailingPictureFails`,
`TestPlayability_AnOutputThatDoesNotStartAtAKeyframeIsRefused`). The same output passes every gate
and replaces its source with both keys off (`TestPlayability_TheKeysTurnItOff`). A second fixture
keeps the keyframes and changes the pictures: an open-GOP stream whose RASL pictures are relabelled
RADL in place decodes linearly frame for frame, and a decoder starting at a keyframe decodes
pictures that reference one it never saw (`TestColdStart_RASLPicturesRelabelledRADLFail`, in
Matroska, MP4 and MPEG-TS). Every shape an encoder writes - open and closed GOPs, HEVC, H.264 and
AV1, a variable frame rate, HDR10, Dolby Vision profile 8.1, in Matroska, MP4, MPEG-TS, AVI and ASF
- passes with a cold start at every keyframe, and with the segments alone
(`TestCheck_EveryShapeAnEncoderWritesPasses`).

### What it costs

Per encode: one packet listing and one remux, each a read of the whole output; one more decode of
the whole video (the segments), in processes of a few seconds each; and up to 16 + 64 short
decodes, each three seconds of video or the one group of pictures it starts, where that is longer.
The remux runs once for both checks. The perceptual gate's two full decodes and the encode itself
remain the costs that dominate a job.

### What it proves and what it does not

- It proves the pinned ffmpeg's software decoder starts the output cleanly at every keyframe it was
  started at and decodes every segment on its own. A keyframe flag on a picture that is not a random
  access point, a segment a decoder cannot start, a stream that cannot be put into the fragmented
  MP4 a media server streams: each is refused before the swap.
- It does not prove that every client plays the file. ffmpeg decoding cleanly is one decoder's
  verdict. Issue #200's output decodes cleanly in ffmpeg on every path tried and still stalled Plex
  clients; a browser's decoder, a TV's hardware decoder and Apple's frameworks each behave in their
  own way, and no gate here runs them. No hardware decoder is used either.
- The cold starts are a sample. With segments on, every segment start is a cold start too, so a
  keyframe is skipped only where it falls inside a segment and outside the sample; such a keyframe
  is decoded only from the keyframe before it.
- It judges whether the pictures decode, not whether their timestamps are right: a decoder's
  timestamps are renumbered before anything reads them, and an output's timing against its
  source's is a separate check's to hold.
- It exercises the decoder's start, not a demuxer's seek: the remux hands each decoder the stream
  from its keyframe, as a correct seek would. A container index that points somewhere other than
  where its keyframe flags say is not looked for. Segments are fragmented MP4 only, not MPEG-TS.

What the gate cannot see is what the encode settings can avoid. For a library a media server
streams, `x265_open_gop: false` codes closed GOPs, every keyframe an IDR that needs nothing before
it, and the fidelity gate then holds the output to that ([closed GOPs](encode-plan.md#closed-gop));
`keep_bit_depth: true` keeps an 8-bit source 8-bit for clients that do not decode HEVC Main 10
([the output bit depth](encode-plan.md#bit-depth)).

### The keys

`playability_cold_starts` (default 16; 0 turns cold starts off) and `playability_segments`
(default `true`) tune the gate or turn either half off, per library root
([docs/profiles.md](../profiles.md#playability)). At their defaults they move no profile digest;
any other value is a different profile and digests apart. No guard reads them, so changing them
re-opens no row: a file parked by this gate comes back through `holdfast requeue --failed` or by
its path ([docs/requeue.md](../requeue.md)).

## Where the encode is written, and why the swap shape never moves

Where the encode is WRITTEN is configurable and the swap is not. By default it is
a temp beside the source and that rename is the only filesystem mutation there is.
With `scratch_dir` set the encoder writes into the scratch directory, nothing at
all appears under the source's directory until the gates have accepted, and the
accepted bytes are then copied into a temp beside the source - proved identical to
what the gates passed - for the same rename. Never a rename or a move out of the
scratch directory onto the source or into its directory: one swap shape on every
mount is what keeps the EXDEV refusal meaning what it says.

The scratch directory's own reference - what it costs, what it does not move and
how it is validated - is [docs/scratch.md](../scratch.md).

## What the invariant does not close

The guard that refuses a swap onto a source rewritten mid-encode compares
attributes at a resolution the storage decides, so it has a residual window, and
that window is wider on a network mount than on a local one. Neither window is
something the swap shape narrows. Both are measured, recorded per job and written
down in [docs/filesystem.md](../filesystem.md#residual-window-local).

What a swap CHANGES about the file it publishes - mode, ownership, modification
time, ACLs and xattrs - is a separate obligation, stated in
[docs/docker.md](../docker.md#swap-metadata).
