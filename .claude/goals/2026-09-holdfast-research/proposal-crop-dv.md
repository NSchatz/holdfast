# P5 - Crop on Dolby Vision sources (I7)

Proposal for Checkpoint T. Research: `research-streams-hdr.md` sections 1 and 4 and
`verify-streams-hdr.md` (claims 3, 6 and 7; its corrections override the research). Every claim a
version, licence, option or code section rests on was re-checked on 2026-09-29 against a primary
source, and the `dovi_tool` behaviour was re-run on synthetic material in a scratch lab that is
not committed; see "Claims re-verified". Code references are at `30d245f`.

## What this decides

When `crop` is on (T26, opt-in) and the source is Dolby Vision (the profile 8 carry of I6, or the
opt-in profile 7 to 8.1 conversion of T25), may holdfast crop it? Cropping removes the letterbox
bars from the picture, but the RPU's level 5 (L5) metadata, which states the active picture area as
offsets from each edge, still describes the bars, so the replacement would carry metadata that no
longer describes its own picture. Until this is decided, I7 refuses crop on DV sources and the file
still encodes uncropped.

## Where things stand

**Decided (not re-opened here):**

- T13: crop and DV/HDR10+ transcode are in scope. T25: DV profile 8 and HDR10+ are carried through
  libx265; profile 7 converts to 8.1 only when opted in (the enhancement layer is dropped); profile 5
  stays skipped; `dovi_tool` and `hdr10plus_tool` are pinned into the image.
- T26: crop is off by default and consensus-sampled; VMAF runs against the identically cropped
  source, plus a gate that the removed area was black. I5: off until configured. I6: P8 and HDR10+
  carry is on by default on the `cpu` encoder once the metadata gates pass.
- I7: crop refused on DV sources until this proposal is decided. Goal 8 item 6 and its line F
  implement whichever option is approved, with I7 as the default.

**Open (decided here at Checkpoint T):** whether a DV source is ever cropped, and if so, what
happens to its L5 and what proves the result.

**The code today:**

- `internal/hdr/hdr.go:46-53` (`ClassFrom`) classifies a source as DV on a `dvhe`, `dvh1`, `dvav` or
  `dav1` codec tag, or on a "DOVI configuration record" or "Dolby Vision" side-data block.
- `internal/engine/engine.go:2692-2700` skips every DV source (`SkipDolbyVision`) and `:2701-2703`
  every HDR10+ source. Goal 8 lifts these per T25.
- There is no crop code under `internal/` yet. `docs/profiles.md:68-81` lists the keys an encode
  profile may carry; `crop` is not among them, and `:49-52` keeps every gate out of a profile's
  reach, which the blackness gate and the L5 gate below inherit.
- The profile 7 path goal 8 builds is already a pre-pass: `dovi_tool -m 2 convert --discard` on the
  raw HEVC stream, then the encode reads raw HEVC with an explicit frame rate, refusing variable
  frame rate (brief section 12, item 3).

### What the primary sources and the lab show

1. **L5 is the active area, and ffmpeg carries it verbatim.** `libavutil/dovi_meta.h` defines
   `AVDOVIDmLevel5` as the "Active area definition" with `left_offset`, `right_offset`, `top_offset`
   and `bottom_offset`; `libavcodec/dovi_rpuenc.c:497-500` writes the four offsets, 13 bits each,
   exactly as they arrive.
2. **Nothing in ffmpeg adjusts it on a crop.** `libavfilter/vf_crop.c` has no Dolby Vision or
   side-data reference; the DV metadata side data is flagged colour-dependent and not
   size-dependent (`libavutil/side_data.c:44-45`), so a filter that drops size-dependent side data
   on a size change keeps it;
   the `dovi_rpu` bitstream filter offers only `strip` and `compression`. The verifier's test
   (a 320x240 source with 40 px bars and L5 top 40, bottom 40, cropped to 320x160 with
   `-dolbyvision 1`) was re-read today with `dovi_tool info -s`: the output RPU still says
   `L5 offsets: top=40, bottom=40`.
