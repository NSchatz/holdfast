# Adversarial verification: research-streams-hdr.md

Verifier run 2026-09-29, all URLs/API reads dated 2026-09-29. Local tests used the pinned
`ffmpeg N-125875-g5d4d3bdc61-20260731` (libx265 4.2+37) with tiny lavfi clips (320x240, 24
frames, `-threads 2`, x265 `pools=2`). Scratch: `/scratch/verify-streams` (`m/` video, `a/` `a2/`
audio, `ln/` loudnorm, `cd/` cropdetect, `src/` fetched sources, `tools/` fresh tool downloads).
Source files were read at the pinned commit `5d4d3bdc61` (committed 2026-07-31T13:34:50Z) with
`gh api repos/FFmpeg/FFmpeg/contents/<path>?ref=5d4d3bdc61`. `libx265.c` at that commit is
byte-identical to master today. Nothing in /workspace was changed.

## Verdicts

| # | Claim | Verdict |
|---|---|---|
| 1 | dovi_tool 2.3.4 (2026-09-10), hdr10plus_tool 1.7.2 (2025-12-27), MIT, static musl amd64+arm64 | CONFIRMED |
| 2 | libx265 takes `dhdr10-info` and emits 2094-40; ffmpeg does not carry HDR10+ through a libx265 re-encode | CONFIRMED |
| 3 | `-dolbyvision 1` carries the RPU (P8.1, one per frame); default `auto` dropped it | Behaviour CONFIRMED. Stated mechanism REFUTED. Two prerequisites were missing |
| 4 | `x265-params dolby-vision-rpu=` is rejected | PARTLY REFUTED: the option is not honoured, but it is ignored with a warning and ffmpeg exits 0 |
| 5 | P7 to 8.1 = `-m 2 convert --discard`; mode 5 is wrong | README facts CONFIRMED; "mode 5 wrong" UNCERTAIN |
| 6 | Raw .hevc stream copy into mkv fails with "unknown timestamp" | CONFIRMED; no valid workaround found |
| 7 | Crop leaves stale L5 offsets; dovi_tool can edit L5 | CONFIRMED (tested) |
| 8 | eac3 silently maps 7.1 to 5.1 with exit 0 | CONFIRMED |
| 9 | Packet counts differ across audio codecs | CONFIRMED (caveat: ac3 and eac3 counts match each other) |
| 10 | Single-pass loudnorm outputs 192 kHz unless followed by aresample | CONFIRMED (linear-mode nuance) |
| 11 | Sonarr/Radarr and Jellyfin parse `<name>.<lang>[.forced][.sdh].<ext>` | CONFIRMED (source) |
| 12 | 10-bit absolute cropdetect limit fails; fractional works; all-black gives a negative crop | CONFIRMED |

## Evidence

### 1. Tool releases: CONFIRMED
- `gh api repos/quietvoid/dovi_tool/releases/latest`: tag `2.3.4`, `published_at 2026-09-10T00:02:20Z`,
  not a prerelease. `hdr10plus_tool`: `1.7.2`, `2025-12-27T17:14:54Z`. The tag lists have nothing newer.
  The `/license` endpoint returns `MIT` for both repos.
- The musl assets for x86_64 and aarch64 exist for both tools. I downloaded all four fresh, and their
  sha256 values match the API `digest` field and the values in the research. `readelf` shows no
  `PT_INTERP` and no `NEEDED` entries in any of the four. The x86_64 builds are static-pie; the
  aarch64 builds are static EXEC.
- Only `hdr10plus_tool` x86_64 ships a `.sha256` asset. There is none for arm64 and none for dovi_tool.

### 2. HDR10+ through libx265: CONFIRMED
- An encode with `-x265-params dhdr10-info=h10p.json` gave 24 of 24 frames carrying
  `HDR Dynamic Metadata SMPTE2094-40 (HDR10+)`. `hdr10plus_tool --verify extract` printed
  "Dynamic HDR10+ metadata detected."
- A plain `-c:v libx265` re-encode of that file had 0 of 24 frames with the metadata, and hdr10plus_tool
  printed "File doesn't contain dynamic metadata". Adding `-udu_sei 1` still gave 0 of 24.
- Source `libavcodec/libx265.c@5d4d3bdc61`:
  - The only T.35 payload the wrapper writes is A53 closed captions (L811).
  - `udu_sei` passes only `AV_FRAME_DATA_SEI_UNREGISTERED` (L822).
  - There is no reference to `AV_FRAME_DATA_DYNAMIC_HDR_PLUS`.

