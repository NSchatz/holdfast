# What bounds an encode's memory

An encode that grows toward the container's memory limit fails fast instead of sitting at
the limit. This document says what holdfast does about it, and records the attempt to
reproduce the growth that led to it.

## The watchdog

At startup `serve` and `run` read the memory limit of the cgroup holdfast runs in: cgroup
v2's `memory.max`, walked from the process's own cgroup up to the mount root, the smallest
numeric value on that walk winning and `max` meaning that level sets none. The same
`HOLDFAST_CGROUP_ROOT` that steers the CPU quota reading steers this one. The threshold is
**85% of that limit**, rounded down to a whole byte, and it has no configuration key.

The run states what it established in exactly one record, component `encode.memory`:

- `info` `encode memory watchdog armed`, with `memory_limit_bytes` and
  `memory_threshold_bytes`, where a limit was found.
- `info` `encode memory watchdog not armed: no cgroup memory limit is set`, where every
  `memory.max` says `max` or none exists (a cgroup-v1-only host among them).
- `warn`, naming the `memory.max` it tried and what it read there, where one exists and
  cannot be read or does not parse. Every encode then runs unwatched.

While an encode's ffmpeg runs, its resident memory (`VmRSS` in `/proc/<pid>/status`, the
figure the kernel reports for that process) is sampled once a second. At or above the
threshold the process is sent `SIGTERM`, and `SIGKILL` three seconds later if it is still
there. The job is recorded `failed` at the `encode` gate, failure class `transient`, with a
reason giving the resident bytes, the threshold and the limit, and one `warn` record
`FAIL (encode aborted for memory, source untouched)` carries the same figures. The working
file is removed, the source is not touched, and `max_failures` decides whether the file is
tried again. An aborted encode is failed even where ffmpeg exits 0 after being asked to
stop.

The trigger is the encode's own resident memory, never the cgroup's `memory.current`: that
counts page cache and tmpfs, which a long encode fills legitimately and the kernel reclaims.
A sample that cannot be read (the process has just exited) decides nothing. The watchdog
never cancels the job's context, so a `serve` drain on `SIGTERM` is still an interruption
and a memory abort is still a failure. It watches the encode's ffmpeg only; the VMAF
measurement's ffmpeg is not watched.

Where no limit can be established nothing is aborted, and holdfast encodes exactly as it did
before the watchdog existed. The watchdog protects liveness, not the no-loss invariant: the
verify gates are what stand between an encode and the swap, and an aborted encode never
reaches them.

## The mux-queue bounds

Every ffmpeg invocation carries three output options, applying to every output stream:
`-max_muxing_queue_size 128`, `-muxing_queue_data_threshold 52428800` and
`-thread_queue_size 8`. Each is the pinned ffmpeg's own default for that queue, so passing
it never loosens a bound; it makes the bound a property of holdfast's command line rather
than of the ffmpeg build that runs it. The reproduction below found that these queues are
not where the memory goes.

## The reproduction attempt

The report this answers: on one cover-art film, ffmpeg's resident memory reached 7.1 GB in
an 8 GB cgroup, `memory.events max` reached 19,048, the encoder used under one core, and the
output grew about 440 MB a minute for the 4.5 minutes before the attempt ended. The next film
encoded normally at about 15 cores and 2.7 GB. The hypothesis was a demux or mux queue
buffering without bound while the video encoder was starved.

### What was run

- **Source**, generated for the run and never committed: Matroska, one H.264 video stream at
  3840x2160, 24 fps, 60 seconds, 20 Mbit/s, `yuv420p`; one attached picture, a 1000x1500
  MJPEG cover carried as a Matroska attachment (`image/jpeg`), which ffprobe reports as an
  attached-picture video stream; one AC-3 audio stream, 48 kHz, 384 kbit/s; one SubRip
  subtitle stream of 30 cues.
- **Command**: the argv the production `FFmpegEncoder` builds for that source under the
  default configuration (libx265, preset `slow`, CRF 22, `yuv420p10le`, Matroska output, the
  cover copied out and attached), captured from the encoder itself and replayed. No
  `pools` or `frame-threads` were passed, which is the argv the reporting build produced;
  libx265 therefore sized itself from the host's CPU count.
- **CPU constraint**: `taskset -c 2`, one core for the whole ffmpeg process.
- **Measurement**: `VmRSS` sampled once a second for a fixed window (75 seconds, and one run
  of 240), then the process killed; the peak is the largest sample. Runs were one at a time.
  "Without the bounds" is the same argv with the three options removed.
- **Machine**: Intel Xeon E5-2680 v4 at 2.40 GHz, 56 logical CPUs visible, 157 GiB host
  memory, Linux 6.12.107, inside a container whose cgroup allows 5 CPUs and 16 GiB.
- **Build**: ffmpeg `N-125875-g5d4d3bdc61`, the build the Dockerfile pins.
- **Commit**: `68db9f7`. Every argv replayed is byte for byte the one that commit builds.
- **Previous figure**: none. This is the first measurement of this kind in this repository,
  so it is the figure a later one is compared against, taken the same way.

### Figures

| run | runs | peak ffmpeg resident memory, mean | spread (max - min) | output written |
|---|---|---|---|---|
| with the bounds | 3 | 4146 MiB | 6 MiB | 0 bytes |
| without the bounds | 3 | 4157 MiB | 6 MiB | 0 bytes |
| with the bounds, decoder at `-threads 1` | 1 | 4153 MiB | single run | 0 bytes |
| with the bounds, libx265 `pools=1:frame-threads=1` | 1 | 2650 MiB | single run | 0 bytes |
| with the bounds, 240-second window | 1 | 4407 MiB | single run | 10.7 MB |

The first four rows are 75-second windows. In every run the resident memory rose steeply from
the start (about 2.1 GiB at 5 seconds, 2.85 GiB at 10) and reached 98% of its 75-second peak
between 40 and 44 seconds. The process used the whole of its one core throughout, and libx265
had not produced a packet by the end of any of those runs, so nothing was written to the
output.

The 240-second run follows the same curve and keeps flattening: 4139 MiB at 60 seconds, 4317
at 120, 4400 at 180 and 4407 at 235. Its first output appeared between 60 and 90 seconds, and
the file was 10.7 MB after four minutes, a few MB a minute.

### Verdict

**The reported growth did not reproduce.** A starved 4K encode does reach several GiB, but
most of it is there before any output is written, it levels off near 4.4 GiB, and neither the
memory nor the output grows with time the way 7.1 GB and 440 MB a minute of output describe.

**No mux or demux queue grew.** Removing the three bounds changed the peak by 11 MiB, inside
the spread between runs, and single-threading the decoder changed nothing. What moved the
figure was libx265's own threading: one pool thread and one frame thread took 1.5 GiB off the
peak. The memory is libx265's per-frame-thread and lookahead buffers, sized from the host's
CPU count - which is what the `pools` and `frame-threads` holdfast now derives from the
cgroup CPU quota or the `x265_cpus` key address - rather than a queue an ffmpeg option
bounds.

So the bounds make the documented queues explicit, and the watchdog is what bounds the
outcome. Whether the reported file reaches the threshold or encodes normally under the
landed build is the operator's to run against that file.
