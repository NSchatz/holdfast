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
	return DeriveColorArgsFrom(
		p.ColorField(ctx, f, "color_primaries"),
		p.ColorField(ctx, f, "color_transfer"),
		p.ColorField(ctx, f, "color_space"),
		p.ColorField(ctx, f, "color_range"),
		p.SideDataFlat(ctx, f),
	)
}

// DeriveColorArgsFrom is the pure core of DeriveColorArgs: it derives the encoder args
// from a source's already-probed colour tags (prim/trc/spc/rng — each already
// normalised: "" means "not signalled") and its flat side data, with no ffprobe
// dependency. TRANSCODE-PERF calls this directly from a single probe.VideoProps
// snapshot instead of re-probing four colour fields plus the side data at encode time;
// DeriveColorArgs is the thin prober-backed wrapper over it, so the two cannot drift
// (the unit tests exercise both). See DeriveColorArgs for the propagation semantics.
//
// It is DeriveColor spelled as arguments: the description an encode writes, rendered as
// the -color_* flags and the libx265 parameter suffix that write it.
func DeriveColorArgsFrom(prim, trc, spc, rng, flat string) (ffmpegFlags []string, x265Params string) {
	c := DeriveColor(prim, trc, spc, rng, flat)
	return c.FFmpegFlags(), c.X265Params()
}

// Color is the colour description an encode writes into its output: the source's own four
// tags, each "" where the source signals none (and then written nowhere), with the HDR10
// defaults filled in where the source carries HDR10 static metadata but under-signals them,
// and that static metadata itself.
//
// It is a DECLARATION of what the output will carry, which is why it is a value rather than
// a command line: the encode writes it through FFmpegFlags and X265Params, and anything that
// later asks what the output should carry reads the same value rather than re-deriving it.
type Color struct {
	// Primaries, Transfer, Matrix and Range are the -color_primaries, -color_trc,
	// -colorspace and -color_range values, in ffmpeg's spelling.
	Primaries, Transfer, Matrix, Range string
	// HDR10 reports that the source carries HDR10 static metadata: a PQ transfer, or a
	// mastering-display block.
	HDR10 bool
	// MasterDisplay and MaxCLL are that static metadata in libx265's own spelling, each ""
	// where the source's block is absent or cannot be read whole. Both are "" when HDR10 is
	// false.
	MasterDisplay, MaxCLL string
}

// DeriveColor derives the colour description an encode writes from a source's
// already-probed colour tags (prim/trc/spc/rng, each normalised, "" meaning "not
// signalled") and its flat side data. Passes through only tags the source actually
// signals - EXCEPT that a source carrying HDR10 static metadata (PQ transfer or a
// mastering-display block) is, by definition, bt2020/PQ, so those are defaulted when the
// source under-signals them (common with H.264 HDR). SDR/HLG get their tags passed through
// with no HDR10 metadata.
func DeriveColor(prim, trc, spc, rng, flat string) Color {
	hasMD := strings.Contains(flat, "Mastering display metadata")
	c := Color{Primaries: prim, Transfer: trc, Matrix: spc, Range: rng, HDR10: trc == "smpte2084" || hasMD}
	if !c.HDR10 {
		return c
	}
	if c.Primaries == "" {
		c.Primaries = "bt2020"
	}
	if c.Transfer == "" {
		c.Transfer = "smpte2084"
	}
	if c.Matrix == "" {
		c.Matrix = "bt2020nc"
	}
	if c.Range == "" {
		c.Range = "tv"
	}
	c.MasterDisplay = MasterDisplay(flat)
	c.MaxCLL = MaxCLL(flat)
	return c
}

// FFmpegFlags are the encoder-agnostic -color_* output options that write c, in the order
// primaries, transfer, matrix, range; a tag c does not carry is not written.
func (c Color) FFmpegFlags() []string {
	var flags []string
	if c.Primaries != "" {
		flags = append(flags, "-color_primaries", c.Primaries)
	}
	if c.Transfer != "" {
		flags = append(flags, "-color_trc", c.Transfer)
	}
	if c.Matrix != "" {
		flags = append(flags, "-colorspace", c.Matrix)
	}
	if c.Range != "" {
		// Range is signalled via the ffmpeg -color_range flag only; bash
		// derive_color_args deliberately sets no x265 range param (x265 infers range
		// from the VUI / -color_range), so the asymmetry with prim/trc/spc is intended.
		flags = append(flags, "-color_range", c.Range)
	}
	return flags
}

