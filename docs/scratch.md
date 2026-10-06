# The scratch directory - where the encode's working file lives

By default holdfast writes each encode's working file **beside the source**, in the
source's own directory, and finalizes with an atomic same-directory `rename()`. That
is the shipped behaviour and it needs no configuration.

`scratch_dir` moves only the **working file**. It does not move the swap.

```yaml
# Where the encoder writes its working file. Empty (the default) is beside the
# source. Must be an absolute path, must exist, must be writable, and must not
# overlap any library root.
scratch_dir: /mnt/cache/holdfast

# The free-space floor, in GiB, the scratch filesystem must clear at startup.
# 0 disables the floor. Default 50.
scratch_min_free_gb: 50
```

## What the working file is called

Beside the source the working file is `<stem>.__transcoding__.<ext>.holdfast-part`
(`<stem>.__transcoding__.<n>.<ext>.holdfast-part` when an earlier file already holds that
name, or another job in flight is writing it), where `<ext>` is the extension of the
container the encode writes: the source's own, or `container_ext` where that forces one. Two
sources in one directory that share a stem and encode to one container - `ep.mp4` and
`ep.avi` under `container_ext: mkv` - therefore get two working files, and neither job ever
clears, reads or renames the other's. They share one swap target too, and the two swaps onto
it are taken one at a time: whichever job reaches it second finds the first one's replacement
there and refuses to overwrite it. It ends in `.holdfast-part` so that a media server
or an *arr app scanning the folder by extension does not offer a half-written encode as an
extra version of the film or as a duplicate. The name still carries the container extension
ahead of that suffix, and the `__transcoding__` marker, which is how holdfast itself
recognises the file.

The one exception is a name with no room for the suffix: where `.holdfast-part` would take
the name past the filesystem's 255-byte name limit, or the path past its 4095-byte path
limit, the working file takes the name every earlier build wrote,
`<stem>.__transcoding__.<ext>`, and the run logs that it did. Such a file swaps exactly as it
always has; the cost is that a scanner may list that one working file while it encodes.

A name ending in `.holdfast-part` gives ffmpeg nothing to choose a container from, so the
encode NAMES its container: the one ffmpeg itself chooses for a file called `x.<ext>`. That is
not always the container the extension looks like - `m4v` is ffmpeg's `ipod` muxer, `wmv` and
`asf` are `asf`, `vob` is `svcd`, and `m2ts` is `mpegts` in its 192-byte BDAV packet mode - and
where ffmpeg refuses the job's own arguments for `x.<ext>` (an HEVC stream in `m4v` or `wmv`,
for one) the job fails, with the source untouched, rather than succeeding in some other
container. holdfast knows ffmpeg's choice for `mkv`, `mp4`, `avi`, `mov`, `m4v`, `ts`, `m2ts`,
`wmv`, `flv`, `webm`, `mpg`, `mpeg`, `vob`, `mts`, `m2t`, `3gp`, `3g2`, `asf` and `ogv`,
whatever their case. A job whose output extension is none of those fails with a reason naming it,
before ffmpeg runs or anything is written beside the source; set `container_ext` to one of them
for such files, or take the extension out of `video_exts`.

A working file a build before the suffix existed left behind is named
`<stem>.__transcoding__.<ext>` and is still recognised: it is never a source, the stale-temp
sweep takes it when it is a killed run's partial encode, and it is held back, and reported,
when it is a finished encode no record names.

The working file inside `scratch_dir` is not in a library folder and keeps the name below.

## What actually happens

1. The encoder writes its output into `scratch_dir` under a per-job working name
   (`<stem>.<tag>.__transcoding__.<ext>`; the tag is derived from the source's path,
   so two files that share a basename in different directories cannot collide).
2. The acceptance gates run against that file - codec, duration and packet parity,
   strictly-smaller, per-type stream-count parity, full decode integrity, VMAF mean,
   worst-frame floor and chroma floor. Nothing at all has been written under the
   source's directory yet.
