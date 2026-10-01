package crop

import "fmt"

// THE DOLBY VISION CROP (proposal P5, option (c); docs/design/crop.md#dolby-vision). A Dolby
// Vision RPU names the picture's active area itself (level 5, offsets from each edge). A crop
// that leaves L5 alone leaves the replacement describing bars that are gone, so a DV source is
// cropped ONLY to the rectangle its own L5 names, and only where two independent witnesses
// agree: the mastering-side metadata (L5, one identical non-zero rectangle on every frame) and
// the pixels (the cropdetect consensus, within L5TolerancePx on every side). The rectangle is
// then L5's exactly; the engine zeroes L5 in the pre-pass (`dovi_tool -m 0 -c convert`) and an
// L5 gate holds every output frame to 0/0/0/0. Anything else is refused, and the file is
// encoded uncropped with its Dolby Vision carried.

// L5TolerancePx is how far, in pixels, the cropdetect consensus may differ from L5 on a side
// before the two are said to DISAGREE. ASSUMED (proposal P5's 2 px); calibrated on the 10-bit
// fixture, whose libx265 ringing keeps one row of each 40-row bar above cropdetect's limit, so
// the consensus reads 39 where L5 says 40 (TestDecideDolbyVision_TheFixtureConsensusAgreesWithL5).
const L5TolerancePx = 2

// The refusal tokens of a Dolby Vision crop, beside ReasonDolbyVision (no L5 was read).
const (
	// ReasonL5Unreadable: dovi_tool failed, or its L5 export did not parse.
	ReasonL5Unreadable = "dolby-vision-l5-unreadable"
	// ReasonL5FrameRate: the source's frame rate is not one constant rate starting at zero,
	// which the raw stream the L5 zeroing writes is read at.
	ReasonL5FrameRate = "dolby-vision-variable-frame-rate"
	// ReasonL5Absent: some frame carries no L5, or every frame's L5 is zero (no bars named).
	ReasonL5Absent = "dolby-vision-l5-zero-or-absent"
	// ReasonL5Varies: frames carry different L5 rectangles (a shot-varying active area).
	ReasonL5Varies = "dolby-vision-l5-varies"
	// ReasonL5Odd: an L5 offset the chroma subsampling cannot cut at.
	ReasonL5Odd = "dolby-vision-l5-odd-offset"
	// ReasonL5Disagrees: L5 and the cropdetect consensus differ by more than L5TolerancePx on
	// a side, or L5 names no picture inside the frame.
	ReasonL5Disagrees = "dolby-vision-l5-disagrees"
	// ReasonL5ZeroingFailed: the pre-pass that zeroes L5 did not complete.
	ReasonL5ZeroingFailed = "dolby-vision-l5-zeroing-failed"
	// ReasonL5GateFailed: an earlier attempt's cropped output failed the L5 gate, so this
	// one encodes uncropped.
	ReasonL5GateFailed = "dolby-vision-l5-gate-failed"
)

// L5Reading is what was read of a DV source's RPU for the crop decision.
type L5Reading struct {
	// Frames is the source's frame count; L5 the frame-indexed active areas, a frame without
	// an L5 block absent from the map.
	Frames int
	L5     map[int]Edges
	// Failed is why dovi_tool or its export could not be read, "" where it was.
	Failed string
	// FrameRate is why the source's frame rate is not one constant rate from zero, "" where it is.
	FrameRate string
}

// decideDolbyVision is Decide for a Dolby Vision source.
func decideDolbyVision(in Inputs) Decision {
	f, r := in.Frame, in.DolbyVision.L5
	switch {
	case r == nil:
		return Refuse(f, ReasonDolbyVision, "the source carries Dolby Vision and its RPU's active area (level 5) "+
			"was not read, so a crop would leave that metadata describing bars that are no longer there")
	case r.Failed != "":
		return Refuse(f, ReasonL5Unreadable, "the Dolby Vision L5 could not be read: "+r.Failed)
	case r.FrameRate != "":
		return Refuse(f, ReasonL5FrameRate, "the L5 zeroing writes a raw stream read at one constant rate: "+r.FrameRate)
	case !f.Known():
		return Refuse(f, ReasonUnknownFrame, "the source's dimensions were not established")
	}
	l5, reason, detail := oneL5(r)
	if reason != "" {
		return Refuse(f, reason, detail)
	}
	if in.Consensus.Reason != "" {
		return Refuse(f, in.Consensus.Reason, in.Consensus.Detail)
	}
	ax, ay, ok := alignmentOf(in.PixelFormats)
	if !ok {
		return Refuse(f, ReasonUnknownPixelFormat, fmt.Sprintf("the chroma subsampling of %v could not be read", in.PixelFormats))
	}
	if l5.Left%ax != 0 || l5.Right%ax != 0 || l5.Top%ay != 0 || l5.Bottom%ay != 0 {
		return Refuse(f, ReasonL5Odd, fmt.Sprintf("L5 (%s) is not aligned to %dx%d chroma", l5, ax, ay))
	}
	rect := Rect{X: l5.Left, Y: l5.Top, W: f.W - l5.Left - l5.Right, H: f.H - l5.Top - l5.Bottom}
	if !rect.Inside(f) || f.W%ax != 0 || f.H%ay != 0 {
		return Refuse(f, ReasonL5Disagrees, fmt.Sprintf("L5 (%s) names no aligned picture inside the %dx%d frame", l5, f.W, f.H))
	}
	c := in.Consensus.Edges
	if absDiff(c.Left, l5.Left) > L5TolerancePx || absDiff(c.Right, l5.Right) > L5TolerancePx ||
		absDiff(c.Top, l5.Top) > L5TolerancePx || absDiff(c.Bottom, l5.Bottom) > L5TolerancePx {
		return Refuse(f, ReasonL5Disagrees, fmt.Sprintf("L5 names %s and the picture's bars are %s (tolerance %d px)",
			l5, c, L5TolerancePx))
	}
	return Decision{Frame: f, Rect: rect, FromL5: true}
}

// oneL5 is the one L5 rectangle every frame carries, or why there is none.
func oneL5(r *L5Reading) (Edges, string, string) {
	if r.Frames <= 0 || len(r.L5) == 0 {
		return Edges{}, ReasonL5Absent, "the source's RPU carries no L5"
	}
	first, ok := r.L5[0]
	for i := 0; i < r.Frames; i++ {
		e, has := r.L5[i]
		if !has {
			return Edges{}, ReasonL5Absent, fmt.Sprintf("frame %d of %d carries no L5", i, r.Frames)
		}
		if ok && e != first {
			return Edges{}, ReasonL5Varies, fmt.Sprintf("frame 0's L5 is %s and frame %d's is %s", first, i, e)
		}
	}
	if len(r.L5) != r.Frames {
		return Edges{}, ReasonL5Absent, fmt.Sprintf("%d L5 records for %d frames", len(r.L5), r.Frames)
	}
	if first.None() {
		return Edges{}, ReasonL5Absent, "every frame's L5 is zero: the RPU names no bars"
	}
	return first, "", ""
}

// alignmentOf is the coarsest alignment of a set of pixel formats.
func alignmentOf(fmts []string) (int, int, bool) {
	if len(fmts) == 0 {
		return 0, 0, false
	}
	ax, ay := 1, 1
	for _, pf := range fmts {
		x, y, ok := Alignment(pf)
		if !ok {
			return 0, 0, false
		}
		ax, ay = max(ax, x), max(ay, y)
	}
	return ax, ay, true
}

func absDiff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}
