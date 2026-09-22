// Package downscale is the registry of the resolution ceiling this build will apply, and
// the one place that decides what a `max_height` profile value MEANS.
//
// It exists for the same reason internal/deinterlace does: the value an operator writes has
// to resolve to one filter expression, and that ONE expression has to reach the encoder, the
// perceptual gate and the terminal row. A ceiling re-derived at each of those three sites is
// three answers to "what was this file scaled to", and the gate's whole claim is that it
// measured the encode that is about to replace somebody's file.
//
// # Two expressions, and why they point in opposite directions
//
// A downscaling job needs two scaling steps and they are NOT the same step.
//
//   - The ENCODE scales the source DOWN to the ceiling. That is the transformation the
//     operator asked for, and it is what makes the replacement fewer pixels than the source.
//   - The COMPARISON scales the DISTORTED OUTPUT back UP to the source's own resolution, and
//     leaves the reference exactly as it is.
//
// The second direction is the one that matters and it is not interchangeable with its
// mirror. Scaling the REFERENCE down instead would compare the encode against a source that
// was itself degraded first: the detail the downscale threw away is gone from both sides, so
// nothing measures its loss, every downscaled encode scores near the top of the scale, and
// the floors pass outputs they exist to refuse. The swap then deletes the source, and
// nothing re-runs that measurement because the thing it was a measurement OF no longer
// exists. So the reference is never filtered here, and the distorted is scaled up to meet
// it.
//
// # The scaler is NAMED
//
// Both expressions carry `flags=` explicitly rather than taking libswscale's default. The
// default is a property of the ffmpeg build, not of this configuration, so a score taken
// under one build and a score taken under another would be two measurements with nothing on
// the row to tell them apart - exactly the reason the comparison pixel format is named by
// holdfast rather than negotiated by libavfilter (see internal/vmaf).
package downscale

import "strconv"

// Scaler is the libswscale algorithm every scaling step this build performs is done with,
// spelled once so the encode, the comparison and the terminal row cannot name three.
//
// Lanczos is chosen for the direction that decides a file. The up-scale in the comparison
// has to reconstruct as little as possible of its own: a soft resampler invents detail the
// encode did not produce and would flatter it, and flattering the distorted side is the
// direction that loses somebody's source. It is a WIRE FORMAT - it lands on the terminal row
// - so changing it changes what an old row means.
const Scaler = "lanczos"

// FilterName is the ffmpeg filter every expression here is built on, spelled once so the
// startup check that asks whether this build PROVIDES it and the code that composes it
// cannot name two different filters.
const FilterName = "scale"

// MinHeight is the smallest ceiling this build will target. It is 2 because a scaled
// dimension has to be EVEN (see Resolve) and zero is not a picture; below that there is no
// encode to gate.
const MinHeight = 2

// Scale is one RESOLVED downscale: the source it is about, and the dimensions the encode
// will produce. The zero value scales nothing, which is what every configuration that sets
// no ceiling resolves to.
//
// It carries the SOURCE dimensions as well as the target because the comparison needs them:
// the distorted output is scaled back up to exactly those, and a caller holding only the
// target would have to go and read the source again to find out what to score against.
type Scale struct {
	// MaxHeight is the ceiling as configured, 0 when none is.
	MaxHeight int
	// SourceWidth and SourceHeight are the source's own coded dimensions.
	SourceWidth, SourceHeight int
	// Width and Height are what the encode produces, both EVEN and in the source's aspect
	// ratio. Both are 0 when nothing is scaled.
	Width, Height int
}

// Enabled reports whether this resolved scale actually scales anything. It is false both for
// a configuration with no ceiling and for a source already at or below one, and those two
// are the same statement to every caller: the encode, the gate and the row are what they
// would have been before this key existed.
func (s Scale) Enabled() bool { return s.Width > 0 && s.Height > 0 }

