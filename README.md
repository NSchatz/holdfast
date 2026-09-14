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

*Every claim about another tool in this section and its subsection was checked **as of September 2026**,
against that project's own licence text or project page. Other tools move: re-check before you choose.*

Tdarr is capable, but it is **licensed under an
[EULA](https://github.com/HaveAGitGat/Tdarr/blob/master/LICENSE.md)**: the licence is provided in three
tiers - Personal Free, Personal Subscription, and Business Subscription & Trial - and it prohibits
redistribution, reverse engineering, or any unauthorized use of the software without explicit permission
from Tdarr. It is also **UI/DB-configured** (state can be lost on a container rebuild), and it
historically **replaced the original file before/regardless of its health check** - a documented
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
  packets and streams). All three floors are **on by default**. An output that cannot be *measured* is
  rejected, not assumed good.
- **Config-as-code.** YAML, validated, in git - not clickops that vanishes on rebuild.
- **Open source** (AGPL-3.0).

### We are not the only tool that verifies before it replaces

[**Alchemist**](https://github.com/bybrooklyn/alchemist) (AGPL-3.0, Rust) works the same axis: it
"never overwrites anything until the new file passes its quality checks", and it ships its own
*Migrate from Tdarr* guide. If you are choosing between us, choose on the difference, not on a claim of
uniqueness we would not be able to defend - and the difference does not run one way.

**Where Alchemist is ahead.** Seven capabilities it has and holdfast does not, two of them capabilities
holdfast only half has, said plainly rather than left out:

- **Per-library profiles**, giving movies, TV and home videos different behaviour per library. holdfast
  half has this: a library root or a path glob overrides the encode settings and the gates, and no
  more ([docs/profiles.md](docs/profiles.md)).
- **Audio stream rules** - commentary stripping, language filtering, default-track retention. holdfast
  has none, by design: audio, subtitles and attachments are stream-copied untouched.
- **Sonarr/Radarr webhook intake**, through a narrowed webhook token with optional container path
  translations. holdfast has no webhook receiver at all; an *arr calls the generic scan endpoint behind
  the one control token ([docs/api-reference.md](docs/api-reference.md)).
- **A Jellyfin integration** - a narrowed plugin token for enqueue, completion events, job details and
  library refresh. holdfast ships nothing of the kind.
- **Named API tokens with access classes** - read-only, webhook, plugin, full access. holdfast has one
  bearer token at one access level, the known limitation recorded further down this page.
- **An off-peak scheduler with a priority queue.** holdfast half has this: a daily `run_window` and a
  per-core load cap, and no priority queue - work is taken in the order the scan finds it.
- **Automatic hardware selection with CPU fallback** across NVIDIA, Intel, AMD and Apple. holdfast will
  not guess: `encoder:` is configured, and a hardware encoder with no usable device stops the run
  rather than quietly falling back to CPU.

**Two more tools work this ground.** Both are described from their own project pages and nothing else -
no ranking, no popularity, no weight class, because no source this project could obtain carries one.

[**FileFlows**](https://fileflows.com/) designs, schedules and runs automated file-processing pipelines
from a single server up to a distributed cluster, offloading tasks to multiple nodes, and transcodes to
AV1, HEVC or H.264 with hardware acceleration, VMAF-optimized encoding and Dolby Vision support. Its own
site offers a free tier and carries a pricing page, so "free" there names a tier and not the product.

[**Unmanic**](https://github.com/Unmanic/unmanic) (GPL-3.0, Python, plugin-based, with a web UI) calls
itself a library optimiser: it converts a library into a single uniform format, manages file movements
based on timestamps, and runs custom commands against a file based on its size. It monitors files and
directories, so a modified or newly added file is tested against its configured presets again.

<a id="differentiator-gate"></a>

**The difference is where the default sits, and it is a claim about holdfast alone.** holdfast's verify
gate is **default-on**, **layered** and **fails closed**. Layered means every layer runs rather than the
first one that answers: structural parity (codec, duration, packets, per-type stream counts,
strictly-smaller), then full decode-integrity, then three VMAF floors - the mean (`min_vmaf`), the worst
frame (`vmaf_min_pool`) and chroma (`vmaf_min_chroma`, which the luma-only VMAF model cannot see at
all). Fails closed means an output that cannot be measured is rejected rather than assumed good: an
ffmpeg without libvmaf stops the tool instead of quietly downgrading the gate, and a score that could
not be produced is never read as a score that passed. That is the whole claim, and it is narrower and
truer than "the only one that checks".

## Non-goals

Four boundaries, and they are boundaries rather than a backlog: **no distributed or remote
processing**; **not a media server and not a library manager**; **interlaced sources are skipped, not
converted**; and **HDR10 static metadata is preserved while Dolby Vision and HDR10+ dynamic metadata
are detect-and-skipped**. Each is stated in full below, in this one section.

Codec-only, same-content re-encoding (no resolution downscaling): **interlaced**, exotic-chroma and
`multi-video-stream` sources are **skipped, not converted**; HDR10 **static** metadata is preserved
while Dolby Vision and HDR10+ **dynamic** metadata is **detect-and-skipped** rather than guessed at;
and embedded artwork is carried through unencoded. It transcodes files in a library other tools
manage - not a media server.

**Distributed or remote processing is a non-goal by design, not a missing feature.** holdfast is one
process: no server/node split, no remote workers. The no-loss argument rests on an atomic
same-filesystem `rename(2)` - it either happened or it did not, so a failure never leaves a partial
file where the source was. A remote worker encoding to its own disk and shipping the result back is a
**copy**, not a rename, and every gate here would have to be re-argued for that primitive. To use more
of one machine, raise `workers` (default 1, deliberately - see **[docs/docker.md](docs/docker.md)**).

<a id="non-goal-library-manager"></a>

**Library management is a permanent non-goal.** No renaming to a scheme, no moving between folders, no folder
organisation, no metadata fetch, no duplicate detection, no deletion of anything but a source whose verified
replacement passed: these are filesystem mutations the verify gate cannot cover, so use the tools that manage the library instead.

Every gate in this tool is one judgement made by comparing two video files, and not one of those operations
can be judged that way. Whether a file belongs in another folder, or under another name, or is a duplicate
worth losing, is a question about a library's conventions, and no decoder can answer it. Shipping them would
mean shipping mutations with nothing to gate them, in the same binary that offers a gate for everything else
it does. Plex, Jellyfin and the *arr tools are where that work belongs: point holdfast at the library they
manage, and leave the managing to them.

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
holdfast analyze --config config.yaml  # what is in the library, reading only (--health: what is broken)
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

### Per-job settings, and where the encode works

`encode_profiles` overrides the top-level encode settings per job (ordered; the first profile whose
`match` glob selects a source wins), `bitrate_kbps` swaps the quality target for a target-bitrate rate
control, and neither is reachable from a flag: **[docs/profiles.md](docs/profiles.md)**. `scratch_dir`
moves the encode's **working file** elsewhere and nothing else - the accepted result is still copied
back beside the source and finalized by the same atomic rename: **[docs/scratch.md](docs/scratch.md)**.

### The undo window (`restore`) - off by default

The swap is the one irreversible thing holdfast does, and every gate in front of it is an **estimate**.
The delete is not. `undo_window_hours` buys a bounded period in which a swap can be walked back; at its
default of `0` a swap is **FINAL**, and startup says so.

The original is kept by a second **hard link**, so retention costs **no space at the moment it is
taken** - but the space a swap reclaimed **does not come back until the window closes**, which for a
first library pass means holding every original it replaced, so the API reports
`bytes_held_by_undo_window` **separately** from the reclaimed totals. A source whose original cannot be
retained is **skipped, not swapped**, and a restore refuses rather than overwrite a file that has
changed since the swap. `holdfast restore` lists what is held and puts an original back; how to turn the
window on and off, what it costs, when it closes and what it deliberately does not offer:
**[docs/undo.md](docs/undo.md)**.

### Edit the YAML and the tool obeys (`requeue`)

A terminal row is an **answer computed from configuration**, and only for the configuration it was
computed under: each records the values the guard that wrote it read, and a scan **re-opens** one whose
values have moved. So lowering `min_bitrate_kbps` or changing the target codec reaches the files a
previous configuration answered; an edit to a key no guard read reaches nothing; re-opening is not
re-encoding; `holdfast requeue` is the LOCAL lever for the rest - **[docs/requeue.md](docs/requeue.md)**.

### Web API + UI (`serve`)

`holdfast serve` runs a REST API + [SSE](https://developer.mozilla.org/docs/Web/API/Server-sent_events)
live stream and an **embedded web dashboard** (baked into the single binary - no assets to deploy). It is
a **read-and-control** surface on top of the config-as-code engine: the YAML file stays the source of
truth and the SQLite store stays the source of job state. The API can only **read the store, start a
scan, and pause/resume the feeding of new files** - it never touches a media file, so the data-safety
invariant is entirely unaffected.

Every endpoint, what it answers and which of them need the token:
**[`docs/api-reference.md`](docs/api-reference.md)**.

Fail-safes: the server **binds `127.0.0.1` by default**, and that bind is the whole of what
protects the read endpoints and the dashboard - they carry no authentication of their own, so
a reverse proxy in front of them is the only barrier there is (the reverse-proxy posture is in
[docs/docker.md](docs/docker.md), and it is worth reading before you give holdfast a hostname);
the mutating endpoints require a bearer token, reached **by reference**
(`server_auth_token: file:/run/secrets/holdfast-token` - a literal token there, or in
`HOLDFAST_SERVER_AUTH_TOKEN`, refuses to start; see [docs/secrets.md](docs/secrets.md)) and
are **disabled entirely when no token is configured**; pause only ever
*delays* work - it never interrupts an encode or the atomic swap. **Known limitation:** single-token auth
(no per-user accounts); the queue/history views are capped at the most recent rows, not the whole ledger -
but they now say what they were capped *against*, and `holdfast export` gives you the whole thing.

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
