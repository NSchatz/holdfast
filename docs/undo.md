# The undo window

holdfast never destroys a source until a replacement has passed every gate. The gates
are good and they are layered, but each of them is still an **estimate**: a machine's
verdict that two files look the same. The **delete is not an estimate**, and until this
existed it was final the microsecond it happened - the swap is a rename over the
source, so the original went with it.

The undo window is a bounded period in which that one irreversible act can be walked
back.

**It is off by default.** With `undo_window_hours: 0` - the shipped default - nothing
below applies, a swap is final, and `holdfast validate` and every startup say so.

## Turning it on

```yaml
undo_window_hours: 24     # keep each replaced original retrievable for a day
```

Then, at any point inside the window:

```bash
holdfast restore --config config.yaml                     # what is held, and for how long
holdfast restore --config config.yaml /media/tv/ep.mkv    # put that original back
```

`restore` is a **local command**, deliberately. Putting an original back overwrites a
library file with older bytes, and this tool's HTTP surface is read-and-control: it can
read the ledger, start a scan and pause one, and it touches no media file. A mutating
endpoint would need an authorization story the API does not currently have, so the undo
lives at the command line where the operator already is.

## What it costs

**Nothing at the moment it is taken.** The original is held by a second **hard link** to
the same data, not by a copy. One inode, two names, no additional bytes - which is the
only design a tool whose purpose is reclaiming disk could honestly offer.

**But the space a swap reclaimed does not come back until the window closes.** That is
the real cost of the setting, and it is worth doing the arithmetic before choosing a
number: a first pass over a library holds **every original it replaced** for
`undo_window_hours`. A pass that reclaims 400 GB with a 24-hour window needs those 400
GB to still be free 24 hours from now.

Because that is easy to misread, the two figures are reported **separately** and are
never added together:

| figure | what it is |
|---|---|
| `bytes_reclaimed_lifetime` | space the swaps have returned |
| `bytes_held_by_undo_window` | space still allocated under a retained original, which has NOT been returned |

A reclaimed figure that quietly included held space would tell you a disk is free when
it is not.

## How it behaves

**A source it cannot retain is skipped, not swapped.** The window's whole promise is
that a swap can be undone. If the second link cannot be taken - the retention area
cannot be created, the name is occupied, the filesystem refuses - the file is skipped
with the reason `undo-retention-failed` and the source is left byte-for-byte intact. It
is a **mutable** skip, exactly like the hardlink guard: fix the condition and the next
scan reclaims the file.

**The retention lives beside the file it protects,** in a `.holdfast-undo` directory in
the source's own directory. That is not a preference, it is a constraint: a hard link
**cannot cross a mounted filesystem** (`link(2)` fails with `EXDEV`), so the only
location guaranteed to work is the one the source is already on. There is deliberately
no configurable retention path and no cross-filesystem trash directory - either would
be a setting that works until the day somebody's library spans two mounts.

**A retained original is never a source.** The scan does not enumerate the retention
area, and a retained original is excluded by name whatever extension it carries. Feeding
one back to the encoder would re-encode the bytes you were given a window to recover and
swap the result over them.

**A retained original does not trip the hardlink guard.** holdfast skips a file with
more than one hard link (an *arr import that is also an active seed), and a retention is
a second link. A run interrupted between taking the link and completing the swap would
otherwise park the very file the window was protecting - so a link this tool can PROVE
is its own (the same inode, and either a live retention record or the retained name it
would itself have chosen) is discounted. A foreign extra link still skips, unchanged.

## When the window closes

At the **start of every scan pass**, holdfast releases every retention whose window has
passed, and reports what that returned:

```text
undo window: released retained original(s) released=3 bytes_returned=12884901888 unrestorable=0
```

`bytes_returned` is the space the filesystem actually got back, which is not always the
size of the file whose name was removed. `unlink(2)` frees a file's data only "if that
name was the last link to a file"; if something else has since linked the same data, the
release removes its name and returns **zero**, and says zero. (The same manual page
notes the space is also held while any process still has the file open, which nothing
can observe from here - so a returned figure is what became available, at best,
immediately.)

If a record's retained original is no longer on disk - somebody deleted it - it is
reported as **unrestorable**, the record is dropped, and zero bytes are reported. A
ledger that promises a restore it cannot perform is worse than one that says nothing.

## What a restore refuses to do

Every refusal exits non-zero and has changed **nothing** on disk.

- **Nothing is retained for that path** - never was, or the window closed and the
  retention was released, or it has already been restored.
- **The retained original is gone.** Reported as unrestorable; the record is dropped.
- **The file at that path is not the one holdfast swapped in.** The record carries the
  `size:mtime` fingerprint of what the swap left behind. If it has moved - a
  re-download, an *arr upgrade, a human - restoring would destroy that newer content
  exactly as a swap over a rewritten source would. Both fingerprints are named.
- **A file has appeared at the original's own path** after a container-changing swap
  removed it. Restoring would clobber it, so it does not.

## After a restore

The restored file is recorded in the ledger as `restored-original`, which does two
things: a ledger read for that path reports the restore rather than the swap that has
just been undone, and the next scan does **not** re-encode the file - through the very
gates that passed the encode you rejected. Change the file and it is a new fingerprint,
a new row, and an ordinary candidate again.

## Deliberately not offered

- **No copy-based retention.** A copy doubles the disk cost of a tool whose purpose is
  reclaiming disk, and cannot be taken atomically before a rename.
- **No configurable retention path, no cross-filesystem trash.** See `EXDEV` above.
- **No automatic restore.** Nothing here decides on its own that an encode was bad. The
  window buys you the time to decide; the decision stays yours.
- **No HTTP endpoint.** See the authorization note above.
