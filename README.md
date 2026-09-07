# holdfast

**A config-as-code, data-safe, self-hosted media transcoder — an open-source [Tdarr](https://tdarr.io) replacement.**

![The holdfast web dashboard: live queue, per-status summary, reclaimed-space total, and history — served from the single binary by `holdfast serve`.](docs/dashboard.png)

`holdfast` watches a media library, re-encodes bloated non-HEVC/non-AV1 video to a smaller modern codec
to reclaim disk space, and — the whole point — **never destroys a source until a replacement is provably
faithful**. It is configured entirely by **YAML** (config-as-code), so what it does is reviewable and
reproducible from git, not hidden in a UI database.

> **Status: feature-complete for a first release, not yet released.** This repository was built phase by
> phase from a mature, battle-tested Bash predecessor (see _Provenance_). **The data-safety core
> (`TRANSCODE-1`)** is the heart of it: `holdfast run` performs one oneshot scan of the library roots —
> skip guards → same-directory temp encode → the full verify gate → atomic swap → delete — proven by a
> real-ffmpeg fixture suite that reds on the specific regression. Built on top of it: colour/HDR
> preservation (`TRANSCODE-3`), the VMAF perceptual gate (`TRANSCODE-4`), a persistent crash-safe queue +
> worker pool (`TRANSCODE-5`), hardware/AV1 encoders (`TRANSCODE-6`), the REST/SSE API + embedded web UI
> (`TRANSCODE-7`, shown above), observability + host-fair scheduling (`TRANSCODE-8`), and **packaging: a
> multi-arch, non-root container image bundling a pinned ffmpeg (`TRANSCODE-9`)**. The first tagged release
> is a deliberate human act and has not been cut. See the roadmap for the full plan.

## Why another transcoder?

Tdarr is capable but **closed-source** and **UI/DB-configured** (state can be lost on a container rebuild),
and it historically **replaced the original file before/regardless of its health check** — a documented
data-loss class ([#355](https://github.com/HaveAGitGat/Tdarr/issues/355),
[#511](https://github.com/HaveAGitGat/Tdarr/issues/511),
[#683](https://github.com/HaveAGitGat/Tdarr/issues/683)). `holdfast` takes the useful capability surface
and fixes the trust gaps:

- **Never replace before verify.** Encode to a same-directory temp; the source is replaced only by an
  **atomic same-filesystem rename**, and only after the output passes *every* gate: correct codec,
  duration/packet parity, strictly smaller, per-type stream-count parity, full decode-integrity, and a
  **VMAF** perceptual-quality check - its **average** (`min_vmaf`), its **worst frame**
  (`vmaf_min_pool`) *and* its **colour** (`vmaf_min_chroma`, which the luma-only VMAF model cannot
  see at all). The comparison is made in one pixel format holdfast **names and records**, not one
  ffmpeg negotiated. Any failure leaves the source byte-for-byte untouched.
- **The source can't be swapped out from under a running encode.** The source's `size:mtime` is
  re-checked immediately before the swap: if something else (Plex, an *arr, you) rewrote or replaced it
  while the encode ran — hours, on a real film — the swap is **refused** rather than atomically
  overwriting the newer content with a re-encode of the stale bytes. A **symlinked** source is
  **skipped**, never replaced in place (which would orphan the real file it points at).
- **The swap is made durable, not just atomic.** A `rename` is atomic for a concurrent reader, but
  POSIX does not make it *persistent* until the containing directory is `fsync`'d — a power loss an
  instant after `rename()` returns can otherwise lose it, and in the container-changing case the
  source was already removed, leaving the entry pointing at nothing. holdfast `fsync`s the encode
  **before** the rename and the parent directory **after** it (the POSIX durable-rename recipe); if
  that directory `fsync` fails the source is **kept**, never removed under an unproven rename. True
  power-loss survival is filesystem- and hardware-dependent (and untestable in CI without a power-cut
  harness) — this is the portable discipline, documented as such, not an absolute guarantee.
- **The quality gate bounds the worst frame, not just the average.** An average hides local damage —
  Netflix says so outright — so a short destroyed segment inside an otherwise-clean encode passes a
  mean-only gate, and passes every structural check too (it decodes fine and carries the right duration,
  packets and streams). Both floors are **on by default**. An output that cannot be *measured* is
  rejected, not assumed good.
- **Config-as-code.** YAML, validated, in git — not clickops that vanishes on rebuild.
- **Open source** (AGPL-3.0).

### We are not the only tool that verifies before it replaces

[**Alchemist**](https://github.com/bybrooklyn/alchemist) (AGPL-3.0, Rust) works the same axis: it validates
output quality before promoting the result, keeps your originals untouched until the new file passes, and
ships its own *Migrate from Tdarr* guide. If you are choosing between us, choose on the difference, not on
a claim of uniqueness we would not be able to defend.

**The difference is where the default sits.** Alchemist's VMAF scoring is **opt-in**. `holdfast`'s gate is
**default-on, layered, and fails closed**: structural parity (codec, duration, packets, per-type stream
counts, strictly-smaller) *and* full decode-integrity *and* VMAF — both its average **and** its worst
frame. An output that cannot be **measured** is **rejected**, never assumed good; an ffmpeg without libvmaf
stops the tool rather than quietly downgrading the gate. That is the whole claim, and it is narrower and
truer than "the only one that checks".

## Non-goals

Codec-only, same-content re-encoding (no resolution downscaling); HDR10 **static** metadata is preserved
but Dolby Vision / HDR10+ dynamic metadata is **detect-and-skipped**; interlaced and exotic-chroma sources
are **skipped, not converted**. It transcodes files in a library other tools manage (Plex/Jellyfin/*arr) —
it is not a media server or library manager.

## Quick start

**Docker (the supported path).** The image bundles a pinned, checksum-verified ffmpeg with libx265,
libsvtav1 and **libvmaf** — the perceptual gate needs it, and an output that cannot be measured is
rejected rather than accepted, so the right ffmpeg is not a convenience:

```bash
mkdir -p state && sudo chown 1000:1000 state   # must be writable by the user: in the compose file
cp config.example.yaml config.yaml             # then edit the three container keys below
docker compose config -q && docker compose up -d
```

A container config differs from a bare-metal one in exactly three places — miss the third and the
dashboard is unreachable from the host (the API would be bound to the *container's* loopback):

```yaml
library_roots: [/media]     # the CONTAINER path your library is mounted at
state_dir: /state           # the mounted volume — it must survive restarts
server_addr: 0.0.0.0:8080   # compose publishes it on 127.0.0.1 only
```

See **[docs/docker.md](docs/docker.md)** for volumes, permissions, timezone, GPU passthrough and the
security posture — and **[docs/migration.md](docs/migration.md)** if you are coming from Tdarr or from the
Bash transcoder.

**From source:**

```bash
cp config.example.yaml config.yaml   # then edit library_roots
holdfast validate --config config.yaml
holdfast run --config config.yaml   # one scan: re-encode bloated non-HEVC video, safely
holdfast serve --config config.yaml # HTTP API + web dashboard (scan on demand / on an interval)
holdfast restore --config config.yaml  # what the undo window is holding (see below)
holdfast export --config config.yaml --out ledger.ndjson  # the whole ledger, as NDJSON
```

`run`/`serve` need `ffmpeg` and `ffprobe` on `PATH` (or set `HOLDFAST_FFMPEG` / `HOLDFAST_FFPROBE`); they
exit non-zero if they are missing rather than silently doing nothing. Use a build with **libx265** and
**libvmaf** — a distro ffmpeg typically lacks the latter, which is why the image exists.

### The filesystem check at startup

The no-loss contract holdfast is built on is stated for a **local** filesystem: an atomic
same-filesystem rename whose failure means it did not happen, a stat that can see a concurrent
rewrite, and a SQLite WAL that works at all. None of those hold on a NAS. So before the first encode,
`run` and `serve` both classify every path this run would act on - every library root, the state
directory, and every filesystem mounted beneath a root - and start or refuse the whole run in one
decision, printing the exact line that would permit each path it refused:

```yaml
allow_non_local:
  - /media/tv        # per PATH, never a global switch; the guarantee is REDUCED here and it says so
```

Only a positive identification counts as local: an unrecognised type, an overlay, a `tmpfs` and
anything in user space (FUSE) are all treated as not-local, because a false warning costs one line of
configuration and a false clear costs a film. **[docs/filesystem.md](docs/filesystem.md)** has the
recognised-local set, the opt-in rules and what the startup traversal costs.

### The undo window (`restore`) — off by default

The swap is the one irreversible thing holdfast does, and every gate in front of it is an **estimate**.
The delete is not. `undo_window_hours` buys a bounded period in which a swap can be walked back:

```yaml
undo_window_hours: 24     # 0 (the default) = a swap is FINAL, and startup says so
```

```bash
holdfast restore --config config.yaml                     # what is held, and for how long
holdfast restore --config config.yaml /media/tv/ep.mkv    # put that original back
```

The original is kept by a second **hard link**, so retention costs **no space at the moment it is
taken** — but the space a swap reclaimed **does not come back until the window closes**, which for a
first library pass means holding every original it replaced. So the API reports
`bytes_held_by_undo_window` **separately** from the reclaimed totals, and a release reports the bytes it
**actually** returned (removing a name frees the data only when it was the last one). A source whose
original cannot be retained is **skipped, not swapped**, and a restore refuses rather than overwrite a
file that has changed since the swap. Full reference, including what it costs and what it deliberately
does not offer: **[docs/undo.md](docs/undo.md)**.

### Web API + UI (`serve`)

`holdfast serve` runs a REST API + [SSE](https://developer.mozilla.org/docs/Web/API/Server-sent_events)
live stream and an **embedded web dashboard** (baked into the single binary — no assets to deploy). It is
a **read-and-control** surface on top of the config-as-code engine: the YAML file stays the source of
truth and the SQLite store stays the source of job state. The API can only **read the store, start a
scan, and pause/resume the feeding of new files** — it never touches a media file, so the data-safety
invariant is entirely unaffected.

| Method & path | Auth | Purpose |
|---|---|---|
| `GET /` | — | the embedded dashboard |
| `GET /api/summary` | — | counts per status + bytes reclaimed (**lifetime** and this-run) + `bytes_held_by_undo_window` (space a retained original still holds, never folded into either reclaimed figure; `null` = unreadable) + paused/scanning + the **whole-ledger aggregates** (see below) |
| `GET /api/queue` | — | pending + active jobs, capped, with `queue_total` — see *The total behind a cap* |
| `GET /api/history?limit=N` | — | recent terminal jobs (done/skipped/failed) with their recorded outcome, capped, with `history_total` — see below |
| `GET /api/events` | — | SSE: a fresh snapshot on every state change |
| `GET /metrics` | — | Prometheus metrics (when `metrics_enable`, default on) |
| `POST /api/rescan` | token | start a library scan (409 if paused / scanning / outside the run window) |
| `POST /api/pause` | token | stop feeding **new** files (in-flight encodes finish safely) |
| `POST /api/resume` | token | clear the pause flag |

Fail-safes: the server **binds `127.0.0.1` by default** (front it with a reverse proxy for real
multi-user); the mutating endpoints require a bearer token (`server_auth_token`, best set via
`HOLDFAST_SERVER_AUTH_TOKEN`) and are **disabled entirely when no token is set**; pause only ever
*delays* work — it never interrupts an encode or the atomic swap. **Known limitation:** single-token auth
(no per-user accounts); the queue/history views are capped at the most recent rows, not the whole ledger —
but they now say what they were capped *against*, and `holdfast export` gives you the whole thing.

### The total behind a cap

`GET /api/queue` returns at most **500** rows and `GET /api/history` at most **200**. A truncated view that
says nothing about what it truncated reads as the whole ledger, and a client cannot work it out for itself
(the summary counts answer a different question — rows *per status*, not the rows a response selected). So
every capped response carries the total it capped against, counted in the server over **every matching row
in the `jobs` table**:

| Response | Field |
|---|---|
| `GET /api/queue` | `queue_total` |
| `GET /api/history?limit=N` | `history_total` |
| the SSE snapshot | both |

```json
"history_total": {
  "available": true, "unavailable": "",
  "covers": "every row in the ledger with status done, skipped, failed",
  "cap": 200, "count": 41237
}
```

- **`count`** is the number of matching rows in the ledger, **never the number of rows returned**. Asking
  for fewer rows than the cap (`?limit=5`) reports the *same* `count`; only `cap` moves with the request.
- **`available`** is `false` when the total could not be read, and `count` is then an explicit **`null`**,
  never `0` — a zero would claim the ledger is empty beside rows the caller can see. The rows still ship:
  one unreadable figure never costs an operator the records.
- The dashboard renders that total in each table's cap notice, and when the total is unavailable it says so
  **and shows no figure in its place**.

### The recorded outcome — the proof a swap was safe

A terminal job carries the evidence the engine used to decide, so you can audit a swap after the fact
instead of trusting it. Every terminal row in `/api/history` (and in the SSE snapshot) reports:

| Field | On | What it is |
|---|---|---|
| `reason` | failed | the error that rejected it (the encode error, or **which gate** refused the output) |
| `reason` | skipped | **which guard** fired — `already-at-target-codec`, `low-bitrate`, `hardlinked`, `symlinked-source`, `interlaced`, `dolby-vision`, `hdr10-plus`, `incomplete-hdr-metadata`, `exotic-pixel-format`, `target-already-exists`, `undo-retention-failed`, `restored-original` |
| `encoder` | any job that reached the encoder | the encoder that ran (`cpu`, `svtav1`, `nvenc`, …) — a skip, or a file with no readable video stream, never gets that far and records none |
| `vmaf_mean`, `vmaf_min` | done, and a VMAF-rejected failure | the pooled harmonic mean **and the worst frame** |
| `vmaf_model` | as above | the libvmaf model that produced them |
| `vmaf_pix_fmt` | as above | the single pixel format **both streams were converted to** before scoring - chosen and named by holdfast, so a score says which pixels were compared |
| `vmaf_chroma`, `vmaf_chroma_metric` | as above | the worst frame's chroma measurement and what it is (`psnr_cb/psnr_cr min (dB)`) - the only figure on the row that says whether the **colour** survived |
| `source_bytes`, `output_bytes` | done | the sizes either side of the swap |
| `encode_ms` | done, and a failure after the encode ran | wall-clock encode time |

**A `null` means "not recorded", and you must read it that way.** It is never a zero. A numeric field is
`null` — not `0` — whenever the fact was not measured (VMAF disabled, or a row written before these
columns existed), because a VMAF of `0.0` is a *destroyed frame*, not a missing measurement, and rendering
one as the other would be inventing evidence about a swap nobody checked.

**A VMAF score is not interpretable without its model or the format it was measured in**, which is why
all three travel together. Read `vmaf_mean`/`vmaf_min` with the limits in mind: VMAF is a regression onto
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

An outcome is recorded per *attempt*, not per file: **claiming a job for a retry clears it**, so a file
that is being re-encoded never advertises the rejected attempt's score while it is in flight.

The **dashboard renders all of this per file** — size before → after and percent reclaimed, the encoder,
the encode duration, and the VMAF pair shown with its model, its pooling and its luma-only blind spot — so
the proof is on the page, not only in the JSON. A skipped row names its guard; a failed row shows its
reason; a fact that was never recorded reads "not recorded", never `0`.

### An in-flight job — how far it has got

A terminal row says what happened; an **active** row says what is happening. Every job in `/api/queue`
(and in the SSE snapshot's `queue`) carries `updated_at`, the timestamp of its last transition, and the
snapshot carries `now`, the server's clock when the frame was built — together those are how the
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
while that encoder is running: the moment a job moves on — to `verifying`, or back to `pending` after a
crash, or to a terminal state — all three fields go back to `null`, because the process that produced the
figure has exited and nothing is measuring the verify phase. A carried-over percentage frozen beside a
state it does not describe is the one thing this surface must never show.

The same `null` rule applies, and it bites harder here: an encoder that has not reported yet, and a
source whose container reports no duration, are both **unrecorded**, and a `0` would read as "0% encoded"
— a figure nobody measured. The dashboard shows those as *unknown*. Progress is **not persisted**: it is
state about a running process, so after a restart an in-flight job simply has none reported yet, and a
finished row never carries one. There is deliberately **no ETA** — every figure here is measured, and a
predicted finish time is not.

The reclaimed figure is a **durable lifetime total** (`bytes_reclaimed_lifetime`): a one-time baseline
summed from the recorded `source_bytes`/`output_bytes` on every done row, plus this process's reclaims — so
it survives a restart rather than resetting to zero. `bytes_reclaimed_session` is kept alongside it as the
honest this-run number.

**Known limitations.** Rows written before these columns existed carry no outcome and read as "not
recorded" — a measurement never taken cannot be reconstructed, and such a row also contributes nothing to
the lifetime total (never counted as a zero-reclaim). Queue/history views are still capped at the most
recent rows, not the whole ledger — each now reports the total it was capped against (see *The total behind
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

### Bounding the ledger — `history_retention_rows` (off by default)

The `jobs` table only grows: one terminal row per file holdfast has finished with, for the life of the
install. At library scale that record becomes unbounded, so there is a bound — and it **ships disabled**.

```yaml
history_retention_rows: 0     # the DEFAULT, and what an absent key means: keep every row
# history_retention_rows: 50000   # keep at most 50,000 terminal rows; prune the oldest beyond it
```

With a value `n > 0`, holdfast brings the terminal rows back within `n` **after each scan completes** — no
operator action, no API call, no separate command. A negative or fractional value is a **startup refusal**
naming the key and the value, before the job store is opened.

**A prune cannot be undone, and that is why the default is 0.** Those rows are the record of what holdfast
did to your library *after it deleted your originals*. Nothing recreates them: a later scan re-derives the
file's **current** state instead, so a pruned `done` row for a file still on disk comes back as
`skipped / already-at-target-codec` — proof that the file is at the target codec, not proof that holdfast
put it there. **Export before you bound it** if the record matters to you.

What a prune will never do, whatever you set:

- **It cannot lower the lifetime reclaimed total.** A removed row's contribution to
  `bytes_reclaimed_lifetime` is carried forward durably, in the same transaction that deletes the row, so
  the figure is identical either side of a prune — on the running server *and* after the restart that
  re-reads it from the database. (That second half is where a naive prune fails silently: the server reads
  the total once, at startup, so deleting contributing rows shows a correct figure until the next restart.)
- **It cannot cause a file to be encoded again.** A terminal row is a *decision*, not only a record: it is
  what holds that file out of the encoder on every later scan, and the guards that would re-derive the same
  verdict run under whatever configuration is current, so a deleted row means a re-encode the moment the
  configuration it was taken under has moved (a different `encoder` target codec, a lowered
  `min_bitrate_kbps`, a file parked at `max_failures`). **So a row is only ever removed when the scan
  listed the directory that file should be in and the file was not there**: gone, or replaced by different
  content. Retention bounds what your library has *finished with*.
- **It cannot take anything the undo window is holding**, and it does not release it either. A retained
  original is a file that is still present, so the `done` row for the swap that produced it is a row the
  prune may not remove — and the retention itself lives in its own table the prune never reads or writes,
  so `holdfast restore` works exactly the same either side of a pass. That also holds for the row a
  restore leaves behind: putting an original back writes a `skipped / restored-original` row, and that row
  is the only thing standing between the rescued bytes and the same gates that passed the encode you just
  rejected, so retention keeps it for as long as the file is there. The two figures stay separate too — a
  prune returns no space, so `bytes_held_by_undo_window` does not move across one.
- **It never touches a media file.** The store records job state and nothing else.

**What that costs you, plainly.** A library that is not churning has one terminal row per file and every
one of them is load-bearing, so **its ledger is bounded by the library and not by `history_retention_rows`,
and a prune pass may remove nothing at all**. The ledger can therefore sit above the bound; the retention
pass logs how many rows it kept, and splits them into the files that are still in the library and the
directories this run could not list. If your `jobs.db` is large because your library is large, this key is
not the tool for it: that is one row per file you own, and deleting it would cost you a second lossy
generation of the file it describes.

**Known limitations.** The bound is a **row count** only — there is no age-based or per-status policy. It
does not shrink `jobs.db` on disk: pruning bounds the rows, and SQLite reuses the freed pages (there is no
`VACUUM`). Non-terminal rows are never pruned — they are work, not history. Rows under a directory the run
could not list are kept, deliberately and indefinitely: an unmounted subtree looks exactly like a library
you emptied, and pruning on that absence would re-encode the lot when it came back. With retention enabled,
each pass stats the files behind the terminal rows it examines, which is one extra stat per row on top of
the scan that just ran. And with retention disabled (the default) the table's growth is visible through the
metrics that already exist: `holdfast_queue_depth{state}` is read from the store on every scrape, over every
status including the terminal ones.

### Taking the record elsewhere — `holdfast export`

```bash
holdfast export --config config.yaml                        # newline-delimited JSON on stdout
holdfast export --config config.yaml --out ledger.ndjson    # or to a file
```

Every terminal row, oldest first, one JSON object per line, using **the same field names `/api/history`
publishes for a row** — the export calls that same projection, so the two cannot drift. It is a local,
operator-run read of your own store: no listener, no port, no network surface, and it never writes to the
store it reads.

**"Never writes" is enforced, not promised.** The ledger is opened `mode=ro`, so SQLite itself refuses
every write, and — unlike starting the daemon — the export **does not migrate**. That matters most in the
one situation you would reach for it: around an upgrade. A read that quietly bumped the schema would leave
your ledger unopenable by the holdfast still running against it, because a database from the *future* is a
refusal (see *Schema versioning*). So the rows, the schema and its version are exactly as the export found
them. Upgrading the store stays a deliberate act — `holdfast run` or `holdfast serve`.

**A `null` means "not recorded" here exactly as it does in the API.** An unmeasured VMAF, size or duration
is an explicit `null` and never a `0`, because a VMAF of `0.0` is a *destroyed frame* and a size of `0`
would invent a 100% reclaim. A *measured* zero exports as `0`, and the two stay distinguishable.

Failure is loud and leaves nothing behind. `--out` **refuses to overwrite an existing file** (the export it
would replace may be the only copy of rows a prune has since removed); a destination that cannot be created
or written exits non-zero naming that path with no partial file left; and a job store that is missing,
unreadable, or **at any schema version but this build's** — written by a newer holdfast, or by an older one
this command will not migrate on its way past — exits non-zero naming the store path and writes no export.
An empty ledger is an empty export and **exit 0** — distinguishable from every one of those.

**Known limitations.** The store must be at this build's schema version: export from an older ledger by
starting holdfast once (which migrates it) and exporting after, or by using the holdfast that wrote it.
SQLite needs to create its WAL index beside the database to read it, so the *directory* has to be
writable — exporting straight from a read-only copy of `state_dir` will not work. And it is the **job**
ledger only: what the undo window is currently holding is separate state with a lifetime of hours, not a
record of what holdfast did, and `holdfast restore` with no argument is what prints it.

#### Schema versioning

The job store (`<state_dir>/jobs.db`) carries a schema version in SQLite's `PRAGMA user_version` and is
migrated forward on startup, in a transaction per step, so the version and the shape move together or not
at all. **A migration failure is a refusal to start**, never a silent downgrade to a partial schema — and
a database written by a *newer* holdfast is likewise refused rather than opened and quietly written
through a schema that cannot see all of its columns.

Migrating is the **daemon's** job, not a reader's: `holdfast export` opens the store read-only and refuses
a version mismatch in **either** direction rather than repairing one, so reading the ledger can never be
what upgrades it.

### Observability & host-fair scheduling (`serve`)

- **Prometheus** (`/metrics`, default on): `holdfast_files_total{outcome}`, `holdfast_bytes_reclaimed_total`,
  `holdfast_encode_duration_seconds`, `holdfast_vmaf_score` (perceptual-quality distribution), and a
  `holdfast_queue_depth{state}` gauge read live from the store. Metrics are read-only instrumentation —
  best-effort, never affecting file handling.
- **Notifications** (`notify_url`, [shoutrrr](https://shoutrrr.nickfedor.com/)): one service URL fans out to
  ntfy/Discord/Gotify/… — a message per failed file and a per-scan summary. Sends run off the engine's path,
  and a send failure is logged, never crashing the daemon or altering files. Empty URL disables it.
- **Host-fair scheduling**: a daily `run_window` (`HH:MM-HH:MM`), a per-core `max_load` cap, and an optional
  Tautulli-aware pause (`tautulli_url` + `tautulli_api_key`) that holds off while someone is streaming.
  Scheduling only ever **delays** new work — it never interrupts an in-flight encode or bypasses a gate, and
  a Tautulli outage **fails open** (never halts transcoding). **Known limitation:** Plex-aware pause needs an
  operator-supplied Tautulli endpoint; otherwise the run-window + load cap are the fairness mechanism.

## Build

Requires Go 1.25+.

```bash
make build        # -> ./holdfast
make test         # go test -race ./...
make check        # THE gate — see the `check:` target in the Makefile for what it runs.
                  # CI and the release workflow run this same target, not a copy of it.

make image        # build the container image (docker buildx)
make image-smoke  # build it, then drive a REAL encode inside it and assert the no-loss
                  # contract held. This — not "it built" — is the packaging gate CI runs.
```

The Go test suite drives **real ffmpeg**: it fails loudly if `ffmpeg`/`ffprobe` (or `libvmaf`) are
missing rather than skipping, because a skipped safety proof is a false green.

## Provenance

`holdfast` is the standalone extraction and full build-out of a config-as-code HEVC transcoder that began
life as a Bash script inside a private homelab repo. That predecessor already proved the no-loss contract
(verify-then-swap-then-delete, HDR-aware, crash-safe) against a real-ffmpeg fixture suite; this project
ports it to Go and grows it into a production application (persistent queue, worker pool, hardware-encoder
matrix, web UI, observability). The phased plan and its research live in the umbrella that tracks this repo.

## License

[AGPL-3.0](./LICENSE).

### Running a modified holdfast on a network

The dashboard offers its Corresponding Source: every response the root path serves carries a link to the
source, the licence name and the build identity the `version` subcommand reports. AGPL-3.0 section 13 binds
whoever runs a **modified** holdfast over a network to offer that source, so if you fork this, point the
offer at **your** tree. It is a build-time value on both paths that build the binary, and you never patch
the embedded HTML to change it:

```bash
make build SOURCE_URL=https://git.example.org/me/holdfast
make image SOURCE_URL=https://git.example.org/me/holdfast
docker buildx build --build-arg SOURCE_URL=https://git.example.org/me/holdfast .
```

Leave it unset and the binary offers this tree. The value must be an absolute `http://` or `https://` URL:
`serve` **refuses to start** on anything else and names the value it rejected, rather than serving an offer
nobody can follow or quietly falling back to upstream, which would tell your users that upstream is the
source of a binary it is not.