3. **`ffprobe` cannot see L5.** Its `print_dovi_metadata` prints the RPU header, the mapping and the
   colour metadata, and no extension block (checked in `fftools/ffprobe.c` and on the cropped
   output with the pinned build). Any L5 check needs `dovi_tool` (`extract-rpu`, then
   `export -d level5` or `info -s`).
4. **`dovi_tool` 2.3.4 rewrites L5 three ways, all re-run today on synthetic material:**
   - the global `-c` / `--crop` flag ("Set active area offsets to 0 (meaning no letterbox bars)") on
     `convert` with `-m 0`: L5 went from 40/40 to 0/0/0/0 on all 24 frames;
   - `--edit-config` on `convert` with an `active_area` preset applied to `"all"`: 40/40 became
     20/20 on all frames. `docs/editor.md` says HEVC operations support only the `"all"` edit for
     the active area, never frame ranges;
   - the `editor` subcommand on an extracted RPU binary, which does accept frame ranges, followed by
     `inject-rpu`: 20/20 on all frames.
   `export -d level5` writes the L5 of every frame as an editor config (presets plus frame-range
   edits), which serves both as the input for computing new offsets and as the reader for a gate.
   Quirk found: that export sets `"crop": true` even when its presets are non-zero, so a gate must
   compare the presets and the edits, never the flag.
5. **The only route for an edited RPU into the encode is the source bitstream.** libx265 takes the
   RPU from each decoded frame's `AV_FRAME_DATA_DOVI_METADATA` (`libavcodec/libx265.c:848-860`), and
   raw HEVC cannot be stream-copied into mkv with this ffmpeg (verify claim 6), so injecting after
   the encode is out. An edit therefore means the profile 7 pre-pass shape for every cropped DV
   source, profile 8 included: extract the video to raw HEVC, rewrite it with `dovi_tool`, encode
   from raw HEVC with `-f hevc -framerate <source rate>`, map audio and subtitles from the original,
   refuse variable frame rate, and budget a working file the size of the video stream.
6. **A rewritten L5 survives the encode (run today under the heavy lock).** Each variant was
   encoded from raw HEVC with `-f hevc -framerate 24`, `-vf crop=...`, `-dolbyvision 1`, VBV
   settings and mastering-display metadata, then read back with `extract-rpu` and
   `export -d level5`; every output kept a DOVI record of profile 8, compatibility id 1, with 24
   RPUs for 24 frames:

   | Pre-pass | Crop | Output | Output L5 (frames 0-23) |
   |---|---|---|---|
   | none (control) | 320x160 | 320x160 | top 40, bottom 40 (stale) |
   | `dovi_tool -m 0 -c convert` | 320x160 | 320x160 | 0/0/0/0 |
   | `--edit-config`, preset 20/20 on `"all"` | 320x200 | 320x200 | top 20, bottom 20 |
   | `editor` preset 20/20, then `inject-rpu` | 320x200 | 320x200 | top 20, bottom 20 |
7. **Not verified, and not relied on:** what a display or player does with a stale L5 (`ASSUMED`:
   some players use L5 to zoom past letterbox bars, which on an already-cropped picture would cut
   real picture); and whether L1 (per-shot brightness) was measured over the active area only
   (`ASSUMED` yes; if not, removing bars shifts the true frame average, which no option here
   rewrites). The proposal rests on the fail-safe rule alone: a replacement whose metadata no
   longer describes its picture is a confident wrong result.

## Options

### (a) Refuse crop on every DV source, permanently

The I7 default made final: a DV source with `crop` on encodes uncropped, its DV carried, and the
row records the refusal and its reason.

- **Costs:** DV files with bars keep them, so the space and encode time a crop would save on them
  is not saved (black bars are cheap in HEVC, so the space is likely small; `ASSUMED`, not measured
  here). Nothing new to build, verify or pin beyond goal 8's own work.

### (b) Rewrite L5 to the source's L5 minus the crop, and prove it

