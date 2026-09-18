package engine

import (
	"fmt"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/probe"
)

// The deinterlace, resolved in ONE place for the three sites that must agree about it: the
// encoder that applies the filter, the perceptual gate that builds its reference through
// the same filter, and the terminal row that records what produced the replacement.
//
// They agree because each of them resolves from the SAME two inputs - the library profile
// the engine handed down and the source's own probe snapshot - through the functions below
// and through nothing else. A site that read the configuration for itself would be a second
// answer to "what was applied", and the perceptual gate's whole claim is that it scored
// this encode rather than something adjacent to it.

// deinterlaceFor resolves a profile's configured deinterlace and refuses what this build
// will not run. It answers about the CONFIGURATION alone, so it is what a caller asks
// before it has a source in hand.
//
// config.Validate refuses both of these at start, naming the key and the root. This is the
// backstop behind that, and it is not redundant: a Config assembled in Go never passes
// through Validate, and the refusal has to be a property of the engine rather than of
// whoever remembered to call the validator before handing it a file to delete.
func deinterlaceFor(prof config.Profile) (deinterlace.Filter, error) {
	f, ok := prof.DeinterlaceFilter()
	if !ok {
		return deinterlace.Filter{}, fmt.Errorf("deinterlace %q is not a value this build can resolve "+
			"to a filter (known: %v), so nothing was encoded", prof.Deinterlace, deinterlace.Known())
	}
	if f.EmitsOneFramePerField() {
		return deinterlace.Filter{}, fmt.Errorf("deinterlace %q emits one frame per FIELD, which would "+
			"double the output's frame count: refusing to encode. Packet-count parity and duration "+
			"parity are graded against the SOURCE's frame count, so such an output could only be "+
			"accepted by weakening a gate that stands in front of the deletion of a source",
			prof.Deinterlace)
	}
	return f, nil
}

// deinterlaceWanted reports whether this profile asks for a deinterlace AND is in a state
// that can perform one.
//
// A REMUX-ONLY root cannot: it stream-copies the video, so nothing re-encodes and no filter
// can run. The two keys together are not a contradiction to refuse at startup - remux_only
// is about what is done to the video, deinterlace about what is done to an interlaced one -
// but a job under both must fall back to what this tool did before the key existed and SKIP
// the interlaced source. The alternative is a remux that carries the interlacing through
// while the row claims a deinterlace, which is a false provenance claim on the one record
// that outlives the source.
//
// An operator who removes remux_only offers those files back with
// `requeue --guard interlaced`: the guard recorded the deinterlace value it read, and that
// value has not moved.
func deinterlaceWanted(prof config.Profile) bool {
	return prof.DeinterlaceEnabled() && !prof.RemuxOnlyEnabled()
}

// deinterlaceApplied is the filter that runs for THIS source: the profile's configured one
// where the source's container reports an interlaced field order, and none otherwise.
//
// The field-order test is what keeps a root-wide key from transforming files it was never
// about. Every file under a root reaches the encoder, and a deinterlacing filter applied to
// progressive content is not a no-op - it interpolates from fields that are not there and
// softens every frame it touches. Only the four interlaced spellings license it; the
// unknown case never reaches here at all, because the scan-type guard classifies it and
// skips (see SkipUnknownFieldOrder), and the telecined case is skipped beside it.
//
// props may be nil, which is what a direct caller of the exported encoder passes when it
// did not pre-probe. Nothing is applied then: a filter is never run on a source nobody
// established the scan type of.
func deinterlaceApplied(prof config.Profile, props *probe.VideoProps) (deinterlace.Filter, error) {
	f, err := deinterlaceFor(prof)
	if err != nil || !f.Enabled() || !deinterlaceWanted(prof) {
		return deinterlace.Filter{}, err
	}
	if props == nil || !interlacedFieldOrder(props.FieldOrder()) {
		return deinterlace.Filter{}, nil
	}
	return f, nil
}

// interlacedFieldOrder reports whether a normalised field_order is one of the four
// spellings that mean INTERLACED. It is the one place they are enumerated, so the guard
// that skips an interlaced source and the encoder that deinterlaces one cannot disagree
// about which sources those are.
func interlacedFieldOrder(order string) bool {
	switch order {
	case "tt", "bb", "tb", "bt":
		return true
	default:
		return false
	}
}
