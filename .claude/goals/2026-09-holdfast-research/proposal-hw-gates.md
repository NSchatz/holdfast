# P2 - Hardware encodes against the gates (T10)

Proposal for Checkpoint T. Research: `research-hw-encode.md` Q3-Q4 and `verify-hw-encode.md`
(its corrections override the research). Every claim a number, range or API rests on was
re-checked on 2026-09-29 against a primary source; see "Claims re-verified". Code references are
at `30d245f`.

## What this decides

How output from a hardware encoder (`nvenc`, `av1_nvenc`, `qsv`, `vaapi`, `amf`, and the T27
additions) is held against the gates before a swap. T10 made this research-and-propose: the owner
decides here. Until the owner decides, the existing gates apply unchanged to hardware output (T10);
nothing in this proposal is built before approval, and the invariant is never weakened by default.

**Flag, stated plainly: "same gates, no exceptions" was NOT chosen as a final answer in T10.** T10
lists it under "Not chosen (as a final decision)", beside per-encoder VMAF targets. The
recommendation below keeps the same gates for every encoder and adds two things, so approving it
settles T10 that way by the owner's deliberate choice; it is not a default the program may assume.

## Where things stand

- **The gates are defined on the output.** `verifyOutput` (`internal/engine/verify.go:124-232`)
  runs: temp exists, codec (`:148-154`), length parity (`:158-160`), strictly smaller with
  `min_savings_percent` (`:164-169`), the intended-stream check (`:186-195`), full decode
  (`:200-202`), then VMAF (`:228-229`). The only encoder-dependent input is the expected codec
  (`targetCodec`, `:148`).
- **The VMAF floors** default to `min_vmaf` 95, `vmaf_min_pool` 60, `vmaf_min_chroma` 30
  (`internal/config/config.go:108-110`); the argument is `docs/design/quality-gate.md:10-30`. A
  per-root profile can already carry its own encoder and VMAF floors
  (`internal/config/config.go:1523`).
- **One quality number for every encoder.** `crf` (default 22, `internal/config/config.go:91`,
  validated 0-51 at `internal/config/profile.go:377-378`) is passed as `-crf` to `libx265` and
  `libsvtav1`, as `-cq` to NVENC (`internal/engine/encode.go:801-807`), as `-global_quality` to QSV
  (`:808-809`), as `-qp` to VAAPI (`:810-817`) and as `-qp_i`/`-qp_p` to AMF (`:818-823`); the
  config doc says so (`internal/config/config.go:296-299`). The default preset is `slow`
  (`internal/config/config.go:92`).
- **Pixel format.** `buildArgs` emits `-pix_fmt <derived>` for every encoder
  (`internal/engine/encode.go:781`); `hdr.DerivePixFmt` floors depth at 10
  (`internal/hdr/probe.go:117-132`), so most jobs request `yuv420p10le`. The VAAPI branch then
  uploads `format=nv12` (`internal/engine/encode.go:815`, and `:927` on the bitrate path): every
  VAAPI job is 8-bit.
- **HDR10 static metadata** is passed explicitly only to `libx265` (`-x265-params`
  master-display/max-cll, `internal/hdr/probe.go:92-100`, wired at
  `internal/engine/encode.go:296-307`); other encoders rely on ffmpeg's side-data propagation.
- **No output-side fidelity check exists.** The probe reads first-frame and stream side data
  (`internal/probe/probe.go:430-447`) and the engine uses it on the SOURCE before encoding
  (`internal/engine/engine.go:2697-2716`); `verifyOutput` never reads pixel format, colour tags or
  HDR10 side data of the OUTPUT.
- `docs/docker.md:340-343` already promises hardware output the identical gate.

### What the primary sources say

