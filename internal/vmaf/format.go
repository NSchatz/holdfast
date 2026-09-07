package vmaf

import (
	"github.com/NSchatz/holdfast/internal/hdr"
)

// chromaRank orders the subsampling tokens by how much chroma detail they carry.
// It is the whole basis of ComparisonFormat's choice: converting UP this ladder
// interpolates chroma samples the source never had (it invents nothing about the
// DIFFERENCE between two frames, because both sides are converted identically),
// while converting DOWN averages neighbouring chroma samples away - and averaging
// away chroma is precisely the class of damage the chroma floor exists to catch.
// A comparison made at 4:2:0 can therefore HIDE 4:4:4 chroma damage. The comparison
// always moves up.
var chromaRank = map[string]int{"420": 0, "422": 1, "444": 2}

// ComparisonFormat names the single pixel format both streams are converted to
// before they are scored, given the reference's and the distorted stream's own
// formats.
//
// Why holdfast names it at all. libavfilter will happily negotiate a common format
// on its own when two inputs disagree - and `pixel_format: auto` guarantees they
// disagree, because it floors output bit depth at 10 while most sources are 8-bit.
// The negotiated choice is not neutral: upconverting the reference and
// downconverting the distorted stream are different measurements of the same pair,
// no cited document settles which libavfilter picks, and nothing recorded the answer
// afterwards. So the score was a number whose meaning depended on an undocumented
// negotiation. Naming the format makes the measurement deterministic AND reportable
// - Result.PixelFormat travels with the score to the ledger, the event and the API.
//
// The rule is "discard nothing from either side": the RICHEST chroma subsampling of
// the two and the DEEPER of the two bit depths, always planar YUV little-endian.
// Both properties matter for a gate that must not miss damage. A shallower depth
// would quantise a difference toward zero; a coarser subsampling would average one
// away. Neither can ever make a bad encode look good here.
//
// It is a pure function of the pair, which is what makes it deterministic: the same
// source and output score in the same format every time, on any host, whatever
// libavfilter would have negotiated.
//
// ok=false when either format is outside the recognized planar-YUV family or deeper
// than 12 bits - the same bound DerivePixFmt refuses to encode at, so the comparison
// never has to name a format the encode path itself would not produce. The caller
// REJECTS the encode on a false: an output holdfast cannot name a comparison format
// for is an output it cannot honestly score, and an unscored output is never swapped
// in. (The engine skips exotic sources long before the encoder, so this is a belt on
// a brace, not a live path.)
func ComparisonFormat(reference, distorted string) (string, bool) {
	ref, okRef := hdr.ParsePixFmt(reference)
	dis, okDis := hdr.ParsePixFmt(distorted)
	if !okRef || !okDis {
		return "", false
	}
	if ref.Depth > 12 || dis.Depth > 12 {
		return "", false
	}
	out := hdr.PixFmt{Chroma: ref.Chroma, Depth: ref.Depth, Endian: "le"}
	if chromaRank[dis.Chroma] > chromaRank[ref.Chroma] {
		out.Chroma = dis.Chroma
	}
	if dis.Depth > out.Depth {
		out.Depth = dis.Depth
	}
	return out.String(), true
}
