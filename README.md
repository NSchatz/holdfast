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
  **VMAF** perceptual-quality check — both its **average** (`min_vmaf`) *and* its **worst frame**
  (`vmaf_min_pool`). Any failure leaves the source byte-for-byte untouched.
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
holdfast run --config config.yaml     # one scan: re-encode bloated non-HEVC video, safely
holdfast serve --config config.yaml   # HTTP API + web dashboard (scan on demand / on an interval)
holdfast resolve --config config.yaml # list (and resolve) any job whose swap outcome is unknown
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
| `GET /api/summary` | — | counts per status + bytes reclaimed (**lifetime** and this-run) + paused/scanning + the **whole-ledger aggregates** (see below) |
| `GET /api/queue` | — | pending + active jobs |
| `GET /api/history?limit=N` | — | recent terminal jobs (done/skipped/failed, plus `indeterminate` and `applied-despite-error`) with their recorded outcome — see below |
| `GET /api/events` | — | SSE: a fresh snapshot on every state change |
| `GET /metrics` | — | Prometheus metrics (when `metrics_enable`, default on) |
| `POST /api/rescan` | token | start a library scan (409 if paused / scanning / outside the run window) |
| `POST /api/pause` | token | stop feeding **new** files (in-flight encodes finish safely) |
| `POST /api/resume` | token | clear the pause flag |

Fail-safes: the server **binds `127.0.0.1` by default** (front it with a reverse proxy for real
multi-user); the mutating endpoints require a bearer token (`server_auth_token`, best set via
`HOLDFAST_SERVER_AUTH_TOKEN`) and are **disabled entirely when no token is set**; pause only ever
*delays* work — it never interrupts an encode or the atomic swap. **Known limitation:** single-token auth
(no per-user accounts); the queue/history views are capped at the most recent rows, not the whole ledger.

### The recorded outcome — the proof a swap was safe

A terminal job carries the evidence the engine used to decide, so you can audit a swap after the fact
instead of trusting it. Every terminal row in `/api/history` (and in the SSE snapshot) reports:

| Field | On | What it is |
|---|---|---|
| `reason` | failed | the error that rejected it (the encode error, or **which gate** refused the output) |
| `reason` | skipped | **which guard** fired — `already-at-target-codec`, `low-bitrate`, `hardlinked`, `symlinked-source`, `interlaced`, `dolby-vision`, `hdr10-plus`, `incomplete-hdr-metadata`, `exotic-pixel-format`, `target-already-exists` |
| `encoder` | any job that reached the encoder | the encoder that ran (`cpu`, `svtav1`, `nvenc`, …) — a skip, or a file with no readable video stream, never gets that far and records none |
| `vmaf_mean`, `vmaf_min` | done, and a VMAF-rejected failure | the pooled harmonic mean **and the worst frame** |
| `vmaf_model` | as above | the libvmaf model that produced them |
| `source_bytes`, `output_bytes` | done | the sizes either side of the swap |
| `encode_ms` | done, and a failure after the encode ran | wall-clock encode time |
| `guard_attributes`, `guard_time_resolution` | any job that reached the swap | which source attributes the source-mutation guard compared (`size,mtime`) and the resolution of the timestamp it compared (`1s`) - the granularity that check actually achieved |
| `guard_residual_window` | as above | which of the two documented residual windows applies to the storage the guard ran against: `residual-window-local` or `residual-window-network`. A **class label**, never a duration - see [docs/filesystem.md](docs/filesystem.md#residual-window-local) |
| `swap_cause` | a swap failure with a distinct cause | today only `cross-filesystem` - the temp and the target were not on the same mounted filesystem. Absent for every other failure |

**A `null` means "not recorded", and you must read it that way.** It is never a zero. A numeric field is
`null` — not `0` — whenever the fact was not measured (VMAF disabled, or a row written before these
columns existed), because a VMAF of `0.0` is a *destroyed frame*, not a missing measurement, and rendering
one as the other would be inventing evidence about a swap nobody checked.

**A VMAF score is not interpretable without its model**, which is why the two always travel together.
Read `vmaf_mean`/`vmaf_min` with the limits in mind: VMAF is a regression onto a *subjective* opinion
scale under one viewing condition, `vmaf_v0.6.1` is **luma-only** (structurally blind to chroma damage),
and the scores are **not comparable across different sources**. The number bounds measured perceptual
quality against *your* source; it is not a proof of fidelity.

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
recent rows, not the whole ledger; the aggregate figures below are not.

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

#### Schema versioning

The job store (`<state_dir>/jobs.db`) carries a schema version in SQLite's `PRAGMA user_version` and is
migrated forward on startup, in a transaction per step, so the version and the shape move together or not
at all. **A migration failure is a refusal to start**, never a silent downgrade to a partial schema — and
a database written by a *newer* holdfast is likewise refused rather than opened and quietly written
through a schema that cannot see all of its columns.

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
