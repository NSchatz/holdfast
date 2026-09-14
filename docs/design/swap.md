# The swap

The one irreversible thing holdfast does, and the argument behind the rule
`CLAUDE.md` states. This document is that argument's single home: `CLAUDE.md`
names the invariant and links here rather than restating it.

## The invariant

<a id="swap-invariant"></a>

**No source is mutated until a replacement has passed every gate.** The gates are
correct codec, duration and packet parity, strictly-smaller output, per-type
stream-count parity, full decode-integrity, and VMAF. The encode goes to a
same-directory temp, and the swap itself is the only filesystem mutation there
is: an atomic same-filesystem rename(2) out of that temp onto the source. This is
the exact fix for Tdarr's documented replace-before-verify data loss.

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
