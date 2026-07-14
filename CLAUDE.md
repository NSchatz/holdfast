# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

## What this is

`transcode` — a **config-as-code, data-safe, self-hosted media transcoder**; an open-source Tdarr
replacement written in **Go**. It reclaims disk by re-encoding bloated video to a smaller modern codec and
**never destroys a source until a replacement is provably faithful**.

**North star.** A production-trustworthy tool a stranger can `docker run`, point at a library with a YAML
file, and trust: it reclaims space, **never trades a good file for a broken or worse one**, tells them what
it did, survives crashes/restarts, and is fully declarable in git.

## The invariant that governs every change (do not weaken it)

**Never mutate a source until a replacement passed every gate.** Encode to a **same-directory** temp; the
swap is the **only** filesystem mutation and is an **atomic same-filesystem `rename()`**; it runs only
after the output passes: correct codec + duration/packet parity + strictly-smaller + per-type stream-count
parity + full decode-integrity + VMAF. Any gate failure discards the temp and leaves the source
byte-for-byte intact. This is the exact fix for Tdarr's documented replace-before-verify data loss.

Fail-safe rule: ambiguous / malformed / unsupported input → **skip with a logged reason** or a **typed
error**, never a confident wrong result and never a silent loss.

## Status & roadmap

Built phase by phase. `TRANSCODE-0` wired CLI + config + logging + CI + packaging. `TRANSCODE-1`
implemented the **data-safety core**: `run` does one oneshot scan — skip guards → same-dir temp encode
(CPU libx265) → the full verify gate → atomic swap → delete — proven by a real-ffmpeg fixture suite (cases
1–17). `TRANSCODE-3` added **colour/HDR + source-property fidelity**: HDR10
static metadata (mastering-display + MaxCLL) and colour primaries/transfer/matrix are now carried through
the encode instead of silently dropped; Dolby Vision / HDR10+ (dynamic metadata a generic re-encode can't
preserve) are detected and SKIPPED; an interlaced source is SKIPPED (never deinterlaced); an exotic/
unrecognized source pixel format is SKIPPED (never silently subsampled) while a recognized one has its
chroma subsampling preserved and bit-depth floored at 10; a VFR source is no longer forced to CFR; and the
output container now matches the source's own extension by default (in-place transcode) so a stream type
that doesn't round-trip through a different container (e.g. MP4 `mov_text` into MKV) isn't force-migrated.
Proven by fixture cases 18–22 (HDR) plus source-property cases (a)-(e). `TRANSCODE-4` added the **VMAF
perceptual-quality gate** — the last no-loss layer: after the structural checks pass, VMAF compares the
output against the source and rejects an encode that decodes fine but *looks* worse (default-on, pooled
harmonic-mean < 95 → reject; libvmaf-unavailable-while-enabled → reject, never accept an unmeasured
output). `TRANSCODE-5` replaced the flat-file ledger with a **persistent,
crash-safe SQLite/WAL job store + worker pool**: `internal/store` is a `path+fingerprint`-keyed jobs table
(`pending/probing/encoding/verifying/done/skipped/failed`) opened with `SetMaxOpenConns(1)` (serializes
every access — the actual fix for "database is locked" under concurrency) and an explicit transaction
around `Claim`'s read-modify-write (the mutual-exclusion guard so two workers can never encode the same
source); `RunOneshot` calls `RecoverStale` first (resets any job a prior crashed run left active back to
pending — safe because the swap, the only mutation, never ran) then fans the scanned file list out to
`Cfg.EffectiveWorkers()` goroutines over a channel. The data-safety invariant is unchanged: the store only
ever records job STATE, never touches the filesystem. `TRANSCODE-6` (this is the current state)
generalized the CPU-libx265-only encoder into a **codec matrix**: `internal/encoder` is a registry of
`Spec`s (`cpu`→libx265/hevc — the archival default, `svtav1`→libsvtav1/av1, plus the hardware encoders
`nvenc`/`av1_nvenc`/`qsv`/`vaapi`/`amf`) with a ROBUST runtime capability check (`Available`) that actually
encodes a tiny real clip to a temp file and ffprobes the result — exit-code alone is unreliable for
hardware encoders (`hevc_nvenc` can exit 0 while writing nothing when no device is present). The engine now
knows its `targetCodec` (hevc or av1, derived from the configured encoder in `New`); the skip-already-
target guard and `verifyOutput`'s output-codec check are both generalized off it (no longer hardcoded
"hevc"). `FFmpegEncoder.Encode` builds per-encoder args from the `Spec`: colour (`-color_*`), pixel format,
and `-fps_mode passthrough` are universal; libx265 alone gets `-x265-params` (HDR10 static-metadata
master-display/max-cll is a libx265-only mechanism — AV1/hardware carry colour tags but not that block, an
explicitly out-of-scope, documented limitation matching the bash transcoder's pre-existing NVENC gap).
`cmd/transcode`'s `cmdRun` calls `encoder.RequireAvailable` before building the engine — a configured-but-
unavailable encoder (e.g. `nvenc` with no GPU) fails LOUD and exits non-zero, **never** a silent fallback to
cpu. The verify/VMAF gate is unchanged and fully encoder-agnostic: a hardware/AV1 encode is held to the
exact same no-loss bar as CPU libx265. `TRANSCODE-7` (this is the current state) added the **HTTP API +
embedded web UI** via a new `transcode serve` command: a **chi** REST API (`/api/summary|queue|history`),
an **SSE** live stream (`/api/events`) that pushes a fresh store-derived snapshot on every job-state
change, and a single self-contained dashboard **embedded with `go:embed`** (served at `/`). It is a
**read-and-control** surface — the YAML config stays the source of truth and the SQLite store stays the
source of job state; the API can only read the store, start a scan, and pause/resume the feeding of NEW
files. Nothing in `internal/server`/`internal/webui` ever touches a media file: the data-safety invariant
lives entirely in the engine. Additive, non-invasive wiring — the engine gained an optional `Observer`
(fire-and-forget Event on each transition, carrying reclaimed bytes on a swap) and a `Paused func() bool`
hook checked between files (pause DELAYS work, never interrupts an in-flight encode/swap); the store
gained read-only `List`/`Summary`. Fail-safes: bind **`127.0.0.1` by default**, a bearer token on the
mutating endpoints (disabled entirely when unset), constant-time token compare. `TRANSCODE-8` (this is the
current state) added **observability + host-fair scheduling** to `serve`: **Prometheus** metrics at
`/metrics` (`internal/metrics` — files_total{outcome}, bytes_reclaimed_total, encode_duration + VMAF
histograms, and a queue_depth gauge read from the store on scrape), best-effort **shoutrrr** notifications
(`internal/notify` — per-file failure + per-scan summary, sent off the engine's path via a buffered worker
so a slow endpoint never stalls an encode; disabled when `notify_url` is empty), and **host-fair
scheduling** (`internal/schedule` — a daily run-window, a per-core CPU-load cap, and an optional
Tautulli-aware pause; it only ever DELAYS new work and a Tautulli outage fails OPEN). All additive: the
engine's single `Observer` now fans out to hub+metrics+notify (each non-blocking); the `Paused` hook now
also consults the scheduler (throttled) so a closing run-window stops feeding NEW files without touching an
in-flight encode; the `Event` gained `EncodeDuration` + `VmafScore` (surfaced from a now-score-returning
`verifyOutput` — the error still governs the gate) and Done is emitted exactly once. `TRANSCODE-9` (this is
the current state) is **packaging + release + migration**: a production **multi-arch** (amd64/arm64),
**non-root**, **distroless** image bundling ffmpeg **pinned by release tag AND verified by SHA-256** — the
same build CI runs the fixture safety proof against, so the image ships the ffmpeg that was actually proven
(note an ffmpeg lacking libvmaf would not silently weaken the gate, it would STOP the tool: an unmeasured
output is rejected, never accepted). The pin lives in the **Dockerfile's `FFMPEG_*` ARGs and nowhere else**:
`scripts/install-ffmpeg.sh` PARSES them, and CI/release call it, because a pin duplicated into a workflow
and held in step by a comment is not a pin — both files would stay internally consistent while the proof
silently detached from the artifact. The runtime base is distroless **`cc`**, not `base`: ffmpeg carries a
`DT_NEEDED` on **`libgcc_s.so.1`**, which `base` does not ship, so `base` builds perfectly and then dies at
the dynamic loader the first time the engine execs ffmpeg. Every stage that RUNs anything is pinned to
`$BUILDPLATFORM` and the binary cross-compiles (`CGO_ENABLED=0`, pure-Go SQLite), so the arm64 image needs
no QEMU. The image bakes **no `TRANSCODE_*` env vars**, deliberately: env BEATS the YAML file, so a baked
default would silently override the user's config-as-code — and for `server_addr` it would quietly widen the
127.0.0.1 fail-safe. **Only NVIDIA hardware encoding works in the image** (the NVIDIA toolkit injects the
libs ffmpeg dlopens); `qsv`/`vaapi`/`amf` need a VA-API userspace driver a distroless image cannot carry —
`/dev/dri` is only the kernel device — so they are a documented limitation, not a supported path.
`scripts/smoke-image.sh` is the packaging gate, and it is the SHARED unit: `ci.yml`'s `package` job runs it
on every PR, and `release.yml` runs the same script — before it pushes, and then AGAIN against **both arches
of** the image it pulls back from the registry. That second run is not belt-and-braces: buildx cannot push a
multi-arch manifest it only loaded locally, so the push is a cache REBUILD, and "equivalent inputs" is a
gate by equivalence, which this repo does not accept. Note the ordering, which is load-bearing: the push
publishes the **version tag only**, and **`:latest` is promoted (by `imagetools`, same digest, no rebuild)
only after the pushed artifact passes** — push `:latest` first and a failing gate has already handed every
`docker compose pull` user an image it just rejected. A release also runs the **full** `make check`, not
just govulncheck: `ci.yml` does not trigger on tags, a tag can point at any commit, and a release gated only
on a vuln scan would happily publish an image whose verify/swap logic is red. (The gate is a script, not a workflow, precisely so a
human can run it too: `make image-smoke`.) It does not check that the image *built* — that proves nothing
about the engine; it drives a REAL oneshot encode inside the container and asserts the source was replaced
by a smaller HEVC file with no temp left behind. Its fixture is CBR-padded on purpose: x264 ABR does not
pad and `testsrc2` is trivially compressible, so a plain `-b:v 8M` lands at ~873 kbps — under the default
`min_bitrate_kbps`, whereupon the engine correctly SKIPS the file and the smoke test asserts against a file
the encoder never touched. **A fixture that never reaches the encoder is not a smoke test** (this shipped
broken once and the refuter caught it); the script now asserts the fixture is above the guard, so that
failure mode is loud. `release.yml` publishes on a **tag push only** — a deliberate
human act, never on a merge — and `workflow_dispatch` is ALWAYS a full dry run (both arches, both smoke
tests, the real binaries; pushes nothing). There is deliberately no `publish` input to tick: the only thing
that can publish is a tag, so a release always carries a real tag name — a dispatch-publish could only ever
push `0.0.0-dev-<sha>` and move `:latest` onto it. Note it is NOT a reusable workflow called from
CI: a called workflow cannot hold permissions its caller lacks, so a PR-triggered call declaring
`packages: write` would fail to load — hence the shared *script* rather than a shared workflow. **Not yet released** (cutting a tag is a human call — the umbrella's `PUB-FLIP` gate).
`docs/docker.md` is the deployment reference (volumes, permissions, TZ, GPU passthrough, security posture);
`docs/migration.md` covers the cutover from the Bash transcoder and from Tdarr. The full phased plan lives
in the umbrella that tracks this repo (`operations/roadmaps/transcode.md`).

## Layout

- `cmd/transcode` — the CLI (`run` / `serve` / `validate` / `version`), structured `slog` logging. `run`
  builds and drives the engine oneshot with a signal-cancellable context; `serve` (TRANSCODE-7) wires the
  same engine to the API/UI and runs until SIGTERM (graceful HTTP drain). Engine setup shared by both is
  factored into `buildEngine`.
- `internal/server` (TRANSCODE-7) — the HTTP surface: a `Controller` (pause flag + scan orchestration, the
  single source of truth for both, wired into `engine.Paused`), an SSE `Hub` (the `engine.Observer`;
  coalesces events off the engine's critical path and broadcasts store-derived snapshots — an engine
  worker never blocks on a slow client), and the chi router with read endpoints, token-gated mutating
  endpoints, and the SSE stream. Imports `engine`/`store`/`config`; the engine does NOT import it (the
  Observer is a func the engine defines and `server` supplies — no cycle).
- `internal/webui` (TRANSCODE-7) — the single `go:embed`-ed dashboard (`index.html`, vanilla JS + inline
  CSS, no external/CDN assets) + its handler, served at `/` under a tight CSP.
- `internal/metrics` (TRANSCODE-8) — Prometheus `client_golang` collectors on a private registry: an
  `engine.Observer` adapter (counts terminal outcomes; records reclaimed bytes + encode-duration + VMAF on
  the Done event) + a queue-depth collector that reads `store.Summary` at scrape time + a `/metrics`
  handler. Read-only; a store hiccup on scrape just omits the gauge.
- `internal/notify` (TRANSCODE-8) — best-effort shoutrrr notifications: an `engine.Observer` that tallies a
  scan and fires a per-file-failure message + a per-scan summary. Sends run on a background worker over a
  buffered channel (never on an engine goroutine), and a send failure is logged, never propagated — it can
  never crash the daemon or alter file handling. Empty URL = disabled.
- `internal/schedule` (TRANSCODE-8) — host-fair scheduling: a pure, unit-tested run-window predicate
  (`Window.Contains`, wrap-around aware), a per-core load cap (from `/proc/loadavg`), and an optional
  minimal Tautulli client. `MayRun`/`MayRunThrottled` answer "may new work start now?" — advisory only
  (delays, never a gate), fail-open on a monitoring outage. No internal imports (so `config.Validate` can
  import it to check `run_window`).
- `internal/config` — **koanf** layered config (defaults ← YAML file ← `TRANSCODE_*` env), unknown-key
  rejection, and strict `Validate()` (refuses `/`, `$HOME`, or a symlink resolving to either; refuses when
  `$HOME` is unknown). An explicit zero in the file/env overrides a default (not clobbered). `PixelFormat`
  defaults to `"auto"` (derive per source; a forced value is back-compat); `ContainerExt` defaults to
  `"source"` (match the source file's own extension; a forced value overrides). The `validate` subcommand +
  a CI schema self-test (reds on an invalid config) back it.
- `internal/probe` — ffprobe/ffmpeg inspection helpers (codec, bitrate, duration, packet count, decode
  healthcheck, stream counts, fingerprint, nlink, colour fields, side-data (frame + stream, flat), pix_fmt,
  field_order, codec_tag_string); UNKNOWN values are never coerced to 0.
- `internal/hdr` (TRANSCODE-3) — the colour/HDR + pixel-format port of the bash transcoder's HDR logic:
  pure functions (`ClassFrom`, `MasterDisplay`, `MaxCLL`, `StaticMetadataIncomplete`, `DerivePixFmt`) that
  are unit-tested with no ffmpeg dependency, plus prober-backed `Classify`/`DeriveColorArgs` used by the
  engine and encoder. **Fail-safe by construction**: an incomplete HDR10 static-metadata block, or an
  unrecognized pixel format, is never guessed — the caller skips.
- `internal/vmaf` (TRANSCODE-4) — runs libvmaf (via ffmpeg) to score an output vs its source; `Available`
  reports whether the build has libvmaf, `Score` returns the pooled harmonic-mean + min VMAF. The engine's
  `verifyOutput` rejects a below-threshold or unmeasurable encode (never accept an unmeasured output).
- `internal/encoder` (TRANSCODE-6) — the codec matrix registry: `Spec` (config key, ffmpeg `-c:v` codec,
  output target codec, hardware flag) + `Lookup`/`Known` + a robust `Available` capability check (encodes a
  tiny real clip to a temp file and ffprobes the RESULT rather than trusting ffmpeg's exit code — the only
  way to catch a hardware encoder that exits 0 while writing nothing when no device is present) +
  `RequireAvailable` (fail-loud helper for `cmd/transcode`). No import of `internal/config` (avoids a
  cycle) — `internal/config.Validate` imports `internal/encoder` instead.
- `internal/store` (TRANSCODE-5) — the persistent, crash-safe SQLite/WAL job store that replaced
  `internal/ledger`: a `path+fingerprint`-keyed `jobs` table with `Claim`/`Advance`/`Finish`/`RecoverStale`/
  `Get`. `Claim` is the cross-worker mutual-exclusion guard (an explicit transaction around its
  read-modify-write); `SetMaxOpenConns(1)` + `busy_timeout` + WAL + `synchronous=NORMAL` avoid "database is
  locked" under concurrent workers without serializing on fsync-per-commit latency.
- `internal/engine` — the orchestrator: `ProcessFile` (skip guards — already-at-TARGET-CODEC (TRANSCODE-6
  generalized this off a hardcoded "already HEVC"), low-bitrate, hardlinked, **interlaced, DV/HDR10+,
  HDR10-with-incomplete-metadata, exotic pixel format** (TRANSCODE-3) — → **`Store.Claim`** (TRANSCODE-5) →
  encode → verify → atomic swap → delete), `verifyOutput` (the layered no-loss gate — its output-codec
  check is also generalized off the engine's `targetCodec`, TRANSCODE-6), `Encoder` (interface;
  `FFmpegEncoder` + test fakes — `FFmpegEncoder.Encode`/`buildArgs` now select the ffmpeg codec and
  per-encoder quality args from `internal/encoder.Lookup(Cfg.Encoder)`; colour args, derived pixel format,
  and `-fps_mode passthrough` stay universal across every encoder via `internal/hdr`), `RunOneshot`
  (`RecoverStale` → stale-temp cleanup → scan → fan out to a `Cfg.EffectiveWorkers()`-sized worker pool over
  a channel; a worker's in-flight temp is local to its own `ProcessFile` call, never a shared field, since N
  workers each hold at most one temp at a time). **This is the risk-critical heart — do not weaken the
  invariant.**
- `internal/logging`, `internal/version` — logger construction, build-stamped version.
- `.github/workflows/ci.yml` — the gate (installs the pinned ffmpeg via `scripts/install-ffmpeg.sh` for the
  engine proof) + a `package` job (TRANSCODE-9) that builds BOTH arches and runs the image smoke gate.
  `.github/workflows/release.yml` (TRANSCODE-9) — tag-triggered: runs the full `make check`, builds both
  arches, smokes them, pushes the version tag, re-smokes what it pulled back, and only then promotes
  `:latest` and cuts the release. Publishing happens on a **tag push only**; `workflow_dispatch` is always
  a dry run.
- `Dockerfile` (TRANSCODE-9) — the production image (multi-arch, distroless `cc`, non-root, pinned ffmpeg);
  its `FFMPEG_*` ARGs are the single source of truth for the pin. `scripts/install-ffmpeg.sh` — installs
  exactly that pin by parsing them (CI + release + local dev all use it). `docker-compose.yml` — the example
  deployment. `scripts/smoke-image.sh` — the packaging gate: a real encode inside the image, asserting the
  no-loss contract. `NOTICE` — the image redistributes GPL ffmpeg binaries, so it carries their licence and
  source offer.

## Build / test / gate

Requires Go 1.25+. The CI gate (and `make check`) is: **`gofmt -l` clean, `go vet`, `go build`,
`go test -race`, `staticcheck` (pinned), `govulncheck` (pinned)**. All pinned in `.github/workflows/ci.yml`
for reproducibility. **Never claim green without running it.** Every phase that touches the engine must
also extend the fixture suite so it *reds on the specific regression* — a data-safety tool proves its
unhappy paths, not just that tests pass.

## Conventions

- Small, testable functions; fail safe; match Go idiom and the existing layout.
- No secrets in the repo, ever (synthetic `config.example.yaml` only; real `config.yaml` is gitignored).
- Commit as `Noah Schatz <noah.lane.schatz@gmail.com>`; **no** `Co-Authored-By` / AI co-author trailer.
- Conventional Commits.
