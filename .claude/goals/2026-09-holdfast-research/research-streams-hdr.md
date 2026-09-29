# holdfast research: HDR metadata, audio, subtitles, crop (T19/T20/T24/T25/T26)

Read date for every URL below: 2026-09-29. "TESTED" means I ran it in this container against
the pinned ffmpeg `N-125875-g5d4d3bdc61-20260731` (BtbN autobuild-2026-07-31-14-10, the
Dockerfile pin; libx265 reports `HEVC encoder version 4.2+37-b81f650e`). Scratch media and
downloaded tools are in `/cache/tmp/plan-2026-09-holdfast/scratch-media/`. "ASSUMED" means
memory-only, not verified today. "LEAD" means a forum/secondary source, not authoritative.
Nothing in /workspace was changed.

---

## 1. HDR10+ and Dolby Vision through libx265

### 1.1 Tool releases, licences, binaries

| Tool | Latest | Published | Licence | linux amd64 asset | linux arm64 asset |
|---|---|---|---|---|---|
| dovi_tool | 2.3.4 | 2026-09-10 | MIT | `dovi_tool-2.3.4-x86_64-unknown-linux-musl.tar.gz` sha256 `1844258e13c26607b32224bf1fa82b595d3b35949f5467405fda560daad32b3f` | `dovi_tool-2.3.4-aarch64-unknown-linux-musl.tar.gz` sha256 `b4f22a7db56954efe4ed8d02276d0f991799e84602f3584711be6c9df950cb15` |
| hdr10plus_tool | 1.7.2 | 2025-12-27 | MIT | `hdr10plus_tool-1.7.2-x86_64-unknown-linux-musl.tar.gz` sha256 `06385f37a639d61ba21d4be3150c863846933bc3b58110e094d8fc8f1c2249f2` (also a published `.sha256` asset, same value) | `hdr10plus_tool-1.7.2-aarch64-unknown-linux-musl.tar.gz` sha256 `5fb90607cd94296640f1fc2355207b8107b67baac96d37481423d08a9fce437d` |

- Sources: `gh api repos/quietvoid/dovi_tool/releases/latest` and `.../hdr10plus_tool/releases/latest`
  (GitHub API `digest` field), https://github.com/quietvoid/dovi_tool/releases/tag/2.3.4,
  https://github.com/quietvoid/hdr10plus_tool/releases/tag/1.7.2; licence via
  `gh api repos/quietvoid/{dovi_tool,hdr10plus_tool}/license` -> `MIT`.
- dovi_tool release cadence: 2.3.1 (2025-08-22), 2.3.2 (2026-04-05), 2.3.3 (2026-07-12), 2.3.4 (2026-09-10).
- TESTED: both amd64 tarballs downloaded, sha256-verified, run (`dovi_tool 2.3.4`,
  `hdr10plus_tool 1.7.2`). They are musl static builds, so they drop into the distroless
  `cc-debian12` runtime with a COPY, like ffmpeg. MIT is AGPL-3.0-compatible for bundling
  (ASSUMED, standard licence-compatibility knowledge).
- Both are Rust; no month-end-retention problem like BtbN, but a pin should still be
  tag + sha256 per arch in the Dockerfile (same pattern as FFMPEG_*).

### 1.2 Does the pinned ffmpeg's libx265 support HDR10+? YES (TESTED)

```
ffmpeg -f lavfi -i testsrc2=s=320x240:r=24:d=1 -pix_fmt yuv420p10le -c:v libx265 \
  -x265-params "dhdr10-info=hdr10p_min.json:hdr10-opt=1:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc" out_hdr10p.hevc
```
x265 log: `tools: ... sao dhdr10-info`; ffprobe `-show_frames` on the output shows
`side_data_type=HDR Dynamic Metadata SMPTE2094-40 (HDR10+)`. So the BtbN x265 is built with
HDR10+ (x265 only honours `dhdr10-info` when built with `ENABLE_HDR10_PLUS`; ASSUMED from x265
build docs, but the positive test makes it moot for this pin).

