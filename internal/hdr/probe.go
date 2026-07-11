package hdr

import (
	"context"
	"regexp"
	"strconv"
	"strings"
)

// prober is the minimal surface Classify/DeriveColorArgs need from
// *probe.Prober — declared as an interface here (rather than importing
// internal/probe directly) so this package has no import-cycle risk and stays a
// thin, testable seam over whatever probing implementation the engine wires in.
type prober interface {
	CodecTagString(ctx context.Context, f string) string
	ColorField(ctx context.Context, f, field string) string
	SideDataFlat(ctx context.Context, f string) string
}

// Classify probes f and returns its HDR classification (dv|hdr10plus|hdr10|other).
// Port of bash hdr_class: codec-agnostic, feeds ClassFrom the codec tag, the
// frame+stream side data, and the color_transfer tag.
func Classify(ctx context.Context, p prober, f string) string {
	tag := p.CodecTagString(ctx, f)
	flat := p.SideDataFlat(ctx, f)
	trc := p.ColorField(ctx, f, "color_transfer")
	return ClassFrom(tag, flat, trc)
}

// DeriveColorArgs derives colour-preservation args for the encoder from the
// source: ffmpegFlags are encoder-agnostic `-color_*` output flags; x265Params is
// the ":k=v…" suffix to append to -x265-params (libx265-only). Passes through only
// tags the source actually signals — EXCEPT that a source carrying HDR10 static
// metadata (PQ transfer or a mastering-display block) is, by definition, bt2020/PQ,
// so those are defaulted when the source under-signals them (common with H.264
// HDR). SDR/HLG get their tags passed through with no HDR10 params. Port of bash
// derive_color_args.
func DeriveColorArgs(ctx context.Context, p prober, f string) (ffmpegFlags []string, x265Params string) {
	prim := p.ColorField(ctx, f, "color_primaries")
	trc := p.ColorField(ctx, f, "color_transfer")
	spc := p.ColorField(ctx, f, "color_space")
	rng := p.ColorField(ctx, f, "color_range")
	flat := p.SideDataFlat(ctx, f)

	hasMD := strings.Contains(flat, "Mastering display metadata")
	isHDR10 := trc == "smpte2084" || hasMD
	if isHDR10 {
		if prim == "" {
			prim = "bt2020"
		}
		if trc == "" {
			trc = "smpte2084"
		}
		if spc == "" {
			spc = "bt2020nc"
		}
		if rng == "" {
			rng = "tv"
		}
	}

	var x strings.Builder
	if prim != "" {
		ffmpegFlags = append(ffmpegFlags, "-color_primaries", prim)
		x.WriteString(":colorprim=" + prim)
	}
	if trc != "" {
		ffmpegFlags = append(ffmpegFlags, "-color_trc", trc)
		x.WriteString(":transfer=" + trc)
	}
	if spc != "" {
		ffmpegFlags = append(ffmpegFlags, "-colorspace", spc)
		x.WriteString(":colormatrix=" + spc)
	}
	if rng != "" {
		// Range is signalled via the ffmpeg -color_range flag only; bash
		// derive_color_args deliberately sets no x265 range param (x265 infers range
		// from the VUI / -color_range), so the asymmetry with prim/trc/spc is intended.
		ffmpegFlags = append(ffmpegFlags, "-color_range", rng)
	}
	if isHDR10 {
		if md := MasterDisplay(flat); md != "" {
			x.WriteString(":master-display=" + md)
		}
		if cll := MaxCLL(flat); cll != "" {
			x.WriteString(":max-cll=" + cll)
		}
		x.WriteString(":hdr10-opt=1:repeat-headers=1")
	}
	return ffmpegFlags, x.String()
}

// chromaRe splits a pix_fmt into its chroma-subsampling token (420/422/444/other)
// and its bit-depth suffix, e.g. "yuv420p10le" -> "420", "10le".
var pixFmtRe = regexp.MustCompile(`^yuv(j?)(420|422|444)p(\d*)(le|be)?$`)

// DerivePixFmt preserves the source's chroma subsampling (4:2:0/4:2:2/4:4:4) while
// flooring the bit-depth at 10 (8-bit -> 10-bit for compression + no banding;
// 10/12/16-bit sources keep their own depth). Recognized examples: yuv420p ->
// yuv420p10le, yuvj420p -> yuv420p10le (the "j" = full-range JPEG variant; the
// range itself is carried separately via -color_range), yuv422p -> yuv422p10le,
// yuv444p -> yuv444p10le, yuv420p10le -> itself, yuv420p12le -> itself,
// yuv422p10le -> itself. An unrecognized/exotic pix_fmt (4:1:1, RGB, paletted,
// etc.) returns ok=false — the caller must SKIP rather than silently subsample or
// guess.
func DerivePixFmt(srcPixFmt string) (out string, ok bool) {
	m := pixFmtRe.FindStringSubmatch(srcPixFmt)
	if m == nil {
		return "", false
	}
	chroma := m[2]
	depthStr := m[3]
	if depthStr == "" {
		depthStr = "8"
	}
	depth, err := strconv.Atoi(depthStr)
	if err != nil {
		return "", false
	}
	// libx265 encodes only 8/10/12-bit. A deeper source (e.g. 16-bit) must be
	// SKIPPED, not silently reduced to 12-bit — this tool never silently loses
	// precision. (16-bit consumer video is essentially nonexistent; skipping is safe.)
	if depth > 12 {
		return "", false
	}
	if depth < 10 {
		depth = 10 // floor at 10-bit (8 -> 10: better compression, no banding)
	}
	endian := m[4]
	if endian == "" {
		endian = "le"
	}
	return "yuv" + chroma + "p" + strconv.Itoa(depth) + endian, true
}
