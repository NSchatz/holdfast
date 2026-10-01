package crop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// The detection's sampling. Each is ASSUMED unless cited, and each errs toward NO crop.
const (
	// Samples is how many points of the file cropdetect is run at. ASSUMED: HandBrake's scan
	// takes 10 previews by default (LEAD: its preview count, libhb/scan.c as above), and this
	// build takes the same number.
	Samples = 10
	// FramesPerSample is how many consecutive frames each sample decodes; cropdetect's box is
	// cumulative over them (reset=0: "never reset, and returns the largest area encountered",
	// research-streams-hdr.md section 4.1). ASSUMED.
	FramesPerSample = 4
	// EdgePercent is how much of the file is skipped at each end before the samples are
	// spread, so opening logos and end credits - the frames most often black or boxed
	// differently - are not read as bars. ASSUMED ("the first and last few percent", brief
	// section 12 item 6).
	EdgePercent = 5
	// Limit is cropdetect's black threshold, FRACTIONAL so the filter scales it by the frame's
	// bit depth: an absolute limit of 24 is below 10-bit limited-range black (64) and finds no
	// bars at all, where the fraction 24/255 finds them (TESTED on the pinned ffmpeg,
	// research-streams-hdr.md section 4.2 and verify-streams-hdr.md section 12; re-run here
	// 2026-10-01 by TestDetect_TenBitPQ_FractionalLimitCropsAndAbsoluteDoesNot). The value is
	// cropdetect's own default, `ffmpeg -h filter=cropdetect` on the pinned build: "limit
	// ... (default 0.0941176)".
	Limit = "0.0941176"
)

// SamplePositions are the times, in seconds, a file of this duration is sampled at: Samples
// points spread evenly over the middle of the file, each at the centre of its share of it.
func SamplePositions(durationSec float64) []float64 {
	if durationSec <= 0 {
		return nil
	}
	lo := durationSec * EdgePercent / 100
	span := durationSec - 2*lo
	out := make([]float64, Samples)
	for i := range out {
		out[i] = lo + span*(float64(i)+0.5)/Samples
	}
	return out
}

// cropdetectSpec is the filter every sample runs. round=2 asks cropdetect for even dimensions
// (filters.texi: "Use 2 to get only even dimensions"); skip=0 because its default skips the
// first two frames of every sample, which are most of a sample this short; reset=0 makes the
// box cumulative over the sample.
func cropdetectSpec(limit string) string {
	return "cropdetect=limit=" + limit + ":round=2:skip=0:reset=0"
}

// SampleArgs is the command line one sample runs: a seek to at, FramesPerSample frames of the
// first video stream through cropdetect, and nothing written.
func SampleArgs(source string, at float64, limit string) []string {
	return []string{"-hide_banner", "-nostdin", "-loglevel", "info",
		"-ss", strconv.FormatFloat(at, 'f', 3, 64), "-i", source,
		"-map", "0:" + vmaf.ScoredStream, "-frames:v", strconv.Itoa(FramesPerSample),
		"-vf", cropdetectSpec(limit), "-an", "-sn", "-dn", "-f", "null", "-"}
}

// Detect samples the source with cropdetect and takes the consensus of what it saw. A sample
// ffmpeg could not run, or whose log carries no box, counts as an invalid sample, never as a
// box: it is discarded, and too many of them is a consensus of no crop.
func Detect(ctx context.Context, ffmpeg, source string, durationSec float64, f Frame) Consensus {
	return detectWith(ctx, ffmpeg, source, durationSec, f, Limit)
}

func detectWith(ctx context.Context, ffmpeg, source string, durationSec float64, f Frame, limit string) Consensus {
	at := SamplePositions(durationSec)
	if at == nil {
		return Consensus{Reason: ReasonDetectFailed, Detail: "the source's duration was not established, so " +
			"there is nowhere to spread the samples"}
	}
	boxes := make([]Box, 0, len(at))
	for _, t := range at {
		if ctx.Err() != nil {
			return Consensus{Reason: ReasonDetectFailed, Detail: "detection was interrupted: " + ctx.Err().Error()}
		}
		out, err := exec.CommandContext(ctx, ffmpeg, SampleArgs(source, t, limit)...).CombinedOutput()
		b, ok := ParseBox(string(out))
		if err != nil || !ok {
			// An invalid box: the consensus counts it as discarded.
			b = Box{X1: 1, X2: 0, Y1: 1, Y2: 0}
		}
		boxes = append(boxes, b)
	}
	return Agree(boxes, f)
}