// SetParams is the setparams filter that stamps c's primaries and transfer onto every frame
// before the encoder sees it, and "" where c carries neither.
//
// The pinned ffmpeg (N-125875-g5d4d3bdc61) gives an encoder the colour primaries and transfer
// of the frames it is handed, not the -color_primaries and -color_trc options: measured
// 2026-09-30, those two options change neither the bitstream nor the container where the
// decoded frames carry a tag, and leave both unset where they do not (-colorspace and
// -color_range do take effect, through the filter graph's own negotiation). libx265 writes
// all three tags from X265Params regardless; every other encoder writes what its frames say.
// So a declared tag the source's frames do not carry - the HDR10 defaults for a source that
// under-signals them - reaches such an encoder only when the frames are relabelled, which is
// what setparams does (it relabels and converts nothing; see
// https://ffmpeg.org/ffmpeg-filters.html#setparams , read 2026-09-30).
func (c Color) SetParams() string {
	var kv []string
	if c.Primaries != "" {
		kv = append(kv, "color_primaries="+c.Primaries)
	}
	if c.Transfer != "" {
		kv = append(kv, "color_trc="+setparamsTransfer(c.Transfer))
	}
	if len(kv) == 0 {
		return ""
	}
	return "setparams=" + strings.Join(kv, ":")
}

// setparamsTransfer spells a transfer the way the setparams filter names it: ffmpeg's older
// names for the two gamma curves (gamma22, gamma28) are bt470m and bt470bg there, and every
// other name is the same.
func setparamsTransfer(trc string) string {
	switch trc {
	case "gamma22":
		return "bt470m"
	case "gamma28":
		return "bt470bg"
	}
	return trc
}

// X265Params is the ":k=v..." suffix libx265 writes c with (libx265-only): the three colour
// tags c carries, then - for HDR10 - the mastering display and content light level it has
// and the HDR10 signalling options.
func (c Color) X265Params() string {
	var x strings.Builder
	if c.Primaries != "" {
		x.WriteString(":colorprim=" + c.Primaries)
	}
	if c.Transfer != "" {
		x.WriteString(":transfer=" + c.Transfer)
	}
	if c.Matrix != "" {
		x.WriteString(":colormatrix=" + c.Matrix)
	}
	if c.HDR10 {
		if c.MasterDisplay != "" {
			x.WriteString(":master-display=" + c.MasterDisplay)
		}
		if c.MaxCLL != "" {
			x.WriteString(":max-cll=" + c.MaxCLL)
		}
		x.WriteString(":hdr10-opt=1:repeat-headers=1")
	}
	return x.String()
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
	p, ok := ParsePixFmt(srcPixFmt)
	if !ok {
		return "", false
	}
	// libx265 encodes only 8/10/12-bit. A deeper source (e.g. 16-bit) must be
	// SKIPPED, not silently reduced to 12-bit — this tool never silently loses
	// precision. (16-bit consumer video is essentially nonexistent; skipping is safe.)
	if p.Depth > 12 {
		return "", false
	}
	if p.Depth < 10 {
		p.Depth = 10 // floor at 10-bit (8 -> 10: better compression, no banding)
	}
	return p.String(), true
}

// PixFmt is a recognized planar-YUV pixel format taken apart: its chroma
// subsampling, its bit depth, and its byte order. It exists so there is ONE parser
// for this format family - DerivePixFmt (which format to ENCODE to) and
// vmaf.ComparisonFormat (which format to COMPARE in) both have to read a pix_fmt,
// and a second regex somewhere else would be a second thing to drift.
type PixFmt struct {
	// Chroma is the subsampling token: "420", "422" or "444".
	Chroma string
	// Depth is the bit depth (8 for a name with no numeric suffix).
	Depth int
	// Endian is "le" or "be". It is "" when the parsed name carried no suffix,
	// which is every 8-bit name; String() spells that as "le" for a deeper format.
	Endian string
}

// String spells the ffmpeg pix_fmt name back. 8-bit carries no numeric or
// endianness suffix ("yuv420p"); anything deeper carries both ("yuv420p10le"). The
// full-range "j" variant is deliberately NOT reproduced: range travels separately
// (-color_range), and yuvj* is deprecated as a pixel format.
func (p PixFmt) String() string {
	if p.Depth <= 8 {
		return "yuv" + p.Chroma + "p"
	}
	e := p.Endian
	if e == "" {
		e = "le"
	}
	return "yuv" + p.Chroma + "p" + strconv.Itoa(p.Depth) + e
}

// ParsePixFmt takes a pix_fmt name apart, e.g. "yuv420p10le" -> {420, 10, le},
// "yuvj420p" -> {420, 8, ""} (the "j" is the full-range JPEG variant; the range
// itself is carried separately).
//
// ok=false for anything outside the recognized family (4:1:1, RGB, paletted, or the
// empty string a failed ffprobe returns). Every caller treats that as "do not
// guess": the encoder path SKIPS the file, and the comparison path REJECTS the
// encode rather than measuring it in a format nobody chose.
func ParsePixFmt(pixFmt string) (PixFmt, bool) {
	m := pixFmtRe.FindStringSubmatch(pixFmt)
	if m == nil {
		return PixFmt{}, false
	}
	depthStr := m[3]
	if depthStr == "" {
		depthStr = "8" // no numeric suffix is the 8-bit spelling ("yuv420p")
	}
	d, err := strconv.Atoi(depthStr)
	if err != nil {
		return PixFmt{}, false
	}
	return PixFmt{Chroma: m[2], Depth: d, Endian: m[4]}, true
}
