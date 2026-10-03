# Migrating to holdfast

Two starting points are covered: the **Bash transcoder** this project grew out of, and
**Tdarr**, which it was written to replace.

The good news in both cases is the same, and it is a consequence of the design rather than a
migration feature: **there is no state to import.** `holdfast` decides what to do with a file
by *looking at the file* — a source already in the target codec is skipped after a cheap
`ffprobe`. So pointing it at a library another tool has already processed is safe and cheap: it
re-examines everything and re-encodes only what is still bloated. A migration that needs no
database import cannot corrupt one.

What you should **not** do is run two transcoders over the same library at once. Stop the old
one first. Both write a temp file next to the source and both delete sources; nothing good comes
of them racing.

---

## From the Bash transcoder (its predecessor in a private repo)

Same contract, same guards, same defaults — this is a port, not a rewrite. `transcode.conf`
became YAML, and the environment-variable overrides became `HOLDFAST_<KEY>`.

### Config mapping

| `transcode.conf` | `config.yaml` | Notes |
|---|---|---|
| `MEDIA_ROOT=/mnt/media` | `library_roots: [/mnt/media]` | Now a **list** — several roots are supported. |
| `VIDEO_EXTS="mkv mp4 …"` | `video_exts: [mkv, mp4, …]` | A YAML list, not a space-separated string. |
| `ENCODER=cpu` \| `nvenc` | `encoder: cpu` \| `nvenc` | Same keys; `svtav1`, `av1_nvenc`, `qsv`, `vaapi`, `amf`, `x264`, `h264_nvenc`, `h264_qsv`, `h264_vaapi`, `h264_amf`, `av1_qsv`, `av1_vaapi`, `av1_amf` are new. |
| `CRF=22` | `crf: 22` | Also the CQ/QP target for the hardware encoders. |
| `PRESET=slow` | `preset: slow` | Mapped to SVT-AV1's numeric scale for `svtav1`; ignored by the hardware encoders. |
| `NVENC_CQ=24`, `NVENC_PRESET=p5` | *(collapsed into `crf`)* | The per-encoder quality knobs are now one knob. `NVENC_PRESET` has no equivalent. |
| `PIXEL_FORMAT=yuv420p10le` | `pixel_format: auto` | **Behaviour change — see below.** |
| `NVENC_PIXEL_FORMAT=p010le` | *(gone)* | `pixel_format` is unified across encoders. |
| `CONTAINER_EXT=mkv` | `container_ext: source` | **Behaviour change — see below.** |
| `MIN_BITRATE_KBPS=2500` | `min_bitrate_kbps: 2500` | Identical. |
| `MIN_SAVINGS_PERCENT=0` | `min_savings_percent: 0` | Identical. |
| `DURATION_TOLERANCE_SEC=1` | `duration_tolerance_sec: 1` | Identical. |
| `MAX_FAILURES=3` | `max_failures: 3` | Identical. The counter resets on migration (the old ledger is not imported). |
| `SKIP_HARDLINKED=1` | `skip_hardlinked: true` | Identical. |
| `DRY_RUN=1` | `dry_run: true` | Identical. |
| `ONESHOT=1` | `holdfast run` | Oneshot is now a **subcommand**, not a flag. |
| `SLEEP_INTERVAL=3600` | `scan_interval_sec: 3600` | Only meaningful under `holdfast serve`. |

The ledger and heartbeat under `/config/state/` have no equivalent and are not imported. Their
replacement is the SQLite job store in `state_dir` — which is *crash-safe*, so a killed run
resumes rather than restarting. Delete the old `/config/state/` once you are happy.

### Two behaviour changes worth knowing before you cut over

Both are *deliberate improvements*, and both mean the new tool will not do exactly what the old
one did on some files.

1. **`pixel_format` now defaults to `auto`, not a forced `yuv420p10le`.** The Bash tool forced
   every source to 10-bit 4:2:0 — which silently *subsampled* a 4:2:2 or 4:4:4 source. `auto`
   preserves the source's chroma subsampling and floors bit-depth at 10, and it **skips** a
   source whose pixel format it does not recognise rather than guessing. To reproduce the old
   behaviour exactly, set `pixel_format: yuv420p10le`. Don't, unless you know you want it.

2. **`container_ext` now defaults to `source`, not a forced `mkv`.** The Bash tool rewrote
   every source into MKV, which cannot carry some MP4-native stream types (e.g. `mov_text`
   subtitles). The default is now an **in-place** transcode: `mp4` → `mp4`, `mkv` → `mkv`. Set
   `container_ext: mkv` to force the old behaviour.

Everything else the new tool adds is a **stricter** gate, never a looser one: a VMAF perceptual
check, interlaced/Dolby-Vision/HDR10+/exotic-pixel-format skip guards, and HDR10 static-metadata
preservation. Files the Bash tool would have happily re-encoded may now be **skipped with a
logged reason**. That is the tool working.

### Cutover

**1. Stop the old one.** Do not run both.