1. **The quality scales differ per encoder** (pinned ffmpeg `N-125875-g5d4d3bdc61`,
   `ffmpeg -h encoder=<name>`, run 2026-09-29; source at `5d4d3bdc61`):
   - `hevc_nvenc -cq`: 0-51, "0 means automatic", constant quality in VBR rate control;
     `av1_nvenc -cq`: 0-63. The value becomes `rcParams.targetQuality` (`libavcodec/nvenc.c:1146`).
   - `hevc_qsv`: `global_quality` with no maxrate and no lookahead selects ICQ
     (`libavcodec/qsvenc.c:623-625`); "For the ICQ modes, global quality range is 1 to 51"
     (`doc/encoders.texi:3739-3740`).
   - `hevc_vaapi -qp`: 0-52, "Constant QP (for P-frames; scaled by qfactor/qoffset for I/B)"; an
     explicit `qp` forces CQP and fails if the driver lacks it (`libavcodec/vaapi_encode.c:1321`),
     while `global_quality` tries ICQ and silently falls back to CQP
     (`libavcodec/vaapi_encode.c:1333-1336`); an explicit `rc_mode` fails loud instead
     (`:1318`).
   - `hevc_amf`: `-qp_i`/`-qp_p` -1 to 51 under `-rc cqp`; a `-rc qvbr` mode with
     `-qvbr_quality_level` -1 to 51 also exists.
   - `libsvtav1 -crf`: 0-63; `libx265 -crf` is a float in ffmpeg's option table.

   So `crf: 22` today means an x265 rate factor, an NVENC constant-quality target, a QSV ICQ level,
   a VAAPI fixed quantiser (no rate control at all) and an AMF fixed quantiser: five different
   instructions. How far apart their outputs land is unmeasured (ASSUMED large). Also found: the
   0-51 validation caps `av1_nvenc` and `svtav1` below the top of their 0-63 scales.
2. **Efficiency (correction V3 applied).** Arunruangsirilert and Katto (arXiv:2511.18686) compared
   hardware encoders with "the slowest presets that can yield real-time encoding at 1080p60" on an
   i7-13700H - "slow for libx264, faster for libx265, and preset 8 for SVT-AV1" - because comparing
   with the slowest preset "wouldn't be fair". Against those anchors, at 1080p QSV averaged +0.51
   VMAF and NVENC +0.38; Intel saved "about 5% of data", NVENC needed "about 10% more bits to reach
   the same quality as Intel". The software side ran a live-streaming CBR command line. No AMD
   encoder was measured. The paper supports "hardware HEVC is roughly at libx265 `faster` in
   real-time conditions"; it does not measure `medium` or holdfast's default `slow` at CRF 22.
3. **No primary source measures hardware output against holdfast's floors.** Whether a hardware
   encode clears 95/60/30 AND is strictly smaller is an empirical question per encoder and per
   source; the T43 hardware reports are what measure it. The structural gates are
   encoder-agnostic by construction.
4. **Fidelity holes VMAF cannot see.** `hevc_vaapi` accepts only `vaapi` surfaces
   (`vaapi_encode_h265.c:1213`), so the format before `hwupload` is the bit depth. `hevc_nvenc`
   lists `p010le` and `hevc_qsv` lists `nv12 p010le ...`, neither lists `yuv420p10le` (pinned
   binary), and ffmpeg then auto-selects a format with only a warning, "Incompatible pixel format
   ... auto-selecting" (`fftools/ffmpeg_mux_init.c:468-497`, log at `:490`), which holdfast's
   `-loglevel error` hides. HDR10: NVENC switches mastering and light-level output on only when the
   metadata is in stream-level `decoded_side_data` at init (`libavcodec/nvenc.c:1388-1395`,
   `:1601-1606`); QSV attaches it per frame from frame side data
   (`libavcodec/qsvenc_hevc.c:175-234`); VAAPI's `-sei` defaults to `hdr+a53_cc`
   (`vaapi_encode_h265.c:1154`); AMF sets it from frame side data (`libavcodec/amfenc.c:460-464`).
   The VMAF model is luma-only (`docs/design/quality-gate.md:23-26`) and scores pictures, not
   metadata, so a dropped mastering block passes it, and PQ tags on 8-bit samples are at most
   weakly visible to it (ASSUMED).

## Options

### (a) Same gates for every encoder, plus per-encoder quality scales and an output fidelity gate

Every existing gate and floor stays exactly as it is for every encoder. Goal 4 adds:

- **An output fidelity gate** (additive, runs before VMAF): the output's bit depth, chroma
  subsampling, colour primaries, transfer, matrix and range, and the HDR10 mastering-display and
  content-light side data must equal the source's, or equal what the encode plan declares it
  changes (for example the 8-to-10-bit floor). It reads the output with the probe readers holdfast
  already uses on sources (`internal/probe/probe.go:430-447`) plus the stream's `pix_fmt` and
  `color_*` fields. A mismatch is a deterministic reject with a named field; the source is kept.
- **Explicit pixel formats** in every argv (`p010le`/`nv12` for NVENC and QSV, `format=p010` before
  `hwupload` with `main10` for VAAPI), so ffmpeg never auto-selects silently.
