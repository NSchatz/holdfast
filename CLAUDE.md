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
strictly-smaller, per-type stream-count parity, output fidelity (bit depth, chroma, colour
tags and HDR10 metadata as the plan declares), full decode-integrity, and VMAF.
Where the encode is WRITTEN is configurable and the swap is not.

Fail-safe rule: ambiguous, malformed or unsupported input SKIPS with a logged
reason or returns a typed error. Never a confident wrong result, never a silent
loss.

Identifier rule: the phase IDs `TRANSCODE-1`...`TRANSCODE-17` (there is no
`TRANSCODE-10`) are historical labels and must survive - git log names the work by
them. The underscore forms are pre-rename identifiers and must not exist;
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
- **Every job's encode is declared once, and the command line and every gate read that one
  plan** - [`docs/design/encode-plan.md`](docs/design/encode-plan.md#encode-plan).
- **An output replaces its source only when it carries the bit depth, chroma, colour tags and
  HDR10 metadata its plan declares** - [`docs/design/encode-plan.md`](docs/design/encode-plan.md#fidelity).
- **A hardware encoder runs only after a real encode through a job's own command line came out
  faithful at each bit depth** - [`docs/design/hardware.md`](docs/design/hardware.md#probe).
- **A job whose hardware is missing or fails is encoded by nothing else unless its root says
  `hw_fallback: software`** - [`docs/design/hardware.md`](docs/design/hardware.md#fallback).
- **A hardware decode hands the filters, the encoder and every gate the frames a software decode
  would** - [`docs/design/hardware.md`](docs/design/hardware.md#decode).
- **An audio track is re-encoded only on request, only from a lossless codec, into a layout the
  codec carries as declared** - [`docs/design/audio.md`](docs/design/audio.md#reencode).
- **Loudness runs in two passes to EBU R 128, resampled to the declared rate, its mode read from the
  encoder's report** - [`docs/design/audio.md`](docs/design/audio.md#loudness).
- **A transformed audio track replaces nothing until its length, layout, rate and loudness match its
  plan and every audio stream decodes** - [`docs/design/audio.md`](docs/design/audio.md#audio-gates).
- **A subtitle sidecar is published only after the swap commits and its parse-back gate passes, and never over an existing file** - [`docs/design/subtitles.md`](docs/design/subtitles.md#sidecars).
- **A picture is cropped only where spread samples agree on its bars and the area removed is black on every frame; a Dolby Vision picture only to its RPU's own active area, zeroed and gated** - [`docs/design/crop.md`](docs/design/crop.md#crop).
- **Dynamic HDR is carried only through libx265, and its output replaces the source only when its DOVI record and every frame's RPU and HDR10+ match its plan** - [`docs/design/dynamic-hdr.md`](docs/design/dynamic-hdr.md#dynamic-hdr).
- **The queue decides only the order files are offered in: priority, then the declared order, never which files** - [`docs/design/queue-order.md`](docs/design/queue-order.md#queue-order).
- **The health sweep reads every source and reports; it never moves, renames, deletes or repairs a file** - [`docs/design/health-sweep.md`](docs/design/health-sweep.md#health-sweep).
- **A media server is told about a swap only after it has committed, once, by directory; and a file being played is held, which only ever delays** - [`docs/design/media-clients.md`](docs/design/media-clients.md#media-clients).

## Layout

- `cmd/holdfast` - the CLI: `run` (oneshot engine), `serve` (same engine behind
  the HTTP API, graceful drain on SIGTERM), `analyze` (a read-only census of the
  library roots; `--health` adds a full-decode report), `plan` (what this
  configuration would do to the library and what it would save), `resolve`
  (operator determination for a job parked indeterminate, made durable BEFORE any
  licensed removal), `restore` (the undo window's operator half; LOCAL by design,
  never an HTTP endpoint, because it overwrites a library file with older bytes),
  `requeue` (offers a file a terminal row already answered back to the pipeline;
  LOCAL for the same reason `restore` is, by ratified operator decision), `export`,
  `validate`, `version`.
- `internal/engine` - the orchestrator. `ProcessFile` runs the skip guards, the
  encode, the gates and the swap.
- `internal/startup` - the whole-run start-or-refuse decision over the storage every
  path sits on, taken before anything else runs.
- `internal/probe` - ffprobe/ffmpeg inspection: codec, bitrate, duration, packets.
- `internal/hdr` - colour, HDR and pixel-format fidelity.
- `internal/deinterlace`, `internal/downscale` - what a `deinterlace` or a `max_height`
  value means: one filter expression the encoder, the perceptual gate and the terminal
  row all read.
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
- `internal/hwdevice` - the render nodes a hardware encoder can open, and the one VAAPI and QSV
  are each assigned.
- `internal/store` - the persistent job and outcome state.
- `internal/fsclass` - the one enumeration of what this build calls local.
- `internal/diskfree` - free space on a path's filesystem, the one answer the startup
  floor and the per-job space check share.
- `internal/cpuquota`, `internal/memlimit` - the cgroup CPU quota and memory limit this
  process really has: they size the libvmaf and libx265 threads and set the resident
  memory at which an encode is aborted.
- `internal/schedule` - host-fair run windows.
- `internal/server` - the HTTP surface. holdfast ships no frontend yet: the JSON API
  is the interface, and `/` is a plain-text page carrying the source offer (a web UI
  was decided by the owner (T14, T18) and is not built).
- `internal/sourceoffer` - the AGPL section 13 Corresponding Source offer the root
  path carries.
- `internal/metrics`, `internal/notify` - Prometheus collectors and best-effort
  shoutrrr notifications.
- `internal/config` - koanf layered config: defaults, then YAML, then `HOLDFAST_*`.
- `internal/corpus` - locates the repository root and the Markdown it ships; the
  one place a check that reads the shipped documents gets its file set from.
- `internal/docscheck` - the checks that what the build publishes and the anchored
  statements the documents owe are written down (metric names, posture statements).
- `internal/heapmeasure` - the one measurement of what a piece of work retained in
  memory, relative to a baseline; the engine's scale tests use it.
- `internal/secret` - the credential wrapper: a reference is parsed, resolved once at
  start, and handed to exactly one consumer. `internal/secretscan` + `scripts/secret-scan`
  - the repository's own secret scanner, behind `scripts/secret-scan.sh`.
- `internal/logging`, `internal/version` - logger construction, build stamping.
- `scripts/hw-report.sh` + `scripts/hwreport` - the redacted hardware report the owner runs on a GPU host and
  commits under `testdata/hw-reports/`, and the test that proves no host identity survives in one.
- `scripts/client-report.sh` + `scripts/clientreport` - the owner's redacted live check of Plex, Sonarr and Radarr, committed under `testdata/client-reports/`.
- `Dockerfile`, `.github/workflows/ci.yml` - the multi-arch distroless image and
  the gate.

## Build / test / gate

Go 1.25+. The gate is `make check`, and the `check:` target IS its definition -
read the target rather than any prose about it (`make tier-full` is `check`; `make tier-fast` its quick subset). The Makefile owns the tool pins
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
- A credential is reached BY REFERENCE. `server_auth_token`, `server_read_token`, `notify_url`,
  `tautulli_api_key`, `radarr_api_key`, `sonarr_api_key` and `plex_token` carry `file:<path>` or
  `cmd:<argv>`, never a value, and a literal in the file or in `HOLDFAST_*` refuses to start - a
  credential in holdfast's environment is inherited by every `ffmpeg` child. `config.SecretBearingKeys`
  is the closed list; a new credential-bearing key joins it or it is not one. A resolved value is a
  `secret.Value`, which renders as `<redacted>` through `fmt`, `slog`, JSON and text; `Expose()` is the
  only route to the plaintext, so grep for it to find every site that reads one. `docs/secrets.md` is
  the reference.
- Commit as the repository's configured git identity; no `Co-Authored-By` and no AI
  co-author trailer.
- No owner identity in a tracked file: the owner's name and email appear only in `LICENSE`
  and `NOTICE`, and synthetic identities stand in everywhere else. `make identity-scan`
  enforces it in `check`, reading the identity from the repository's first commit at run
  time, so the rule carries no copy of what it guards.
- Conventional Commits.
- Plain hyphens only - no en or em dashes, anywhere.
- No dates and no narrated history in this file. Git holds that, and the plan of
  record is the program brief `.claude/goals/2026-09-holdfast.md`, not a roadmap in
  the umbrella - decided by the owner (T2, T8).

## References

`docs/secrets.md` the reference forms, the resolver contract and its documented timeout bound, the
scanner's ruleset, exit codes and one setup step, and the identity scan · `docs/docker.md` deployment
(volumes, permissions, TZ, GPU passthrough, security posture) · `docs/migration.md` the cutover from the
Bash transcoder and Tdarr · `docs/post-swap-hook.md` the Radarr, Sonarr and Plex clients: their keys and
requests, the arr re-download warning, the drain bound and the Plex play hold · `docs/requeue.md` what a
terminal row records about the configuration it was decided under, and the lever for the rows a
configuration change cannot reason about · `docs/test-mass.md` how much of this repository is test code,
what `scripts/test-mass.sh` counts, what was retired for grading presentation rather than behaviour, and
why line coverage is not assertion · `docs/mutation-testing.md` the mutation score floor, the figure it
is applied to, which packages are in the mutation domain and why each exclusion is there, what a pull
request runs against what the schedule runs, and how to reproduce either by hand ·
`docs/hardware-reports.md` how a hardware report is run, what it records and what it redacts · `docs/client-reports.md` the same for the owner's live check of Plex, Sonarr and Radarr ·
`docs/encode-memory.md` the encode memory watchdog, the mux-queue bounds on every ffmpeg argv, and the
reproduction attempt behind them.
