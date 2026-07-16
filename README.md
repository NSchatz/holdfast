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
holdfast run --config config.yaml   # one scan: re-encode bloated non-HEVC video, safely
holdfast serve --config config.yaml # HTTP API + web dashboard (scan on demand / on an interval)
```

`run`/`serve` need `ffmpeg` and `ffprobe` on `PATH` (or set `HOLDFAST_FFMPEG` / `HOLDFAST_FFPROBE`); they
exit non-zero if they are missing rather than silently doing nothing. Use a build with **libx265** and
**libvmaf** — a distro ffmpeg typically lacks the latter, which is why the image exists.

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
| `GET /api/summary` | — | counts per status + bytes reclaimed (**lifetime** and this-run) + paused/scanning |
| `GET /api/queue` | — | pending + active jobs |
| `GET /api/history?limit=N` | — | recent terminal jobs (done/skipped/failed) with their recorded outcome — see below |
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

The reclaimed figure is a **durable lifetime total** (`bytes_reclaimed_lifetime`): a one-time baseline
summed from the recorded `source_bytes`/`output_bytes` on every done row, plus this process's reclaims — so
it survives a restart rather than resetting to zero. `bytes_reclaimed_session` is kept alongside it as the
honest this-run number.

**Known limitations.** Rows written before these columns existed carry no outcome and read as "not
recorded" — a measurement never taken cannot be reconstructed, and such a row also contributes nothing to
the lifetime total (never counted as a zero-reclaim). Queue/history views are still capped at the most
recent rows, not the whole ledger.

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