- **Per-encoder quality keys.** Proposed shape (goal 4 fixes the spelling):

  ```yaml
  crf: 22            # unchanged: libx265 and libsvtav1
  quality:           # optional, keyed by registry key; validated per encoder's own scale
    nvenc: <1-51>    # -cq (0, "automatic", refused)
    av1_nvenc: <1-63>
    qsv: <1-51>      # -global_quality, ICQ
    vaapi: <0-52>    # -qp, CQP (explicit, so an unsupported mode fails loud)
    amf: <0-51>      # -qp_i/-qp_p under -rc cqp
  ```

  An absent key inherits `crf`, so an existing configuration yields byte-identical argv (I5). No
  numeric default is invented: a shipped default not measured on hardware is marked `ASSUMED` until
  a hardware report calibrates it (brief section 0.8), and changing a default later is its own owner-visible
  commit because it changes argv for existing hardware configurations.

Costs: goal 4's work (the gate, one failing fixture per field, explicit formats, the keys and their
validation); one more config key; the explicit pixel formats DO change the argv of existing
hardware configurations (the VAAPI 10-bit path above all), so that part lands as goal 5's
recorded argv fix with its golden argv, while the quality keys, when absent, change nothing; hardware jobs rejected as not smaller or below a floor cost a
wasted encode and keep the source; the right hardware values stay `ASSUMED` until the owner's
hardware reports land. Effect on the invariant: strengthened; one gate is added, none relaxed, and
no line of `docs/design/swap.md` or `docs/design/quality-gate.md` is removed (the swap document's
gate list gains the fidelity gate by addition).

### (b) Per-encoder VMAF targets

`min_vmaf`, `vmaf_min_pool` and `vmaf_min_chroma` settable per encoder, typically lower for
hardware.

Costs: a floor stops describing the output the owner accepts and starts describing the machine that
made it, against the argument in `docs/design/quality-gate.md:14-21`; the worst-frame floor is the
one that catches local damage (`internal/config/config.go:470-488`), exactly where weaker hardware
rate control is expected to fail (ASSUMED); it removes or rewrites lines of `quality-gate.md`, which
brief section 4 allows only under an approved P2; T10 did not choose it as a final decision either. It also adds
little: a per-root profile can already lower floors for a library that uses a hardware encoder
(`internal/config/config.go:1523`). A stricter-only variant (per-encoder floors may only be higher)
leaves the invariant intact but adds configuration with no measured need. Effect on the invariant:
weakened in the lowering form.

### (c) Relaxed gates for hardware output

Skip VMAF for hardware to save time, accept an output that is not smaller, or loosen length parity
for hardware timestamp behaviour.

Costs: directly weakens "no source is mutated until a replacement passed every gate"; reintroduces
the replace-before-verify failure `docs/design/swap.md:11-16` exists to prevent; needs lines of
`swap.md` or `quality-gate.md` removed. Effect on the invariant: broken. Not recommended.

### (d) Follow-on, not a T10 answer: hardware first, CPU on rejection

Try hardware; on a gate reject, requeue on `cpu`. Two encodes per rejected file and both attempts
recorded; it belongs with T11's `hw_fallback` and the queue work, after (a) has real rejection
rates.

## Recommendation

Approve option (a): the same gates and floors for every encoder, plus per-encoder quality keys and
an additive output fidelity gate, built in goal 4 as brief section 8 describes. The gates already measure the
output rather than the encoder, so hardware needs no exception there; what it lacks is a quality
number that means something on its own scale and the fidelity checks the `libx265` path gets
implicitly and the hardware paths lose silently (8-bit VAAPI, NVENC and QSV format auto-selection,
NVENC mastering metadata only from stream side data).

Stated plainly: "same gates, no exceptions" was not chosen as a final answer in T10, so approving
(a) is the owner's deliberate choice to settle T10 this way. Until the owner approves an option,
the existing gates apply unchanged to hardware output, and nothing here is built.

## Test plan

All on fakes, synthetic lavfi fixtures and golden argv; no goal runs a GPU (T9), and hardware tests
stay behind the `hwlive` build tag that no CI or Makefile target sets.

- **Golden argv** for every registry encoder with and without a `quality.<key>`: absent key gives
  byte-identical argv to today (I5); a present key reaches the right flag (`-cq`,
  `-global_quality`, `-qp`, `-qp_i`/`-qp_p`); every argv names its pixel format explicitly.
- **Validation:** each encoder's range reds at its edges (0 for `nvenc`, 64 for `av1_nvenc`, 53 for
  `vaapi`, an unknown encoder key under `quality`), and `holdfast validate` names the scale.
