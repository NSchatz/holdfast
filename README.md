# transcode

**A config-as-code, data-safe, self-hosted media transcoder — an open-source [Tdarr](https://tdarr.io) replacement.**

![The transcode web dashboard: live queue, per-status summary, reclaimed-space total, and history — served from the single binary by `transcode serve`.](docs/dashboard.png)

`transcode` watches a media library, re-encodes bloated non-HEVC/non-AV1 video to a smaller modern codec
to reclaim disk space, and — the whole point — **never destroys a source until a replacement is provably
faithful**. It is configured entirely by **YAML** (config-as-code), so what it does is reviewable and
reproducible from git, not hidden in a UI database.

> **Status: early build-out.** This repository is being built phase by phase from a mature, battle-tested
> Bash predecessor (see _Provenance_). **The data-safety core is implemented (`TRANSCODE-1`)**: `transcode
> run` performs one oneshot scan of the library roots — skip guards → same-directory temp encode → the full
> verify gate → atomic swap → delete — and is proven by a real-ffmpeg fixture suite (cases 1–17) that reds
> on the specific regression. Built since: colour/HDR preservation (`TRANSCODE-3`), the VMAF perceptual
> gate (`TRANSCODE-4`), a persistent crash-safe queue + worker pool (`TRANSCODE-5`), hardware/AV1 encoders
> (`TRANSCODE-6`), and the **REST/SSE API + embedded web UI** (`TRANSCODE-7`, `transcode serve` — shown
> above). Still to come: observability + host-fair scheduling (`TRANSCODE-8`), packaging + release
> (`TRANSCODE-9`). See the roadmap for the full plan.

## Why another transcoder?

Tdarr is capable but **closed-source** and **UI/DB-configured** (state can be lost on a container rebuild),
and it historically **replaced the original file before/regardless of its health check** — a documented
data-loss class ([#355](https://github.com/HaveAGitGat/Tdarr/issues/355),
[#511](https://github.com/HaveAGitGat/Tdarr/issues/511),
[#683](https://github.com/HaveAGitGat/Tdarr/issues/683)). `transcode` takes the useful capability surface
and fixes the trust gaps:

- **Never replace before verify.** Encode to a same-directory temp; the source is replaced only by an
  **atomic same-filesystem rename**, and only after the output passes *every* gate: correct codec,
  duration/packet parity, strictly smaller, per-type stream-count parity, full decode-integrity, and a
  **VMAF** perceptual-quality check. Any failure leaves the source byte-for-byte untouched.
- **Config-as-code.** YAML, validated, in git — not clickops that vanishes on rebuild.
- **Open source** (AGPL-3.0).

## Non-goals

Codec-only, same-content re-encoding (no resolution downscaling); HDR10 **static** metadata is preserved
but Dolby Vision / HDR10+ dynamic metadata is **detect-and-skipped**; interlaced and exotic-chroma sources
are **skipped, not converted**. It transcodes files in a library other tools manage (Plex/Jellyfin/*arr) —
it is not a media server or library manager.

## Quick start

```bash
cp config.example.yaml config.yaml   # then edit library_roots
transcode validate --config config.yaml
transcode run --config config.yaml   # one scan: re-encode bloated non-HEVC video, safely
transcode serve --config config.yaml # HTTP API + web dashboard (scan on demand / on an interval)
```

`run`/`serve` need `ffmpeg` and `ffprobe` on `PATH` (or set `TRANSCODE_FFMPEG` / `TRANSCODE_FFPROBE`); they
exit non-zero if they are missing rather than silently doing nothing. Use a build with **libx265**.

### Web API + UI (`serve`)

`transcode serve` runs a REST API + [SSE](https://developer.mozilla.org/docs/Web/API/Server-sent_events)
live stream and an **embedded web dashboard** (baked into the single binary — no assets to deploy). It is
a **read-and-control** surface on top of the config-as-code engine: the YAML file stays the source of
truth and the SQLite store stays the source of job state. The API can only **read the store, start a
scan, and pause/resume the feeding of new files** — it never touches a media file, so the data-safety
invariant is entirely unaffected.

| Method & path | Auth | Purpose |
|---|---|---|
| `GET /` | — | the embedded dashboard |
| `GET /api/summary` | — | counts per status + session bytes reclaimed + paused/scanning |
| `GET /api/queue` | — | pending + active jobs |
| `GET /api/history?limit=N` | — | recent terminal jobs (done/skipped/failed, with reason) |
| `GET /api/events` | — | SSE: a fresh snapshot on every state change |
| `GET /metrics` | — | Prometheus metrics (when `metrics_enable`, default on) |
| `POST /api/rescan` | token | start a library scan (409 if paused / scanning / outside the run window) |
| `POST /api/pause` | token | stop feeding **new** files (in-flight encodes finish safely) |
| `POST /api/resume` | token | clear the pause flag |

Fail-safes: the server **binds `127.0.0.1` by default** (front it with a reverse proxy for real
multi-user); the mutating endpoints require a bearer token (`server_auth_token`, best set via
`TRANSCODE_SERVER_AUTH_TOKEN`) and are **disabled entirely when no token is set**; pause only ever
*delays* work — it never interrupts an encode or the atomic swap. **Known limitation:** single-token auth
(no per-user accounts); the queue/history views are capped at the most recent rows, not the whole ledger.

### Observability & host-fair scheduling (`serve`)

- **Prometheus** (`/metrics`, default on): `transcode_files_total{outcome}`, `transcode_bytes_reclaimed_total`,
  `transcode_encode_duration_seconds`, `transcode_vmaf_score` (perceptual-quality distribution), and a
  `transcode_queue_depth{state}` gauge read live from the store. Metrics are read-only instrumentation —
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
make build      # -> ./transcode
make test       # go test -race ./...
make check      # gofmt + vet + staticcheck + govulncheck + test (the CI gate)
```

## Provenance

`transcode` is the standalone extraction and full build-out of a config-as-code HEVC transcoder that began
life as a Bash script inside a private homelab repo. That predecessor already proved the no-loss contract
(verify-then-swap-then-delete, HDR-aware, crash-safe) against a real-ffmpeg fixture suite; this project
ports it to Go and grows it into a production application (persistent queue, worker pool, hardware-encoder
matrix, web UI, observability). The phased plan and its research live in the umbrella that tracks this repo.

## License

[AGPL-3.0](./LICENSE).