3. **Only once the gates have accepted**, the result is copied into a temp beside
   the source, built by exactly the same construction as the in-place temp
   (`<stem>.__transcoding__.<ext>.holdfast-part`).
4. The copy is made durable and then **read back off disk and re-digested**. If the
   bytes beside the source are not byte-for-byte the bytes the gates accepted, no
   rename happens, the job records a failure naming the mismatch, and the source is
   left byte-for-byte unchanged.
5. The existing atomic same-directory `rename()` runs, unchanged, with the existing
   pre-swap attribute records and the source re-fingerprint guard in front of it.
6. The scratch working file is removed.

**The finalizing rename always reads a path in the source's own directory** - whether
or not the scratch and the source share a filesystem. holdfast never renames, moves
or copies from the scratch directory onto a source or into its directory. That is
deliberate: the whole no-loss story rests on an atomic same-filesystem rename whose
failure means it did not happen, and `internal/engine/swap.go` refuses to work around
a filesystem boundary rather than copying across it. One swap shape, on every mount.

## What it buys, and what it costs

<a id="scratch-benefit"></a>

Five things, all of them about the SHAPE of the I/O rather than its volume:

- **Seek thrash avoided on a spinning disk.** Without a scratch directory a large
  sequential read and a large sequential write interleave on one spindle for the
  whole encode, and the head spends the transcode moving between them.
- **Parity read-modify-write churn avoided on unRAID, SnapRAID and RAIDZ.** An
  encoder dribbles its output over hours, and on a parity array every one of those
  writes is a read-modify-write of a parity stripe. A scratch location turns that
  into one sequential copy back at the end.
- **Failed or aborted transcodes kept off the array.** Nothing is written under the
  source's directory until the gates have accepted, so an encode that is rejected,
  or a run that is killed halfway, costs the array nothing at all.
- **A better write pattern over NFS or SMB.** One streamed copy rather than an
  encoder's write pattern over the wire.
