# holdfast

**A config-as-code, data-safe, self-hosted media transcoder - an open-source [Tdarr](https://tdarr.io) replacement.**

`holdfast` watches a media library, re-encodes bloated non-HEVC/non-AV1 video to a smaller modern codec
to reclaim disk space, and - the whole point - **never destroys a source until a replacement is provably
faithful**. It is configured entirely by **YAML** (config-as-code), so what it does is reviewable and
reproducible from git, not hidden in a UI database.

> **Status: major version zero, so anything MAY change. The newest release and its notes are on the
> [releases page](https://github.com/NSchatz/holdfast/releases).** This repository
> was built phase by phase from a mature, battle-tested Bash predecessor (see _Provenance_). **The
> data-safety core (`TRANSCODE-1`)** is the heart of it: `holdfast run` performs one oneshot scan of the
> library roots - skip guards → same-directory temp encode → the full verify gate → atomic swap → delete
> - proven by a real-ffmpeg fixture suite that reds on the specific regression. Colour/HDR preservation,
> the VMAF perceptual gate, a crash-safe queue and worker pool, hardware/AV1 encoders, the REST/SSE API,
> observability, host-fair scheduling and a multi-arch non-root image are built on top of it. The plan of
> record is the program brief, [`.claude/goals/2026-09-holdfast.md`](.claude/goals/2026-09-holdfast.md) -
> decided 2026-09-29 by the owner (T2, T8). A minor `v*` tag is cut by that program or by the owner -
> decided 2026-09-29 by the owner (T37) - and renaming the repository or flipping its visibility stays
> the owner's act: [`docs/release.md`](docs/release.md) is the ordered runbook and says which of its steps
> can be undone.

## Why another transcoder?

*Every claim about another tool here, and in [docs/comparison.md](docs/comparison.md), was checked **as
of September 2026** against that project's own licence text or project page. Other tools move: re-check
before you choose.*

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
- **A library health sweep that only reports.** Off by default; with `health_sweep_interval_hours`
  set, `serve` fully decodes every source on that schedule, inside the run window and below the
  encodes, resumes after a restart, and reports each file `ok`, `corrupt` or `unreadable` over the
  read API (`GET /api/health`), the `holdfast_health_sweep_*` metrics and a notification. It
  **never moves, renames, deletes or repairs a file**: what to do about one is the operator's call
  ([docs/design/health-sweep.md](docs/design/health-sweep.md#health-sweep)).
- **Radarr, Sonarr and Plex are told, and a file being played is left alone.** Off by default.
  With a target's address and credential set, a committed swap asks the one Radarr movie or
  Sonarr series that owns the file's directory to rescan (`RescanMovie`, `RescanSeries`) and asks
  Plex for a partial scan of that directory - once each, off the encode workers, and a failed
  request is a warning that changes nothing about the job. With Plex configured, a file Plex is
  playing is not started and not swapped until it stops; the hold only delays, and it fails open.
  Read the re-download warning first
  ([docs/post-swap-hook.md](docs/post-swap-hook.md#x265-warning),
  [docs/design/media-clients.md](docs/design/media-clients.md#media-clients)).
- **Sonarr and Radarr tell holdfast, natively.** Off by default. With `webhook_token` set, a
  `Connect > Webhook` connection pointed at `/api/webhook/sonarr` or `/api/webhook/radarr` queues
  every imported, upgraded or renamed file through the same targeted scan `POST /api/scan` feeds -
  no script and no shim. The credential is one of its own that can only queue a file, sent as the
  connection's Password or an `Authorization` header and never in the URL
  ([docs/docker.md](docs/docker.md#telling-holdfast-about-one-file-sonarr--radarr)).
- **Config-as-code.** YAML, validated, in git - not clickops that vanishes on rebuild.
- **Open source** (AGPL-3.0).

### We are not the only tool that verifies before it replaces

We are not, and the field is described rather than dismissed: **Alchemist** works the same axis and is
ahead of holdfast on six capabilities, **FileFlows** and **Unmanic** work this ground too, and the one
claim holdfast makes for itself is narrow - its verify gate is default-on, layered and fails closed.
**[docs/comparison.md](docs/comparison.md)** has all of it, each claim checked against that project's
own licence text or project page.

## Non-goals

One boundary, stated in full below and settled: [library management](#non-goal-library-manager).
Dolby Vision and HDR10+ are no longer one: they are [carried on the cpu encoder](#dynamic-hdr-carried),
behind gates of their own, and skipped where they cannot be. Distributed processing is no longer one
either: [worker nodes](#worker-nodes) exist, off until configured, and the paragraph below says what
they may do and what the server keeps for itself. Exotic-chroma
and `multi-video-stream` sources are **skipped, not converted**. Four things are NOT boundaries - they
are the transformations this tool makes on request, each **off by default**:
[interlacing](#interlacing-posture), [the resolution ceiling](#downscaling-posture),
[black bars](#crop-posture) and [audio](#audio-posture).

<a id="interlacing-posture"></a>

**Interlaced sources are deinterlaced on request, and skipped otherwise.** `deinterlace` is **off by
default**. Set it (`yadif`/`bwdif`) and a root's interlaced sources are deinterlaced before encoding,
so **the replacement is no longer the same content as the source**: the fields are gone and the swap
deletes the original, as a startup notice says.
**Frame-rate-preserving** only - one frame per field is refused, since it doubles the frame count two
parity gates grade. **Telecined sources are skipped** under their own guard whatever the key says (that
needs inverse telecine, which this build does not do), and so is a cadence nobody could establish. No
floor moves: the gate scores the encode against a reference put through the **same filter at the same
parameters**.

<a id="downscaling-posture"></a>

**Resolution downscaling is available, and `max_height` is off by default.** Set it on a root (or one
band of its `rules`) and every taller source is scaled to it in the source's aspect ratio before
encoding, so **the replacement is no longer the same content as the source**: those pixels are gone and
the swap deletes the original. 4K-to-1080p is the largest reclaim most libraries have and it is a trade,
so it is opted into TWICE where it cannot be walked back: with `undo_window_hours` at its default of `0`
a swap is final, and a file this key would scale is **skipped** until the root also sets
`downscale_acknowledged: true` (or you open the window).
No floor moves: the gate scales the **output back up** and is **scored at the source's resolution**,
against the source as it is, so the figures carry what was lost - scoring against a source resampled
*down* would take that detail out of both sides and hide it, and the row says which resolution it
measured at. Keys: [docs/profiles.md](docs/profiles.md#resolution-rules).

<a id="crop-posture"></a>

**Black bars are cropped on request, and kept otherwise.** `crop` is **off by default**. Set
`crop: auto` on a root and each source's bars are found by sampling it at ten points and cut away
before encoding, so **the replacement is no longer the same content as the source**: those rows
are gone and the swap deletes the original. It is cut only where the samples agree and the area
removed is black on **every frame**; a Dolby Vision source is cut only to the active area its own
RPU names, with that metadata zeroed and gated in the replacement. Samples that disagree (a mixed
aspect ratio), bars with anything in them, an RPU that disagrees with the picture, and every case
it cannot decide encode the whole frame, and the row says why. No floor moves: the gate scores the encode against the source put
through the **same crop**, and a crop gate holds the output to the declared size and the removed
area to black. Key: [docs/profiles.md](docs/profiles.md#crop); the reasoning:
[docs/design/crop.md](docs/design/crop.md).

<a id="audio-posture"></a>

**Audio is re-encoded on request, and copied otherwise.** Every audio key is **off by default**, so
each carried track is stream-copied as it always was (a root still says which tracks it carries:
`audio_languages`, `subtitle_languages`, `keep_commentary`, `remux_only` - see
**[docs/profiles.md](docs/profiles.md#stream-selection)**). Set `audio_reencode: on` with an
`audio_codec` (`aac`, `ac3`, `eac3` or `opus`) and a root's lossless tracks - TrueHD, DTS-HD MA, PCM,
FLAC - are re-encoded at a bitrate per layout, **replacing** the source track unless
`keep_original_audio: true` keeps both; `audio_downmix: stereo` adds a stereo track beside a surround
one; `audio_loudness: ebu_r128` normalises the re-encoded and added tracks to EBU R 128 in two passes.
A layout the codec cannot carry (7.1 into AC-3 or E-AC-3) is copied, and the row says why. Every
transformed track passes its own gates before the swap - decoded length, channel count and layout,
sample rate, a full decode of every audio stream, and loudness within tolerance - and the whole file
must still be strictly smaller. Keys: [docs/profiles.md](docs/profiles.md#audio); the reasoning:
[docs/design/audio.md](docs/design/audio.md).

Text subtitles can also be copied out beside a replacement: with `subtitle_sidecars: text`, each
carried SubRip, ASS and WebVTT stream is written, unconverted, to `<name>.<lang>[.forced].<ext>`
once the swap has committed, never over an existing file and only after it parses back complete;
the embedded streams stay in the replacement, and picture-based and `mov_text` subtitles are skipped
with a reason. See [docs/design/subtitles.md](docs/design/subtitles.md#sidecars).

<a id="worker-nodes"></a>

**Encodes can run on other machines, and the swap still cannot.** Worker nodes are **off until
`node_token` is set**; with it unset nothing about an existing deployment changes. A worker
(`holdfast worker`) only encodes. The server that owns the library names the working file a node's
output lands in, re-runs every gate against **its own copy of the source**, and makes the atomic
same-filesystem `rename(2)` itself - so the no-loss argument is the one it always was, and no
verdict of a worker licenses a swap
([docs/design/nodes.md](docs/design/nodes.md#leases)). Each node reaches media one of two ways,
chosen per node: through its own read-only mount of the library with a path map
(`worker_mode: mapped`, the default), or by the server streaming the source to it over HTTP
(`worker_mode: http`, no mount at all). Either way the output comes back over HTTP with its length
and a sha-256 digest, on a lease the server can refuse, and the node reports the sha-256 of the
source bytes it read, which the server compares with its own before any gate. The transport is TLS
or loopback: a worker refuses a plain `http://` server on any other host unless
`worker_insecure_http: true` says so, and logs that at every start
([docs/design/nodes.md](docs/design/nodes.md#transport)). A complete deployment, both modes, is in
**[docs/docker.md](docs/docker.md#worker-nodes)**.

The limits, stated rather than hidden: only a plan whose command line is self-contained on another
host is leased - a software encoder, software decode, no loudness-normalised track, no dynamic-HDR
carriage - and every other job, hardware encodes included, is encoded by the server
([which jobs](docs/design/nodes.md#leasable)). Every node output costs the server a source hash, a
full decode-integrity pass and a VMAF run, so nodes move the encode off the server and not the
proof. To use more of ONE machine, raising `workers` (default 1) is still the lever
(**[docs/docker.md](docs/docker.md#workers-cpus-and-max-load)**).

<a id="dynamic-hdr-carried"></a>

**Dolby Vision profile 8.1 and HDR10+ are carried through libx265, and only through it.** On the `cpu`
encoder a profile 8.1 source keeps its RPU (`-dolbyvision 1`, with the VBV ceiling and mastering display
x265 requires) and an HDR10+ source keeps its SMPTE2094-40 metadata (extracted by `hdr10plus_tool`,
validated against the source's frame count, written back by x265), by default. holdfast converts Dolby Vision
profile 7 only on request: `dolby_vision_p7: convert` rewrites it to 8.1 with `dovi_tool` before the
encode, discarding the enhancement layer, and the default skips it. Profile 5 stays skipped, and so does
any other profile. Every other encoder skips such a source, as does a remux-only root, because only
libx265 is shown to carry the metadata. The perceptual gate compares pixels and an RPU is not pixels, so
the swap waits on gates of its own: the output's DOVI configuration record must name the planned profile
and compatibility id, and every frame must carry its RPU and its HDR10+ metadata. A source whose metadata
cannot be read, or whose tool is not installed, is skipped by name, never encoded flat. One limit is
stated rather than hidden: an HEVC source is skipped as already at the cpu encoder's target codec before
any of this is asked, and every Dolby Vision profile 7 and 8 source is HEVC
([docs/design/dynamic-hdr.md](docs/design/dynamic-hdr.md#reach)).

<a id="non-goal-library-manager"></a>

**Library management is a permanent non-goal.** No renaming to a scheme, no moving between folders, no
metadata fetch, no duplicate detection, no deletion of anything but a source whose verified replacement
passed.

Every gate here is one judgement made by comparing two video files, and not one of those operations can
be judged that way: whether a file belongs in another folder, under another name, or is a duplicate
worth losing, is a question about a library's conventions that no decoder can answer. Shipping them
would mean shipping mutations with nothing to gate them, in the binary that offers a gate for everything
else. Plex, Jellyfin and the *arr tools are where that work belongs.

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
API is unreachable from the host (it would be bound to the *container's* loopback):

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
holdfast run --config config.yaml --file /media/tv/a.mkv  # ONE file, for real: every gate, and the swap
holdfast run --config config.yaml --queue-order smallest --limit-encodes 5  # the proving pass: five real encodes, smallest first
holdfast run --config config.yaml   # the whole library (--limit 5 bounds it)
holdfast serve --config config.yaml # HTTP API (scan on demand / on an interval)
holdfast resolve --config config.yaml  # a job whose swap outcome is unknown
holdfast restore --config config.yaml  # what the undo window is holding
holdfast export --config config.yaml --out ledger.ndjson  # the whole ledger, as NDJSON
```

`run`/`serve` need `ffmpeg` and `ffprobe` on `PATH` (or set `HOLDFAST_FFMPEG` / `HOLDFAST_FFPROBE`); they
exit non-zero if they are missing. Use a build with **libx265** and
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

### Before you let it near the library (`plan`, and `dry_run`)

<a id="plan-versus-dry-run"></a>
`dry_run` is a full daemon pass that probes, guards and **records a terminal row per file it decided**
without encoding anything - a record of what THAT RUN decided, written into the ledger. `holdfast plan`
is a read that walks the configured coverage set, runs every skip guard and reports what a run would
do - **claiming nothing and writing nothing**, not a job row, not a ledger row, not a byte anywhere. So
`dry_run is a full daemon pass` and `plan is a read`: neither is an alias for the other, and a caller
invoking one never gets the other's observable effect.

```
holdfast plan --config config.yaml            # eligible files, eligible bytes, and which guard skipped the rest
holdfast plan --config config.yaml --json     # the same plan as one JSON document on stdout
```

Every skip token this build records has a row in
**[the guard table](docs/api-reference.md#skip-guards)**.

Per library root, `plan` also publishes that root's **scope** (`roots` in the JSON document): the
`exclude_paths` and `include_paths` in force for it, the whole source-named `library` under it before
any filter, what a path filter kept out (`excluded_by_path_filter`), what a parked job's record held
back (`held_back_by_record`), what is `covered` and `eligible` after them, and the directories it read
and could not read. So "how big is this library, and how much of it will holdfast consider" has one
answer with the difference named, rather than a gap between this report and a census that applies no
filter. A directory an exclude pattern reaches whole is not listed at all, so the files beneath it are
in no figure and it is counted among the directories not read; no reclaim is projected per root.
`holdfast analyze` withholds a filtered file from its sources under the mechanism `path filter`, so
the two commands agree.

The reclaim figure is an **estimate and says so wherever it appears**, derived from the size ratios of
encodes **this install has already completed** and published with the sample size and the spread it came
from. On an install that has never completed one, it is **refused outright with the reason** rather than
emitted as a zero or borrowed from somebody else's average - and a refused projection still exits `0`,
because the report was produced. There is deliberately no per-file estimated saving and no predicted
VMAF: a library-scale ratio printed against one file reads as a measurement of that file, and nothing
here has looked inside it.

### Per-job settings, and where the encode works

`encode_profiles` overrides the top-level encode settings per job (ordered; the first profile whose
`match` glob selects a source wins), `bitrate_kbps` swaps the quality target for a target-bitrate rate
control, and `exclude_paths`/`include_paths` say which paths under a root holdfast may touch at all -
both default to empty, exclude wins over include, and none of the three is reachable from a flag:
**[docs/profiles.md](docs/profiles.md)**. `scratch_dir`
moves the encode's **working file** elsewhere and nothing else - the accepted result is still copied
back beside the source and finalized by the same atomic rename: **[docs/scratch.md](docs/scratch.md)**.

### The order files are offered in

`queue_order` picks which candidate a scan offers its workers first: `path` (the default, the
library's own traversal, the only order that streams), `largest`, `smallest`, `newest`, `oldest`, or
`savings_per_hour` - the source whose encode is estimated to reclaim the most bytes per hour of encode
plus verify work, which probes every candidate once before the first is offered. That estimate is an
ordering key and nothing more: it is published nowhere, in keeping with there being no per-file
estimated saving. A `priority` on a library root, a resolution rule or an encode profile orders ahead
of it, higher first. Both decide **sequence only** - never which files are offered, never any guard,
gate or recorded decision input, so changing either re-opens no row:
**[docs/design/queue-order.md](docs/design/queue-order.md#queue-order)**.

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

### Web API (`serve`)

`holdfast serve` runs a REST API + [SSE](https://developer.mozilla.org/docs/Web/API/Server-sent_events)
live stream. **holdfast currently ships no frontend: the HTTP JSON API is the interface**, and the root
path serves a plain-text page naming the endpoints. (Reversed -
decided 2026-09-29 by the owner (T14, T18): a web UI, a single-page application with its own Node
build, is to ship on top of this API; until that release, this paragraph is what holdfast does.) It is
a **read-and-control** surface on top of the config-as-code engine: the YAML file stays the source of
truth and the SQLite store stays the source of job state. The API can only **read the store, start a
scan, and pause/resume the feeding of new files** - it never touches a media file, so the data-safety
invariant is entirely unaffected. The one route group that carries media bytes is the worker-node
lease protocol under `/api/node/v1`, which is closed until `node_token` is set and takes no other
token: there a node's upload lands in a working file the server named, as a candidate the server's
own gates and rename then decide ([worker nodes](#worker-nodes)).

Every endpoint, what it answers and which of them need a token:
**[`docs/api-reference.md`](docs/api-reference.md)**.

Fail-safes: the server **binds `127.0.0.1` by default**. With `server_read_token` unset -
the shipped default - that bind is the whole of what protects the read endpoints, so a
reverse proxy in front of them is the only barrier there is; set it and the five `/api`
reads require a bearer token of their own, which makes the proxy defence in depth instead.
It does **not** gate the root path or `/metrics`, neither of which carries a library datum
(the reverse-proxy posture is in [docs/docker.md](docs/docker.md), and it is worth
reading before you give holdfast a hostname);
the mutating endpoints require a bearer token, reached **by reference**
(`server_auth_token: file:/run/secrets/holdfast-token` - a literal token there, or in
`HOLDFAST_SERVER_AUTH_TOKEN`, refuses to start; see [docs/secrets.md](docs/secrets.md)) and
are **disabled entirely when no token is configured**; the Sonarr/Radarr webhook intake takes a
third credential of its own (`webhook_token`) that authorises nothing else, and is likewise disabled
without it; pause only ever
*delays* work - it never interrupts an encode or the atomic swap. **Known limitation:** three
single-value tokens and no per-user accounts; the queue/history endpoints are capped at the most recent rows, not the whole ledger -
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

Every field, every figure and the exact semantics: **[`docs/api-reference.md`](docs/api-reference.md)**.


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
matrix, observability). The plan of record and the research behind it are the program brief and its
research in [`.claude/goals/`](.claude/goals/) - decided 2026-09-29 by the owner (T2, T8); the umbrella's
spec pipeline no longer plans holdfast, and its `S0NNN` numbers stay in the history.

## License

[AGPL-3.0](./LICENSE).

### Running a modified holdfast on a network

`holdfast serve` offers its Corresponding Source: every response the root path serves carries the source
URL, the licence name and the build identity the `version` subcommand reports. AGPL-3.0 section 13 binds
whoever runs a **modified** holdfast over a network to offer that source, so if you fork this, point the
offer at **your** tree. It is a build-time value on both paths that build the binary:

```bash
make build SOURCE_URL=https://git.example.org/me/holdfast
make image SOURCE_URL=https://git.example.org/me/holdfast
docker buildx build --build-arg SOURCE_URL=https://git.example.org/me/holdfast .
```

Leave it unset and the binary offers this tree. The value must be an absolute `http://` or `https://` URL:
`serve` **refuses to start** on anything else and names the value it rejected, rather than serving an offer
nobody can follow or quietly falling back to upstream, which would tell your users that upstream is the
source of a binary it is not.