x265 docs (`doc/reST/cli.rst`, https://bitbucket.org/multicoreware/x265_git/raw/master/doc/reST/cli.rst):
- `--dhdr10-info <filename>`: "Inserts tone mapping information as an SEI message. It takes as
  input, the path to the JSON file containing the Creative Intent Metadata..."
- `--dhdr10-opt`: "Inserts SEI only for IDR frames and for frames where tone mapping information has changed."
- `--dolby-vision-profile`: "Currently only profile 5, profile 8.1, profile 8.2 and profile 8.4 enabled".
- `--dolby-vision-rpu <filename>`: "... **CLI ONLY**".

### 1.3 ffmpeg does NOT pass HDR10+ through libx265 on its own (TESTED + source)

- TESTED: decoding `out_hdr10p.hevc` (has SMPTE2094-40 per frame) and re-encoding with plain
  `-c:v libx265` produced output with NO HDR10+ side data (only the x265 info SEI).
- Source: https://raw.githubusercontent.com/FFmpeg/FFmpeg/master/libavcodec/libx265.c has no
  reference to `AV_FRAME_DATA_DYNAMIC_HDR_PLUS` / dhdr10. It DOES map mastering-display and
  content-light side data into x265 params (`handle_side_data`, `handle_mdcv`).
- FFmpeg Changelog (https://raw.githubusercontent.com/FFmpeg/FFmpeg/master/Changelog): 7.0
  "HDR10 metadata passthrough when encoding with libx264, libx265, and libsvtav1"; 8.0
  "HDR10+ metadata passthrough when decoding/encoding with libaom-av1" (AV1/libaom only, not x265).
- Therefore HDR10+ through libx265 needs hdr10plus_tool:
  1. `ffmpeg -i src.mkv -map 0:v:0 -c copy -bsf:v hevc_mp4toannexb -f hevc - | hdr10plus_tool extract -o meta.json -`
     (documented form, hdr10plus_tool README https://raw.githubusercontent.com/quietvoid/hdr10plus_tool/main/README.md)
  2. `-x265-params dhdr10-info=meta.json` on the encode (TESTED above). Frames in the JSON are
     in display order after hdr10plus_tool's reorder step, and must equal the encoded frame count.
  - Alternative post-encode `hdr10plus_tool inject -i enc.hevc -j meta.json -o out.hevc` is
    TESTED working on raw HEVC, but see 1.6: remuxing raw HEVC back into mkv with ffmpeg copy
    fails in this build, so encode-time `dhdr10-info` is the practical path.

### 1.4 ffmpeg DOES carry DV RPU through libx265 (TESTED), removing dovi_tool for P8

- Commit "avcodec/libx265: implement dolby vision coding", 2024-03-29,
  https://github.com/FFmpeg/FFmpeg/commit/39ca87ed1ef876af9622a5aa331e18167fdfdf27 (in 7.1+).
- `ffmpeg -h encoder=libx265` here: `-dolbyvision <boolean> Enable Dolby Vision RPU coding (default auto)`.
- libavcodec/dovi_rpuenc.c (https://raw.githubusercontent.com/FFmpeg/FFmpeg/master/libavcodec/dovi_rpuenc.c):
  profile is guessed from the source RPU header; profile 8 compat id 1 for BT.2020/PQ, 4 for HLG,
  2 for BT.709; profiles 4 and 7 error "Coding of Dolby Vision enhancement layers is currently
  unsupported" when explicitly enabled (auto skips); strict compliance requires yuv420p10 for HEVC.
- TESTED end-to-end (synthetic P8.1 source built with `dovi_tool generate` + `inject-rpu`):
  - `-dolbyvision 1`: output mkv stream side data `DOVI configuration record ... dv_profile=8,
    dv_level=1, rpu_present_flag=1, bl_present_flag=1, dv_bl_signal_compatibility_id=1`;
    `dovi_tool extract-rpu` + `info -s` on the output: `Frames: 24, Profile: 8` (= 24 video frames).
    Mastering display + CLL were carried automatically too.
  - `-dolbyvision 0` and **default `auto`**: NO RPU in output when the input carries no container-level
    DOVI record (raw HEVC input). Auto keys off the stream's DOVI configuration record, so
    holdfast must pass `-dolbyvision 1` explicitly after probing P8, never rely on auto.
  - `x265-params dolby-vision-rpu=...` fails: `[libx265] Unknown option: dolby-vision-rpu.`
    (matches the "CLI ONLY" doc). dovi_tool's classic x265-CLI workflow therefore does not
    apply to an ffmpeg-libx265 pipeline.
- New bitstream filters in this build: `dovi_rpu` (strip / compression) and `dovi_split`
  (modes `bl`, `bl_rpu`, `el`, `el_rpu`; Changelog 9.0: "Bitstream filter to split Dolby Vision
  multi-layer HEVC"). Neither converts a P7 RPU to P8.1.

### 1.5 P7 -> P8.1 (opt-in) and P5

dovi_tool README (https://raw.githubusercontent.com/quietvoid/dovi_tool/main/README.md), verbatim:
- `2` - "Converts the RPU to be profile 8.1 compatible. - Removes luma/chroma mapping for profile 7 FEL."
- `5` - "Converts to profile 8.1, preserving mapping. - Old mode 2."
- `1` - "Converts the RPU to be MEL compatible."; `3` - "Converts profile 5 to 8.1."
- `convert`: "Converts RPU within a single layer HEVC file. The enhancement layer can be discarded using `--discard`."
  Example: `ffmpeg -i input.mkv -c:v copy -bsf:v hevc_mp4toannexb -f hevc - | dovi_tool -m 2 convert --discard -`
- `extract-rpu` "Supports profiles 4, 5, 7, and 8"; accepts mkv directly.
- `-c/--crop`: "Set active area offsets to 0 (meaning no letterbox bars)".

Workable pipeline for T25 opt-in (my synthesis, partially TESTED):
1. `ffmpeg -i src.mkv -map 0:v:0 -c copy -bsf:v hevc_mp4toannexb -f hevc - | dovi_tool -m 2 convert --discard - -o p81.hevc`
   (lossless bitstream rewrite: BL + converted RPU, EL dropped).
2. Encode `p81.hevc` as the video input with `-c:v libx265 -dolbyvision 1` (TESTED path: raw
   HEVC with RPU in -> mkv with P8.1 DOVI record out), mapping audio/subs from the original mkv.
   Caveat: raw HEVC has no timestamps; pass `-f hevc -framerate <exact source rate>`; a VFR
   source would desync -> skip VFR in this mode.
- Alternative: `dovi_tool -m 2 extract-rpu src.mkv -o rpu.bin`, encode BL with ffmpeg to raw HEVC,
  `dovi_tool inject-rpu`, remux. Blocked by 1.6 below.
- FEL vs MEL (ASSUMED/LEAD, community consensus e.g. https://forum.makemkv.com/forum/viewtopic.php?t=26514):
  MEL's enhancement layer carries essentially no residual, so dropping it is visually lossless.
  FEL carries real residual (12-bit extension / detail); dropping it yields the BL-only picture,
  and mode 2 removes the FEL luma/chroma mapping (mode 5 keeping the mapping on FEL would
  apply a reshaping meant for BL+EL to BL alone -> wrong image). So: P7->8.1 must use `-m 2`,
  never `-m 5`, and the VMAF reference is the decoded BL (which is what ffmpeg decodes anyway).
  `dovi_tool info -s` should expose FEL/MEL for logging (ASSUMED output wording; verify on a real P7 sample).
- P5 (IPTPQc2, compat id 0) has no backward-compatible BL; decoding without DV processing gives
  wrong colours, so VMAF on it is meaningless. dovi_tool mode 3 exists (P5 -> 8.1) but is out of scope: P5 stays skipped.

### 1.6 Raw HEVC -> mkv stream-copy fails in this ffmpeg (TESTED)

`ffmpeg -f hevc -framerate 24 -i x.hevc -c copy out.mkv` -> "Can't write packet with unknown
timestamp ... Conversion failed!" for both an injected and an un-injected x265 stream (B-frames).
mp4 copy succeeds but writes no DOVI configuration record. So any "encode, then inject with a
tool, then remux" design needs another muxer (mkvmerge, GPL-2, not in the image) or a
timestamp-generating trick; the encode-time routes in 1.3 and 1.5 avoid it.

### 1.7 Verifying after encode (all TESTED)

- DV: `ffprobe -show_streams` -> `DOVI configuration record` with `dv_profile=8`,
  `dv_bl_signal_compatibility_id` equal to the source's (1 for PQ, 4 for HLG), `rpu_present_flag=1`,
  `el_present_flag` absent/0; per-frame `side_data_type=Dolby Vision RPU Data`;
  `dovi_tool extract-rpu out.mkv` then `dovi_tool info -s` -> `Frames:` == decoded frame count and `Profile: 8`.
- HDR10+: per-frame `side_data_type=HDR Dynamic Metadata SMPTE2094-40 (HDR10+)`;
  `hdr10plus_tool --verify extract out.hevc` -> "Dynamic HDR10+ metadata detected.";
  `hdr10plus_tool extract ... -o x.json; jq '.SceneInfo|length'` == frame count (24 == 24 here).
- Static HDR10: mastering display + CLL side data present (already in internal/hdr).

### Implications for the brief (T25)

- P8 carry needs NO dovi_tool: probe P8 (+ compat id), then `-dolbyvision 1` explicitly; never
  default auto. Add an output gate: DOVI record profile/compat id match source, RPU frame count == video frame count.
- HDR10+ needs hdr10plus_tool (MIT, static musl, pin per arch by sha256): extract JSON pre-encode,
  pass `dhdr10-info=`; gate on SMPTE2094-40 present and metadata count == frame count. Fail-safe:
  an HDR10+ source whose metadata cannot be extracted/validated SKIPS (never silently drops HDR10+).
- P7 -> 8.1 opt-in: dovi_tool `-m 2 convert --discard` pre-pass, then the P8 path; refuse VFR;
  log FEL vs MEL; never mode 5. P5 skipped (already decided).
- Crop + DV interplay: after T26 cropping, RPU L5 active-area offsets describe the old letterbox;
  ffmpeg is not known to rewrite them (ASSUMED). Either use `dovi_tool --crop` in the pre-pass
  or forbid crop on DV sources. Needs a decision/test.
- Tools must be added to the image and to `scripts/install-ffmpeg.sh`-style pinned install for CI;
  `check-pins.sh` should cover them.

---

## 2. Audio (T19/T20)

### 2.1 Encoders here (TESTED `ffmpeg -encoders`)

`aac` (native), `ac3`, `eac3`, `libopus`, `opus` (native, experimental `X` flag), `flac`, `truehd`
(experimental), `dca` (experimental). Configure line has `--disable-libfdk-aac` (libfdk_aac is
nonfree, absent; do not use). BtbN "gpl" variant with `--enable-version3`.

### 2.2 Layout behaviour (TESTED, this is the biggest finding)

| Input | Encoder | Result |
|---|---|---|
| 5.1(side) PCM | eac3 640k | `eac3 5.1(side)`, 313 packets, 10.005 s |
| 5.1(side) | ac3 640k | `ac3 5.1(side)`, 313 packets |
| 5.1(side) | aac 384k | `aac 5.1` (relabelled side->back), 470 packets, 10.021 s |
| 5.1(side) | libopus 256k | `opus 5.1`, 501 packets, 10.008 s |
| **7.1 FLAC** | **eac3 1024k** | **`eac3 5.1(side)` - silently downmixed, no error, exit 0** |
| 7.1 FLAC | libopus 450k | `opus 7.1` |

`ffmpeg -h encoder=eac3` supported layouts stop at `5.1(side) 5.1`: ffmpeg's native eac3 encoder
is AC-3-based and cannot do 7.1; ffmpeg auto-negotiates a downmix without warning. Decoded
durations were all 10.00 s.

### 2.3 Bitrates (primary docs)

- Opus, https://wiki.xiph.org/Opus_Recommended_Settings : stereo music storage "96 - 128" kb/s
  ("Opus at 128 KB/s (VBR) is pretty much transparent"); 5.1 "128 - 256"; 7.1 "256 - 450".
- AAC native: https://trac.ffmpeg.org/wiki/Encode/AAC returned 403 (Anubis) today; ASSUMED from
  memory: native aac is the recommended non-fdk choice, CBR ~64 kb/s per channel is a common
  guidance (so ~128 stereo, ~384 5.1); native VBR (`-q:a`) is marked experimental.
- AC-3/E-AC-3: ASSUMED (not re-verified): AC-3 max 640 kb/s; common 5.1 at 640 (ac3) / 640-1024 (eac3); stereo 192-256.

### 2.4 Loudness (TESTED + filters.texi)

filters.texi (https://raw.githubusercontent.com/FFmpeg/FFmpeg/master/doc/filters.texi), verbatim:
"Support for both single pass (livestreams, files) and double pass (files) modes. ... In dynamic
mode, to accurately detect true peaks, the audio stream will be upsampled to 192 kHz. Use the
-ar option or aresample filter to explicitly set an output sample rate." And `linear`: requires
all `measured_*`; "If any of these conditions aren't met, normalization mode will revert to dynamic."
Defaults I=-24, LRA=7, TP=-2.
- TESTED: single-pass loudnorm on 48 kHz input produced **192000 Hz** output; `aresample=48000`
  after it restores 48 kHz. Duration unchanged (10.000 s both ways). Pass 1 with
  `print_format=json` emits `input_i/input_tp/input_lra/input_thresh/target_offset` for pass 2;
  the JSON also reports `normalization_type` (dynamic vs linear), which pass 2 should assert.
- EBU R128 target is -23 LUFS (ASSUMED; EBU R 128 spec, not fetched today). ffmpeg default -24 is ATSC A/85-ish.

### 2.5 Downmix (TESTED)

Measured left-output gain with the default swresample stereo matrix (`aformat=channel_layouts=stereo`),
per single-channel 5.1(side) test signal at -18.1 dBFS: FL -> -25.7 (-7.7 dB), FC -> -28.7 (-10.7),
SL -> -28.7 (-10.7), **LFE -> silence (-91)**. I.e. centre and surrounds at -3 dB relative to front,
LFE dropped, then the whole matrix normalised down ~7.7 dB to avoid clipping (resampler.texi
https://raw.githubusercontent.com/FFmpeg/FFmpeg/master/doc/resampler.texi : `clev`, `slev`,
`lfe_mix_level` "used when there is a LFE input but no LFE output"). Dropping LFE matches the
conventional Lo/Ro downmix (ASSUMED: ATSC A/52 downmix equations exclude LFE). An explicit
`pan=stereo|FL=FL+0.707*FC+0.707*SL+0.5*LFE|...` includes LFE but risks clipping.
Note: a plain `-ac 2` is applied at output AFTER `-af`, so level checks must run in the graph.
- Recommendation: default matrix (Lo/Ro, no LFE) then loudnorm, which also recovers the -7.7 dB.

### 2.6 Gating audio

- Packet-count parity does NOT transfer across codecs (frame sizes differ: 313 eac3 vs 470 aac vs
  501 opus for identical 10 s). Use instead: decoded-duration parity (within one codec frame plus
  encoder priming; aac showed +16 ms container duration, opus +8 ms), **channel COUNT parity vs the
  intended layout** (catches the eac3 7.1->5.1 silent downmix), sample rate equals intended
  (catches loudnorm's 192 kHz), a full decode with `-xerror` (decode-integrity), and a
  post-encode `ebur128`/loudnorm measure within tolerance of target.
- Treat side/back 5.1 as equivalent (aac/opus relabel), but compare channel count strictly.

### Implications for the brief (T19/T20)

- Codec/layout matrix must be explicit: eac3/ac3 max 6 channels; 7.1 sources either go to
  libopus (or aac) or are skipped for eac3 targets. Never rely on ffmpeg auto-negotiation; set
  `-ch_layout` explicitly and gate channel count.
- loudnorm: two-pass (measure JSON, then linear pass), always followed by `aresample=<source rate>`;
  gate that pass 2 reported linear, else log. Single-pass dynamic is an acceptable fallback only
  if stated.
- Stereo downmix: default swr matrix (LFE excluded) + loudnorm; skip adding one when the source
  already has a stereo track of the same language (my suggestion).
- Replacing a lossless track is a data-loss event for that track: the gate list above must be
  in the swap invariant, and `keep_original_audio: true` stays the escape hatch.
- libfdk_aac: absent from the build and nonfree; never.

---

## 3. Subtitles (T24)

### 3.1 Extraction (TESTED)

- Lossless = stream copy into the NATIVE text container only: `subrip -> .srt`, `ass -> .ass`,
  `webvtt -> .vtt`. Copy across formats fails ("Could not write header (incorrect codec parameters ?)")
  for subrip->.vtt and ass->.srt.
- Cross-format needs transcoding: subrip -> webvtt and mov_text -> srt worked with text and `<i>`
  intact on a simple sample; ass -> srt drops styling/positioning (lossy, ASSUMED detail but inherent).
  mov_text (mp4) has no native sidecar format: extract to .srt (transcode, note it).
- Bitmap subs (PGS `hdmv_pgs_subtitle`, `dvd_subtitle`, `dvb_subtitle`) are not text: out of T24 scope.
- Flags: `ffprobe -show_entries stream_disposition=default,forced,hearing_impaired` returns
  `disposition:forced=1`, `disposition:hearing_impaired=1` (TESTED from mkv FlagForced / FlagHearingImpaired);
  language from `tag:language` (ISO 639-2, e.g. `eng`, `ger`).

### 3.2 Naming that media servers parse

- Jellyfin docs (https://raw.githubusercontent.com/jellyfin/jellyfin.org/master/docs/general/server/media/_video-external-streams.md):
  examples `Film.default.en.forced.ass`, `Film.en.sdh.srt`; flags Default `default`, Forced
  `forced`, `foreign`, Hearing Impaired `sdh`, `cc`, `hi` ("`hi` by itself will resolve as a Hindi
  language track"). Code: `Emby.Naming/Common/NamingOptions.cs` (delimiter `.`).
- Sonarr/Radarr source (https://raw.githubusercontent.com/Sonarr/Sonarr/develop/src/NzbDrone.Core/Parser/LanguageParser.cs,
  Radarr identical): `SubtitleLanguageRegex` accepts tags `forced|foreign|default|cc|psdh|sdh`
  before or after a 2-3 letter ISO code, delimiters `-_. `.
- Plex article https://support.plex.tv/articles/200471133-adding-local-subtitles-to-your-media/
  returned 403 today; via search snippet (LEAD until read): `Movie_Name (Release Date).[lang].forced.ext`,
  and `sdh`/`cc` recognised on PMS >= 1.20.3.3401 with the new Plex Movie agent.
- So `<name>.<lang>[.forced].<ext>` is parsed by all three; adding `.sdh` for hearing_impaired is
  parsed by all three too (suggestion, not in the decided pattern). Plex docs historically show
  ISO 639-1 (`en`); Sonarr/Jellyfin accept 2 or 3 letters; ASSUMED Plex also accepts 639-2.

### 3.3 Pitfalls

- ASS fonts: mkv ASS usually relies on font attachments (`codec_type=attachment`,
  mimetype `font/ttf`, `application/x-truetype-font`, etc.). A sidecar `.ass` loses them; players
  fall back to system fonts (typesetting breaks). Since embedded subs are kept, the in-file track
  still works; the sidecar is a convenience copy. Log it.
- Multiple tracks with same lang+flags collide on one name: needs a deterministic disambiguator
  (Sonarr's parser supports a title/copy-number segment) or skip the later one with a reason.
- "Never overwrite existing sidecars": check with O_EXCL create (`os.OpenFile(..., O_CREATE|O_EXCL)`), write via temp + link/rename-no-replace.
- Sidecars are named after the FINAL output basename: if the container changes (mp4 -> mkv), the
  sidecar name must follow the replacement, and must be written only after the swap commits (or
  be tolerated as orphans if the swap is refused).
- Text encoding: ffmpeg outputs UTF-8; source srt in legacy code pages decode per `-sub_charenc` (ASSUMED detail).

### Implications for the brief (T24)

- Copy-only to native format for subrip/ass/webvtt; mov_text -> srt transcoded and labelled;
  bitmap subs skipped with a reason. Flags from disposition; lang from tag, map to 639-1 when a
  clean mapping exists else keep 639-2 (decision point).
- Gate: the sidecar parses back with ffprobe and has the same event count as the source stream (cheap, catches truncation).
- Sidecar writes are not source mutations, but they must follow the swap ordering and never clobber.

---

## 4. Crop (T26)

### 4.1 cropdetect semantics (filters.texi + `ffmpeg -h filter=cropdetect`, TESTED)

- `limit` default 0.0941176 (=24/255): "An intensity value greater to the set value is considered
  non-black ... You can also specify a value between 0.0 and 1.0 which will be scaled depending on
  the bitdepth". Changelog 2.6: "cropdetect support for non 8bpp, absolute (if limit >= 1) and
  relative (if limit < 1.0) threshold".
- `round` default 16 ("Use 2 to get only even dimensions"); `skip` default 2; `reset_count`/`reset`
  default 0 = "never reset, and returns the largest area encountered"; `max_outliers` default 0;
  `mode` `black` | `mvedges` (6.0 changelog "detect crop-area based on motion vectors and edges";
  needs `mestimate` or `-flags2 +export_mvs`); `high`/`low` Canny thresholds; `mv_threshold` 8.
- Output per frame in log: `x1 x2 y1 y2 w h x y pts t limit crop=W:H:X:Y` (also as frame metadata).

### 4.2 Synthetic test results (TESTED; 1920x1080 frame, 1920x800 picture padded 140 px top/bottom)

- 8-bit x264 and 10-bit PQ x265, default limit: `crop=1920:800:0:140` on 94/94 frames. `round=2:limit=0.1`: same.
- **10-bit with absolute `limit=24`: `crop=1920:1072:0:4`** (no real crop). 10-bit limited-range black
  is 64, so an absolute 8-bit threshold is below black. Always pass a fractional limit.
- Pitfall clip (2 s full black, then letterboxed; a white box burned into the bottom bar for the last 1 s):
  - per-frame (`reset=1`): 46 frames `crop=-1918:-1078:1920:1080` (all-black frames emit a
    NEGATIVE, invalid crop), 48 frames `1920:800:0:140`, 24 frames `1920:900:0:140`.
  - cumulative (`reset=0`): `1920:900:0:140` - the union keeps the burned-in bar content. Good (conservative).

### 4.3 Established consensus (primary: HandBrake source)

HandBrake `libhb/scan.c` (https://raw.githubusercontent.com/HandBrake/HandBrake/master/libhb/scan.c,
last commit 2026-06-05): per preview frame, a row/column is "dark" if its average luma (clamped at 16)
< 32 and every pixel within +-16 of the average; ignores a thin border up to 1% of height; discards a
preview whose crop exceeds 1/4 of the frame on any side ("fooled by frames with a lot of black like
titles, credits & fade-thru-black"); needs >2 previews; default "Smart" = MEDIAN per side, switching
to LOOSE (minimum per side, "no non-black pixels will be cropped from any frame") when >= 4 previews
(6 at >=30, 8 at >40) are more than 9 px below the median (mixed aspect ratio). Tdarr/Unmanic crop
plugins: not researched in budget (LEAD only; ASSUMED they wrap cropdetect over a few seeks).

### 4.4 Verifying the removed area was black (TESTED)

`crop=<bar rect>,signalstats` then read `lavfi.signalstats.YMAX` (and YAVG) per frame:
- 8-bit clean bars: YMAX 16-20 (limit 24 -> pass).
- Burned-in content in bar: 24 frames YMAX=235 -> fail, correctly.
- **10-bit x265 bars: YMAX 73-138** (black 64; limit 0.094*1023 = 96): encode ringing next to the
  picture edge pushes some frames over. A YMAX gate at the cropdetect limit would false-red; use
  YAVG near black plus a YMAX/percentile ceiling scaled by bit depth, or exclude the 2-4 rows
  adjacent to the picture edge. Needs calibration on real content.

### Implications for the brief (T26)

- Sample N timestamps spread over the file (skip the first/last few %), run cropdetect per sample with
  fractional `limit`, `round=2`; discard invalid/negative and >1/4-frame results; take the LOOSE
  (largest-area / minimum-per-side) consensus, not HandBrake's median. Refuse (skip crop, still
  encode) if fewer than K valid samples or if samples disagree beyond a tolerance (mixed AR).
- Blackness gate over the removed rectangles on the SOURCE, over the whole file or dense sampling,
  bit-depth scaled; YAVG + bounded YMAX; a fail disables crop for that job (fail safe), never
  crops through content.
- VMAF reference = source with the identical `crop=` applied (already decided); keep dimensions
  even for 4:2:0 and record the crop rect on the job row.
- DV sources: L5 metadata vs crop (see section 1 implications).
