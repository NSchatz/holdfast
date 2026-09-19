# How a scan enumerates

A scan lists the library and hands what it finds to its workers as it finds it. The first
file reaches a worker while the tree is still being read, and what the scan holds does not
grow with the number of files in the library. This document says which file goes first, and
what the arrangement costs.

Which file goes first is `queue_order`'s to decide, and its default - `path` - is the
traversal described immediately below. The four other values are a sort laid over that
traversal and are described under [a declared queue order](#declared-queue-order).

## The order files are handed out in

<a id="enumeration-order"></a>

Under `queue_order: path`, which is the default, a scan hands source files to its workers
directory by directory, in the order the coverage
set names them, and within one directory in entry-name order. The coverage set is the
sequence the startup walk traversed, which is depth-first through each library root in
entry-name order, so the whole sequence is fixed before the scan begins. An engine built
without the startup check walks the roots itself, in the order they are configured, and hands
files out as the walk reaches them - which is the same rule with the walk supplying the
directory sequence instead of the coverage set.

The order is total and deterministic: every source this run may act on is handed out exactly
once, and two scans over an unchanged library hand them out in the same sequence. It does not
depend on the worker count, and it does not depend on the order the workers happen to finish
in - the enumeration produces the sequence on its own goroutine and the workers only consume
it. It does not depend on the order a listing came back in either: entry-name order is
imposed by the enumeration, not inherited from whatever the filesystem returned, so a library
on a filesystem that lists a directory unsorted gets the same sequence as one that does not.

It is deliberately not a global sort of the full paths, and the difference is visible in an
ordinary library: a file named `zz.mkv` sitting in a directory is handed out before
everything inside that directory's subdirectories, where sorting the full path strings would
put it last. A global sort cannot be produced without holding every path at once, which is
the cost this arrangement exists to remove - and it is exactly the cost the four other
`queue_order` values pay, in the one form that is affordable, for the one thing they buy.

The read-only pass behind `holdfast plan` traverses the library the same way and reports what
it found in that same sequence. There is one hand-out order in this repository, so a plan
predicts the order a scan will work in as well as the set of files it will offer. That holds
under every `queue_order`: the order is imposed in one place, which both the scan and the
plan drive.

## A declared queue order

<a id="declared-queue-order"></a>

`queue_order` names the order a scan offers its candidates in. It decides SEQUENCE and never
membership: the coverage bound, the path filters and the record-based hold-backs settle
which files are candidates before any ordering runs, so the same files are offered whatever
the key says, in a different order.

| value | what goes first |
|---|---|
| `path` | the traversal above: the default, and what this tool has always done |
| `largest` | the biggest source, by byte count |
| `smallest` | the smallest source |
| `newest` | the most recently modified source |
| `oldest` | the least recently modified source |

Any other value, the empty string included, refuses to start and names the five it accepts.

Each of the four keyed orders is TOTAL and deterministic in the sense the traversal is: two
candidates carrying the same key break on the full path ascending, a path occurs once, and
two scans over an unchanged library therefore offer the same files in the same sequence. A
candidate whose key could not be read - it vanished between the listing and the ordering, or
this process may not look at it - is offered after every candidate whose key WAS read, in
path order among the others like it, with the reason recorded. It is never dropped: this
decides sequence, and a file left out of a queue is a file that is never processed.

### What a keyed order costs, and what `path` does not

`path` reads no metadata at all and holds nothing per candidate. It is the traversal, so the
figures below and every property above them are unchanged by this key existing.

Each keyed order reads ONE attribute per candidate - the size and the modification time come
out of the same read - and holds one `(key, path)` pair per candidate until the listing is
finished, because a key cannot be sorted before every key has been seen. That is the whole
of what it holds: not the listing, not the file information the key came out of, not the
fingerprint. It follows that a keyed order does NOT reach its first worker before the
library has been listed, and `path` remains the only value that does.

<a id="queue-order-memory-figures"></a>

Measured by `TestEnumerate_AKeyedOrderHoldsOnlyTheKeyAndThePath`, over a synthetic library
of 100,000 candidates spread over 1,000 directories whose paths are at most 120 bytes. The
reading is live heap above a baseline taken in the same process after the fixture was built,
sampled 50 times across the listing.

| library | held per candidate | held in total |
|---|---|---|
| 100,000 candidates under `largest` | 149.4 bytes | 14.94 MB |

- Hardware: Intel Xeon E5-2680 v4 at 2.40 GHz, GOMAXPROCS=5, Linux amd64, in a container on
  a shared host.
- Build: `3385c19`, on `sdd/S0095-holdfast-queue-ordering`. Go 1.25.14.
- Date: 2026-09-19.
- Runs: 6, `-count=3` twice - three with the suite's own flags (`-race -covermode=atomic`)
  and three without, because the figure has to hold under the flags the gate runs.
- Spread: 14,938,256 to 14,943,856 bytes, which is 149.38 to 149.44 bytes per candidate.
  The two sets of flags are inside each other's spread.

There is no previous figure to state this against: nothing in this repository held a queue
before it. The figure to compare it with is the one directly above - what the `path` order
holds, which does not grow with the number of files at all - and the difference between
them is the cost of being able to ask for an order at all.

## What a scan that stops early reports

Cancelling or pausing stops the enumeration where it stands. The directories it had not
reached are not listed, and a directory that was not listed is not reported as observed:
`observed` names the directories THIS scan listed successfully and no others. That is what the
ledger retention pass needs it to mean, because it is entitled to read a file missing from an
observed directory as a file that is gone. A directory wrongly named there costs an undo
record; one missing from it costs only a retention decision that waits for the next scan.

The limit of that rule is a scan that begins while holdfast is already paused. It lists
nothing, so it observes nothing, so no terminal ledger row is spent on its evidence and the
retention pass removes nothing at all. Under a pause held across every pass, the ledger is not
pruned - deliberately, and in the safe direction: the pass draws no conclusion from a library
it never looked at. The files it never handed out are still pending for the first scan after
the resume.

## What the enumeration costs

The retained cost of a scan is one directory's listing, plus whatever is in flight between
the enumeration and the workers, plus the record of which directories have been listed
(`observed`). Only the last of those grows, and it grows with the number of DIRECTORIES, not
with the number of files: it is the evidence the ledger retention pass reads, so it has to
describe the whole pass by the time the scan returns.

Enumeration remains linear in the total number of directory entries under the coverage set.
That has not changed and is not expected to.

### The measured figures

<a id="enumeration-memory-figures"></a>

Measured by `BenchmarkScan_EnumerationAtScale`, over synthetic libraries of media-shaped
paths spread over the same fixed 10,000 directories. Peak heap is live heap during the scan
above a baseline read after the fixture was built and before the scan began, sampled 50 times
per run at both sizes. Time to first file is the interval from the scan starting to the first
source reaching a worker.

| library | peak heap | time to first file | whole scan |
|---|---|---|---|
| 100,000 paths | 445 KiB | 3.9 ms | 5.2 s |
| 1,000,000 paths | 445 KiB | 3.8 ms | 48.9 s |

- Hardware: Intel Xeon E5-2680 v4 at 2.40 GHz, GOMAXPROCS=5, Linux amd64, in a container on
  a shared host.
- Build: `1cf0cfd`, on `sdd/S0099-holdfast-streaming-enumeration`. Go 1.25.14.
- Date: 2026-09-18.
- Runs: 3, `-benchtime 1x -count=3`. The table reports the median of the three.
- Spread: peak heap 445,376 to 456,120 bytes at 100,000 paths and 442,672 to 456,496 bytes at
  1,000,000 paths; time to first file 3.05 ms to 4.84 ms and 3.59 ms to 4.19 ms respectively.

Ten times the library costs the same peak heap and reaches its first worker just as quickly.
Both figures are dominated by the constants: the peak is essentially the `observed` map for
10,000 directories, which is the same map at both sizes, and the time to first file is one
directory listing. The whole scan takes ten times as long, which is the linear part and is
the figure that is expected to scale.

### What it cost before

The same benchmark against a scan that materialises every path before feeding the first one -
the arrangement this replaced - taken the same way, on the same machine, on the same day:

| library | peak heap | time to first file | whole scan |
|---|---|---|---|
| 100,000 paths | 20.5 MiB | 3.8 s | 4.6 s |
| 1,000,000 paths | 200.4 MiB | 29.3 s | 37.4 s |

- Runs: 1, `-benchtime 1x -count=1`. One run each, so there is no spread to report: these
  figures exist to be an order of magnitude and not a baseline, and the build they were taken
  against was a deliberate local edit rather than a commit.

That build holds 460 times as much at a million paths and takes 29 seconds to reach its first
worker where this one takes four milliseconds - on storage ten times slower, which is what a
cold network mount is, that is minutes of a daemon looking hung. Both of its figures grow with
the number of files: ten times the library, ten times the heap and eight times the wait. Those
are the two shapes this document exists to keep apart.

### Running it again

The benchmark is deliberately not part of `make check` - the `test` target runs no `-bench`,
so this compiles on every gate run and executes on none of them. The target that runs it also
refuses a repository whose figures above have gone missing, or that records them without the
hardware, the build or the date they were taken on:

```sh
make check-enumeration-memory
```

It takes about a minute per run and writes nothing outside its own temporary directory: the
library is generated on demand through the scan's own listing seam, so a million paths cost
no inodes and nothing here touches real media.

A figure means something only against a stored previous figure taken the same way. Record the
machine, the build, the date, the number of runs and the spread beside any figure added here,
and state a change against the row above it rather than against a threshold.
