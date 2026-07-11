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

Built phase by phase. `TRANSCODE-0` (this scaffold) wires CLI + config + logging + CI + packaging; the
engine is not implemented yet (`run` touches no files). Next: `TRANSCODE-1` (data-safety core port with a
ported real-ffmpeg fixture suite). The full phased plan lives in the umbrella that tracks this repo
(`operations/roadmaps/transcode.md`).

## Layout

- `cmd/transcode` — the CLI (`run` / `validate` / `version`), structured `slog` logging.
- `internal/config` — YAML config load + strict validation (refuses `/` or `$HOME` as a library root).
- `internal/logging`, `internal/version` — logger construction, build-stamped version.
- `.github/workflows/ci.yml` — the gate (see below). `Dockerfile` — packaging stub (hardened in TRANSCODE-9).

## Build / test / gate

Requires Go 1.23+. The CI gate (and `make check`) is: **`gofmt -l` clean, `go vet`, `go build`,
`go test -race`, `staticcheck` (pinned), `govulncheck` (pinned)**. All pinned in `.github/workflows/ci.yml`
for reproducibility. **Never claim green without running it.** Every phase that touches the engine must
also extend the fixture suite so it *reds on the specific regression* — a data-safety tool proves its
unhappy paths, not just that tests pass.

## Conventions

- Small, testable functions; fail safe; match Go idiom and the existing layout.
- No secrets in the repo, ever (synthetic `config.example.yaml` only; real `config.yaml` is gitignored).
- Commit as `Noah Schatz <noah.lane.schatz@gmail.com>`; **no** `Co-Authored-By` / AI co-author trailer.
- Conventional Commits.