- **Fidelity gate, one fixture per field** (bit depth, chroma, primaries, transfer, matrix, range,
  mastering display, content light), made on the CPU path: for example an 8-bit output where the
  plan says 10-bit, a PQ source encoded without its mastering block. Each reds with the field named
  and the source byte-identical afterwards.
- **A fake hardware ffmpeg** (the existing fake-script pattern) that writes an 8-bit output for a
  10-bit plan is rejected by the fidelity gate, proving the VAAPI `nv12` hole is caught even before
  goal 5 fixes the argv.
- **Gate integrity:** no existing assertion removed, `func Test` counts do not fall, no line of
  `swap.md` or `quality-gate.md` removed.
- **Calibration** of hardware quality defaults is a NEEDS-OWNER hardware report per encoder (goal 6,
  T43), never an agent measurement.

## Claims re-verified

| Claim | Source (URL, read 2026-09-29) | Outcome |
|---|---|---|
| `crf` reused as `-cq`, `-global_quality`, `-qp`, `-qp_i`/`-qp_p` | `internal/engine/encode.go:801-823` at `30d245f` | confirmed |
| Every VAAPI job uploads `nv12` (8-bit) | `internal/engine/encode.go:815,927`; `internal/hdr/probe.go:117-132` | confirmed |
| `hevc_nvenc -cq` 0-51, `av1_nvenc -cq` 0-63 | pinned ffmpeg `-h encoder=`; https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/nvenc.c | confirmed; the AV1 scale (0-63) is new relative to the research |
| QSV `global_quality` selects ICQ, range 1-51 | https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/qsvenc.c (623-625); https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/doc/encoders.texi (3739-3740) | confirmed |
| VAAPI `-qp` is constant QP 0-52; `global_quality` ICQ falls back to CQP silently | https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/vaapi_encode.c (1318-1336, 1527) | confirmed; silent ICQ-to-CQP fallback is new |
| AMF `-qp_i`/`-qp_p` -1 to 51; `qvbr` mode exists | pinned ffmpeg `-h encoder=hevc_amf` | confirmed |
| `hevc_vaapi` accepts only `vaapi` surfaces | https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/vaapi_encode_h265.c (1213) | confirmed |
| NVENC and QSV do not list `yuv420p10le`; auto-select logs only a warning | pinned ffmpeg `-h encoder=`; https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/fftools/ffmpeg_mux_init.c (468-497) | confirmed |
| HDR10 carriage per hardware encoder | https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/nvenc.c (1388-1395, 1601-1606); .../qsvenc_hevc.c (175-234); .../vaapi_encode_h265.c (1154); .../amfenc.c (460-464) | confirmed |
| Hardware HEVC near x265 "faster/medium" | https://arxiv.org/html/2511.18686 ; https://arxiv.org/abs/2511.18686 | corrected: anchors are real-time presets (libx265 `faster`), CBR live-streaming setup, no AMD; nothing measured at `medium` or `slow` |
| NVENC needs about 10% more bits than Intel | https://arxiv.org/html/2511.18686 | confirmed (omitted by the research) |
| Hardware output passes holdfast's floors at useful sizes | none found | not verifiable without hardware; left to the T43 reports |

## Sources

- FFmpeg source at the pinned revision `5d4d3bdc61`, read 2026-09-29:
  https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/nvenc.c ,
  https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/qsvenc.c ,
  https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/qsvenc_hevc.c ,
  https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/vaapi_encode.c ,
  https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/vaapi_encode_h265.c ,
  https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/libavcodec/amfenc.c ,
  https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/fftools/ffmpeg_mux_init.c ,
  https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/doc/encoders.texi
- The pinned ffmpeg binary (`N-125875-g5d4d3bdc61-20260731`, the build `Dockerfile:55-58` pins),
  `ffmpeg -h encoder=<name>` for `hevc_nvenc`, `av1_nvenc`, `hevc_qsv`, `hevc_vaapi`, `hevc_amf`,
  `libx265`, `libsvtav1`, run 2026-09-29 (no device used).
- K. Arunruangsirilert, J. Katto, "Evaluation of Hardware-based Video Encoders on Modern GPUs for
  UHD Live-Streaming", arXiv:2511.18686 (submitted 2025-11-24), https://arxiv.org/html/2511.18686 ,
  read 2026-09-29.
- holdfast at `30d245f`: `internal/engine/verify.go`, `internal/engine/encode.go`,
  `internal/config/config.go`, `internal/config/profile.go`, `internal/hdr/probe.go`,
  `internal/probe/probe.go`, `docs/design/quality-gate.md`, `docs/design/swap.md`,
  `docs/docker.md`.
