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
implemented the **data-safety core** (this is the current state): `run` does one oneshot scan — skip
guards → same-dir temp encode (CPU libx265) → the full verify gate → atomic swap → delete — proven by a
real-ffmpeg fixture suite (cases 1–17). Next: colour/HDR (`TRANSCODE-3`), VMAF (`TRANSCODE-4`), the
SQLite/WAL queue + worker pool (`TRANSCODE-5`), hardware/AV1 (`TRANSCODE-6`), API/UI (`TRANSCODE-7`). The
full phased plan lives in the umbrella that tracks this repo (`operations/roadmaps/transcode.md`).

## Layout

- `cmd/transcode` — the CLI (`run` / `validate` / `version`), structured `slog` logging; `run` builds and
  drives the engine oneshot with signal-cancellable context.
- `internal/config` — **koanf** layered config (defaults ← YAML file ← `TRANSCODE_*` env), unknown-key
  rejection, and strict `Validate()` (refuses `/`, `$HOME`, or a symlink resolving to either; refuses when
  `$HOME` is unknown). An explicit zero in the file/env overrides a default (not clobbered). The `validate`
  subcommand + a CI schema self-test (reds on an invalid config) back it.
- `internal/probe` — ffprobe/ffmpeg inspection helpers (codec, bitrate, duration, packet count, decode
  healthcheck, stream counts, fingerprint, nlink); UNKNOWN values are never coerced to 0.
- `internal/ledger` — the resumable size:mtime TSV (done/skipped/failed; failed is retryable). SQLite in T-5.
- `internal/engine` — the orchestrator: `ProcessFile` (skip guards → encode → verify → atomic swap →
  delete), `verifyOutput` (the layered no-loss gate), `Encoder` (interface; `FFmpegEncoder` + test fakes),
  scan + crash-safe temp cleanup. **This is the risk-critical heart — do not weaken the invariant.**
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
