# CLAUDE.md

Guidance for Claude Code working in this repository.

## What this is

`holdfast` - a config-as-code, data-safe, self-hosted media transcoder in Go; an
open-source Tdarr replacement. It reclaims disk by re-encoding bloated video to a
smaller modern codec and never destroys a source until a replacement is provably
faithful.

North star: a stranger can `docker run` it, point it at a library with a YAML
file, and trust it. It reclaims space, never trades a good file for a broken or
worse one, says what it did, survives crashes and restarts, and is declarable in
git.

## The invariant that governs every change (do not weaken it)

**Never mutate a source until a replacement passed every gate.** Encode to a
same-directory temp; the swap is the ONLY filesystem mutation and is an atomic
same-filesystem `rename()`; it runs only after the output passes: correct codec,
duration and packet parity, strictly-smaller, per-type stream-count parity, full
decode-integrity, and VMAF. Any gate failure discards the temp and leaves the
source byte-for-byte intact. This is the exact fix for Tdarr's documented
replace-before-verify data loss.

Fail-safe rule: ambiguous, malformed or unsupported input SKIPS with a logged
reason or returns a typed error. Never a confident wrong result, never a silent
loss.

Identifier rule: the phase IDs `TRANSCODE-1`...`TRANSCODE-15` are historical
labels and must survive - git log and the roadmap name the work by them. The
underscore forms are pre-rename identifiers and must not exist;
`scripts/check-pins.sh` fails on any that reappear and is mutation-tested to
prove it still bites. A line may quote a banned identifier only to prohibit it,
and only with a `rename-guard-allow` marker.

## Layout

- `cmd/holdfast` - the CLI: `run` (oneshot engine), `serve` (same engine behind
  the API and UI, graceful drain on SIGTERM), `resolve` (operator determination
  for a job parked indeterminate, made durable BEFORE any licensed removal),
  `restore` (the undo window's operator half; LOCAL by design, never an HTTP
  endpoint, because it overwrites a library file with older bytes), `requeue`
  (offers a file a terminal row already answered back to the pipeline; LOCAL for
  the same reason `restore` is, by ratified operator decision), `export`,
  `validate`, `version`.
- `internal/engine` - the orchestrator. `ProcessFile` runs the skip guards, the
  encode, the gates and the swap.
- `internal/probe` - ffprobe/ffmpeg inspection: codec, bitrate, duration, packets.
- `internal/hdr` - colour, HDR and pixel-format fidelity.
- `internal/vmaf` - libvmaf via ffmpeg; the perceptual gate.
- `internal/encoder` - the codec matrix registry.
- `internal/store` - the persistent job and outcome state.
- `internal/fsclass` - the one enumeration of what this build calls local.
- `internal/schedule` - host-fair run windows.
- `internal/server`, `internal/webui` - the HTTP surface and the embedded
  dashboard.
- `internal/metrics`, `internal/notify` - Prometheus collectors and best-effort
  shoutrrr notifications.
- `internal/config` - koanf layered config: defaults, then YAML, then `HOLDFAST_*`.
- `internal/docscheck` - a mechanical check, on the ordinary test step, that the
  docs still describe the build.
- `internal/logging`, `internal/version` - logger construction, build stamping.
- `Dockerfile`, `.github/workflows/ci.yml` - the multi-arch distroless image and
  the gate.

## Build / test / gate

Go 1.25+. The gate is `make check`, and the `check:` target IS its definition -
read the target rather than any prose about it. The Makefile owns the tool pins
and CI invokes the same target, so a PR, a release and a human run the identical
thing.

CI adds three things `check` deliberately does not: the config-schema self-test
(proves `validate` reds on a bad config), the image smoke gate
(`scripts/smoke-image.sh`, needs Docker), and the dashboard gate
(`make webui-check`, needs node and a browser engine). `check` must stay green on
a machine with no browser, so the dashboard suites skip there and `webui-check`
is where a missing runtime is a failure.

The gate needs the pinned ffmpeg (`scripts/install-ffmpeg.sh`) and a browser on
`PATH` (or `HOLDFAST_BROWSER`): a criterion about what the DASHBOARD SHOWS is
graded against a page loaded in a real engine. Neither is skipped when absent - a
grader that skips is a false green.

Never claim green without running it. Every change that touches the engine
extends the fixture suite so it reds on that specific regression: a data-safety
tool proves its unhappy paths.

## Conventions

- Small, testable functions; fail safe; match Go idiom and the existing layout.
- No secrets, ever. Synthetic `config.example.yaml` only; real `config.yaml` is
  gitignored.
- Commit as `Noah Schatz <noah.lane.schatz@gmail.com>`; no `Co-Authored-By` and
  no AI co-author trailer.
- Conventional Commits.
- Plain hyphens only - no en or em dashes, anywhere.
- No dates and no narrated history in this file. Git holds that, and the phased
  plan lives in the umbrella's `documentation/roadmaps/holdfast.md`.

## References

`docs/docker.md` deployment (volumes, permissions, TZ, GPU passthrough, security
posture) · `docs/migration.md` the cutover from the Bash transcoder and Tdarr ·
`docs/webui.md` the dashboard reference.
