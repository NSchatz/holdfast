# holdfast

**A config-as-code, data-safe, self-hosted media transcoder - an open-source [Tdarr](https://tdarr.io) replacement.**

![The holdfast web dashboard: live queue, per-status summary, reclaimed-space totals, whole-ledger figures, and history with each swap's proof of safety - served from the single binary by `holdfast serve`.](docs/dashboard.png)

`holdfast` watches a media library, re-encodes bloated non-HEVC/non-AV1 video to a smaller modern codec
to reclaim disk space, and - the whole point - **never destroys a source until a replacement is provably
faithful**. It is configured entirely by **YAML** (config-as-code), so what it does is reviewable and
reproducible from git, not hidden in a UI database.

> **Status: `v0.1.0` released (2026-07-18); major version zero, so anything MAY change.** This repository was built phase by
> phase from a mature, battle-tested Bash predecessor (see _Provenance_). **The data-safety core
> (`TRANSCODE-1`)** is the heart of it: `holdfast run` performs one oneshot scan of the library roots -
> skip guards → same-directory temp encode → the full verify gate → atomic swap → delete - proven by a
> real-ffmpeg fixture suite that reds on the specific regression. Built on top of it: colour/HDR
> preservation (`TRANSCODE-3`), the VMAF perceptual gate (`TRANSCODE-4`), a persistent crash-safe queue +
> worker pool (`TRANSCODE-5`), hardware/AV1 encoders (`TRANSCODE-6`), the REST/SSE API + embedded web UI
> (`TRANSCODE-7`, shown above), observability + host-fair scheduling (`TRANSCODE-8`), and **packaging: a
> multi-arch, non-root container image bundling a pinned ffmpeg (`TRANSCODE-9`)**. Cutting a tag is a
> deliberate human act: [`docs/release.md`](docs/release.md) is the ordered runbook, says which of its
> steps can be undone, and carries the record of what `v0.1.0` already published. See the roadmap for the
> full plan.

## Why another transcoder?

Tdarr is capable but **closed-source** and **UI/DB-configured** (state can be lost on a container rebuild),
and it historically **replaced the original file before/regardless of its health check** - a documented
data-loss class ([#355](https://github.com/HaveAGitGat/Tdarr/issues/355),
[#511](https://github.com/HaveAGitGat/Tdarr/issues/511),
[#683](https://github.com/HaveAGitGat/Tdarr/issues/683)). `holdfast` takes the useful capability surface
and fixes the trust gaps:

- **Never replace before verify.** Encode to a same-directory temp; the source is replaced only by an
  **atomic same-filesystem rename**, and only after the output passes *every* gate: correct codec,
  duration/packet parity, strictly smaller, per-type stream-count parity, full decode-integrity, and a
  **VMAF** perceptual-quality check - its **average** (`min_vmaf`), its **worst frame**
  (`vmaf_min_pool`) *and* its **colour** (`vmaf_min_chroma`, which the luma-only VMAF model cannot
  see at all). The comparison is made in one pixel format holdfast **names and records**, not one
  ffmpeg negotiated. Any failure leaves the source byte-for-byte untouched.
- **The source can't be swapped out from under a running encode.** The source's `size:mtime` is
  re-checked immediately before the swap: if something else (Plex, an *arr, you) rewrote or replaced it
  while the encode ran - hours, on a real film - the swap is **refused** rather than atomically
  overwriting the newer content with a re-encode of the stale bytes. A **symlinked** source is
  **skipped**, never replaced in place (which would orphan the real file it points at).
- **The swap is made durable, not just atomic.** A `rename` is atomic for a concurrent reader, but
  POSIX does not make it *persistent* until the containing directory is `fsync`'d - a power loss an
  instant after `rename()` returns can otherwise lose it, and in the container-changing case the
  source was already removed, leaving the entry pointing at nothing. holdfast `fsync`s the encode
  **before** the rename and the parent directory **after** it (the POSIX durable-rename recipe); if
  that directory `fsync` fails the source is **kept**, never removed under an unproven rename. True
  power-loss survival is filesystem- and hardware-dependent (and untestable in CI without a power-cut
  harness) - this is the portable discipline, documented as such, not an absolute guarantee.
- **The quality gate bounds the worst frame, not just the average.** An average hides local damage -
  Netflix says so outright - so a short destroyed segment inside an otherwise-clean encode passes a
  mean-only gate, and passes every structural check too (it decodes fine and carries the right duration,
  packets and streams). Both floors are **on by default**. An output that cannot be *measured* is
  rejected, not assumed good.
- **Config-as-code.** YAML, validated, in git - not clickops that vanishes on rebuild.
- **Open source** (AGPL-3.0).

### We are not the only tool that verifies before it replaces

[**Alchemist**](https://github.com/bybrooklyn/alchemist) (AGPL-3.0, Rust) works the same axis: it validates
output quality before promoting the result, keeps your originals untouched until the new file passes, and
ships its own *Migrate from Tdarr* guide. If you are choosing between us, choose on the difference, not on
a claim of uniqueness we would not be able to defend.

**The difference is where the default sits.** Alchemist's VMAF scoring is **opt-in**. `holdfast`'s gate is
**default-on, layered, and fails closed**: structural parity (codec, duration, packets, per-type stream
counts, strictly-smaller) *and* full decode-integrity *and* VMAF - both its average **and** its worst
frame. An output that cannot be **measured** is **rejected**, never assumed good; an ffmpeg without libvmaf
stops the tool rather than quietly downgrading the gate. That is the whole claim, and it is narrower and
truer than "the only one that checks".

## Non-goals

Codec-only, same-content re-encoding (no resolution downscaling); HDR10 **static** metadata is preserved
but Dolby Vision / HDR10+ dynamic metadata is **detect-and-skipped**; interlaced and exotic-chroma sources
are **skipped, not converted**. It transcodes files in a library other tools manage (Plex/Jellyfin/*arr) -
it is not a media server or library manager.

## Quick start

**Docker (the supported path).** The image bundles a pinned, checksum-verified ffmpeg with libx265,
libsvtav1 and **libvmaf** - the perceptual gate needs it, and an output that cannot be measured is
rejected rather than accepted, so the right ffmpeg is not a convenience:

```bash
mkdir -p state && sudo chown 1000:1000 state   # must be writable by the user: in the compose file
cp config.example.yaml config.yaml             # then edit the three container keys below
docker compose config -q && docker compose up -d
```

A container config differs from a bare-metal one in exactly three places - miss the third and the
dashboard is unreachable from the host (the API would be bound to the *container's* loopback):

```yaml
library_roots: [/media]     # the CONTAINER path your library is mounted at
state_dir: /state           # the mounted volume - it must survive restarts
server_addr: 0.0.0.0:8080   # compose publishes it on 127.0.0.1 only
```

See **[docs/docker.md](docs/docker.md)** for volumes, permissions, timezone, GPU passthrough and the
security posture - and **[docs/migration.md](docs/migration.md)** if you are coming from Tdarr or from the
Bash transcoder.

**From source:**

```bash
cp config.example.yaml config.yaml   # then edit library_roots
holdfast validate --config config.yaml
holdfast run --config config.yaml   # one scan: re-encode bloated non-HEVC video, safely
holdfast serve --config config.yaml # HTTP API + web dashboard (scan on demand / on an interval)
holdfast resolve --config config.yaml  # list (and resolve) any job whose swap outcome is unknown
holdfast restore --config config.yaml  # what the undo window is holding (see below)
holdfast export --config config.yaml --out ledger.ndjson  # the whole ledger, as NDJSON
```

`run`/`serve` need `ffmpeg` and `ffprobe` on `PATH` (or set `HOLDFAST_FFMPEG` / `HOLDFAST_FFPROBE`); they
exit non-zero if they are missing rather than silently doing nothing. Use a build with **libx265** and
**libvmaf** - a distro ffmpeg typically lacks the latter, which is why the image exists.

### The filesystem check at startup

The no-loss contract holdfast is built on is stated for a **local** filesystem: an atomic
same-filesystem rename whose failure means it did not happen, a stat that can see a concurrent
rewrite, and a SQLite WAL that works at all. None of those hold on a NAS. So before the first encode,
`run` and `serve` both classify every path this run would act on - every library root, the state
directory, and every filesystem mounted beneath a root - and start or refuse the whole run in one
decision, printing the exact line that would permit each path it refused:

```yaml
allow_non_local:
  - /media/tv        # per PATH, never a global switch; the guarantee is REDUCED here and it says so
```

Only a positive identification counts as local: an unrecognised type, an overlay, a `tmpfs` and
anything in user space (FUSE) are all treated as not-local, because a false warning costs one line of
configuration and a false clear costs a film. **[docs/filesystem.md](docs/filesystem.md)** has the
recognised-local set, the opt-in rules and what the startup traversal costs.

### The undo window (`restore`) - off by default

The swap is the one irreversible thing holdfast does, and every gate in front of it is an **estimate**.
The delete is not. `undo_window_hours` buys a bounded period in which a swap can be walked back:

```yaml
undo_window_hours: 24     # 0 (the default) = a swap is FINAL, and startup says so
```

```bash
holdfast restore --config config.yaml                     # what is held, and for how long
holdfast restore --config config.yaml /media/tv/ep.mkv    # put that original back
```

The original is kept by a second **hard link**, so retention costs **no space at the moment it is
taken** - but the space a swap reclaimed **does not come back until the window closes**, which for a
first library pass means holding every original it replaced. So the API reports
`bytes_held_by_undo_window` **separately** from the reclaimed totals, and a release reports the bytes it
**actually** returned (removing a name frees the data only when it was the last one). A source whose
original cannot be retained is **skipped, not swapped**, and a restore refuses rather than overwrite a
file that has changed since the swap. Full reference, including what it costs and what it deliberately
does not offer: **[docs/undo.md](docs/undo.md)**.

### Web API + UI (`serve`)

`holdfast serve` runs a REST API + [SSE](https://developer.mozilla.org/docs/Web/API/Server-sent_events)
live stream and an **embedded web dashboard** (baked into the single binary - no assets to deploy). It is
a **read-and-control** surface on top of the config-as-code engine: the YAML file stays the source of
truth and the SQLite store stays the source of job state. The API can only **read the store, start a
scan, and pause/resume the feeding of new files** - it never touches a media file, so the data-safety
invariant is entirely unaffected.

| Method & path | Auth | Purpose |
|---|---|---|
| `GET /` | - | the embedded dashboard |
| `GET /api/summary` | - | counts per status + bytes reclaimed (**lifetime** and this-run) + `bytes_held_by_undo_window` (space a retained original still holds, never folded into either reclaimed figure; `null` = unreadable) + paused/scanning + the **whole-ledger aggregates** (see below) |
| `GET /api/queue` | - | pending + active jobs, capped, with `queue_total` - see *The total behind a cap* |
| `GET /api/history?limit=N` | - | recent terminal jobs (done/skipped/failed, plus `indeterminate` and `applied-despite-error`) with their recorded outcome, capped, with `history_total` - see below |
| `GET /api/events` | - | SSE: a fresh snapshot on every state change |
| `GET /metrics` | - | Prometheus metrics (when `metrics_enable`, default on) |
| `POST /api/rescan` | token | start a library scan (409 if paused / scanning / outside the run window) |
| `POST /api/pause` | token | stop feeding **new** files (in-flight encodes finish safely) |
| `POST /api/resume` | token | clear the pause flag |

Fail-safes: the server **binds `127.0.0.1` by default**, and that bind is the whole of what
protects the read endpoints and the dashboard - they carry no authentication of their own, so
a reverse proxy in front of them is the only barrier there is (the reverse-proxy posture is in
[docs/docker.md](docs/docker.md), and it is worth reading before you give holdfast a hostname);
the mutating endpoints require a bearer token (`server_auth_token`, best set via
`HOLDFAST_SERVER_AUTH_TOKEN`) and are **disabled entirely when no token is set**; pause only ever
*delays* work - it never interrupts an encode or the atomic swap. **Known limitation:** single-token auth
(no per-user accounts); the queue/history views are capped at the most recent rows, not the whole ledger -
but they now say what they were capped *against*, and `holdfast export` gives you the whole thing.

### The total behind a cap

`GET /api/queue` returns at most **500** rows and `GET /api/history` at most **200**. A truncated view that
says nothing about what it truncated reads as the whole ledger, and a client cannot work it out for itself
(the summary counts answer a different question - rows *per status*, not the rows a response selected). So
every capped response carries the total it capped against, counted in the server over **every matching row
in the `jobs` table**:

| Response | Field |
|---|---|
| `GET /api/queue` | `queue_total` |
| `GET /api/history?limit=N` | `history_total` |
| the SSE snapshot | both |

```json
"history_total": {
  "available": true, "unavailable": "",
  "covers": "every row in the ledger with status done, skipped, failed",
  "cap": 200, "count": 41237
}
```

- **`count`** is the number of matching rows in the ledger, **never the number of rows returned**. Asking
  for fewer rows than the cap (`?limit=5`) reports the *same* `count`; only `cap` moves with the request.
- **`available`** is `false` when the total could not be read, and `count` is then an explicit **`null`**,
  never `0` - a zero would claim the ledger is empty beside rows the caller can see. The rows still ship:
  one unreadable figure never costs an operator the records.
- The dashboard renders that total in each table's cap notice, and when the total is unavailable it says so
  **and shows no figure in its place**.

### The record, and what to read for it

A terminal job carries the evidence the engine used to decide, so a swap can be
audited after the fact instead of trusted: which gate refused an output, which
guard skipped a file, which encoder ran, the VMAF mean AND worst frame, the model
and pixel format both streams were converted to before scoring, and the chroma
measurement that says whether the COLOUR survived.

An in-flight job reports how far it has got. The whole-ledger figures say what
they are computed over and mark what they cannot cover. The ledger can be bounded
(`history_retention_rows`, off by default) and exported (`holdfast export`).
`serve` also carries the observability and host-fair scheduling surfaces.

Every field, every figure and the exact semantics: **[`docs/api-reference.md`](docs/api-reference.md)**.
The dashboard's own methodology is in
[`docs/dashboard-methodology.md`](docs/dashboard-methodology.md).


## Build

Requires Go 1.25+.

```bash
make build        # -> ./holdfast
make test         # go test -race ./...
make check        # THE gate - see the `check:` target in the Makefile for what it runs.
                  # CI and the release workflow run this same target, not a copy of it.

make image        # build the container image (docker buildx)
make image-smoke  # build it, then drive a REAL encode inside it and assert the no-loss
                  # contract held. This - not "it built" - is the packaging gate CI runs.
```

The Go test suite drives **real ffmpeg**: it fails loudly if `ffmpeg`/`ffprobe` (or `libvmaf`) are
missing rather than skipping, because a skipped safety proof is a false green.

## Provenance

`holdfast` is the standalone extraction and full build-out of a config-as-code HEVC transcoder that began
life as a Bash script inside a private homelab repo. That predecessor already proved the no-loss contract
(verify-then-swap-then-delete, HDR-aware, crash-safe) against a real-ffmpeg fixture suite; this project
ports it to Go and grows it into a production application (persistent queue, worker pool, hardware-encoder
matrix, web UI, observability). The phased plan and its research live in the umbrella that tracks this repo.

## License

[AGPL-3.0](./LICENSE).

### Running a modified holdfast on a network

The dashboard offers its Corresponding Source: every response the root path serves carries a link to the
source, the licence name and the build identity the `version` subcommand reports. AGPL-3.0 section 13 binds
whoever runs a **modified** holdfast over a network to offer that source, so if you fork this, point the
offer at **your** tree. It is a build-time value on both paths that build the binary, and you never patch
the embedded HTML to change it:

```bash
make build SOURCE_URL=https://git.example.org/me/holdfast
make image SOURCE_URL=https://git.example.org/me/holdfast
docker buildx build --build-arg SOURCE_URL=https://git.example.org/me/holdfast .
```

Leave it unset and the binary offers this tree. The value must be an absolute `http://` or `https://` URL:
`serve` **refuses to start** on anything else and names the value it rejected, rather than serving an offer
nobody can follow or quietly falling back to upstream, which would tell your users that upstream is the
source of a binary it is not.
