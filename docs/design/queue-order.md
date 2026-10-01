# Queue order

Which file a scan offers its workers first: the queue `priority` an operator writes on a library
root, a resolution rule or an encode profile, then the declared `queue_order`, then the path. This
document is that argument's single home: `CLAUDE.md` names the rule and links here rather than
restating it. The keys are described in [`docs/profiles.md`](../profiles.md#queue-priority) and
[`docs/enumeration.md`](../enumeration.md#declared-queue-order); the code is
`internal/engine/queueorder.go`, `internal/config/priority.go` and `internal/queuekey`.

## The rule

<a id="queue-order"></a>

**The queue decides only the order files are offered in: priority first, then the declared order,
then the path - never which files are offered, and never anything about a file once it is.** The
coverage bound, the path filters and the record-based hold-backs settle the set of candidates
before any ordering runs; every one of them is offered exactly once whatever the queue says, and
the guards, the gates, the encode's command line and the terminal row a file earns are what they
would have been in any other position.

The only harm an order can do is to membership - a candidate dropped is never processed, one
offered twice is raced - so membership is pinned: a candidate whose key or priority cannot be read
is offered LAST, in path order among its kind, with one `warn` record naming the file, the order,
what was tried and what happens next. It is never dropped, and it never fails the pass.

With no `priority` written anywhere and `queue_order: path` (the defaults), the enumeration's own
stream is the queue: nothing is held per candidate, nothing is read for ordering, and the first
file reaches a worker while the library is still being listed. Every configuration written before
either key existed is offered in exactly the sequence it was.

## Priority

<a id="priority"></a>

`priority` is a whole number from -1000 to 1000, default 0. **Higher is offered first.** It may be
written in three places, and a file's priority is the FIRST of these that names one:

1. the resolution rule that decides the file - the first rule of its root whose `when` admits the
   source, exactly the rule that decides its knobs (a rule may name only a priority: it orders
   the files it takes, and under first match it takes them from every later rule, which
   `holdfast validate` shows by printing the list in order);
2. the encode profile that decides the file - the first whose `match` selects it;
3. the library root the file lives under (a `library_roots` entry, beside `path`).

Otherwise it is 0. First match wins for priority as for every knob: a matching rule that names no
priority hands the question to the encode profile and the root, never to a later rule, and a
matching encode profile that names none hands it to the root, never to a later profile.

**It decides sequence and nothing else, so it is recorded nowhere a decision is.** It is not a
profile knob and not a rule knob: it is in no profile digest, in no rule's canonical text, in no
decision input and on no terminal row. Adding, changing or removing a priority therefore re-opens
no row - a library half processed under one set of priorities is not re-encoded because the set
changed (`TestQueueOrder_PriorityIsInNoDecisionInput`, `TestPriority_ReopensNoRowAndMovesNoDigest`).
What it does not cover is a NEW RULE: adding a rule - even one that names only a priority - changes
which rule decides the files its band takes, and so their knobs, which is a change to the rule list
and is judged as one (its band enters the rule list's canonical text). Writing a priority on a rule
that already exists moves nothing.

It is never a top-level key: there is nothing at the top level for a priority to select, and a
`priority` there (or `HOLDFAST_PRIORITY`) would read as though it did something. It is refused at
start, saying where the key belongs. A value outside the range, a fraction, a word or a key with no
value is refused naming the key and where it was written.

A rule's priority under a banded `when` needs the source height to know which rule decides a file.
That height is read once per candidate per pass, through the ordering's one probe: under
`savings_per_hour` it is the same probe the key is read from, and under any other order it is a
probe of that root's candidates alone, taken before the first is offered. A root whose rules name no
priority reads no height for this, whatever its bands. A candidate whose height cannot be read is
offered last, with the warn record above.

Under `path` with a priority configured, files of equal priority keep the traversal's own order -
the key is each candidate's position in it - and the queue is held until the listing is finished,
because the highest-priority file may be the last one listed. `path` streams only where no priority
is written anywhere.

## Savings per hour

<a id="savings-per-hour"></a>

`queue_order: savings_per_hour` offers first the source whose encode is estimated to reclaim the
most bytes per hour of the work it costs: the encode, plus the full decode-integrity read of the
output, plus the VMAF comparison. Size is a poor proxy for that (S0164, the operator's report quoted
in the umbrella spec): the bytes an encode saves depend on the source's bitrate for its picture,
not on its size, and largest-first also puts the slowest jobs first.

**The estimate orders and is never published.** No per-file estimated saving appears in `plan`,
the API, a metric or a log record; `README.md` rules it out deliberately. The key-reading pass logs
how many keys it read and how many it could not, never a figure.

### The inputs

Read once per candidate per pass, from the snapshot probe every job already takes
(`probe.VideoProps`): the source's video bitrate (the stream's, else the container's), its coded
width and height, and its container duration. Any of them missing, zero or not finite - or the probe
finding no video stream, failing, or the file having vanished - is an unreadable key.

From the configuration that would decide the file: the root's profile with its first matching rule
laid over it (the height is in the snapshot), the job's resolved `bitrate_kbps` (the matching
encode profile's, else the top level's), the output picture after any `max_height` ceiling, and
`remux_only`.

### The formula

    pixels       = width x height                                  (the source's)
    outPixels    = the output picture's width x height (the source's own unless a ceiling applies)
    OutputKbps   = source kbit/s                                   under remux_only
                 = bitrate_kbps                                    where it is set (> 0)
                 = BitsPerPixel x outPixels x FrameRate / 1000     otherwise
    SavedBytes   = (source kbit/s - OutputKbps) x 1000 / 8 x duration
    Frames       = duration x FrameRate
    WorkSec      = Frames x pixels x (1/Encode + 1/Decode + 1/VMAF)
    BytesPerHour = max(1, floor(SavedBytes / WorkSec x 3600))      where SavedBytes > 0
                 = 0                                               otherwise

The queue is sorted by priority descending, then `BytesPerHour` descending, then the full path
ascending. A source whose saving is not positive keys 0, so it is offered after every source with a
positive saving and in path order among its kind; it is still offered. The duration appears in both
`SavedBytes` and `WorkSec` and cancels from the ratio: two sources of one picture size and bitrate
earn one key whatever their lengths - a long film is worth more in all, never more per hour of work.

### The constants

| constant | value | where it comes from |
|---|---|---|
| `FrameRate` | 24000/1001 frames per second | ASSUMED. The snapshot probe does not read the frame rate, and adding a field to it would change the probe every job takes. It is the same for every candidate, so it scales every key alike and changes no ranking. |
| `BitsPerPixel` | 0.065 | Derived, not measured here. S0164's operator report: a 1080p source at 21.6 Mbps saves about 85%, i.e. comes out near 3,240 kbit/s; 3,240,000 / (1920 x 1080 x 24000/1001) = 0.0652, rounded to 0.065. One figure serves every encoder family: no per-family figure is measured here, so none is invented (ASSUMED for `av1` and `h264`). |
| `Encode` | 5,730,000 source pixels per second | Measured here 2026-10-01 (below). |
| `Decode` | 104,000,000 source pixels per second | Measured here 2026-10-01 (below). |
| `VMAF` | 10,300,000 source pixels per second | Measured here 2026-10-01 (below). |

The three throughputs were measured here 2026-10-01 with the pinned ffmpeg (N-125875-g5d4d3bdc61)
on 4 CPUs of an Intel Xeon E5-2680 v4, under the heavy lock, over one synthetic clip of 120 frames
of 1920x1080 (248,832,000 pixels), at the shipped defaults:

    ffmpeg -f lavfi -i "testsrc2=duration=5.005:size=1920x1080:rate=24000/1001,noise=alls=12:allf=t" \
      -frames:v 120 -c:v ffv1 -pix_fmt yuv420p src.mkv
    ffmpeg -i src.mkv -c:v libx265 -preset slow -crf 22 -pix_fmt yuv420p10le out.mkv   # 43.42 s
    ffmpeg -i out.mkv -f null -                                                        #  2.39 s
    ffmpeg -i out.mkv -i src.mkv -lavfi "[0:v]format=yuv420p10le[d];[1:v]format=yuv420p10le[r];[d][r]libvmaf" -f null -   # 24.06 s

248,832,000 / 43.42 = 5,730,815; / 2.39 = 104,113,808; / 24.06 = 10,342,145; each rounded to three
significant figures. Real content encodes at a different speed than a synthetic clip, and another
host at another; the absolute figure is not a promise about any library. It does not need to be
one: the three throughputs are the same for every candidate, so the ranking depends on them only
through the work being proportional to pixels times frames.

### Worked example

S0164's AC-5, the operator's own case: a 1920x1080 source at 21.6 Mbps lasting 45 minutes (about
7.3 GB) against a 1920x1080 source at 6.1 Mbps lasting 180 minutes (about 8.2 GB), neither under a
bitrate target or a ceiling. `largest` offers the 180-minute source first. Step by step:

| step | 21.6 Mbps, 45 min | 6.1 Mbps, 180 min |
|---|---|---|
| size, bitrate x duration / 8 | 21,600 x 1000 x 2,700 / 8 = 7,290,000,000 bytes | 6,100 x 1000 x 10,800 / 8 = 8,235,000,000 bytes |
| pixels | 1920 x 1080 = 2,073,600 | 2,073,600 |
| `OutputKbps` = 0.065 x 2,073,600 x 23.976024 / 1000 | 3,231.58 kbit/s | 3,231.58 kbit/s |
| excess, source - `OutputKbps` | 18,368.42 kbit/s | 2,868.42 kbit/s |
| `SavedBytes` = excess x 125 x duration | 6,199,340,260 bytes (85.0% of the source) | 3,872,361,039 bytes (47.0%) |
| `Frames` = duration x 23.976024 | 64,735.26 | 258,941.06 |
| work per pixel = 1/5,730,000 + 1/104,000,000 + 1/10,300,000 | 281.2228 ns | 281.2228 ns |
| `WorkSec` = `Frames` x 2,073,600 x 281.2228 ns | 37,749.96 s (10.49 h) | 150,999.84 s (41.94 h) |
| `BytesPerHour` = floor(`SavedBytes` / `WorkSec` x 3600) | **591,195,994** | **92,321,289** |

So `savings_per_hour` offers the 45-minute source first - it reclaims about 6.4 times as many bytes
per hour of work - where `largest` offers the 180-minute one first. `TestEstimate_ReproducesTheWorkedExample`
asserts the estimator reproduces every figure in this table to the rounding printed and the key
exactly, and `TestQueueOrder_AC5_TheOperatorsCase` asserts both orders over two real files of those
sizes.

### What it costs

Every candidate is probed once, before the first is offered: `savings_per_hour` does NOT reach its
first worker before the library has been listed and every key read, and `path` remains the only
order that streams. On a large library that is minutes, so the pass logs a progress record at least
once per 1,000 candidates and, before the first candidate is offered, one record of how many keys
were read and how many could not be. A pause or a cancellation while keys are being read stops the
reading - no probe starts after the stop is observed - and offers nothing in that pass; the next
pass reads every key again. The read-only `plan` pass counts the ordering's probes into the
snapshots it reports. Nothing here is cached between passes.
