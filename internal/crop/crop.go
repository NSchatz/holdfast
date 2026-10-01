// Package crop is the one place that decides what a `crop` profile value MEANS for one source:
// whether its picture carries black bars, which rectangle of it is the picture, and whether
// this build will cut the bars away (docs/design/crop.md).
//
// It exists for the reason internal/deinterlace and internal/downscale do: the answer has to
// resolve to ONE filter expression, and that one expression has to reach the encoder, the
// perceptual gate's reference and the terminal row. A crop re-derived at each of those sites
// would be three answers to "which pixels were kept", and the gate's whole claim is that it
// measured the encode that is about to replace somebody's file.
//
// A crop is the one picture transformation whose input is GUESSED rather than read: nothing
// in a file says "these rows are bars". So the guess is made the conservative way at every
// step, and every step that cannot be made confidently answers NO CROP, never a smaller
// picture: the file is then encoded uncropped, exactly as it would have been with the key off,
// and the row says why. The decision (Decide) is pure and table-tested; the measurements it is
// made from (Detect, Blackness) run the pinned ffmpeg.
package crop

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/hdr"
)

// The values of the `crop` key.
const (
	// Off: nothing is detected and nothing is cropped. The default (brief I5: a new
	// transformation is off until configured).
	Off = "off"
	// Auto: a source's black bars are detected and, where every check below agrees, cut away.
	Auto = "auto"
)

// Frame is a picture's coded size.
type Frame struct{ W, H int }

// Known reports whether both dimensions were established.
func (f Frame) Known() bool { return f.W > 0 && f.H > 0 }

// Rect is the rectangle of the frame that is KEPT, in ffmpeg's crop spelling: width, height,
// and the offset of its top-left corner.
type Rect struct{ W, H, X, Y int }

// Empty reports whether this is the zero rectangle, which crops nothing.
func (r Rect) Empty() bool { return r == Rect{} }

