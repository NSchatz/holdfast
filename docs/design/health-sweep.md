# Health sweep

What the library health sweep does, what it never does, and why it is shaped the way it is.
This document is that argument's single home: `CLAUDE.md` names the rule and links here rather
than restating it. The keys are described in [`docs/profiles.md`](../profiles.md#health-sweep);
the code is `internal/health`, the engine's half is `internal/engine/healthsources.go`, and the
daemon's wiring is `cmd/holdfast/healthsweep.go`.

## The rule

<a id="health-sweep"></a>

**The health sweep reads every source and reports; it never moves, renames, deletes or repairs a
file.** Under `holdfast serve`, with `health_sweep_interval_hours` above 0, it fully decodes
every source the enumeration offers, once per interval, and records for each file `ok`,
`corrupt` or `unreadable` with the reason. The record is the whole of its output: the ledger,
`GET /api/health`, the `holdfast_health_sweep_*` metrics and one notification per sweep that
found a problem. With the key at 0 (the default) none of this runs and nothing is decoded.

The owner chose report-only (decision T29). Quarantine and a repair re-encode were considered and
not chosen, and neither is built: a file the sweep found damaged is an operator's decision, and a
tool that moved or rewrote it on the strength of a decode would be acting on a guess about the
cause. No route of the read API acts on a finding, and there is no command that does either.

## What "report-only" means here

The sweep holds no handle that can write a library file. Each check is three reads:

1. `stat` of the path - its size and modification time are the fingerprint the result is keyed
   against.
2. `open` for reading, closed at once - so a file this process may not read is reported
   `unreadable` rather than handed to ffmpeg and reported `corrupt`.
3. ffmpeg's decode to the null muxer, with the input named through the `file:` protocol. The
   null muxer writes nothing, and `-nostdin` keeps ffmpeg off the terminal.

Nothing is created beside a source: no temp, no sidecar, no marker. The ledger rows live in the
state directory, which is never under a library root. A read can move a file's access time where
the mount records one; that is the kernel's bookkeeping of a read, not a write by holdfast, and
it is why the proof below compares the modification time and not the access time.

The proof is `TestHealthSweep_IsReportOnly_NoFileMovedRenamedOrDeleted` in `internal/health`. It
builds a synthetic library (lavfi clips, a truncated copy, a copy with its middle overwritten, a
zero-byte file, a text file under a video name, a FIFO and a subdirectory), snapshots every
entry's name, size, modification time, mode, inode and sha256, sweeps it through the pinned
ffmpeg, and asserts the two snapshots are identical and that each file was classified as it
should be.

## What it decodes, and how it classifies

<a id="which-files"></a>

The sweep reads exactly the set a scan would offer: it is the engine's own enumeration
(`HealthSources`), so the path filters (`exclude_paths`, `include_paths`), the retention area,
the work-in-progress names and the record-based hold-backs apply to it as they apply to a scan.
A file a filter keeps out of the pipeline is kept out of the sweep.

The decode is the one `holdfast analyze --health` and the engine's decode-integrity gate run:
every video stream (`-map 0:v`) to the null muxer with `-xerror -err_detect +explode`, so a
concealable decode error fails the file instead of being hidden. Audio streams are demuxed and
not decoded.

<a id="classify"></a>

| Result | When |
| --- | --- |
| `ok` | ffmpeg exited 0 and printed no error. |
| `corrupt` | ffmpeg ran and either exited non-zero or printed an error. The reason is the last line it printed. |
| `unreadable` | The path could not be stat'd or opened for reading, is not a regular file (a FIFO is never opened: opening one for reading blocks), or its fingerprint moved during the decode twice in a row. |

The `corrupt` reading is stricter than the gate's on one point, deliberately. A truncated file
decodes up to where its bytes stop, prints `File ended prematurely` and exits 0. The encode-side
gate has length and packet checks of its own for that; the sweep has nothing else, so an error
ffmpeg prints is a failed file even when it exits 0. A false positive costs a line in a report.

A verdict is about one set of bytes. The fingerprint is taken again after the decode and, if it
moved, the file is decoded once more; a file that changes under both attempts is `unreadable`,
never `ok`. A file gone between the listing and the check is not recorded: there is nothing
there to report on.

No verdict is not a verdict. If ffmpeg cannot be started, or the daemon is stopping, nothing is
recorded for that file, the sweep stays unfinished, and it resumes at the next attempt.

## Scheduling, and its place below encodes

<a id="schedule"></a>

A sweep is due `health_sweep_interval_hours` after the previous one FINISHED, read from the
ledger, so a restart neither resets the interval nor forgets a sweep it interrupted. With no
sweep recorded, the first is due as soon as the daemon starts. The daemon asks once a minute
(ASSUMED fine-grained enough for an interval in hours and a window in minutes).

The sweep is lower in importance than an encode, and is wired to be:

- **It starts no decode the encode workers would not start.** Before every decode it asks the
  operator's pause and then the host-fair scheduler: the run window (`run_window`), the load cap
  (`max_load`) and the streaming pause (Tautulli). While any says no, it starts nothing new; a
  decode already running finishes, as an encode in flight does. A sweep that falls due outside
  the window opens no ledger row until the window opens, and one that crosses the window's end
  continues from the same file when it opens again.
- **It is small by default.** `health_sweep_workers` decodes run at once, one by default, at
  most 16.
- **It runs at the lowest CPU priority.** Each decode is set to nice 19 as it starts, so where
  an encode and a decode compete for the CPU the encode is favoured (the behaviour of nice(2),
  https://man7.org/linux/man-pages/man2/nice.2.html, read 2026-10-01). How much an encode loses
  to a niced decode on a given host is not measured here (ASSUMED).

It runs beside the encode workers rather than only between scans, because a library under
conversion may never be idle and a sweep that waited for idleness might never run. The
interactions an operator should know:

- A decode adds to the host's load average whatever its priority. With `max_load` set, a sweep
  decode counts toward the cap, so it can delay the next encode start. Keep
  `health_sweep_workers` at 1 on a host near its cap.
- A decode reads every byte of a file. On a spinning disk shared with encodes, it competes for
  the disk's bandwidth; the priority above is CPU priority only.

## Resuming

<a id="resume"></a>

Progress is in the ledger, schema v24: one `health_sweeps` row per sweep (started, finished,
and its counts once finished) and one `health_checks` row per file it checked, keyed by sweep and
path, carrying the size and modification time it was checked against, the time, the result and
the reason. A restart mid-sweep resumes the same sweep: a file already checked in it is skipped
while its size and modification time still match, and decoded again if either moved. A sweep is
finished only when the enumeration has been exhausted with every offered file checked.

When a new sweep starts, the per-file checks of every sweep older than the newest finished one
are dropped, so the ledger holds the sweep under way and the last finished one and does not grow
with every interval. The sweep rows, with their counts, are kept.

Nothing in the encode pipeline reads either table. What a sweep records cannot hold a file back
from an encode, license one or move a gate.

## Reporting

<a id="reporting"></a>

- **`GET /api/health`**, behind the read token like every other read: the sweep's state (`off`,
  `idle`, `running`, `waiting` with the reason), the sweep under way and the last finished one,
  each with its counts and up to 500 of the files it found corrupt or unreadable. Times are Unix
  seconds, and a time that does not exist is `null`. See
  [`docs/api-reference.md`](../api-reference.md#health).
- **Metrics:** `holdfast_health_sweep_files_checked_total{result}` counts results as they are
  recorded; `holdfast_health_sweep_corrupt_files`, `holdfast_health_sweep_unreadable_files` and
  `holdfast_health_sweep_last_completed_timestamp_seconds` describe the newest finished sweep and
  are absent until one has finished, because a library nobody has swept is not a library with no
  corrupt files.
- **One notification per finished sweep that found a problem**, through `notify_url`, naming up
  to ten files with their reasons and counting the rest. A clean sweep sends nothing.
