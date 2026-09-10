package vmaf

import (
	"github.com/NSchatz/holdfast/internal/hdr"
)

// chromaRank orders the subsampling tokens by the chroma detail they carry, and is the
// whole basis of ComparisonFormat's choice. Converting UP the ladder invents nothing
// about the DIFFERENCE between two frames, because both sides are converted identically;
// converting DOWN averages neighbouring chroma away, which is the damage the chroma
// floor exists to catch. A comparison at 4:2:0 can HIDE 4:4:4 damage, so it moves up.
var chromaRank = map[string]int{"420": 0, "422": 1, "444": 2}

// ComparisonFormat names the single pixel format both streams are converted to before
// they are scored.
//
// libavfilter would negotiate one itself, and `pixel_format: auto` guarantees the inputs
// disagree because it floors output depth at 10 while most sources are 8-bit. That
// negotiation is not neutral - upconverting the reference and downconverting the
// distorted stream are different measurements of the same pair - so holdfast names it:
// the score is deterministic, and Result.PixelFormat travels with it to the ledger.
//
// The rule is "discard nothing from either side": the RICHEST chroma subsampling of the
// two and the DEEPER of the two bit depths, always planar YUV little-endian. A shallower
// depth would quantise a difference toward zero and a coarser subsampling would average
// one away, so neither can make a bad encode look good here.
//
// ok=false when either format is outside the recognized planar-YUV family or deeper than
// 12 bits, the bound DerivePixFmt refuses to encode at. The caller REJECTS the encode on
// a false: an output holdfast cannot honestly score is never swapped in.
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