Read the source L5 (`export -d level5`), subtract the crop per side for every frame range, rewrite
with the `editor` subcommand, re-inject, encode, and gate that the output's L5 equals the expected
value on every frame. Refuse the crop when it exceeds the L5 offset on any side (the RPU says that
area is picture).

- **Costs:** the pre-pass of finding 5 for every cropped DV source (profile 8 gains a path it does
  not otherwise need, with the variable-frame-rate refusal and the working-file disk); the
  extract, edit and inject route because `--edit-config` cannot do frame ranges; per-range
  arithmetic and its rounding rules (odd offsets against even 4:2:0 dimensions); an L5 is inserted
  where the source had none (`docs/editor.md`), which is a new claim about the picture the source
  never made; the most fixtures of any option.

### (c) Crop to the RPU's own active area, or not at all

Crop a DV source only when every frame's L5 is one identical, non-zero rectangle with even offsets
for 4:2:0, and the T26 consensus crop agrees with it within a small tolerance (2 px per side,
`ASSUMED`, to be calibrated). The crop rectangle is then the L5 rectangle itself, the blackness gate
still proves the removed area black, the pre-pass zeroes L5 with the documented flag
(`dovi_tool -m 0 -c convert` for profile 8; `dovi_tool -m 2 -c convert --discard` for opted-in
profile 7, the same invocation goal 8 already runs plus one flag), and a gate proves every output
frame's L5 is 0/0/0/0. Anything else falls back to (a).

- **Costs:** the same pre-pass as (b) for the files that qualify; an L5 read on every DV source that
  has `crop` on. It refuses more files than (b): shot-varying L5 (mixed aspect ratio), residual black
  rows, odd offsets, and an RPU whose L5 disagrees with the pixels. Gains over (b): no arithmetic and
  no frame-range editing; the result is exactly what `dovi_tool` documents as "no letterbox bars";
  and two independent witnesses (the mastering-side metadata and the pixels) must agree before any
  picture is removed.

### (d) Crop only where nothing goes stale

Crop only when the source L5 is absent or all zero, so nothing in the RPU describes bars and the
output is right without any rewrite and without the pre-pass.

- **Costs:** it still needs `dovi_tool` to read L5 (finding 3); the RPU disagrees with the pixels by
  construction (it says no bars while cropdetect found bars), which is the kind of ambiguity the
  fail-safe rule says to skip; and how many sources qualify is unknown.

## Recommendation

**Option (c): crop a Dolby Vision source only to the rectangle its own RPU names as the active
area, with L5 zeroed by `dovi_tool -c` in the pre-pass and a gate proving 0/0/0/0 on every output
frame; every other DV source falls back to (a) and encodes uncropped with the reason recorded.**

- Ship order inside goal 8: the (a) refusal lands first (it is I7), then (c) on top of the profile 7
  pre-pass machinery, so the fallback is proven before the feature exists.
- The decision function is pure and table-tested: inputs are the exported L5 config, the consensus
  crop, the frame size and the pixel format; outputs are a crop rectangle or a named refusal
  (`crop refused: Dolby Vision L5 varies by shot`, `... L5 is zero or absent`, `... L5 disagrees
  with the picture`, `... odd L5 offset for 4:2:0`, `... variable frame rate`, `... dovi_tool
  failed`).
- The output gate reads L5 with `dovi_tool extract-rpu` and `export -d level5`, requires one preset
  of 0/0/0/0 whose edits cover every frame, and runs beside goal 8's RPU-count gate; a failure
  discards the output like any gate failure, and the next attempt encodes uncropped.
- No new config key: `crop` keeps its T26 opt-in, and the DV rule applies whenever it is on.
- T26 is kept, not bent: the sampled consensus must still agree or there is no crop, and the
  blackness gate still runs over the removed area. What changes on a DV source is only which of two
  agreeing rectangles is cut, and T26 does not fix that; if L5 claims more rows than the pixels
  show as black, the blackness gate refuses.
