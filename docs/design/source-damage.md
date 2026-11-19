# A damaged source

The argument behind the container-damage guard (`source-damaged`), and the one kind of demuxer
line it does not count. The [design index](README.md) names the rule and links here.

## The guard

<a id="source-damaged"></a>

**A source is skipped as damaged only on a line its demuxer logged, and a line on the closed
benign list is excused only where the file's own structure proves the shape it describes.**

The snapshot probe every file already pays for runs ffprobe at `-v error`, and its stderr is read
line by line. ffmpeg names the context that logged each line, and a line counts only where that
context is the demuxer (`[matroska,webm @ 0x...]`, the name ffprobe reports as `format_name`). A
decoder's line never counts: decoders complain about healthy files routinely, a capture that
starts mid-GOP among them. A probe that did not run to completion reports nothing, and a line
whose context could be the demuxer or the video decoder (a raw `.h264` stream) is not attributed
to the demuxer.

A damaged source is a skip and not a failure, since nothing was attempted, and it is the one skip
logged at `error`: the action it asks for, replacing the file, is a human's. That flag is only
worth anything while it means damage. A file it flags that is not damaged costs an operator a look
at a sound file and buries the one that needs replacing.

## Benign demuxer lines

<a id="benign-demuxer-lines"></a>

Some demuxers log a line at error level that can describe a file whose every stream they demux
whole. `benignDemuxerLines` (`internal/probe/props.go`) is the closed list of such lines, and a line
on it is set aside only where the file proves the shape the line describes:

- an entry is matched on the demuxer that logged it AND its whole message, never a part of either,
  so it cannot swallow another diagnostic that shares its words, or the same words from another
  demuxer;
- the line alone is never enough. The same line can be the only symptom of real damage, so each
  entry carries a proof read off the file itself, and a line whose proof does not hold, or cannot
  be read, is damage and is what the guard reports;
- it excuses only itself. Any other line from the demuxer is damage, alone or beside a benign one,
  and the diagnostic the guard reports is that other line;
- each entry carries its argument in a comment. A line nobody has argued for is damage.

There is one entry.

**`[mov,mp4,m4a,3gp,3g2,mj2 @ 0x...] Referenced QT chapter track not found`.** The movie's track
reference (`tref` `chap`) names a QuickTime chapter track the movie does not carry. A remux that
dropped the chapter track and kept the reference leaves exactly that; the mov demuxer logs this,
reads no chapters from the missing track and carries on. On a real library of about 18,000 files
this one line, alone, was 243 of the 244 files the guard flagged; the 244th was a Matroska file
with EBML errors, the damage the guard exists for.

That ffmpeg decodes such a file cleanly does not make the line benign. The mov demuxer reads a box
of size 0 as running to the end of its parent, so a zeroed stretch at the start of a track
swallows that track and every box after it in the movie, the chapter track among them, without a
further word. Zero the first 146 bytes of the audio track in a chaptered MP4 and ffprobe prints
only this line and reads one video stream; a strict full decode exits 0. Zero the audio track from
its first child to the end of the movie and ffprobe reads it as a data stream. Every later gate
measures the encode against what ffprobe reads, so none of them can see a track the movie lost:
excused on the line alone, that file would be encoded without its audio and the source replaced.

So the line is excused only on positive evidence that the movie is whole and the reference really
dangles, read off the file's own boxes (`chapterReferenceDangles`, `internal/probe/qtchapter.go`):

- the file's top-level boxes walk to its end exactly, each with a valid size, and exactly one of
  them is the movie (`moov`); a size of 0 is accepted only for a top-level `mdat`, where it means
  the media runs to the end of the file;
- inside the movie, every container the walk descends into (the movie, each track, and each track's
  `tref`, `edts`, `mdia`, `minf`, `dinf` and `stbl`) is filled exactly by boxes of a valid,
  non-zero size: no zero-size box, none overrunning its parent, no stray bytes. This is what sees a
  swallowed track, and a track zeroed from inside;
- the movie holds as many tracks as ffprobe reported streams of every type (`format=nb_streams`,
  which rides on the snapshot probe at no cost), so no track was read as something else or skipped;
- every track carries exactly one `tkhd`, which gives its track ID;
- the chapter references name at least one track ID, and no track carries any ID they name.

Anything else, or a box the walk cannot read, keeps the line as damage. The walk runs only on a
file that logged the line, reads box headers and the few IDs it needs with bounded reads, never
the whole file, and gives up past 65,536 boxes. A file that is sound but stored in a shape the walk
does not accept is still flagged: the cost of a doubt is an operator's look, never a lost track.

### Safety

Passing this guard decides nothing about the swap. A file it lets through is encoded and faces
[every gate](swap.md#swap-invariant) like any other source: codec, duration and packet parity,
strictly smaller, per-type stream counts, fidelity, the full decode, chapter parity and VMAF. Those
gates measure the output against what ffprobe reads from the source, which is why the proof above
has to establish that what it reads is the whole movie: a benign line widens what is offered to the
gates only where the file shows nothing was lost before them.

### What chapter parity does with such a source

[Chapter parity](swap.md#chapter-parity) compares the chapter table ffprobe reads from the source
with the one it reads from the output. ffmpeg reads no chapters from the missing track, so the
source's table is what it does read: the file's Nero `chpl` chapters where it carries them, and
none where it does not. The encode copies that same table, so the output carries it (as Matroska
chapters, or as an MP4 chapter track ffmpeg writes whole, with no dangling reference) and the gate
passes. Such a source does not fail chapter parity after its encode.

That is why it is encoded rather than skipped under a token of its own. A distinct skip would spare
the human flag too, but it would keep a sound file at its size for a chapter track it never had,
and the encode it would avoid is not a wasted one. The engine suite runs the real pipeline over
such a file, with its Nero chapters and without, into Matroska and into MP4, with the movie before
the media and after it, with every gate on, and the file is replaced. The same file with a second
demuxer error beside the benign one, and the two damaged files above, are still skipped
`source-damaged` with no ffmpeg run.
