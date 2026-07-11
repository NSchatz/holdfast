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
exact same no-loss bar as CPU libx265. Next: API/UI (`TRANSCODE-7`). The full phased plan lives in the
umbrella that tracks this repo (`operations/roadmaps/transcode.md`).

## Layout

- `cmd/transcode` — the CLI (`run` / `validate` / `version`), structured `slog` logging; `run` builds and
  drives the engine oneshot with signal-cancellable context.
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
- `.github/workflows/ci.yml` — the gate (installs ffmpeg for the engine proof). `Dockerfile` — packaging
  stub (hardened in TRANSCODE-9).

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