// String is the rectangle as the row records it: `W:H:X:Y`, cropdetect's own spelling.
func (r Rect) String() string {
	if r.Empty() {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d:%d", r.W, r.H, r.X, r.Y)
}

// Spec is the ffmpeg filter expression that keeps this rectangle, and "" for the zero one.
// `exact=1` is spelled out: without it the crop filter rounds the offsets to the chroma
// subsampling of the frames it is handed (libavfilter/vf_crop.c, "exact" option, ffmpeg -h
// filter=crop on the pinned build, read 2026-10-01), which would make the pixels kept depend on
// the pixel format the chain happens to carry. Every rectangle this package emits is already
// aligned to that subsampling (Decide), so exact=1 changes nothing for it and refuses to let
// anything else quietly move.
func (r Rect) Spec() string {
	if r.Empty() {
		return ""
	}
	return "crop=" + strconv.Itoa(r.W) + ":" + strconv.Itoa(r.H) + ":" + strconv.Itoa(r.X) + ":" +
		strconv.Itoa(r.Y) + ":exact=1"
}

// Inside reports whether the rectangle lies wholly inside the frame and keeps some picture.
func (r Rect) Inside(f Frame) bool {
	return r.W > 0 && r.H > 0 && r.X >= 0 && r.Y >= 0 && r.X+r.W <= f.W && r.Y+r.H <= f.H
}

// Edges are how many pixels are removed from each side of a frame.
type Edges struct{ Left, Right, Top, Bottom int }

// None reports whether nothing is removed on any side.
func (e Edges) None() bool { return e == Edges{} }

// String renders the edges for a reason text.
func (e Edges) String() string {
	return fmt.Sprintf("left %d, right %d, top %d, bottom %d", e.Left, e.Right, e.Top, e.Bottom)
}

// EdgesOf is the edges a rectangle removes from a frame.
func EdgesOf(r Rect, f Frame) Edges {
	return Edges{Left: r.X, Right: f.W - r.X - r.W, Top: r.Y, Bottom: f.H - r.Y - r.H}
}

// Box is what one cropdetect sample saw: the bounding box of every pixel it found brighter
// than its limit, over the frames of the sample, as cropdetect's own x1, x2, y1 and y2 (the
// first and last non-black column and row, inclusive). A sample of frames with nothing above
// the limit reports x1 > x2 or y1 > y2 - the "negative crop" an all-black frame produces.
type Box struct{ X1, X2, Y1, Y2 int }

// The constants a consensus is taken with. Each is either cited or marked ASSUMED, and each
// errs toward NO crop: a missed crop costs a few kilobytes of black, a wrong one cuts picture.
const (
	// ImplausibleFraction: a sample that removes more than 1/ImplausibleFraction of the frame
	// on any side is discarded. HandBrake discards a preview whose crop exceeds a quarter of
	// the frame on a side, because it was "fooled by frames with a lot of black like titles,
	// credits & fade-thru-black" (libhb/scan.c,
	// https://raw.githubusercontent.com/HandBrake/HandBrake/master/libhb/scan.c , as read for
	// research-streams-hdr.md section 4.3 on 2026-09-29; the rule restated here 2026-10-01).
	ImplausibleFraction = 4
	// MinValidSamples is the fewest valid samples a crop may rest on. HandBrake needs more than
	// two previews before it crops at all (libhb/scan.c, as above); three is the smallest count
	// that satisfies it.
	MinValidSamples = 3
	// TolerancePx is how far, in pixels, any valid sample may remove more than the consensus on
	// one side before the samples are said to DISAGREE (a mixed aspect ratio, or a dark scene
	// mistaken for bars), and no crop is made. HandBrake counts a preview more than 9 px from
	// its median as a different aspect ratio (libhb/scan.c, as above); this build uses the same
	// 9 px against the loose consensus. ASSUMED as the right figure for every resolution: it is
	// HandBrake's, not measured here.
	TolerancePx = 9
)

// Consensus is what a set of samples agrees on, or why it agrees on nothing.
type Consensus struct {
	// Edges is the LOOSE consensus: the minimum each side was removed by over the valid
	// samples, so no pixel any valid sample found non-black is ever cropped. It is meaningful
	// only where Reason is "".
	Edges Edges
	// Samples, Valid and Discarded count what the consensus was taken over.
	Samples, Valid, Discarded int
	// Reason is the refusal token where the samples support no crop, and "" where they do.
	Reason string
	// Detail says the same in words, naming the figures.
	Detail string
}

// The refusal tokens a crop decision records. They are a closed, stable vocabulary: the row
// carries them, and an operator keys off them.
const (
	// ReasonNoBars: the consensus removes nothing - the source carries no bars to cut.
	ReasonNoBars = "no-bars"
	// ReasonTooFewSamples: fewer than MinValidSamples samples were valid.
	ReasonTooFewSamples = "too-few-samples"
	// ReasonSamplesDisagree: valid samples differ on a side by more than TolerancePx.
	ReasonSamplesDisagree = "samples-disagree"
	// ReasonUnknownFrame: the source's dimensions were not established.
	ReasonUnknownFrame = "unknown-frame"
	// ReasonUnknownPixelFormat: a pixel format whose chroma subsampling or bit depth could not
	// be read, so neither the alignment nor the black level is known.
	ReasonUnknownPixelFormat = "unknown-pixel-format"
	// ReasonUnaligned: no rectangle that keeps every non-black pixel is aligned to the chroma
	// subsampling.
	ReasonUnaligned = "unaligned"
	// ReasonDolbyVision: the source carries Dolby Vision, whose RPU names its own active area
	// (level 5); a crop would leave that metadata describing bars that are gone (brief I7,
	// proposal P5). Phase 1 refuses every such crop.
	ReasonDolbyVision = "dolby-vision"
	// ReasonBarsNotBlack: the blackness check found content in the area the crop would remove.
	ReasonBarsNotBlack = "bars-not-black"
	// ReasonDetectFailed: detection could not run or could not be read.
	ReasonDetectFailed = "detect-failed"
	// ReasonRemuxOnly: a remux-only root re-encodes nothing, so nothing can be cropped.
	ReasonRemuxOnly = "remux-only"
)

// Reasons is the whole refusal vocabulary, in the order it is documented.
var Reasons = []string{ReasonNoBars, ReasonTooFewSamples, ReasonSamplesDisagree, ReasonUnknownFrame,
	ReasonUnknownPixelFormat, ReasonUnaligned, ReasonDolbyVision, ReasonBarsNotBlack, ReasonDetectFailed,
	ReasonRemuxOnly, ReasonL5Unreadable, ReasonL5FrameRate, ReasonL5Absent, ReasonL5Varies, ReasonL5Odd,
	ReasonL5Disagrees, ReasonL5ZeroingFailed, ReasonL5GateFailed}

// edgesOfBox turns one sample's box into the edges it would remove, and reports whether the
// sample is VALID: a box with nothing in it (the negative crop of all-black frames), a box
// outside the frame, or one removing more than 1/ImplausibleFraction of the frame on a side
// is not.
func edgesOfBox(b Box, f Frame) (Edges, bool) {
	if b.X1 > b.X2 || b.Y1 > b.Y2 || b.X1 < 0 || b.Y1 < 0 || b.X2 >= f.W || b.Y2 >= f.H {
		return Edges{}, false
	}
	e := Edges{Left: b.X1, Right: f.W - 1 - b.X2, Top: b.Y1, Bottom: f.H - 1 - b.Y2}
	maxW, maxH := f.W/ImplausibleFraction, f.H/ImplausibleFraction
	if e.Left > maxW || e.Right > maxW || e.Top > maxH || e.Bottom > maxH {
		return Edges{}, false
	}
	return e, true
}

// Agree takes the loose consensus of a set of samples over a frame: invalid and implausible
// samples are discarded; fewer than MinValidSamples valid ones is too few; the consensus is the
// minimum per side; and any valid sample removing more than TolerancePx beyond it on a side is
// a disagreement. Either refusal is a consensus of NO crop.
func Agree(boxes []Box, f Frame) Consensus {
	c := Consensus{Samples: len(boxes)}
	if !f.Known() {
		c.Reason, c.Detail = ReasonUnknownFrame, "the source's dimensions were not established"
		return c
	}
	var valid []Edges
	for _, b := range boxes {
		if e, ok := edgesOfBox(b, f); ok {
			valid = append(valid, e)
		}
	}
	c.Valid, c.Discarded = len(valid), len(boxes)-len(valid)
	if len(valid) < MinValidSamples {
		c.Reason = ReasonTooFewSamples
		c.Detail = fmt.Sprintf("%d of %d samples were valid, and a crop rests on at least %d", len(valid),
			len(boxes), MinValidSamples)
		return c
	}
	m := valid[0]
	for _, e := range valid[1:] {
		m = Edges{Left: min(m.Left, e.Left), Right: min(m.Right, e.Right), Top: min(m.Top, e.Top),
			Bottom: min(m.Bottom, e.Bottom)}
	}
	for _, e := range valid {
		if e.Left-m.Left > TolerancePx || e.Right-m.Right > TolerancePx || e.Top-m.Top > TolerancePx ||
			e.Bottom-m.Bottom > TolerancePx {
			c.Reason = ReasonSamplesDisagree
			c.Detail = fmt.Sprintf("a sample removes %s where the samples agree on at most %s (tolerance %d px): "+
				"a mixed aspect ratio, or a dark scene read as bars", e, m, TolerancePx)
			return c
		}
	}
	c.Edges = m
	return c
}

// DolbyVision is what the decision knows of a source's Dolby Vision.
//
// Proposal P5 was approved as option (c): a DV source is cropped only to the active area its
// own RPU names (level 5), with L5 zeroed in the pre-pass and gated on the output
// (docs/design/crop.md#dolby-vision). L5 is the reading that decision rests on; where none was
// taken - a DV source the engine does not carry, or one it never read - the crop is refused.
type DolbyVision struct {
	// Present: the source carries Dolby Vision (hdr.ClassFrom answered ClassDV).
	Present bool
	// L5 is what was read of the source's RPU, nil where nothing was (decideDolbyVision).
	L5 *L5Reading
}

// DolbyVisionOf classifies a source from the same three probe strings the engine's DV guard
// reads (hdr.ClassFrom), so the crop decision consults the source itself and does not rely on
// that guard having skipped it.
func DolbyVisionOf(codecTag, flatSideData, colorTransfer string) DolbyVision {
	return DolbyVision{Present: hdr.ClassFrom(codecTag, flatSideData, colorTransfer) == hdr.ClassDV}
}

// Inputs are everything one crop decision reads.
type Inputs struct {
	// Frame is the source's coded size.
	Frame Frame
	// PixelFormats are the formats the cropped picture passes through: the source's (the
	// crop filter runs on the decoded frames) and the plan's output format (the encoder is
	// handed the cropped frames). The rectangle is aligned to the coarsest chroma subsampling
	// among them.
	PixelFormats []string
	// DolbyVision is the source's Dolby Vision, consulted here whatever the engine's guards did.
	DolbyVision DolbyVision
	// Consensus is what the cropdetect samples agreed on.
	Consensus Consensus
}

// Decision is the whole answer for one source: the rectangle kept, or why none is.
type Decision struct {
	// Frame is the source's size the rectangle is within.
	Frame Frame
	// Rect is the rectangle kept, and the zero Rect where nothing is cropped.
	Rect Rect
	// Reason is the token from Reasons where nothing is cropped, and "" where Rect is applied.
	Reason string
	// Detail says why in words.
	Detail string
	// FromL5 is true where Rect is a Dolby Vision source's own L5 active area.
	FromL5 bool
}

// Applied reports whether this decision crops anything.
func (d Decision) Applied() bool { return d.Reason == "" && !d.Rect.Empty() }

// ZeroesL5 reports whether this decision crops a Dolby Vision source to its RPU's own active
// area, so the pre-pass must zero L5 and the L5 gate must hold the output to it.
func (d Decision) ZeroesL5() bool { return d.Applied() && d.FromL5 }

// Refuse is a decision of no crop for a reason.
func Refuse(f Frame, reason, detail string) Decision {
	return Decision{Frame: f, Reason: reason, Detail: detail}
}

// Alignment is the multiple a crop's width and x offset (ax) and its height and y offset (ay)
// must be, for a pixel format's chroma subsampling: 4:2:0 halves chroma in both axes, so both
// must be even; 4:2:2 halves it horizontally, so the width; 4:4:4 does not subsample. ok is
// false for a format whose subsampling cannot be read.
func Alignment(pixFmt string) (ax, ay int, ok bool) {
	p, ok := hdr.PixelLayout(pixFmt)
	if !ok {
		return 0, 0, false
	}
	switch p.Chroma {
	case "420":
		return 2, 2, true
	case "422":
		return 2, 1, true
	case "444":
		return 1, 1, true
	}
	return 0, 0, false
}

// Decide is the crop decision: the inputs to a rectangle, or to a named refusal.
//
// The order is the order of authority. A Dolby Vision source is refused before anything about
// its picture is read (phase 1 of P5). Then the frame, the samples and the alignment, each
// refusing where it cannot answer. The rectangle keeps every pixel the loose consensus kept:
// its edges are rounded OUTWARD to the alignment (an offset down, a far edge up), never inward,
// so aligning it can only keep more.
func Decide(in Inputs) Decision {
	f := in.Frame
	if in.DolbyVision.Present {
		return decideDolbyVision(in)
	}
	if !f.Known() {
		return Refuse(f, ReasonUnknownFrame, "the source's dimensions were not established")
	}
	if in.Consensus.Reason != "" {
		return Refuse(f, in.Consensus.Reason, in.Consensus.Detail)
	}
	ax, ay := 1, 1
	if len(in.PixelFormats) == 0 {
		return Refuse(f, ReasonUnknownPixelFormat, "no pixel format was named to align the crop to")
	}
	for _, pf := range in.PixelFormats {
		x, y, ok := Alignment(pf)
		if !ok {
			return Refuse(f, ReasonUnknownPixelFormat, fmt.Sprintf("the chroma subsampling of %q could not be read, "+
				"so the alignment a crop needs is unknown", pf))
		}
		ax, ay = max(ax, x), max(ay, y)
	}
	e := in.Consensus.Edges
	if e.None() {
		return Refuse(f, ReasonNoBars, "the samples agree that the source carries no bars")
	}
	x0, x1 := alignDown(e.Left, ax), alignUp(f.W-e.Right, ax)
	y0, y1 := alignDown(e.Top, ay), alignUp(f.H-e.Bottom, ay)
	r := Rect{X: x0, Y: y0, W: min(x1, f.W) - x0, H: min(y1, f.H) - y0}
	if r.W%ax != 0 || r.H%ay != 0 || !r.Inside(f) {
		return Refuse(f, ReasonUnaligned, fmt.Sprintf("no rectangle keeping %s of a %dx%d frame is aligned to "+
			"%dx%d chroma", e, f.W, f.H, ax, ay))
	}
	if r == (Rect{W: f.W, H: f.H}) {
		return Refuse(f, ReasonNoBars, "aligned to the chroma subsampling, the bars round away to nothing")
	}
	return Decision{Frame: f, Rect: r}
}

func alignDown(v, a int) int { return v - v%a }

func alignUp(v, a int) int {
	if r := v % a; r != 0 {
		return v + a - r
	}
	return v
}

// Recorded is how a decision reads on a row and in a log line.
func (d Decision) String() string {
	if d.Applied() {
		return d.Rect.String()
	}
	return "not cropped (" + d.Reason + ")"
}

// ParseBox reads the LAST cropdetect line of a log: the cumulative box over every frame the
// sample decoded (reset=0). ok is false where the log carries no cropdetect line.
func ParseBox(log string) (Box, bool) {
	var last string
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, " x1:") && strings.Contains(line, " crop=") {
			last = line
		}
	}
	if last == "" {
		return Box{}, false
	}
	vals := map[string]int{}
	for _, field := range strings.Fields(last) {
		k, v, ok := strings.Cut(field, ":")
		if !ok {
			continue
		}
		switch k {
		case "x1", "x2", "y1", "y2":
			n, err := strconv.Atoi(v)
			if err != nil {
				return Box{}, false
			}
			vals[k] = n
		}
	}
	if len(vals) != 4 {
		return Box{}, false
	}
	return Box{X1: vals["x1"], X2: vals["x2"], Y1: vals["y1"], Y2: vals["y2"]}, true
}
