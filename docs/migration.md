# Migrating to transcode

Two starting points are covered: the **Bash transcoder** this project grew out of, and
**Tdarr**, which it was written to replace.

The good news in both cases is the same, and it is a consequence of the design rather than a
migration feature: **there is no state to import.** `transcode` decides what to do with a file
by *looking at the file* — a source already in the target codec is skipped after a cheap
`ffprobe`. So pointing it at a library another tool has already processed is safe and cheap: it
re-examines everything and re-encodes only what is still bloated. A migration that needs no
database import cannot corrupt one.

What you should **not** do is run two transcoders over the same library at once. Stop the old
one first. Both write a temp file next to the source and both delete sources; nothing good comes
of them racing.

---

## From the Bash transcoder (`homelab: media/transcoder/`)

Same contract, same guards, same defaults — this is a port, not a rewrite. `transcode.conf`
became YAML, and the environment-variable overrides became `TRANSCODE_<KEY>`.

### Config mapping

| `transcode.conf` | `config.yaml` | Notes |
|---|---|---|
| `MEDIA_ROOT=/mnt/media` | `library_roots: [/mnt/media]` | Now a **list** — several roots are supported. |
| `VIDEO_EXTS="mkv mp4 …"` | `video_exts: [mkv, mp4, …]` | A YAML list, not a space-separated string. |
| `ENCODER=cpu` \| `nvenc` | `encoder: cpu` \| `nvenc` | Same keys; `svtav1`, `av1_nvenc`, `qsv`, `vaapi`, `amf` are new. |
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
| `ONESHOT=1` | `transcode run` | Oneshot is now a **subcommand**, not a flag. |
| `SLEEP_INTERVAL=3600` | `scan_interval_sec: 3600` | Only meaningful under `transcode serve`. |

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
transcode validate --config config.yaml
# in a container: docker compose run --rm transcode validate --config /config/config.yaml
```

**3. Rehearse.** Set `dry_run: true` in `config.yaml` — it changes nothing and tells you exactly
what it *would* do. Read the skip reasons: that is where the two behaviour changes above show up.

```bash
transcode run --config config.yaml
```

**4. Go.** Set `dry_run: false` again, then:

```bash
docker compose up -d
```

Rolling back is starting the old container again: the new tool leaves nothing behind that the
old one trips over (its state lives in `state_dir`, its outputs are ordinary media files).

**One regression to plan for:** the Bash deployment had a compose `healthcheck` watching a
heartbeat file, so a hung script was caught even mid-encode. The image ships no `HEALTHCHECK`
(it has no shell, and "the HTTP port is up" would answer green for a wedged encode). Use the
Prometheus metrics at `/metrics` instead — a `transcode_files_total` that stops advancing is
the honest version of that signal.

---

## From Tdarr

`transcode` is not a Tdarr clone, and a migration is not a port of your Tdarr setup — it is a
replacement of it. Read this before you switch.

### What you give up

- **Plugins and flows.** Tdarr's plugin/flow system is its whole extensibility model.
  `transcode` has none. It does exactly one job — re-encode bloated video to a smaller modern
  codec, safely — and it is configured by a YAML file, not by assembling a pipeline.
- **The Server/Node model.** `transcode` is single-host. (Distributed worker nodes are a
  possible later phase, not a shipped feature.)
- **Anything that is not codec-only re-encoding.** No downscaling, no remuxing-as-a-feature, no
  audio/subtitle mangling, no library management.

### What you get

- **The source is never replaced before the replacement is verified.** This is the reason the
  project exists. Tdarr has a documented history of replacing the original file before (or
  regardless of) its health check ([#355](https://github.com/HaveAGitGat/Tdarr/issues/355),
  [#511](https://github.com/HaveAGitGat/Tdarr/issues/511),
  [#683](https://github.com/HaveAGitGat/Tdarr/issues/683)). `transcode` encodes to a temp file
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
   at the files rather than by trusting a database.
5. Bring it up: `docker compose up -d`, and watch the dashboard.

### What "safe" costs you

The VMAF gate is a **second full decode** of every encode. It is the layer that catches an
output which decodes cleanly but looks worse, and it is why this tool can be trusted to delete
things — but it is not free. On a large library, raise `vmaf_subsample` (sample every Nth frame)
before you consider turning the gate off. If you turn it off, you have given up the guarantee
that made you switch.