// The blackness check's bounds, in 8-bit code values and scaled to the source's bit depth
// (a 10-bit value is the 8-bit one times 4). Calibrated on this package's own fixtures
// (TestBlackness_CalibrationOnTheTenBitFixture): on a 10-bit PQ letterbox encoded by libx265
// the bars' mean stays at 64-67 (black is 64) while compression ringing next to the picture
// pushes single pixels to 224 in the rows touching it and to 149 four rows away
// (research-streams-hdr.md section 4.4 measured 73-138 on its own 10-bit fixture); 8-bit bars
// read 16. Text burned into a bar reads 235 (940 at 10 bits).
const (
	// AvgBound8 bounds the MEAN luma of every removed band on every frame: cropdetect's own
	// threshold (24 of 255, Limit), so the area removed is black by the same measure that
	// found it. ASSUMED as a gate bound beyond that.
	AvgBound8 = 24
	// MaxBound8 bounds the BRIGHTEST pixel of every removed band away from the picture edge,
	// on every frame. ASSUMED: twice AvgBound8, which clears the calibrated ringing (149 of
	// 192 at 10 bits) and refuses anything brighter than dark grey.
	MaxBound8 = 48
	// GuardPx is how many rows (or columns) of a band, next to the picture, are left out of
	// the brightest-pixel bound: compression ringing lives there (research section 4.4, "or
	// exclude the 2-4 rows adjacent to the picture edge"). They are still in the mean.
	// ASSUMED at the top of that range.
	GuardPx = 4
)

// Band is one rectangle of the frame a crop removes, named by its side.
type Band struct {
	Side string
	Rect Rect
}

// Bands are the rectangles a crop removes from a frame: a full-width band above and below the
// kept picture, and a band left and right of it spanning the kept rows, so no pixel is in two.
func Bands(r Rect, f Frame) []Band {
	e := EdgesOf(r, f)
	var out []Band
	if e.Top > 0 {
		out = append(out, Band{"top", Rect{W: f.W, H: e.Top}})
	}
	if e.Bottom > 0 {
		out = append(out, Band{"bottom", Rect{W: f.W, H: e.Bottom, Y: r.Y + r.H}})
	}
	if e.Left > 0 {
		out = append(out, Band{"left", Rect{W: e.Left, H: r.H, Y: r.Y}})
	}
	if e.Right > 0 {
		out = append(out, Band{"right", Rect{W: e.Right, H: r.H, X: r.X + r.W, Y: r.Y}})
	}
	return out
}

// inner is the part of a band away from the picture by GuardPx, and false where nothing is.
func (b Band) inner(guard int) (Rect, bool) {
	r := b.Rect
	switch b.Side {
	case "top", "bottom":
		if r.H <= guard {
			return Rect{}, false
		}
		if b.Side == "bottom" {
			r.Y += guard
		}
		r.H -= guard
	default:
		if r.W <= guard {
			return Rect{}, false
		}
		if b.Side == "right" {
			r.X += guard
		}
		r.W -= guard
	}
	return r, true
}

// NotBlackError is the blackness check's refusal: the band, the frame and the figure that was
// not black.
type NotBlackError struct {
	Side  string
	Frame int
	Stat  string
	Value float64
	Bound float64
}

func (e *NotBlackError) Error() string {
	return fmt.Sprintf("the %s band the crop removes is not black on frame %d (%s %g > %g)", e.Side, e.Frame,
		e.Stat, e.Value, e.Bound)
}

// BlacknessArgs is the command line the check runs: the whole of the first video stream,
// split once per region, each region cropped out and measured by signalstats, the figures
// printed by a metadata filter named after the region so one log carries them all.
func BlacknessArgs(source string, regions []region) []string {
	var g strings.Builder
	g.WriteString("[0:" + vmaf.ScoredStream + "]split=" + strconv.Itoa(len(regions)))
	for i := range regions {
		g.WriteString("[s" + strconv.Itoa(i) + "]")
	}
	for i, rg := range regions {
		g.WriteString(";[s" + strconv.Itoa(i) + "]" + rg.rect.Spec() +
			",signalstats,metadata@" + rg.name + "=mode=print:key=lavfi.signalstats." + rg.stat +
			"[o" + strconv.Itoa(i) + "]")
	}
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "info", "-i", source, "-filter_complex", g.String()}
	for i := range regions {
		args = append(args, "-map", "[o"+strconv.Itoa(i)+"]", "-f", "null", "-")
	}
	return args
}

