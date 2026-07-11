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
1–17). `TRANSCODE-3` (this is the current state) added **colour/HDR + source-property fidelity**: HDR10
static metadata (mastering-display + MaxCLL) and colour primaries/transfer/matrix are now carried through
the encode instead of silently dropped; Dolby Vision / HDR10+ (dynamic metadata a generic re-encode can't
preserve) are detected and SKIPPED; an interlaced source is SKIPPED (never deinterlaced); an exotic/
unrecognized source pixel format is SKIPPED (never silently subsampled) while a recognized one has its
chroma subsampling preserved and bit-depth floored at 10; a VFR source is no longer forced to CFR; and the
output container now matches the source's own extension by default (in-place transcode) so a stream type
that doesn't round-trip through a different container (e.g. MP4 `mov_text` into MKV) isn't force-migrated.
Proven by fixture cases 18–22 (HDR) plus source-property cases (a)-(e). Next: VMAF (`TRANSCODE-4`), the
SQLite/WAL queue + worker pool (`TRANSCODE-5`), hardware/AV1 (`TRANSCODE-6`), API/UI (`TRANSCODE-7`). The
full phased plan lives in the umbrella that tracks this repo (`operations/roadmaps/transcode.md`).

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
- `internal/ledger` — the resumable size:mtime TSV (done/skipped/failed; failed is retryable). SQLite in T-5.
- `internal/engine` — the orchestrator: `ProcessFile` (skip guards — already-HEVC, low-bitrate, hardlinked,
  **interlaced, DV/HDR10+, HDR10-with-incomplete-metadata, exotic pixel format** (TRANSCODE-3) — → encode →
  verify → atomic swap → delete), `verifyOutput` (the layered no-loss gate), `Encoder` (interface;
  `FFmpegEncoder` + test fakes — `FFmpegEncoder` now derives colour args, x265 colour params, and the output
  pixel format from the source via `internal/hdr`, and adds `-fps_mode passthrough`), scan + crash-safe temp
  cleanup. **This is the risk-critical heart — do not weaken the invariant.**
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