- **A media server's change detection left quiet.** A server that rescans a folder on
  any change in it (Plex's partial scan, for one) reacts to every write an encoder makes
  to a working file beside the source, for the whole encode, even though the working
  file's name keeps it out of the library. With a scratch directory outside the library
  folders the folder changes twice: when the accepted copy lands and when it is renamed.

What it costs is a full **write-plus-read cycle** on the scratch device that would not
otherwise happen: the encode is written there and then read back to be copied beside
the source. On an SSD that is **write endurance** spent, on every transcode, at
**video-file sizes** - a cache SSD used this way wears at roughly the rate of the
library you are transcoding.

**What it does not do.** The source's drive is read once and the result is written
once either way, so the number of bytes that drive handles is the same with a scratch
directory as without one. Two things people reach for this setting believing, neither
of which survives that arithmetic:

- it "reduces the bytes written to the source drive" - it does not. <!-- scratch-claim-allow: quoted to refuse it -->
- it "extends the life of the source drive" - it does not. <!-- scratch-claim-allow: quoted to refuse it -->

Reach for it because your array hates the write pattern, not because you think it
spares the drive.

<a id="per-root"></a>

## One per library root

A `library_roots` entry may carry its own `scratch_dir`, which REPLACES the top-level one
for that root's files. `""` there writes that root's encodes beside the source again; an
entry that names none inherits the top-level value. It decides where an encode is written
and nothing about what is encoded, so, like `priority`, it is in no profile digest and
editing it re-opens no row.

```yaml
library_roots:
  - path: /mnt/disk1/movies
    scratch_dir: /mnt/disk1/.holdfast/work
  - path: /mnt/disk1/tv
    scratch_dir: /mnt/disk1/.holdfast/work
  - path: /mnt/disk2/movies
    scratch_dir: /mnt/disk2/.holdfast/work
```

That shape - each drive's own working directory, on the drive, outside its library
folders - is the one for a library spread over several drives whose folders a media
server watches: the encoder's writes stay out of the watched folders, and no drive's
encodes are carried to another drive and back. Nothing else changes. Each value is held
to every rule below, the same file is never checked twice when roots share it, each is
swept at start, and the swap is the copy back and the same same-directory rename, even
though the two directories share a filesystem: one swap shape on every mount
([design/swap.md](design/swap.md#swap-invariant)). The copy costs a write and a read of
the accepted encode on that drive, once per transcode.

## Startup refuses rather than failing mid-encode

Every configured `scratch_dir`, the top-level one and each one an entry names, is checked in the same start-or-refuse decision as the
library roots and the state directory - before the first encode, before the job store
is opened, and before anything is created, renamed or removed. The run is refused,
non-zero, naming the path, the cause and the remedy, if the directory:

- does not exist, or exists and is not a directory;
- cannot be inspected;
- **is a library root, is beneath one, or has one beneath it** (compared after
  symlink resolution). A working area inside the tree holdfast scans is on the same
  storage and delivers none of the benefits above;
- has less free space than `scratch_min_free_gb`;
- cannot be written to by the running user.

`holdfast validate` reports the same causes.

Two honest limits, stated rather than papered over:

- **The floor is a floor, not a prediction.** Startup has no per-file size to check
  against, so `scratch_min_free_gb` is all it can enforce. Backing it up, each job
  re-checks the free space immediately before it encodes and **reserves** its source's
  size on the scratch filesystem until it ends, by whatever route. What a job compares
  its source against is the free space less what the jobs already in flight there have
  reserved, because a free-space reading cannot see bytes an encode is about to write:
  - a source that fits even beside those reservations is encoded;
  - a source that fits the free space but **not beside the reservations waits**: it is
    held without encoding and without a failure recorded, says so once (naming the file,
    the free space and the bytes reserved), and checks again each time a job on that
    filesystem ends. A run cancelled meanwhile stops it with nothing recorded and the
    source untouched;
  - a source that will not fit **even with nothing reserved** fails **that job** -
    naming the scratch path, the free space and the source size, leaving the source
    untouched and continuing the scan.

  The same check and the same reservation run beside the source when `scratch_dir` is
  unset, against the source's own filesystem, and the two share one account wherever
  they share a filesystem. The reservation is conservative: a job's whole source size
  stays reserved even after part of its output has been written, so a nearly full
  filesystem runs fewer jobs at once than it could, never more than fit.
- **A filesystem can fill from outside holdfast** after a run begins. A scratch write
  that fails anyway is an ordinary encode failure, which already discards the working
  file and leaves the source byte-for-byte intact.

## Storage classification

The scratch directory is classified and **reported** at startup alongside the library
roots and the state directory. Storage there that holdfast cannot positively identify
as local does **not** refuse the run and needs no `allow_non_local` entry.

That is not an exception carved out for convenience. The `allow_non_local`
declaration exists because holdfast's no-loss contract needs local rename semantics
**where the irreversible act happens**, and no irreversible act happens in the
scratch directory: the working file is disposable by construction, and the swap still
happens beside the source on storage the existing check has already adjudicated.
Refusing a non-local scratch would be a gate that protects nothing while training an
operator to add declarations. It is still reported, because "the encode is running
over NFS" is the answer to a throughput complaint an operator would otherwise chase
for a week.

## Housekeeping

Working files a killed run left in a scratch directory are discarded when the next
run starts, in every scratch directory some root's files are written to - a directory the
configuration no longer names, at the top level or in any entry, is not swept, and what a
killed run left there stays until it is removed by hand. They are discarded under the same hold-back exceptions the in-place sweep applies: a path a
live record holds back is left alone, and so is a replacement holdfast retained. The
copy made beside the source is built by the existing temp construction, so the
stale-temp sweep, the record-based hold-backs and the record-free stray-replacement
hold all cover it with no new rule, and it is never enumerated as a source.