```bash
docker compose -f media/transcoder/docker-compose.yml down
```

**2. Translate `transcode.conf`** using the table above, then prove it parses:

```bash
holdfast validate --config config.yaml
# in a container: docker compose run --rm holdfast validate --config /config/config.yaml
```

**3. Rehearse.** Set `dry_run: true` in `config.yaml` — it changes nothing and tells you exactly
what it *would* do. Read the skip reasons: that is where the two behaviour changes above show up.

```bash
holdfast run --config config.yaml
```

Then size the job from the API, not from the database. Start the daemon with `dry_run` still `true`
(`docker compose up -d`) and read `GET /api/summary`: its `roots` block gives, per library root,
`candidate_bytes` (what a real run would be asked to encode), `projected_savings_bytes` where that root
already has completed encodes to measure on, `bytes_held_by_undo_window` and `free_bytes`. That is the
whole of "how big is this, and will it fit" - there is nothing to copy off the host and query.

**4. Go.** Set `dry_run: false` again, then:

```bash
docker compose up -d
```

Rolling back is starting the old container again: the new tool leaves nothing behind that the
old one trips over (its state lives in `state_dir`, its outputs are ordinary media files).

**One regression to plan for:** the Bash deployment had a compose `healthcheck` watching a
heartbeat file, so a hung script was caught even mid-encode. The image ships no `HEALTHCHECK`
(it has no shell, and "the HTTP port is up" would answer green for a wedged encode). Use the
Prometheus metrics at `/metrics` instead — a `holdfast_files_total` that stops advancing is
the honest version of that signal.

---

## From Tdarr

`holdfast` is not a Tdarr clone, and a migration is not a port of your Tdarr setup — it is a
replacement of it. Read this before you switch.

### What you give up

- **Plugins and flows.** Tdarr's plugin/flow system is its whole extensibility model.
  `holdfast` has none. It does exactly one job — re-encode bloated video to a smaller modern
  codec, safely — and it is configured by a YAML file, not by assembling a pipeline.
