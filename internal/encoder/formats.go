package encoder

import (
	"strings"

	"github.com/NSchatz/holdfast/internal/hdr"
)

// THE EXPLICIT PIXEL FORMAT: every encode names, on its own command line, a format the
// encoder lists that carries the plan's chroma subsampling and bit depth exactly.
//
// Before this, the command line said `-pix_fmt <plan format>` for every encoder, and several
// encoders do not list the plan's format. ffmpeg then picks the least-lossy format the
// encoder does list and says so only in a warning, "Incompatible pixel format ...
// auto-selecting" (fftools/ffmpeg_mux_init.c:468-496 at 5d4d3bdc61), which holdfast's
// `-loglevel error` hides. For a 4:2:0 10-bit plan on NVENC that pick is p010le and loses
// nothing; for a plan the list cannot carry (4:2:2, 4:4:4 or 12-bit on an encoder that
// has none of those) it is a silent subsample or a silent depth cut. So the encoder is
// handed a format chosen HERE, from its own list, or the job is refused.
//
// Source: https://github.com/FFmpeg/FFmpeg/blob/5d4d3bdc61/fftools/ffmpeg_mux_init.c
// (choose_pixel_fmt, lines 468-496; pix_fmt_parse falls back to it at 542-543), read
// 2026-09-30.

// The "Supported pixel formats:" line of each encoder, verbatim, from the pinned ffmpeg
// (N-125875-g5d4d3bdc61-20260731, the build Dockerfile pins): `ffmpeg -h encoder=<name>`,
// run 2026-09-30 with no device present. TestPixelFormats_MatchThePinnedBinary re-reads
// every line from the binary the gate runs and fails on any difference, so a bump of the
// pinned ffmpeg that changes a list reds rather than leaving a stale table choosing formats.
const (
	pixFmtsLibx265 = "yuv420p yuvj420p yuv422p yuvj422p yuv444p yuvj444p gbrp yuv420p10le " +
		"yuv422p10le yuv444p10le gbrp10le yuv420p12le yuv422p12le yuv444p12le gbrp12le gray " +
		"gray10le gray12le yuva420p yuva420p10le"
	pixFmtsLibsvtav1 = "yuv420p yuv420p10le"
	// hevc_nvenc and av1_nvenc print the same list.
	pixFmtsNVENC = "yuv420p nv12 p010le yuv444p p012le nv24 p016le nv16 p210le p212le p216le " +
		"yuv444p10msble yuv444p12msble yuv444p16le p410le p412le p416le bgr0 bgra rgb0 rgba " +
		"x2rgb10le x2bgr10le gbrp gbrp10msble gbrp16le cuda cuarray"
	pixFmtsQSV   = "nv12 p010le p012le yuyv422 y210le qsv bgra x2rgb10le vuyx xv30le"
	pixFmtsVAAPI = "vaapi"
	pixFmtsAMF   = "nv12 yuv420p p010le amf bgr0 rgb0 bgra argb rgba x2bgr10le rgbaf16le"
)

// uploadFormatsVAAPI are the software formats a VAAPI encode uploads. hevc_vaapi lists only
// `vaapi` surfaces (libavcodec/vaapi_encode_h265.c:1213 at 5d4d3bdc61), so the format the
// software frames are converted to BEFORE `hwupload` is the encode's chroma and depth:
// hwupload "will upload to a surface with the same layout as the software frame"
// (https://trac.ffmpeg.org/wiki/Hardware/VAAPI, read 2026-09-30 through the Wayback
// capture of 2026-01-22). nv12 (4:2:0 8-bit) and p010le (4:2:0 10-bit, with the Main 10
// profile) are the two the wiki's own encode examples upload.
//
// ASSUMED: which surface formats a VAAPI driver accepts for HEVC encode is the driver's, not
// ffmpeg's, and no device was available to ask; this build restricts the upload to these
// two, the ones the wiki documents, and skips every other plan rather than hand a driver a
// layout nobody has seen it take. A hardware report (NEEDS-OWNER, brief T43) confirms or
// widens it.
const uploadFormatsVAAPI = "nv12 p010le"

// semiPlanar are the semi-planar spellings of each planar chroma and depth, little-endian:
// the layout hardware encoders list in place of the planar one. Index: chroma, then depth.
var semiPlanar = map[string]map[int]string{
	"420": {8: "nv12", 10: "p010le", 12: "p012le"},
	"422": {8: "nv16", 10: "p210le", 12: "p212le"},
	"444": {8: "nv24", 10: "p410le", 12: "p412le"},
}

// parseLayout takes a planar-YUV name (hdr.ParsePixFmt's family) or one of the semi-planar
// names above apart into its chroma and depth. ok is false for anything else.
func parseLayout(format string) (hdr.PixFmt, bool) {
	if p, ok := hdr.ParsePixFmt(format); ok {
		return p, true
	}
	for chroma, byDepth := range semiPlanar {
		for depth, name := range byDepth {
			if name == format {
				endian := "le"
				if depth == 8 {
					endian = ""
				}
				return hdr.PixFmt{Chroma: chroma, Depth: depth, Endian: endian}, true
			}
		}
	}
	return hdr.PixFmt{}, false
}

// listed reports whether format is one of the space-separated names in list.
func listed(list, format string) bool {
	if format == "" {
		return false
	}
	for _, f := range strings.Fields(list) {
		if f == format {
			return true
		}
	}
	return false
}

// Uploads reports whether this encoder takes only hardware surfaces, so the format it is
// handed is the software format uploaded before it (`format=<fmt>,hwupload`) rather than a
// `-pix_fmt`.
func (s Spec) Uploads() bool { return s.UploadFormats != "" }

// InputFormat is the format this encoder is handed for a plan's pixel format (one derived by
// hdr.DerivePixFmt, or a forced pixel_format), chosen so it carries the plan's chroma
// subsampling and bit depth EXACTLY:
//
//  1. the plan's format itself, where the encoder lists it;
//  2. else the planar little-endian spelling of the same chroma and depth, where the encoder
//     lists that (a forced semi-planar, big-endian or full-range `yuvj` name reaches an
//     encoder that lists only the plain planar one);
//  3. else the semi-planar spelling of the same chroma and depth (4:2:0: nv12, p010le,
//     p012le; 4:2:2: nv16, p210le, p212le; 4:4:4: nv24, p410le, p412le), where the encoder
//     lists that;
//  4. else nothing, and ok is false: this encoder cannot carry the plan, and the job is
//     skipped (the engine's pixel-format guard) or refused (the plan derivation) rather than
//     left to ffmpeg's silent auto-selection.
//
// For an encoder that Uploads, "lists" means UploadFormats: the answer is the software format
// uploaded, not a -pix_fmt.
//
// Colour range is not part of the answer: a `yuvj` name's full range travels on -color_range,
// exactly as hdr.PixFmt.String drops the `j` when it spells a derived format.
func (s Spec) InputFormat(plan string) (string, bool) {
	list := s.PixelFormats
	if s.Uploads() {
		list = s.UploadFormats
	}
	if listed(list, plan) {
		return plan, true
	}
	p, ok := parseLayout(plan)
	if !ok {
		return "", false
	}
	// Byte order is how a sample is laid out in memory, not what it holds, so the
	// equivalents are spelled little-endian, the order every encoder here lists.
	p.Endian = ""
	if planar := p.String(); listed(list, planar) {
		return planar, true
	}
	if semi := semiPlanar[p.Chroma][p.Depth]; listed(list, semi) {
		return semi, true
	}
	return "", false
}
