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

**Never mutate a source until a replacement passed every gate.** The swap is an
atomic same-filesystem `rename()` from a path in the SOURCE's own directory, and
it runs only after the output passes: correct codec, duration and packet parity,
strictly-smaller, per-type stream-count parity, full decode-integrity, and VMAF.
Where the encode is WRITTEN is configurable and the swap is not.

Fail-safe rule: ambiguous, malformed or unsupported input SKIPS with a logged
reason or returns a typed error. Never a confident wrong result, never a silent
loss.

Identifier rule: the phase IDs `TRANSCODE-1`...`TRANSCODE-15` are historical
labels and must survive - git log and the roadmap name the work by them. The
underscore forms are pre-rename identifiers and must not exist;
`scripts/check-pins.sh` fails on any that reappear and is mutation-tested to
prove it still bites. A line may quote a banned identifier only to prohibit it,
and only with a `rename-guard-allow` marker.

## Design rationale

One argument, one home, each anchored and each named here by its rule only - the
reasoning lives in the document, not in this file.

- **No source is mutated until a replacement has passed every gate** -
  [`docs/design/swap.md`](docs/design/swap.md#swap-invariant).
- **The quality gate bounds the worst frame, not only the average** -
  [`docs/design/quality-gate.md`](docs/design/quality-gate.md#vmaf-pooling).
- **An unreadable figure is reported as null, never as a zero** -
  [`docs/design/ledger-totals.md`](docs/design/ledger-totals.md#null-is-not-zero).

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
- `internal/server` - the HTTP surface. holdfast ships no frontend: the JSON API
  is the interface, and `/` is a plain-text page carrying the source offer.
- `internal/metrics`, `internal/notify` - Prometheus collectors and best-effort
  shoutrrr notifications.
- `internal/config` - koanf layered config: defaults, then YAML, then `HOLDFAST_*`.
- `internal/corpus` - locates the repository root and the Markdown it ships; the
  one place a check that reads the shipped documents gets its file set from.
- `internal/secret` - the credential wrapper: a reference is parsed, resolved once at
  start, and handed to exactly one consumer. `internal/secretscan` + `scripts/secret-scan`
  - the repository's own secret scanner, behind `scripts/secret-scan.sh`.
- `internal/logging`, `internal/version` - logger construction, build stamping.
- `Dockerfile`, `.github/workflows/ci.yml` - the multi-arch distroless image and
  the gate.

## Build / test / gate

Go 1.25+. The gate is `make check`, and the `check:` target IS its definition -
read the target rather than any prose about it. The Makefile owns the tool pins
and CI invokes the same target, so a PR, a release and a human run the identical
thing.

CI adds two things `check` deliberately does not: the config-schema self-test
(proves `validate` reds on a bad config) and the image smoke gate
(`scripts/smoke-image.sh`, needs Docker).

The gate needs the pinned ffmpeg (`scripts/install-ffmpeg.sh`), and it is not
skipped when absent - a grader that skips is a false green.

Never claim green without running it. Every change that touches the engine
extends the fixture suite so it reds on that specific regression: a data-safety
tool proves its unhappy paths.

## Conventions

- Small, testable functions; fail safe; match Go idiom and the existing layout.
- No secrets, ever, and it is MECHANICAL now: `make secret-scan` refuses a tracked
  file carrying an issued credential or named like a credential store, and
  `make install-hooks` (the one setup step) puts it on the pre-commit path. Both
  `secret-scan` and its self-test ride `make check`. Synthetic
  `config.example.yaml` only; real `config.yaml` is gitignored.
- A credential is reached BY REFERENCE. `server_auth_token`, `server_read_token`,
  `notify_url` and `tautulli_api_key` carry `file:<path>` or `cmd:<argv>`, never a value,
  and a literal in the file or in `HOLDFAST_*` refuses to start - a credential in
  holdfast's environment is inherited by every `ffmpeg` child. `config.SecretBearingKeys`
  is the closed list; a new credential-bearing key joins it or it is not one. A resolved value is a
  `secret.Value`, which renders as `<redacted>` through `fmt`, `slog`, JSON and text;
  `Expose()` is the only route to the plaintext, so grep for it to find every site
  that reads one. `docs/secrets.md` is the reference.
- Commit as `Noah Schatz <noah.lane.schatz@gmail.com>`; no `Co-Authored-By` and
  no AI co-author trailer.
- Conventional Commits.
- Plain hyphens only - no en or em dashes, anywhere.
- No dates and no narrated history in this file. Git holds that, and the phased
  plan lives in the umbrella's `documentation/roadmaps/holdfast.md`.

## References

`docs/secrets.md` the reference forms, the resolver contract and its documented timeout
bound, and the scanner's ruleset, exit codes and one setup step ·
`docs/docker.md` deployment (volumes, permissions, TZ, GPU passthrough, security
posture) · `docs/migration.md` the cutover from the Bash transcoder and Tdarr ·
`docs/requeue.md` what a terminal row
records about the configuration it was decided under, and the lever for the rows a
configuration change cannot reason about ·
`docs/test-mass.md` how much of this repository is test code, what `scripts/test-mass.sh`
counts, what was retired for grading presentation rather than behaviour, and why line
coverage is not assertion ·
`docs/mutation-testing.md` the mutation score floor, the figure it is applied to, which
packages are in the mutation domain and why each exclusion is there, what a pull request
runs against what the schedule runs, and how to reproduce either by hand.