- **Filters as a pipeline.** No aspect-ratio changes and no library management; black bars are
  cut only under `crop: auto` ([docs/design/crop.md](design/crop.md)). What a root CAN say about streams is which to carry:
  `audio_languages`, `subtitle_languages` and `keep_commentary` select streams and
  `remux_only` copies the video as well, and whatever is kept is stream-copied untouched
  unless an audio key says otherwise ([docs/profiles.md](profiles.md#stream-selection)). The
  transformations this tool will make on request are deinterlacing, an output height ceiling,
  cropping black bars (`crop: auto`) and the audio keys - re-encoding lossless audio tracks, an added stereo downmix and two-pass EBU R128
  loudness ([docs/design/audio.md](design/audio.md)) - each off by default and each stated in full
  in the [README's non-goals](../README.md#non-goals).

<a id="server-and-nodes"></a>

### What carries over, differently: the Server/Node model

`holdfast` has a server and worker nodes. `holdfast serve` is the server: it owns the library, the
configuration and the job state. `holdfast worker`, the same binary and the same image on another
host, is a node. Nodes are off until `node_token` is set on the server, so a single-host
deployment is still what you get by default.

| Tdarr | holdfast |
|---|---|
| Server | `holdfast serve`, with `node_token` set |
| Node | `holdfast worker`, pointed at the server with `worker_server` |
| a **mapped** node, sharing the file system with the server | `worker_mode: mapped` (the default): the node reads each source through its own read-only mount of the library |
| path translators | `worker_path_map`: an ordered list of `{from, to}` prefixes, the server's view of the library to the node's mount. A source no entry covers is refused, never passed through |
| an **unmapped** node, which downloads and uploads its working files through the server | `worker_mode: http`: the server streams the source to the node on a live lease, and the node needs no mount and no path map |
| the node opens the connection to the server | the same: a worker polls the server and needs no inbound port |
| the same version on Server and Node | enforced: a worker whose build version is not the server's is answered `409` and stops |

What differs, and it is the point:

- **A node only encodes.** In both modes the output comes back over HTTP with its length and a
  sha-256 digest and lands in a working file the server named. The server then re-runs every gate
  against its own copy of the source and makes the same-filesystem rename itself, so the no-loss
  guarantee is the one stated for a single host and no node's verdict licenses a swap. A node never
  writes into the library, mapped or not
  ([docs/design/nodes.md](design/nodes.md#leases)).
- **Work is leased, not assigned.** A lease has a time to live the node's heartbeats renew, and an
  epoch that rises at every grant of the same file, so a node that went quiet and came back cannot
  contribute an output to a job that was since given to another. The node also reports the sha-256
  of the source bytes it read, and the server compares it with its own before any gate - which is
  what catches a wrong path map entry or a stale network-mount cache.
- **One credential, and it can only lease.** `node_token`, by reference on both sides, opens the
  lease endpoints and nothing else - a node leases, uploads and, in http mode, downloads the
  source of a lease it holds - and it cannot pause, scan or read the queue. There is no per-node
  login and no mTLS. The transport is TLS or loopback unless a worker is explicitly told otherwise
  ([docs/docker.md](docker.md#worker-nodes)).
- **No plugin stack runs on a node.** A node runs one ffmpeg command line the server's plan built,
  and refuses, unrun, one outside the shape those plans have.
- **Hardware encoders on a node are not built.** Only a plan that is self-contained on another host
  is leased - a software encoder with software decode, among other conditions
  ([which jobs](design/nodes.md#leasable)); every other job, a hardware encode included, is encoded
  by the server itself. A node with a GPU does not add GPU encodes.
- **Nodes do not take the proof off the server.** Each node output costs the server a source hash,
  a full decode and a VMAF run. To use more of the ONE machine, `workers` is still the lever
  (default 1, and [docs/docker.md](docker.md#workers-cpus-and-max-load) says why).

What has not changed: running a second holdfast `serve` or `run` against one `state_dir` is not
supported ([docs/docker.md](docker.md#one-process-per-state_dir)). More machines means workers
leasing from the one server, never a second server on the same state. The deployment, both modes,
is in [docs/docker.md](docker.md#worker-nodes).

### What you get

- **The source is never replaced before the replacement is verified.** This is the reason the
  project exists. Tdarr has a documented history of replacing the original file before (or
  regardless of) its health check ([#355](https://github.com/HaveAGitGat/Tdarr/issues/355),
  [#511](https://github.com/HaveAGitGat/Tdarr/issues/511),
  [#683](https://github.com/HaveAGitGat/Tdarr/issues/683)). `holdfast` encodes to a temp file
  beside the source, and the source is replaced only by an atomic same-filesystem rename, only
  after the output passes *every* gate: correct codec, duration and packet parity, strictly
  smaller, per-type stream-count parity, full decode-integrity, and VMAF. Any failure discards
  the temp and leaves the source untouched.
- **Config-as-code.** Your configuration is a reviewable YAML file in git, not UI state in a
  database that a container rebuild can lose.
- **Open source**, AGPL-3.0.

### Migrating

1. **Stop Tdarr** (or at minimum remove the library you are handing over). Two tools that both
   delete sources must not share a library.
2. **Do not import anything.** There is no Tdarr DB import and there does not need to be — see
   the top of this page. Point `library_roots` at the same library; already-transcoded files are
   skipped after an `ffprobe`.
3. **Write a `config.yaml`** (start from `config.example.yaml`). The defaults are the safe ones.
4. **Rehearse with `dry_run: true`** and read the skip reasons. A library Tdarr has been through
   will report a lot of "already HEVC" skips — that is the correct answer, arrived at by looking
   at the files rather than by trusting a database. Size the job from `GET /api/summary` while the
   daemon is still serving with `dry_run: true`: its `roots` block reports, per library root,
   `candidate_bytes`, `projected_savings_bytes`, `bytes_held_by_undo_window` and `free_bytes`, so
   there is no need to copy `jobs.db` off the host and query it.
5. Bring it up: `docker compose up -d`, and watch `/api/summary` and `/api/events`.

### What "safe" costs you

The VMAF gate is a **second full decode** of every encode. It is the layer that catches an
output which decodes cleanly but looks worse, and it is why this tool can be trusted to delete
things — but it is not free. On a large library, raise `vmaf_subsample` (sample every Nth frame)
before you consider turning the gate off. If you turn it off, you have given up the guarantee
that made you switch.

**Know what raising `vmaf_subsample` costs you.** The gate has two floors: `min_vmaf` bounds the
encode's *average* quality, and `vmaf_min_pool` bounds its *worst frame* — the one that catches a
short destroyed segment an average would smooth away. Subsampling only measures every Nth frame,
so a damaged frame that is never sampled is never seen, and the worst-frame floor degrades from a
guarantee into a sample. `holdfast validate` warns you when this is set. Leave it at `1` for
content you cannot re-acquire.

**If the gate rejects an encode you believe is fine, lower a threshold — don't disable one.**
The two knobs fail in opposite ways. `min_vmaf: 90` or `vmaf_min_pool: 45` still *bound* how bad
the result may get; `vmaf_min_pool: 0` or `vmaf_enable: false` bound nothing at all, and the tool
goes on deleting sources. Grain-heavy and very dark libraries are where an honest encode legitimately
scores lowest, so they are the real reason to retreat — retreat by a few points, not to zero.
`holdfast validate` warns whenever the gate has been weakened this way.

**What a VMAF score does not mean.** It is a regression onto a *subjective* opinion scale, not a
measure of signal identity: 100 is a scale anchor, not "identical to the source", and a
bit-identical file is not guaranteed to score it. A pass means *"no worse than this against your
source, under this model"* — nothing stronger. The default model is also **luma-only** (blind to
chroma damage, which the structural checks catch instead) and is documented-weak on banding and
dark scenes.