// region is one measured rectangle: a band whole (its mean) or its inner part (its brightest
// pixel).
type region struct {
	name, side, stat string
	rect             Rect
	bound            float64
}

// regionsOf are the regions a crop's bands are measured as, at this bit depth.
func regionsOf(r Rect, f Frame, depth, guard int) []region {
	scale := float64(int(1) << (depth - 8))
	var out []region
	for _, b := range Bands(r, f) {
		out = append(out, region{name: b.Side + "_avg", side: b.Side, stat: "YAVG", rect: b.Rect, bound: AvgBound8 * scale})
		if in, ok := b.inner(guard); ok {
			out = append(out, region{name: b.Side + "_max", side: b.Side, stat: "YMAX", rect: in, bound: MaxBound8 * scale})
		}
	}
	return out
}

// Blackness checks that every band a crop of rectangle r removes from the source is black on
// every frame of the source: each band's mean luma within AvgBound8 and its brightest pixel
// away from the picture within MaxBound8, both scaled to the bit depth of pixFmt. nil means
// black; anything else - a band with content, a format whose depth is unknown, a run that
// failed, a frame count that does not add up - is an error, and the crop is refused.
func Blackness(ctx context.Context, ffmpeg, source string, r Rect, f Frame, pixFmt string) error {
	return blacknessWith(ctx, ffmpeg, source, r, f, pixFmt, GuardPx)
}

func blacknessWith(ctx context.Context, ffmpeg, source string, r Rect, f Frame, pixFmt string, guard int) error {
	if !r.Inside(f) {
		return fmt.Errorf("the crop %s is not inside the %dx%d frame", r, f.W, f.H)
	}
	p, ok := hdr.PixelLayout(pixFmt)
	if !ok || p.Depth < 8 {
		return fmt.Errorf("the bit depth of %q could not be read, so the black level is unknown", pixFmt)
	}
	regions := regionsOf(r, f, p.Depth, guard)
	if len(regions) == 0 {
		return fmt.Errorf("the crop %s removes nothing from the %dx%d frame", r, f.W, f.H)
	}
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg, BlacknessArgs(source, regions)...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("the blackness check could not run: %w: %s", err, tail(stderr.String()))
	}
	return judge(stderr.String(), regions)
}

// judge reads every region's figures out of the log and holds each to its bound. Every region
// must report the same, non-zero number of frames: a region with fewer figures than another
// was not measured on every frame, and an unmeasured frame is not a black one.
func judge(log string, regions []region) error {
	byName := map[string]*region{}
	for i := range regions {
		byName[regions[i].name] = &regions[i]
	}
	counts := map[string]int{}
	for _, line := range strings.Split(log, "\n") {
		name, val, ok := metadataLine(line)
		if !ok {
			continue
		}
		rg, known := byName[name]
		if !known {
			continue
		}
		frame := counts[name]
		counts[name]++
		v, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return fmt.Errorf("the blackness check read %q for the %s band on frame %d: %w", val, rg.side, frame, err)
		}
		if v > rg.bound {
			return &NotBlackError{Side: rg.side, Frame: frame, Stat: rg.stat, Value: v, Bound: rg.bound}
		}
	}
	want := -1
	for _, rg := range regions {
		n := counts[rg.name]
		if n == 0 || (want >= 0 && n != want) {
			return errors.New("the blackness check did not measure every band on every frame, so the bands " +
				"are not established as black")
		}
		want = n
	}
	return nil
}

// metadataLine reads one `[metadata@<name> @ 0x...] lavfi.signalstats.<STAT>=<value>` line.
func metadataLine(line string) (name, value string, ok bool) {
	const pre = "[metadata@"
	i := strings.Index(line, pre)
	if i < 0 {
		return "", "", false
	}
	rest := line[i+len(pre):]
	name, rest, ok = strings.Cut(rest, " ")
	if !ok {
		return "", "", false
	}
	j := strings.Index(rest, "lavfi.signalstats.")
	if j < 0 {
		return "", "", false
	}
	_, value, ok = strings.Cut(rest[j:], "=")
	return name, strings.TrimSpace(value), ok
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 400 {
		return "..." + s[len(s)-400:]
	}
	return s
}
