# Layout

Where each part of the repository lives and what it owns.

- `cmd/holdfast` - the CLI: `run` (oneshot engine), `serve` (same engine behind the HTTP API, graceful drain on
  SIGTERM), `worker` (a node: leases encodes, uploads), `analyze` (a read-only census of the library roots; `--health`
  adds a full-decode report), `plan` (what this configuration would do to the library and what it would save), `resolve`
  (operator determination for a job parked indeterminate, made durable BEFORE any licensed removal), `restore` (the undo
  window's operator half; LOCAL by design, never an HTTP endpoint, because it overwrites a library file with older
  bytes), `requeue` (offers a file a terminal row already answered back to the pipeline; LOCAL for the same reason
  `restore` is, by ratified operator decision), `export`, `validate`, `version`.
- `internal/engine` - the orchestrator. `ProcessFile` runs the skip guards, the encode, the gates and the swap.
- `internal/startup` - the whole-run start-or-refuse decision over the storage every path sits on, taken before anything else runs.
- `internal/probe` - ffprobe/ffmpeg inspection: codec, bitrate, duration, packets.
- `internal/hdr` - colour, HDR and pixel-format fidelity.
- `internal/deinterlace`, `internal/downscale` - what a `deinterlace` or a `max_height` value means: one filter
  expression the encoder, the perceptual gate and the terminal row all read.
- `internal/vmaf` - libvmaf via ffmpeg; the perceptual gate.
- `internal/audio` - the audio plan: which tracks are re-encoded or downmixed, the codec and layout
  matrix, two-pass loudness, and the audio gates' arithmetic.
- `internal/subtitle` - the text subtitle sidecars `subtitle_sidecars: text` writes, and their gate.
- `internal/crop` - what `crop: auto` means for one source: the cropdetect samples, their consensus,
  the decision (a rectangle or a named refusal) and the blackness check of the removed area.
- `internal/dynhdr` - which Dolby Vision and HDR10+ sources are carried, the dovi_tool and hdr10plus_tool
  pre-passes, the Dolby Vision VBV ceiling, and the dynamic-HDR gates' arithmetic.
- `internal/queuekey` - the `savings_per_hour` ordering key: estimated bytes saved per hour of work.
- `internal/health` - the report-only health sweep: its schedule, resumable full decode and read-API report.
- `internal/mediaclient` - the Radarr, Sonarr and Plex clients: the post-swap rescan and the Plex play hold.
- `internal/encoder` - the codec matrix registry.
- `internal/hwdevice` - the render nodes a hardware encoder can open, and the one VAAPI and QSV are each assigned.
- `internal/store` - the persistent job and outcome state.
- `internal/fsclass` - the one enumeration of what this build calls local.
- `internal/diskfree` - free space on a path's filesystem, the one answer the startup floor and the per-job space check share.
- `internal/cpuquota`, `internal/memlimit` - the cgroup CPU quota and memory limit this process really has: they size
  the libvmaf and libx265 threads and set the resident memory at which an encode is aborted.
- `internal/schedule` - host-fair run windows.
- `internal/server` - the HTTP surface, with the webhook intake and the node lease routes. It serves the web UI
  at `/` to a request for HTML; the UI shows and drives only what the JSON API offers, which remains the full
  interface, and every other request to `/` gets the plain-text page carrying the source offer.
- `internal/ui` + `web/` - the embedded web UI and its Svelte source; `scripts/ui.sh` runs its gate steps on the pinned Node and pnpm.
- `internal/sourceoffer` - the AGPL section 13 Corresponding Source offer the root path carries.
- `internal/node` - the worker-node lease protocol's server side: the lease state machine, the caps, the hashed source stream and the digest-checked upload.
- `internal/nodeworker` - the `holdfast worker` loop: acquire, map or download and check the source, encode, upload, complete.
- `internal/metrics`, `internal/notify` - Prometheus collectors and best-effort shoutrrr notifications.
- `internal/config` - koanf layered config: defaults, then YAML, then `HOLDFAST_*`.
- `internal/corpus` - locates the repository root and the Markdown it ships; the one place a check that reads the shipped documents gets its file set from.
- `internal/docscheck` - the checks that what the build publishes and the anchored statements the documents owe are written down (metric names, posture
  statements).
- `internal/heapmeasure` - the one measurement of what a piece of work retained in memory, relative to a baseline; the engine's scale tests use it.
- `internal/secret` - the credential wrapper: a reference is parsed, resolved once at
  start, and handed to exactly one consumer. `internal/secretscan` + `scripts/secret-scan`
  - the repository's own secret scanner, behind `scripts/secret-scan.sh`.
- `internal/logging`, `internal/version` - logger construction, build stamping.
- `scripts/hw-report.sh` + `scripts/hwreport` - the redacted hardware report the owner runs on a GPU host and
  commits under `testdata/hw-reports/`, and the test that proves no host identity survives in one.
- `scripts/client-report.sh` + `scripts/clientreport` - the owner's redacted live check of Plex, Sonarr and Radarr, committed under `testdata/client-reports/`.
- `scripts/` otherwise - `pr-scope.sh` (what a pull request's CI runs), `check-pins.sh` (the pins and the rename guard), `install-ffmpeg.sh` and `install-dynhdr-tools.sh` (the pinned tools),
  `identity-scan.sh`, `govulncheck.sh`, `mutation.sh`, `api-schema`, `smoke-image.sh`, `test-mass.sh`, the release and compose-pin scripts, and a
  `*-selftest.sh` proving each gate still bites.
- `Dockerfile`, `.github/workflows/ci.yml`, `.github/workflows/pr.yml` - the multi-arch distroless image, the nightly full gate and the changed-only PR checks.
