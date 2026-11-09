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
