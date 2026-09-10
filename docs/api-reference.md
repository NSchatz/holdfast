# API and record reference

What every field on a job row means, what the whole-ledger figures are computed
over, how the ledger is bounded, how to take the record elsewhere, and the
observability and scheduling surfaces. Moved out of `README.md`, unchanged.

### The recorded outcome - the proof a swap was safe

A terminal job carries the evidence the engine used to decide, so you can audit a swap after the fact
instead of trusting it. Every terminal row in `/api/history` (and in the SSE snapshot) reports:

| Field | On | What it is |
|---|---|---|
| `reason` | failed | the error that rejected it (the encode error, or **which gate** refused the output) |
| `reason` | skipped | **which guard** fired - `already-at-target-codec`, `low-bitrate`, `hardlinked`, `symlinked-source`, `interlaced`, `dolby-vision`, `hdr10-plus`, `incomplete-hdr-metadata`, `exotic-pixel-format`, `multi-video-stream`, `target-already-exists`, `undo-retention-failed`, `restored-original` |
| `encoder` | any job that reached the encoder | the encoder that ran (`cpu`, `svtav1`, `nvenc`, …) - a skip, or a file with no readable video stream, never gets that far and records none |
| `vmaf_mean`, `vmaf_min` | done, and a VMAF-rejected failure | the pooled harmonic mean **and the worst frame** |
| `vmaf_model` | as above | the libvmaf model that produced them |
| `vmaf_pix_fmt` | as above | the single pixel format **both streams were converted to** before scoring - chosen and named by holdfast, so a score says which pixels were compared |
| `vmaf_stream` | as above | **which video stream** of each file was compared, in the specifier every ffprobe read here uses: `v:0`, the first video stream. A source can carry more than one, so this is what lines a score up against the file it was measured on. Absent on a row whose gate never ran - never a fabricated `v:0` |
| `vmaf_chroma`, `vmaf_chroma_metric` | as above | the worst frame's chroma measurement and what it is (`psnr_cb/psnr_cr min (dB)`) - the only figure on the row that says whether the **colour** survived |
| `source_codec` | would-transcode | the video codec the SOURCE was in when a dry run decided it - `null` when it was never read |
| `source_bytes`, `output_bytes` | done | the sizes either side of the swap |
| `source_bytes` | would-transcode | the size of the file that was decided. `output_bytes` is `null`: nothing encoded it, so there is no output to have a size |
| `encode_ms` | done, and a failure after the encode ran | wall-clock encode time |
| `guard_attributes`, `guard_time_resolution` | any job that reached the swap | which source attributes the source-mutation guard compared (`size,mtime`) and the resolution of the timestamp it compared (`1s`) - the granularity that check actually achieved |
| `guard_residual_window` | as above | which of the two documented residual windows applies to the storage the guard ran against: `residual-window-local` or `residual-window-network`. A **class label**, never a duration - see [docs/filesystem.md](docs/filesystem.md#residual-window-local) |
| `swap_cause` | a swap failure with a distinct cause | today only `cross-filesystem` - the temp and the target were not on the same mounted filesystem. Absent for every other failure |
| `library_root` | any row this build decided | the **cleaned path of the library root** whose profile decided the file. `null` when it was not recorded: a row written before per-library profiles existed, or one no profile decided (a `restored-original` skip is an operator's act, not a gate's) |
| `profile_digest` | as above | a stable identifier for that root's **resolved** overridable knobs. `null` on the same rows `library_root` is null on |

**A `null` means "not recorded", and you must read it that way.** It is never a zero. A numeric field is
`null` - not `0` - whenever the fact was not measured (VMAF disabled, or a row written before these
columns existed), because a VMAF of `0.0` is a *destroyed frame*, not a missing measurement, and rendering
one as the other would be inventing evidence about a swap nobody checked.

**A VMAF score is not interpretable without its model, the format it was measured in or the stream it was
measured on**, which is why they all travel together. Read `vmaf_mean`/`vmaf_min` with the limits in mind: VMAF is a regression onto
a *subjective* opinion scale under one viewing condition, `vmaf_v0.6.1` is **luma-only** (structurally
blind to chroma damage - that is what `vmaf_chroma` is for), and the scores are **not comparable across
different sources**. The number bounds measured perceptual quality against *your* source; it is not a
proof of fidelity.

**The comparison format is a fact, not a guess.** `pixel_format: auto` floors output bit depth at 10, so
an 8-bit source and its replacement routinely disagree - and upconverting the source is not the same
measurement as downconverting the output. holdfast converts both streams to one named format before
scoring (the richer chroma subsampling of the two, at the deeper of the two bit depths, so nothing is
averaged or quantised away on the way in) and records it in `vmaf_pix_fmt`. The same source and output
scored twice are compared in the same format both times.

**The scored stream is a fact too, and it is pinned rather than inferred.** Every ffprobe property read
selects `v:0` and the decode-integrity check decodes `0:v:0`, so the quality gate names the same stream
explicitly in its filtergraph and records it in `vmaf_stream`. On a file carrying more than one video
stream that is what stops the gate measuring a stream the other checks never inspected, and what lets a
reader of the row say which one it was. It is not configurable, for the same reason the probes' stream is
not.

**Which library profile decided the file is part of the record, because it is no longer derivable from
the configuration.** Each `library_roots` entry may carry its own encoder, crf, bitrate floor and VMAF
floors, so the file was judged by *one* of several profiles and the configuration cannot say which. The
row therefore carries both `library_root` and `profile_digest`, and the digest is what makes the pair
survive an edit: the path alone would go on naming `/mnt/tv` after `/mnt/tv` had been changed to mean
something else. Two rows decided under identical resolved values carry the same digest; a row decided
under any different value carries a different one. Run `holdfast validate` to see which of your roots
currently digests to what, beside the resolved value of every knob and the layer that supplied it.

An outcome is recorded per *attempt*, not per file: **claiming a job for a retry clears it**, so a file
that is being re-encoded never advertises the rejected attempt's score while it is in flight.

### The decision inputs a row was taken under

Beside the proof, a terminal row records **the configuration values the decision that wrote it actually
read** - and only those. It is what makes a row re-derivable rather than permanent: a scan offers a
`done` or `skipped` file back to the guards when the values it recorded no longer match the
configuration in force, so an edit to the YAML reaches the files a previous configuration already
answered. Per-guard table, the re-opening rule, and the `holdfast requeue` lever for the rows a
configuration change cannot reason about: **[docs/requeue.md](requeue.md)**.

These values are internal to the store and are **not published on the HTTP surface**, and neither is
requeue: it changes what the engine will do to a media file, which is the same reason `restore` is not
an endpoint either.

### `would-transcode`: what a dry run decided

`dry_run: true` is how you answer "which files would this transcode?" before you let holdfast delete
anything. Every guard runs and **nothing is encoded, swapped or deleted**; a file that passes every guard
is one a run with `dry_run: false` would transcode, and that conclusion is **recorded** as a terminal
`would-transcode` row carrying that file's **source codec** and **source size**. Without that record those
files sit in the state the worker parked them in while deciding (`probing`), so the summary, the outcomes
breakdown and `/metrics` all report the run as having concluded nothing, and an operator reading that page
concludes "nothing qualifies".

Two properties are load-bearing and neither is negotiable:

- **It counts decisions, never transcodes.** Nothing has encoded these files, so the row carries no output
  size, no percentage reclaimed and no VMAF, and the dashboard shows no projected saving anywhere. The
  figure beside the candidate rows is the **total source bytes** they account for and nothing else: the
  size of what is under consideration, with the rows it left out for want of a recorded size counted and
  reported beside it.
- **It is terminal, but re-claimable.** Unlike `done` and `skipped`, a recorded decision does not exclude
  the file from a later run: set `dry_run: false`, run again, and exactly the files that list named are
  the files that get transcoded. Two dry runs over an unchanged file still report **one** candidate.

The **dashboard renders all of this per file** - size before → after and percent reclaimed, the encoder,
the encode duration, and the VMAF pair shown with its model, its pooling and its luma-only blind spot - so
the proof is on the page, not only in the JSON. A skipped row names its guard; a failed row shows its
reason; a fact that was never recorded reads "not recorded", never `0`.

### An in-flight job - how far it has got

A terminal row says what happened; an **active** row says what is happening. Every job in `/api/queue`
(and in the SSE snapshot's `queue`) carries `updated_at`, the timestamp of its last transition, and the
snapshot carries `now`, the server's clock when the frame was built - together those are how the
dashboard shows **how long a file has been in the state it is in**, recomputed from the timestamp on
every tick rather than counted up in the page.

An **encoding** row additionally carries what the encoder itself reports, read from ffmpeg's documented
`-progress` stream rather than estimated from elapsed time:

| Field | What it is |
|---|---|
| `progress_seconds` | the encoder's position in the source timeline |
| `progress_duration_seconds` | the source duration that position is measured against |
| `progress_fraction` | the two divided, in `[0,1]` |

**Only** an encoding row. A figure here is a measurement taken by the encoder, so it is live exactly
while that encoder is running: the moment a job moves on - to `verifying`, or back to `pending` after a
crash, or to a terminal state - all three fields go back to `null`, because the process that produced the
figure has exited and nothing is measuring the verify phase. A carried-over percentage frozen beside a
state it does not describe is the one thing this surface must never show.

The same `null` rule applies, and it bites harder here: an encoder that has not reported yet, and a
source whose container reports no duration, are both **unrecorded**, and a `0` would read as "0% encoded"
- a figure nobody measured. The dashboard shows those as *unknown*. Progress is **not persisted**: it is
state about a running process, so after a restart an in-flight job simply has none reported yet, and a
finished row never carries one. There is deliberately **no ETA** - every figure here is measured, and a
predicted finish time is not.

The reclaimed figure is a **durable lifetime total** (`bytes_reclaimed_lifetime`): a one-time baseline
summed from the recorded `source_bytes`/`output_bytes` on every done row, plus this process's reclaims - so
it survives a restart rather than resetting to zero. `bytes_reclaimed_session` is kept alongside it as the
honest this-run number.

**Known limitations.** Rows written before these columns existed carry no outcome and read as "not
recorded" - a measurement never taken cannot be reconstructed, and such a row also contributes nothing to
the lifetime total (never counted as a zero-reclaim). Queue/history views are still capped at the most
recent rows, not the whole ledger - each now reports the total it was capped against (see *The total behind
a cap*), the aggregate figures below are over the whole table, and `holdfast export` writes all of it.

### Whole-ledger figures

The queue and history views ship at most a few hundred rows, so any statistic derived from that payload
would describe the most recent files while looking exactly like a statistic about your library. Every
published figure is therefore computed **in the server, over every matching row in the `jobs` table**, and
rides both `GET /api/summary` and the SSE snapshot under `aggregates`:

| Figure | What it is |
|---|---|
| `outcomes` | how many rows reached each terminal status (done / skipped / failed) |
| `skips_by_guard` | every skipped row broken down by **which guard** skipped it |
| `size_ratio` | replacement size as a fraction of the original (0.35 = 35% of the original), low / mean / high |
| `encode_ms` | recorded encode wall-clock time, low / mean / high |
| `vmaf_mean`, `vmaf_min` | the spread of the two pooled VMAF statistics across files |

Each one carries the same envelope, and every part of it is load-bearing:

- **`covers`** names the SET the figure is over, and **`window`** is `""` unless the figure is bounded, in
  which case it names the bound. A number whose set is unstated gets read as covering everything you own.
- **`counted`** is how many rows contributed a value; **`excluded`** is how many matching rows recorded
  none. An unrecorded value is **excluded and reported**, never read as `0`: a VMAF of `0.0` is a destroyed
  frame, and an absent size would invent a 100% reclaim.
- **`min` / `mean` / `max`** are `null` (never `0`) when `counted` is 0. A figure nothing contributed to is
  "no data", not an average of zero. They are deliberately **not** a median or a percentile: those SQL
  functions are gated on the SQLite version AND a build flag, and a query that resolves on one build and
  fails on another is a runtime failure on somebody else's machine.
- **`available`** is `false` when the figure could not be read at all, with a fixed `unavailable`
  statement. One unreadable figure never suppresses the rest: the summary, the queue rows and the history
  rows still ship, the SSE broadcast still fires, and the dashboard draws that one card as unavailable
  while the rest of the page renders.

The dashboard shows all of it under **Across the whole ledger**, each figure beside the set it covers and
the count of rows it had to leave out.

#### When holdfast cannot tell what the swap did (`indeterminate`) - and how you get out of it

The swap is an atomic `rename(2)`, and on a **local** filesystem a failed rename means the source is
still there. On a network one it does not: `rename(2)` says outright that "on NFS filesystems, you can
not assume that if the operation failed, the file was not renamed" - a retransmitted request can report
a failure for an operation the server already performed. So after *every* failed swap holdfast re-stats
the source path and decides between four outcomes, and only one of them may say the source is untouched:

| Outcome | What it means |
|---|---|
| `failed` | the swap failed AND the re-stat confirmed the source untouched - which requires the storage to be **positively identified as local**, because a client attribute cache populated before the swap returns the pre-swap answer either way |
| `applied-despite-error` | the rename returned an error but the re-stat established that it **took effect**: the file at the source path is the replacement. Nothing is re-attempted, nothing is deleted, and a later run treats that path normally (the ordinary already-at-target-codec guard skips it) |
| `indeterminate` | holdfast **cannot establish** what happened. The job is **parked**: both files are kept, and nothing encodes, swaps, deletes or re-queues either path - in this run or any later one - until you say what happened |

An unrecognised filesystem counts as **not local**: a false warning costs you a look, a false clear costs
you a film. The set this build recognises as local is the **same one** the startup check prints and
[docs/filesystem.md](docs/filesystem.md#filesystem-types-this-build-classifies-local) states - there is
exactly one such set in the binary, read by the startup check and by the swap-time and guard-time
lookups alike. NFS and SMB/CIFS are known to be network-backed; anything in neither set is
undetermined, and therefore not local.

A parked job is reported at the start of every run, naming both files, and:

```bash
holdfast resolve --config config.yaml            # list every parked job
holdfast resolve --config config.yaml --id 3     # report one: both paths, and what is at each RIGHT NOW
holdfast resolve --config config.yaml --id 3 \
    --determination swap-was-applied \
    --replacement delete                         # record what happened, and dispose of the replacement
```

The report is never conditional on observing either file: a recorded path with nothing at it, or one
that cannot be inspected, is reported as exactly that and the job is still resolvable. The record is
made **durable before** anything is removed, so a store failure costs you a repeated instruction and
never an unrecorded deletion - and a replacement holdfast kept is never handed back to enumeration as if
it were a source. Files holdfast retained this way are named `*.__holdfast-replacement__.*` and are left
alone by every later run, whether or not a record of them survived.

Moving a replacement to that name is itself a write into the media directory, and the failure that
strands a replacement is often the same failure that denies the write - a library that has gone
read-only refuses the swap, the move to the held-back name and the record in the job store alike. So a
replacement can end up left at its `*.__transcoding__.*` working path with nothing recorded about it.
holdfast still will not touch it. The stale-temp sweep that reclaims a killed run's half-written encodes
**examines** each one rather than assuming it is disposable: a file at that path whose content is a
finished encode at the target codec, the length of the source beside it, is kept, reported at every
subsequent run, and stepped around when a fresh encode of the same source picks its own working path.
Removing it is your call, not the tool's. A genuinely half-written encode is shorter than its source and
is still swept, exactly as before.

When `container_ext` makes the output's extension differ from the source's, the swap's target is a
**different path** from the source, so a rename that took effect while reporting an error leaves the
replacement *there* rather than at the source path. holdfast follows the file: a parked job records where
it actually is and retains it under the held-back name, and where the source is instead established
untouched the recorded reason names the second file, so a duplicate is never something you have to
notice for yourself (the next scan's collision guard reconciles it).

### Bounding the ledger - `history_retention_rows` (off by default)

The `jobs` table only grows: one terminal row per file holdfast has finished with, for the life of the
install. At library scale that record becomes unbounded, so there is a bound - and it **ships disabled**.

```yaml
history_retention_rows: 0     # the DEFAULT, and what an absent key means: keep every row
# history_retention_rows: 50000   # keep at most 50,000 terminal rows; prune the oldest beyond it
```

With a value `n > 0`, holdfast brings the terminal rows back within `n` **after each scan completes** - no
operator action, no API call, no separate command. A negative or fractional value is a **startup refusal**
naming the key and the value, before the job store is opened.

**A prune cannot be undone, and that is why the default is 0.** Those rows are the record of what holdfast
did to your library *after it deleted your originals*. Nothing recreates them: a later scan re-derives the
file's **current** state instead, so a pruned `done` row for a file still on disk comes back as
`skipped / already-at-target-codec` - proof that the file is at the target codec, not proof that holdfast
put it there. **Export before you bound it** if the record matters to you.

What a prune will never do, whatever you set:

- **It cannot lower the lifetime reclaimed total.** A removed row's contribution to
  `bytes_reclaimed_lifetime` is carried forward durably, in the same transaction that deletes the row, so
  the figure is identical either side of a prune - on the running server *and* after the restart that
  re-reads it from the database. (That second half is where a naive prune fails silently: the server reads
  the total once, at startup, so deleting contributing rows shows a correct figure until the next restart.)
- **It cannot cause a file to be encoded again.** A terminal row is a *decision*, not only a record: it is
  what holds that file out of the encoder on every later scan, and the guards that would re-derive the same
  verdict run under whatever configuration is current. Deleting it therefore hands the file back with no
  recorded verdict at all - which is a re-encode outright for a row parked at `max_failures` (the count was
  the only thing holding it) and for a `skipped / restored-original` row (see below). **So a row is only ever
  removed when the scan listed the directory that file should be in and the file was not there**: gone, or
  replaced by different content. Retention bounds what your library has *finished with*.

  A row whose recorded decision inputs have MOVED is re-opened by the scan whether it was pruned or not -
  that is the point of recording them - but re-opening runs the guards again rather than the encoder, and a
  file that reaches the same verdict reaches it before anything is encoded. Pruning is what removes the
  verdict itself; a configuration change only asks for it again.
- **It cannot take anything the undo window is holding**, and it does not release it either. A retained
  original is a file that is still present, so the `done` row for the swap that produced it is a row the
  prune may not remove - and the retention itself lives in its own table the prune never reads or writes,
  so `holdfast restore` works exactly the same either side of a pass. That also holds for the row a
  restore leaves behind: putting an original back writes a `skipped / restored-original` row, and that row
  is the only thing standing between the rescued bytes and the same gates that passed the encode you just
  rejected, so retention keeps it for as long as the file is there. The two figures stay separate too - a
  prune returns no space, so `bytes_held_by_undo_window` does not move across one.
- **It never touches a media file.** The store records job state and nothing else.

**What that costs you, plainly.** A library that is not churning has one terminal row per file and every
one of them is load-bearing, so **its ledger is bounded by the library and not by `history_retention_rows`,
and a prune pass may remove nothing at all**. The ledger can therefore sit above the bound; the retention
pass logs how many rows it kept, and splits them into the files that are still in the library and the
directories this run could not list. If your `jobs.db` is large because your library is large, this key is
not the tool for it: that is one row per file you own, and deleting it would cost you a second lossy
generation of the file it describes.

**Known limitations.** The bound is a **row count** only - there is no age-based or per-status policy. It
does not shrink `jobs.db` on disk: pruning bounds the rows, and SQLite reuses the freed pages (there is no
`VACUUM`). Non-terminal rows are never pruned - they are work, not history. Rows under a directory the run
could not list are kept, deliberately and indefinitely: an unmounted subtree looks exactly like a library
you emptied, and pruning on that absence would re-encode the lot when it came back. With retention enabled,
each pass stats the files behind the terminal rows it examines, which is one extra stat per row on top of
the scan that just ran. And with retention disabled (the default) the table's growth is visible through the
metrics that already exist: `holdfast_queue_depth{state}` is read from the store on every scrape, over every
status including the terminal ones.

### Taking the record elsewhere - `holdfast export`

```bash
holdfast export --config config.yaml                        # newline-delimited JSON on stdout
holdfast export --config config.yaml --out ledger.ndjson    # or to a file
```

Every terminal row, oldest first, one JSON object per line, using **the same field names `/api/history`
publishes for a row** - the export calls that same projection, so the two cannot drift. It is a local,
operator-run read of your own store: no listener, no port, no network surface, and it never writes to the
store it reads.

**"Never writes" is enforced, not promised.** The ledger is opened `mode=ro`, so SQLite itself refuses
every write, and - unlike starting the daemon - the export **does not migrate**. That matters most in the
one situation you would reach for it: around an upgrade. A read that quietly bumped the schema would leave
your ledger unopenable by the holdfast still running against it, because a database from the *future* is a
refusal (see *Schema versioning*). So the rows, the schema and its version are exactly as the export found
them. Upgrading the store stays a deliberate act - `holdfast run` or `holdfast serve`.

**A `null` means "not recorded" here exactly as it does in the API.** An unmeasured VMAF, size or duration
is an explicit `null` and never a `0`, because a VMAF of `0.0` is a *destroyed frame* and a size of `0`
would invent a 100% reclaim. A *measured* zero exports as `0`, and the two stay distinguishable.

Failure is loud and leaves nothing behind. `--out` **refuses to overwrite an existing file** (the export it
would replace may be the only copy of rows a prune has since removed); a destination that cannot be created
or written exits non-zero naming that path with no partial file left; and a job store that is missing,
unreadable, or **at any schema version but this build's** - written by a newer holdfast, or by an older one
this command will not migrate on its way past - exits non-zero naming the store path and writes no export.
An empty ledger is an empty export and **exit 0** - distinguishable from every one of those.

**Known limitations.** The store must be at this build's schema version: export from an older ledger by
starting holdfast once (which migrates it) and exporting after, or by using the holdfast that wrote it.
SQLite needs to create its WAL index beside the database to read it, so the *directory* has to be
writable - exporting straight from a read-only copy of `state_dir` will not work. And it is the **job**
ledger only: what the undo window is currently holding is separate state with a lifetime of hours, not a
record of what holdfast did, and `holdfast restore` with no argument is what prints it.

#### Schema versioning

The job store (`<state_dir>/jobs.db`) carries a schema version in SQLite's `PRAGMA user_version` and is
migrated forward on startup, in a transaction per step, so the version and the shape move together or not
at all. **A migration failure is a refusal to start**, never a silent downgrade to a partial schema - and
a database written by a *newer* holdfast is likewise refused rather than opened and quietly written
through a schema that cannot see all of its columns.

Migrating is the **daemon's** job, not a reader's: `holdfast export` opens the store read-only and refuses
a version mismatch in **either** direction rather than repairing one, so reading the ledger can never be
what upgrades it.

### Observability & host-fair scheduling (`serve`)

- **Prometheus** (`/metrics`, default on): `holdfast_files_total{outcome}`, `holdfast_bytes_reclaimed_total`,
  `holdfast_encode_duration_seconds`, `holdfast_vmaf_score` (perceptual-quality distribution), and a
  `holdfast_queue_depth{state}` gauge read live from the store. Metrics are read-only instrumentation -
  best-effort, never affecting file handling.
  The `outcome` label set is `done | skipped | failed | would-transcode | indeterminate |
  applied-despite-error`. **`would-transcode` counts DECISIONS a dry run took, never transcodes that
  happened**: under `dry_run: true` holdfast applies every guard and encodes, swaps and deletes nothing, so
  that series is "how many files a real run would transcode" and not "how many it did". Every one of those
  series is pre-created, so each reads `0` before its first event and an alert can be written against the
  candidate count before the first dry run. The same value appears as a `holdfast_queue_depth{state}`
  series, where it is counted as itself and **not** inside `probing`: that state means claimed and not yet
  decided.
- **Notifications** (`notify_url`, [shoutrrr](https://shoutrrr.nickfedor.com/)): one service URL fans out to
  ntfy/Discord/Gotify/… - a message per failed file and a per-scan summary. Sends run off the engine's path,
  and a send failure is logged, never crashing the daemon or altering files. Empty URL disables it.
- **Host-fair scheduling**: a daily `run_window` (`HH:MM-HH:MM`), a per-core `max_load` cap, and an optional
  Tautulli-aware pause (`tautulli_url` + `tautulli_api_key`) that holds off while someone is streaming.
  Scheduling only ever **delays** new work - it never interrupts an in-flight encode or bypasses a gate, and
  a Tautulli outage **fails open** (never halts transcoding). **Known limitation:** Plex-aware pause needs an
  operator-supplied Tautulli endpoint; otherwise the run-window + load cap are the fairness mechanism.