### 3. DV via `-dolbyvision`: behaviour CONFIRMED, mechanism REFUTED, prerequisites missing
- **Carry works.** A raw P8.1 HEVC input (`dovi_tool generate` then `inject-rpu`) encoded with
  `-dolbyvision 1` produced a DOVI record with `dv_profile=8 dv_level=1 rpu_present_flag=1
  el_present_flag=0 compat_id=1`, and 24 of 24 frames carried `Dolby Vision RPU Data`.
- **Auto drops the RPU even when the input carries a DOVI record.**
  - Default `auto` from the raw input gave 0 of 24 frames with an RPU.
  - Default `auto` from `dv1.mkv`, which has a DOVI configuration record and 24 RPUs, also gave
    0 of 24 (`auto_mkv.mkv`). Explicit `-dolbyvision 1` from the same mkv gave 24 of 24.
  - So the research's reason ("auto keys off the stream's DOVI configuration record") is wrong.
- **Why auto never fires from the CLI.**
  - `dovi_rpuenc.c` L81 is `if (s->enable == FF_DOVI_AUTOMATIC && !hdr) goto skip;`. Here `hdr`
    comes only from `avctx->decoded_side_data[AV_FRAME_DATA_DOVI_METADATA]` (L260-266).
  - The ffmpeg CLI fills `decoded_side_data` only from first-frame side data flagged
    `AV_SIDE_DATA_PROP_GLOBAL` (`fftools/ffmpeg_filter.c` about L2261-2272, then `ffmpeg_enc.c` L208).
  - `AV_FRAME_DATA_DOVI_METADATA` is flagged only `COLOR_DEPENDENT` (`libavutil/side_data.c` L45).
  - So `auto` never engages in a CLI transcode, with or without a container record.
  - The recommendation to always pass `-dolbyvision 1` still stands.
- **Omitted prerequisites.** x265 refuses to open the encoder, exit 183, in two cases:
  - Without VBV it fails with `Dolby Vision requires VBV settings to enable HRD.` Every DV encode
    therefore needs `vbv-maxrate`/`vbv-bufsize`, which constrains CRF.
  - Without mastering-display metadata it fails with
    `Dolby Vision profile - 8.1 requires Mastering display color volume information`.
  - Both failures are loud, which is fail-safe, but the brief must plan for them.

### 4. `dolby-vision-rpu` via x265-params: PARTLY REFUTED
- The log shows `[libx265] Unknown option: dolby-vision-rpu.`, but **the exit code is 0 and an output
  file is written**. `dovi_tool extract-rpu` on it reports "No RPU was found".
- Source: `libx265.c` maps `X265_PARAM_BAD_NAME` to `AV_LOG_WARNING` and continues. The option is
  silently not honoured rather than rejected: a fail-open hazard. holdfast must never pass unknown
  `x265-params` without checking stderr.

### 5. dovi_tool modes: facts CONFIRMED; "mode 5 wrong" UNCERTAIN
- `README.md` (quietvoid/dovi_tool main), verbatim:
  - `2` - "Converts the RPU to be profile 8.1 compatible. - Removes luma/chroma mapping for profile 7 FEL."
  - `5` - "Converts to profile 8.1, preserving mapping. - Old mode 2."
  - Example: `ffmpeg ... | dovi_tool -m 2 convert --discard -`.
- 2.0.0 release notes (2023-01-30): "Changed `mode 2` behaviour to automatically enable
  `remove_mapping` for profile 7 FEL only. For the old behaviour, `mode 5` was added."
- So `-m 2` is the author's current default. Nothing primary says mode 5 is "wrong": it differs only
  for FEL, and the image-quality argument is still community reasoning, untested here.

### 6. Raw HEVC to mkv copy: CONFIRMED, no workaround
- Every variant below exits 234 with `[matroska] Can't write packet with unknown timestamp`:
  - `-f hevc -framerate 24`
  - `-fflags +genpts`
  - `-r 24`
  - `+genpts` with `-r 24`
  - no frame rate given
  - the DV-injected stream
- The mp4 hop (`hevc -> mp4 -> mkv`) exits 0, **but it is not a workaround**. The mp4 is written with
  pts == dts and no composition offsets. After the hop the decoded display-order pts are
  `0,41,166,125,208,83...`, where a direct x265 mkv gives `0,42,83,125,167,208`.
