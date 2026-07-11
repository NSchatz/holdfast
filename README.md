# transcode

**A config-as-code, data-safe, self-hosted media transcoder — an open-source [Tdarr](https://tdarr.io) replacement.**

`transcode` watches a media library, re-encodes bloated non-HEVC/non-AV1 video to a smaller modern codec
to reclaim disk space, and — the whole point — **never destroys a source until a replacement is provably
faithful**. It is configured entirely by **YAML** (config-as-code), so what it does is reviewable and
reproducible from git, not hidden in a UI database.

> **Status: early build-out.** This repository is being built phase by phase from a mature, battle-tested
> Bash predecessor (see _Provenance_). **This commit is the genesis scaffold (`TRANSCODE-0`)**: the CLI,
> config loading/validation, logging, CI, and packaging are wired, but **the transcode engine is not yet
> implemented** — `transcode run` validates your config and then deliberately touches no files. The
> data-safety core lands next (`TRANSCODE-1`). See the roadmap for the full plan.

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
transcode run --config config.yaml   # (genesis: validates, then touches nothing)
```

## Build

Requires Go 1.23+.

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