- Flag: if the owner prefers the smallest surface, (a) is the safe alternative at no build cost;
  (b) is not recommended: its extra reach (partial crops, shot-varying L5) costs per-range
  arithmetic and the extract, edit and inject route, and where the source has no L5 the editor
  inserts one, a claim about the picture the source never made.

## Test plan

Synthetic fixtures only: lavfi video and RPUs from `dovi_tool generate`, never real library content,
never a GPU (T9, T41). Encodes run under the heavy lock; the pure decision function needs no binary.

- **Fixture recipe** (proven today end to end, finding 6):
  `testsrc2=s=320x160` padded to 320x240 (40 px bars), 10-bit PQ through libx265 with
  mastering-display metadata to raw HEVC; `dovi_tool generate -j` with `profile` 8.1, `length` equal
  to the frame count, `level6`, and `level5` offsets as the case needs (`docs/generator.md`: "If not
  specified, L5 metadata is added with 0 offsets"); `inject-rpu`; then an encode with
  `-dolbyvision 1` and VBV settings to an mkv carrying a DOVI configuration record.
- **Cases:**
  - L5 equal to the bars (40/40): cropped to 320x160, output L5 0/0/0/0 on every frame, RPU count
    equals frame count, VMAF against the identically cropped source passes;
  - L5 zero while bars exist: refused, encodes uncropped;
  - L5 varying by frame range (built with the `editor` subcommand, ranges `0-11` and `12-23`):
    refused;
  - odd L5 offsets (41/39): refused;
  - L5 disagreeing with cropdetect beyond the tolerance (20/20 over 40 px bars): refused;
  - text burned into a bar: the blackness gate refuses (T26 fixture, now with DV);
  - variable frame rate: the pre-pass refuses, the file encodes uncropped with DV carried;
  - the bite: a build that drops `-c` from the pre-pass reds the L5 gate on the first case (stale
    40/40 is the observed ffmpeg behaviour, finding 2);
  - `dovi_tool` failing: a fake `dovi_tool` script that exits non-zero, or emits malformed JSON, or
    omits L5, gives a named refusal, never a crop.
- **Golden argv:** the pre-pass for profile 8 (`-m 0 -c convert`) and profile 7
  (`-m 2 -c convert --discard`), the raw HEVC input (`-f hevc -framerate`), and the gate's
  `extract-rpu` and `export` calls.
- **Profile 7 limit:** `dovi_tool generate` makes profile 8.1 or 8.4 RPUs only (`docs/generator.md`),
  so a synthetic profile 7 dual-layer source cannot be built this way; profile 7 with crop is proven
  at the argv level with the fake, and the real pipeline on profile 8.

## Claims re-verified

| Claim | Source (read 2026-09-29) | Outcome |
|---|---|---|
| `dovi_tool` latest release is 2.3.4 (2026-09-10), MIT, with a static musl x86_64 asset of sha256 `1844258e13c26607b32224bf1fa82b595d3b35949f5467405fda560daad32b3f` | https://github.com/quietvoid/dovi_tool/releases/tag/2.3.4 (GitHub API: release, licence, asset digest) | confirmed; downloaded, sha256 matched, `dovi_tool --version` prints 2.3.4 |
| `hdr10plus_tool` latest release is 1.7.2 (2025-12-27) | https://github.com/quietvoid/hdr10plus_tool/releases/tag/1.7.2 | confirmed (not relied on here) |
| `-c` / `--crop` sets the active area offsets to 0 | https://github.com/quietvoid/dovi_tool/blob/2.3.4/README.md and `dovi_tool --help` | confirmed; re-run: 40/40 became 0/0/0/0 on 24 of 24 frames |
| `docs/editor.md` has an `active_area` block with `crop`, `drop_l5`, `presets` and `edits` (verify claim 7) | https://github.com/quietvoid/dovi_tool/blob/2.3.4/docs/editor.md | confirmed; additions: `--edit-config` on HEVC operations supports only the `"all"` active-area edit, an absent L5 is inserted, and `drop_l5` produces non-conformant RPUs |
| `dovi_tool generate` takes `level5` offsets from JSON | https://github.com/quietvoid/dovi_tool/blob/2.3.4/docs/generator.md | confirmed; it generates profile 8.1 or 8.4 only, so no synthetic profile 7 |
| `export -d level5` writes L5 as an editor config | README at 2.3.4 and `dovi_tool export --help` | confirmed; quirk: `"crop": true` is set even when presets are non-zero |
| "Use `dovi_tool --crop` in the pre-pass" suffices (research section 1 implications) | the lab and `docs/editor.md` | corrected: `--crop` is right only when the crop removes the whole letterbox; a partial crop needs presets, and frame ranges need the `editor` subcommand |
| Crop leaves stale L5 (verify claim 7) | the verifier's output RPU re-read with `dovi_tool info -s`; https://github.com/FFmpeg/FFmpeg/blob/master/libavfilter/vf_crop.c at `d85cdd2597` | confirmed |
| `dovi_rpuenc.c` writes L5 verbatim | https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/dovi_rpuenc.c (lines 497-500 at `d85cdd2597`) | confirmed |
| `AVDOVIDmLevel5` carries four edge offsets | https://github.com/FFmpeg/FFmpeg/blob/master/libavutil/dovi_meta.h | confirmed |
| DV metadata side data is colour-dependent, not size-dependent | https://github.com/FFmpeg/FFmpeg/blob/master/libavutil/side_data.c (lines 44-45) | confirmed |
| The `dovi_rpu` bitstream filter cannot edit L5 | https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/bsf/dovi_rpu.c and `ffmpeg -h bsf=dovi_rpu` on the pinned build | confirmed: only `strip` and `compression` |
| `ffprobe` shows no L5 | https://github.com/FFmpeg/FFmpeg/blob/master/fftools/ffprobe.c (`print_dovi_metadata`) and a run on the pinned build | confirmed (new finding) |
| libx265 codes the RPU from each frame's DV metadata, option `dolbyvision` default `auto` | https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/libx265.c (lines 848-860, 1055) | confirmed |
| Raw HEVC cannot be stream-copied into mkv (verify claim 6) | the verifier's test | not re-run today; relied on as the verifier's TESTED result |
| libx265 DV needs VBV settings and mastering-display metadata (verify claim 3) | the verifier's test | not re-run today; relied on as the verifier's TESTED result |
| A rewritten L5 survives the libx265 encode unchanged | the lab encode on the pinned ffmpeg (`N-125875-g5d4d3bdc61`) with `dovi_tool` 2.3.4 | confirmed on 24 of 24 frames for all three rewrite routes (finding 6) |
| Crop without a rewrite leaves L5 stale (re-run, not only re-read) | the same lab, control row | confirmed: 320x160 output, L5 top 40, bottom 40 on 24 of 24 frames |

## Sources

- https://github.com/quietvoid/dovi_tool/releases/tag/2.3.4 (read 2026-09-29)
- https://github.com/quietvoid/dovi_tool/blob/2.3.4/README.md (read 2026-09-29)
- https://github.com/quietvoid/dovi_tool/blob/2.3.4/docs/editor.md (read 2026-09-29)
- https://github.com/quietvoid/dovi_tool/blob/2.3.4/docs/generator.md (read 2026-09-29)
- https://github.com/quietvoid/hdr10plus_tool/releases/tag/1.7.2 (read 2026-09-29)
- https://github.com/FFmpeg/FFmpeg/blob/master/libavfilter/vf_crop.c (read 2026-09-29, master at
  `d85cdd2597`)
- https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/dovi_rpuenc.c (read 2026-09-29)
- https://github.com/FFmpeg/FFmpeg/blob/master/libavutil/dovi_meta.h (read 2026-09-29)
- https://github.com/FFmpeg/FFmpeg/blob/master/libavutil/side_data.c (read 2026-09-29)
- https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/bsf/dovi_rpu.c (read 2026-09-29)
- https://github.com/FFmpeg/FFmpeg/blob/master/fftools/ffprobe.c (read 2026-09-29)
- https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/libx265.c (read 2026-09-29)