- That is silent timing corruption on B-frame streams (pixel hashes match only because the decoder
  reorders by POC). Treat it as forbidden.

### 7. Stale L5 after crop: CONFIRMED (tested)
- Test setup: a letterboxed 320x240 source (40 px bars) with an RPU generated with L5 offsets of
  top 40 and bottom 40.
- Encoded with `-vf crop=320:160:0:40 -dolbyvision 1`, the output is 320x160, and `dovi_tool info -s`
  on its RPU still reports **`L5 offsets: top=40, bottom=40`**.
- Source: `vf_crop.c` has no DOVI references, and `dovi_rpuenc.c` L497-500 writes `dm->l5` verbatim.
- Remedies in dovi_tool:
  - `README.md`: `-c, --crop` "Set active area offsets to 0 (meaning no letterbox bars)".
  - `docs/editor.md`: an `active_area` block with `crop`, `drop_l5`, `presets` and `edits`.

### 8. eac3 7.1: CONFIRMED
- A 7.1 FLAC built with `aevalsrc` and encoded as `eac3 -b:a 1024k` came out as `eac3, 6 ch, 5.1(side)`,
  exit 0, with no warning. ac3 behaves the same way.
- `-h encoder=eac3` lists layouts only up to `5.1(side) 5.1`.
- Useful: an explicit `-ch_layout 7.1` makes eac3 **fail loudly** (exit 234).

### 9. Packet counts: CONFIRMED
- 10 s of 48 kHz stereo produced these counts: aac 470, ac3 313, eac3 313, libopus 501, flac 118,
  mp3 418.
- ac3 and eac3 coincide because both use 1536-sample frames. The count is a function of codec
  frame size, not of content.

### 10. loudnorm 192 kHz: CONFIRMED, with a nuance
- Single pass produced 192000 Hz; appending `aresample=48000` gave 48000 Hz.
- `doc/filters.texi` at the pin, verbatim: "In dynamic mode, to accurately detect true peaks, the audio
  stream will be upsampled to 192 kHz. Use the -ar option or aresample filter to explicitly set an
  output sample rate."
- **Nuance: a true linear pass stays at 48000 Hz** (tested). `af_loudnorm.c` `query_formats` forces
  192 kHz only `if (s->frame_type != LINEAR_MODE)`.
- Pass 2 silently reverts to dynamic, and so to 192 kHz, in three cases, all tested:
  - measured LRA is above the target LRA;
  - the true-peak condition fails;
  - measured LRA is exactly 0 (the `measured_lra != 0` sentinel in `init()`, which is not in the docs).
- So always append aresample, and assert `normalization_type=linear`.

### 11. Sidecar naming: CONFIRMED (source)
- Sonarr `develop` `src/NzbDrone.Core/Parser/LanguageParser.cs` L32 and Radarr `develop` L60 contain the
  identical `SubtitleLanguageRegex`: tags `forced|foreign|default|cc|psdh|sdh`, allowed repeatedly
  before or after `(?<iso_code>[a-z]{2,3})`, with delimiters `-_. `.
- In Sonarr/Radarr the code is resolved through `IsoLanguages.Find`; anything else becomes Unknown.
  There is no `hi` tag.
- Jellyfin `master` `Emby.Naming/Common/NamingOptions.cs`:
  - delimiter `.`
  - forced flags `foreign`, `forced`; default flag `default`
  - hearing-impaired flags `cc`, `hi`, `sdh`
- `ExternalPathParser.cs` sets `IsForced`/`IsDefault` (by substring `Contains`) and `IsHearingImpaired`,
  and resolves the language through `FindLanguageInfo`.
- Not verified: whether Sonarr/Radarr ingest sidecars when "Import Extra Files" is disabled.

### 12. cropdetect: CONFIRMED
- Test clip: 320x240 with 40 px bars, `round=2:reset=1`. Results:

  | Pixel format | `limit` | Result |
  |---|---|---|
  | 8-bit | 24 | `320:160:0:40` |
  | 10-bit | 24 | `320:240:0:0` (no crop) |
  | 10-bit | 0.1, or the default | `320:160:0:40` |
  | 10-bit | 70 | works |

- The 24-fails / 70-works bracket is consistent with 10-bit black at 16<<2 = 64.
- All-black frames with `round=2` give `crop=-318:-238:320:240` in both 8 and 10-bit; with default
  options they give `-304:-224:314:234`.