// Spec is the ffmpeg filter expression the ENCODE applies: the source scaled down to the
// ceiling, in the named scaler. "" when nothing is scaled, which leaves a filter chain
// exactly as it was.
func (s Scale) Spec() string {
	if !s.Enabled() {
		return ""
	}
	return spec(s.Width, s.Height)
}

// ScoreSpec is the ffmpeg filter expression the COMPARISON applies to the DISTORTED input:
// the encoded output scaled back UP to the source's own resolution, in the same named
// scaler, so the pooled statistics are taken at the resolution of the file that is about to
// be deleted. "" when nothing was scaled.
//
// There is deliberately no counterpart for the reference. Filtering the reference is the one
// thing this package must never offer: it would hand the gate a degraded source to grade
// against, and the source is destroyed on the strength of what that gate returns.
func (s Scale) ScoreSpec() string {
	if !s.Enabled() {
		return ""
	}
	return spec(s.SourceWidth, s.SourceHeight)
}

// ScoredWidth and ScoredHeight are the resolution the comparison is made at, which is the
// SOURCE's for a downscaling job and the output's own for every other job. They are what the
// terminal row records beside the score: a pooled VMAF with no resolution named beside it
// cannot be compared with one taken at another.
func (s Scale) ScoredWidth() int  { return s.SourceWidth }
func (s Scale) ScoredHeight() int { return s.SourceHeight }

// spec renders one scale filter at the named scaler.
func spec(w, h int) string {
	return FilterName + "=" + strconv.Itoa(w) + ":" + strconv.Itoa(h) + ":flags=" + Scaler
}

// Resolve is the WHOLE decision: what happens to a source of these dimensions under this
// ceiling.
//
// It scales nothing at all in three cases, each of which is a source this key was not about:
// no ceiling configured, a source whose height is already at or below it, and a source whose
// dimensions were not established. The last of those is never reached by the engine - the
// guard in front of it skips a file whose height nobody could read rather than encoding it
// against a guessed one - and is answered here as well because a Scale that invented a
// source resolution would hand the comparison a reference size nothing measured.
//
// THE TARGET HEIGHT IS THE CEILING and the width follows from the source's aspect ratio,
// rounded to the NEAREST EVEN value. Even in both axes is not a nicety: every pixel format
// this build encodes to is 4:2:0 chroma-subsampled, which has no representation for an odd
// dimension, and an encoder handed one either refuses or silently picks its own. Nearest
// even rather than truncated because truncation always shrinks the frame, and on a narrow
// source the accumulated error is a visibly different aspect ratio.
func Resolve(maxHeight, sourceWidth, sourceHeight int) Scale {
	s := Scale{MaxHeight: maxHeight, SourceWidth: sourceWidth, SourceHeight: sourceHeight}
	if maxHeight < MinHeight || sourceWidth <= 0 || sourceHeight <= 0 {
		return s
	}
	if sourceHeight <= maxHeight {
		return s
	}
	s.Height = maxHeight
	s.Width = evenWidth(sourceWidth, sourceHeight, maxHeight)
	return s
}

// evenWidth is the width that holds the source's aspect ratio at height h, rounded to the
// nearest even number of pixels and never below MinHeight's own floor of 2 - a frame one
// pixel wide is not one this build would encode, and rounding a very wide-aspect source down
// to zero would be a filter expression ffmpeg refuses.
//
// The arithmetic is integer throughout: a float here would make the target dimension of a
// given source depend on the platform's rounding, and the dimension is recorded on a row
// that outlives the source.
func evenWidth(sourceWidth, sourceHeight, h int) int {
	// Round w = sourceWidth*h/sourceHeight to the nearest multiple of 2, by rounding
	// (w/2) to the nearest whole number: add half the divisor before the integer division.
	half := sourceHeight * 2
	w := 2 * ((sourceWidth*h + half/2) / half)
	if w < MinHeight {
		return MinHeight
	}
	return w
}
