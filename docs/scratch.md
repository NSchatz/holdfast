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

## What actually happens

1. The encoder writes its output into `scratch_dir` under a per-job working name
   (`<stem>.<tag>.__transcoding__.<ext>`; the tag is derived from the source's path,
   so two files that share a basename in different directories cannot collide).
2. The acceptance gates run against that file - codec, duration and packet parity,
   strictly-smaller, per-type stream-count parity, full decode integrity, VMAF mean,
   worst-frame floor and chroma floor. Nothing at all has been written under the
   source's directory yet.
3. **Only once the gates have accepted**, the result is copied into a temp beside
   the source, built by exactly the same construction as the in-place temp.
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

Four things, all of them about the SHAPE of the I/O rather than its volume:

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

## Startup refuses rather than failing mid-encode

A configured `scratch_dir` is checked in the same start-or-refuse decision as the
library roots and the state directory - before the first encode, before the job store
is opened, and before anything is created, renamed or removed. The run is refused,
non-zero, naming the path, the cause and the remedy, if the directory:

- does not exist, or exists and is not a directory;
- cannot be inspected;
- **is a library root, is beneath one, or has one beneath it** (compared after
  symlink resolution). A working area inside the tree holdfast scans is on the same
  storage and delivers none of the four benefits above;
- has less free space than `scratch_min_free_gb`;
- cannot be written to by the running user.

`holdfast validate` reports the same causes.

Two honest limits, stated rather than papered over:

- **The floor is a floor, not a prediction.** Startup has no per-file size to check
  against, so `scratch_min_free_gb` is all it can enforce. Backing it up, each job
  re-checks the free space immediately before it encodes and fails **that job** -
  naming the scratch path, the free space and the source size, leaving the source
  untouched and continuing the scan - if the source will not fit.
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

Working files a killed run left in the scratch directory are discarded when the next
run starts, under the same hold-back exceptions the in-place sweep applies: a path a
live record holds back is left alone, and so is a replacement holdfast retained. The
copy made beside the source is built by the existing temp construction, so the
stale-temp sweep, the record-based hold-backs and the record-free stray-replacement
hold all cover it with no new rule, and it is never enumerated as a source.
