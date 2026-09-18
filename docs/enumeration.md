# How a scan enumerates

A scan lists the library and hands what it finds to its workers as it finds it. The first
file reaches a worker while the tree is still being read, and what the scan holds does not
grow with the number of files in the library. This document says which file goes first, and
what the arrangement costs.

## The order files are handed out in

<a id="enumeration-order"></a>

A scan hands source files to its workers directory by directory, in the order the coverage
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
the cost this arrangement exists to remove.

The read-only pass behind `holdfast plan` traverses the library the same way and reports what
it found in that same sequence. There is one hand-out order in this repository, so a plan
predicts the order a scan will work in as well as the set of files it will offer.

### What a declared queue order may build on this

An order that needs a key rather than a position - largest first, newest first - is a
separate decision layered on this one, and it needs no change here. What this rule
guarantees a later order is that the enumeration yields a stable, total sequence of candidate
paths without materialising them, so an order that can be decided from a key may keep
`(key, path)` pairs alone and state its own memory bound, and an order that is already this
sequence keeps nothing at all.

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
