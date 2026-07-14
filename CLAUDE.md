# CLAUDE.md

Guidance for Claude Code (claude.ai/code) working in this repository.

## What this is

`holdfast` — a **config-as-code, data-safe, self-hosted media transcoder**; an open-source Tdarr
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
output). `TRANSCODE-11` closed that gate's **pooling blind spot**. The harmonic mean is an AVERAGE, and an
average hides local damage — Netflix documents this outright ("mean pooling has the risk of hiding poor
quality frames"). `vmaf_min_pool` shipped defaulting to **0 = off**, so the mean was the *sole* VMAF gate,
and `vmaf.Result.Min` — the worst-frame score that would have caught it — was computed and then discarded.
Measured on real libvmaf: an encode with 6 of 720 frames destroyed to VMAF ~37 pools to a harmonic mean of
**98.2** and passes cleanly, whereupon the source is atomically swapped and **deleted**. Every structural
gate passes it too, because a destroyed segment still decodes cleanly and still carries the right duration,
packets and stream counts. In a 2-hour film the same arithmetic buys **over a minute** of ruined video
through the gate. The **worst-frame floor is now ON by default (`vmaf_min_pool: 60`)**. It is the **raw
min**, deliberately, and NOT a low-percentile statistic: a percentile tolerates a *fraction* of frames, but
a segment small enough to sneak past the mean is *by construction* a small fraction of frames — so a
1st-percentile floor tolerates exactly the damage the mean already tolerates, and its blind spot **grows
with runtime** (1% of a 2-hour film is ~72s). The raw min is the only statistic whose guarantee does not
decay with duration. That is **proved by a committed test, not an argument**:
`vmaf.TestPoolingStatistic_OnlyRawMinSeesSubOnePercentDamage` destroys 1 frame of 240 (0.42%) and shows the
harmonic mean (~99) **and** the 1st percentile (~98) are BOTH blind while the raw min reads ~43 — it reds if
anyone "improves" the floor into a percentile. Anti-flake is measured, not assumed:
an honest encode of dark+grainy content — VMAF's documented worst case — bottoms out at a worst frame of
~91, 31 points clear of the floor. Fail-closed throughout: an incomplete libvmaf log (either pooled
statistic absent) is a **rejection**, never a silent fall-back to mean-only, and `vmaf_subsample > 1`
WARNS (it makes the floor a sample, not a guarantee) rather than silently degrading. The fixture pair is
the proof and is a controlled experiment: `TestVmaf_MeanOnlyGateIsBlindToLocalDamage` asserts the
mean-only gate **accepts** the locally-broken encode (pinning the bug, so the floor cannot be dead code),
and `TestVmaf_WorstFrameFloorRejectsLocallyBrokenEncode` asserts the floor **rejects** the identical
encode with the source byte-for-byte intact. **Do not restate "~95 = visually lossless"** — it was asserted
in three files and Netflix's own docs do not support it (VMAF is a regression onto a *subjective* ACR
opinion scale; 100 is a label-normalisation anchor, not "identical to the source"), and the widely-repeated
"~6 VMAF points = 1 JND" figure has **no primary source**. The model is also **luma-only** — structurally
blind to chroma damage, which only the structural gates catch. `TRANSCODE-5` replaced the flat-file ledger with a **persistent,
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
`cmd/holdfast`'s `cmdRun` calls `encoder.RequireAvailable` before building the engine — a configured-but-
unavailable encoder (e.g. `nvenc` with no GPU) fails LOUD and exits non-zero, **never** a silent fallback to
cpu. The verify/VMAF gate is unchanged and fully encoder-agnostic: a hardware/AV1 encode is held to the
exact same no-loss bar as CPU libx265. `TRANSCODE-7` (this is the current state) added the **HTTP API +
embedded web UI** via a new `holdfast serve` command: a **chi** REST API (`/api/summary|queue|history`),
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
output is rejected, never accepted). The pin lives in the **Dockerfile's `FFMPEG_*` ARGs**, and
`scripts/install-ffmpeg.sh` PARSES them (CI/release call it) — because a pin duplicated into a workflow and
held in step by a comment is not a pin: both files stay internally consistent while the proof silently
detaches from the artifact. The **one** place it must be restated is `NOTICE`, which is the GPL source offer
and ships *inside* the image and every tarball, where no Dockerfile is available to point at — so
`scripts/check-pins.sh` (wired into `make check`) FAILS if `NOTICE` or the Go version drifts from the
Dockerfile. Where a value must appear twice, the agreement is enforced, never requested. The runtime base is distroless **`cc`**, not `base`: ffmpeg carries a
`DT_NEEDED` on **`libgcc_s.so.1`**, which `base` does not ship, so `base` builds perfectly and then dies at
the dynamic loader the first time the engine execs ffmpeg. Every stage that RUNs anything is pinned to
`$BUILDPLATFORM` and the binary cross-compiles (`CGO_ENABLED=0`, pure-Go SQLite), so the arm64 image needs
no QEMU. The image bakes **no `HOLDFAST_*` env vars**, deliberately: env BEATS the YAML file, so a baked
default would silently override the user's config-as-code — and for `server_addr` it would quietly widen the
127.0.0.1 fail-safe. **Only NVIDIA hardware encoding works in the image** (the NVIDIA toolkit injects the
libs ffmpeg dlopens); `qsv`/`vaapi`/`amf` each need a vendor userspace library a distroless image cannot
carry (a VA driver for QSV/VAAPI; AMD's `libamfrt64` for AMF, which does NOT go through VA-API) — and
`/dev/dri` is only the kernel device, not a driver — so they are a documented limitation, not a supported
path.
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
push `0.0.0-dev-<sha>` and move `:latest` onto it. **Outstanding: `release.yml` has never executed.** GitHub
offers `workflow_dispatch` only for workflows already on the default branch, so its dry run is unrunnable
from a PR — dispatch it once after this lands and **before the first tag**, or the release path ships having
never run. Note it is NOT a reusable workflow called from
CI: a called workflow cannot hold permissions its caller lacks, so a PR-triggered call declaring
`packages: write` would fail to load — hence the shared *script* rather than a shared workflow. **Not yet released** (cutting a tag is a human call — the umbrella's `PUB-FLIP` gate).
`TRANSCODE-12` **renamed the project `transcode` → `holdfast`**, and it had to land before the first tag
because not one of these surfaces can be redirected afterwards: Go has **no module-path rename primitive**
(golang/go#59766, closed *not planned*), **nothing** rewrites a container-image reference in a user's
compose file, and a renamed Prometheus metric **silently** breaks every dashboard built on it. Blast radius
at rename time was **zero** (private, no tags, nothing on the module proxy, no image ever pushed) — which is
exactly why it went before `PUB-FLIP` and not after. Renamed: the module path, `cmd/holdfast`, the binary,
the image (`ghcr.io/nschatz/holdfast`), the `HOLDFAST_*` env prefix and the `holdfast_*` metric namespace.

**The GitHub repo itself must carry the new name before the first tag.** `release.yml` derives the image
from `github.repository` (`ghcr.io/${owner}/${repo}`, lowercased) — it is not spelled out in the workflow.
So if the repo were still named `transcode` at tag time, the release would publish
`ghcr.io/nschatz/transcode` <!-- rename-guard-allow --> while `docker-compose.yml` and `docs/docker.md` point users at
`ghcr.io/nschatz/holdfast` — **an image that does not exist**. The rename of the GitHub repo is therefore
part of the same irreversible set, not a cosmetic follow-up. GitHub redirects the old name, but only while
`NSchatz/transcode` is **left permanently vacant**: reclaim that name and the redirect dies and the new
repo is served silently in its place.

`TRANSCODE-13` (this is the current state) **persisted the proof**. The engine computed everything needed
to show a swap was safe and the store kept **none** of it: `store.Job` was
`{path, fingerprint, status, fail_count, worker, updated_at}`, so the VMAF mean **and min**, the model,
the encoder, both file sizes, the encode duration, the failure error and **which guard** skipped a file
were all computed and thrown away. Hence: the dashboard could not show fidelity, "reclaimed" reset to `0`
on every restart (it lived only in an in-process counter in the SSE hub), and the README **overclaimed** —
it documented `GET /api/history` as returning jobs *"with reason"* when there was no reason field at all.

The load-bearing half is the **migration mechanism**, and it had to come first. `initSchema` was a bare
`CREATE TABLE IF NOT EXISTS jobs (…)` with **no `PRAGMA user_version`** — which is not a schema, it is a
schema for a database that never changes. `IF NOT EXISTS` matches on the table's NAME, not its SHAPE, so
adding a column to that statement is a **silent no-op against every database that already exists**: the
file keeps its old columns, `Open` reports success, and the process dies later, on a live install, on the
first query naming the column that isn't there. `internal/store/migrate.go` is now an **append-only**
`migrations` slice (the version IS its length — there is no second place to bump), each step applied in a
transaction that **stamps `user_version` in the same transaction**, so a database can never claim a
version whose columns it does not have. v1 is the original schema *with* its `IF NOT EXISTS` — load-bearing,
because a pre-versioning database reports `user_version = 0` and is otherwise indistinguishable from a
fresh file, so v1 must be a no-op on the former and a real create on the latter. **Never edit, reorder or
delete a shipped migration** — a database in the field has already run the old text, so rewriting it
changes only what a FRESH database gets and silently forks the two shapes apart. To change the schema,
append. A migration failure is a **startup refusal** (`store.Open` errors → `cmd/holdfast` exits non-zero),
and a database from the FUTURE is refused too rather than written through a schema that cannot see all of
its columns.

The proof itself is `store.Outcome`, built up in `ProcessFile` as the pipeline learns each fact and handed
to **both** `Store.Finish` and the `Observer` as the *same value* — so the ledger and the live UI cannot
disagree about what happened. **Absence is representable and must stay that way**: every numeric field is a
**pointer**, because `0` is legal for all of them and a VMAF of `0.0` is a *destroyed frame*, not a missing
measurement. NULL/nil is **"not recorded"** and a reader must render it as such — never as `0`, never as a
fabricated score (the API serializes them as explicit JSON `null`, asserted on the raw bytes because
decoding into a struct would erase the distinction). Skip reasons are a **closed vocabulary** (the `Skip*`
constants — treat them as a wire format), because an operator seeing the bare word "skipped" had to go read
the logs to learn which of eight guards fired; a failure's reason is the error text itself. `Finish` writes
the FULL outcome column set every time, so a retried job that finally succeeds cannot sit in the ledger as
"done" still wearing the previous attempt's failure reason. `store.Reclaimed` sums the recorded sizes, so
the lifetime total is durable — persisting **both** sizes rather than just their delta is what buys that.
The anti-vacuity proof is `TestMigrate_V0DatabaseOnDiskGainsTheOutcomeColumns`: it seeds a **real
pre-migration database** (the frozen v0 DDL, with rows) and proves opening it migrates in place and keeps
every row. **A fresh-schema test would pass against the very bug** — a fresh file gets the columns either
way — which is why the v0 fixture is the one that matters; it is mutation-tested against a
`CREATE TABLE IF NOT EXISTS`-only migrate and reds. **Deferred to `-14`:** the dashboard still renders none
of this (`internal/webui` is untouched) and still shows only the session counter.

**The rule when you touch this: hyphen is history, underscore is an identifier.** The phase IDs
`TRANSCODE-1`…`TRANSCODE-15` are **historical labels and must survive** — they are how git log and the
roadmap name the work, and renumbering them would rewrite those references for no gain. The underscore
forms — the old env prefix and the old metric namespace — are pre-rename **identifiers** and must not <!-- rename-guard-allow: TRANSCODE_ transcode_ -->
exist; `scripts/check-pins.sh` FAILS on any that reappear, and it is mutation-tested to prove it still
bites. "transcode" also survives as an ordinary English **verb** ("an in-place transcode") and in
`transcode.conf`, the Bash predecessor's config file named in `docs/migration.md`. A line may quote a
banned identifier only to prohibit it, and only with the `rename-guard-allow` marker — which keeps every
exemption greppable instead of letting a file-level exclusion hide a real leak.

`docs/docker.md` is the deployment reference (volumes, permissions, TZ, GPU passthrough, security posture);
`docs/migration.md` covers the cutover from the Bash transcoder and from Tdarr. The full phased plan lives
in the umbrella that tracks this repo (`operations/roadmaps/holdfast.md`).

## Layout

- `cmd/holdfast` — the CLI (`run` / `serve` / `validate` / `version`), structured `slog` logging. `run`
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
- `internal/config` — **koanf** layered config (defaults ← YAML file ← `HOLDFAST_*` env), unknown-key
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
- `internal/vmaf` (TRANSCODE-4, hardened by TRANSCODE-11) — runs libvmaf (via ffmpeg) to score an output vs
  its source; `Available` reports whether the build has libvmaf, `Score` returns the pooled harmonic-mean +
  min VMAF. Both pooled fields are parsed as **pointers** so "libvmaf did not emit this" is distinguishable
  from "libvmaf measured 0.0" — an incomplete log is a REJECTION, never a zero the gate would read as a real
  score. The engine's `verifyOutput` rejects an encode that is below the mean threshold, **below the
  worst-frame floor**, or unmeasurable (never accept an unmeasured output).
- `internal/encoder` (TRANSCODE-6) — the codec matrix registry: `Spec` (config key, ffmpeg `-c:v` codec,
  output target codec, hardware flag) + `Lookup`/`Known` + a robust `Available` capability check (encodes a
  tiny real clip to a temp file and ffprobes the RESULT rather than trusting ffmpeg's exit code — the only
  way to catch a hardware encoder that exits 0 while writing nothing when no device is present) +
  `RequireAvailable` (fail-loud helper for `cmd/holdfast`). No import of `internal/config` (avoids a
  cycle) — `internal/config.Validate` imports `internal/encoder` instead.
- `internal/store` (TRANSCODE-5; outcome columns + versioning TRANSCODE-13) — the persistent, crash-safe
  SQLite/WAL job store that replaced `internal/ledger`: a `path+fingerprint`-keyed `jobs` table with
  `Claim`/`Advance`/`Finish`/`RecoverStale`/`Get`/`List`/`Summary`/`Reclaimed`. `Claim` is the cross-worker
  mutual-exclusion guard (an explicit transaction around its read-modify-write); `SetMaxOpenConns(1)` +
  `busy_timeout` + WAL + `synchronous=NORMAL` avoid "database is locked" under concurrent workers without
  serializing on fsync-per-commit latency. `migrate.go` owns the schema: an **append-only** `migrations`
  slice versioned by `PRAGMA user_version`, each step + its version stamp in one transaction. `Finish`
  carries a `store.Outcome` — the durable proof of a terminal job (reason / encoder / VMAF mean+min+model /
  both sizes / encode ms), with **nullable** columns so "not recorded" never reads as `0`.
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

Requires Go 1.25+. The gate is **`make check`** — and the `check:` target in the `Makefile` is its
definition, not this sentence. (Enumerating its steps here would be one more copy to drift, which is the
mistake this repo keeps making; read the target.) It covers the real-ffmpeg fixture suite, the linters, and
`scripts/check-pins.sh`. The **Makefile owns the tool pins** and CI
invokes that same target, so the PR gate, the release gate and a human all run the identical thing — the
versions were once restated in `ci.yml` too, which meant bumping one silently drifted the gates apart. CI
adds two things on top: the **config-schema self-test** (proves `validate` REDS on a bad config, not merely
that tests passed) and the **image smoke gate** (`scripts/smoke-image.sh`, needs Docker). **Never claim green
without running it.** Every phase that touches the engine must also extend the fixture suite so it *reds on
the specific regression* — a data-safety tool proves its unhappy paths, not just that tests pass.

## Conventions

- Small, testable functions; fail safe; match Go idiom and the existing layout.
- No secrets in the repo, ever (synthetic `config.example.yaml` only; real `config.yaml` is gitignored).
- Commit as `Noah Schatz <noah.lane.schatz@gmail.com>`; **no** `Co-Authored-By` / AI co-author trailer.
- Conventional Commits.
