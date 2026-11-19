# A damaged source

The argument behind the container-damage guard (`source-damaged`), and the one kind of demuxer
line it does not count. The [design index](README.md) names the rule and links here.

## The guard

<a id="source-damaged"></a>

**A source is skipped as damaged only on a line its demuxer logged, and a line on the closed
benign list is not damage on its own.**

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

Some demuxers log at error level about a file whose every stream they demux whole.
`benignDemuxerLines` (`internal/probe/props.go`) is the closed list of such lines, and a line on it
is set aside as though it had not been logged:

- an entry is matched on the demuxer that logged it AND its whole message, never a part of either,
  so it cannot swallow another diagnostic that shares its words, or the same words from another
  demuxer;
- it excuses only itself. Any other line from the demuxer is damage, alone or beside a benign one,
  and the diagnostic the guard reports is that other line;
- each entry carries its argument in a comment. A line nobody has argued for is damage.

There is one entry.

**`[mov,mp4,m4a,3gp,3g2,mj2 @ 0x...] Referenced QT chapter track not found`.** The movie's track
reference (`tref` `chap`) names a QuickTime chapter track the file does not carry, as a remux that
dropped the chapter track and kept the reference leaves it. The mov demuxer logs this, reads no
chapters from the missing track and carries on: every stream demuxes and decodes cleanly, under a
strict full decode (`-xerror -err_detect +explode`) too. On a real library of about 18,000 files
this one line, alone, was 243 of the 244 files the guard flagged; the 244th was a Matroska file
with EBML errors, the damage the guard exists for.

### Safety

Passing this guard decides nothing about the swap. A file it lets through is encoded and faces
[every gate](swap.md#swap-invariant) like any other source: codec, duration and packet parity,
strictly smaller, per-type stream counts, fidelity, the full decode, chapter parity and VMAF. A
benign line widens what is offered to the gates, never what passes them.

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
such a file, with its Nero chapters and without, into Matroska and into MP4, with every gate on,
and the file is replaced; the same file with a second demuxer error beside the benign one is still
skipped `source-damaged` with no ffmpeg run.
