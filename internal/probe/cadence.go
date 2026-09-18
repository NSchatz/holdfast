package probe

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Cadence classification: what a source's INTERLACING actually is, once something has
// looked at the pixels rather than at the container's field_order tag.
//
// The distinction the pipeline needs is telecine. A telecined source is progressive film
// carried in an interlaced stream by repeating fields on a 3:2 pattern, and deinterlacing
// one is the wrong operation: it interpolates fields that were never a moving picture and
// leaves judder that no perceptual metric flags well - the frames it produces are
// individually plausible, so VMAF scores them highly while the motion is visibly wrong.
// Undoing telecine is inverse telecine (field matching), a different transformation with
// its own gates to argue, and it is deliberately not built here.
//
// So a cadence this detector cannot ESTABLISH is not a licence to transform: it reports
// what it found and the caller skips on anything but a plain interlaced answer.
type CadenceClass string

const (
	// CadenceTelecined: the detector found repeated fields at a rate only a pulldown
	// pattern produces.
	CadenceTelecined CadenceClass = "telecined"
	// CadenceInterlaced: the detector found real interlacing and no pulldown - the one
	// answer a deinterlace is the right operation for.
	CadenceInterlaced CadenceClass = "interlaced"
	// CadenceUndetermined: the detector could not run, could not be read, saw too few
	// frames to judge a five-frame pattern, or disagreed with the container about whether
	// the source is interlaced at all. Every one of them is the same answer to the only
	// question that matters here: nobody established what this source is.
	CadenceUndetermined CadenceClass = "undetermined"
)

// Cadence is one cadence verdict and the evidence behind it, so the line that reports a
// skip can say WHAT was measured rather than only what was concluded. The counts are
// frames of the sample the detector classified, never of the whole file.
type Cadence struct {
	Class CadenceClass
	// Frames is how many frames the detector classified.
	Frames int
	// Repeated is how many of them carried a repeated field, which is what a pulldown
	// pattern leaves behind.
	Repeated int
	// Interlaced is how many of them the detector called interlaced (top-field-first or
	// bottom-field-first) by its multi-frame reading.
	Interlaced int
	// Why names the reason an undetermined verdict is undetermined, and is "" on the other
	// two. A caller that reports "cadence undetermined" without it sends an operator to a
	// file with nothing to look at.
	Why string
}

// Established reports whether the cadence is one the caller may act on at all.
func (c Cadence) Established() bool { return c.Class != CadenceUndetermined }

// Deinterlaceable reports whether this cadence is the one a frame-rate-preserving
// deinterlace is the right operation for: real interlacing, no pulldown.
func (c Cadence) Deinterlaceable() bool { return c.Class == CadenceInterlaced }

// The thresholds, all three measured against real fixtures built with ffmpeg's own
// `interlace` and `telecine` filters (internal/engine's cadence cases drive both):
//
//   - cadenceSampleFrames bounds the decode. A 3:2 pattern repeats every five frames and is
//     stable across a programme, so a few hundred frames answer the question; decoding a
//     two-hour film to re-read the same pattern would put a full decode of the SOURCE in
//     front of every interlaced file, where today the only full decode is of the output.
//   - cadenceMinFrames is the floor below which nothing is concluded. A five-frame pattern
//     cannot be established from a handful of frames, and a detector that answered anyway
//     would be answering from noise.
//   - cadenceTelecineRatio is the share of frames carrying a repeated field that means
//     pulldown. A 3:2 telecine repeats one field in every five, so a clean one measures
//     around 0.4 on this detector (measured: 48 repeated of 120) while a plainly interlaced
//     source measures 0 (measured: 0 of 50). The threshold sits far below the first and far
//     above the second on purpose: it is the fail-safe direction, since a source wrongly
//     called telecined is merely skipped, and one wrongly called interlaced is transformed.
//   - cadenceInterlacedShare is what the detector must see before the container's claim of
//     interlacing is taken as established. It is the DISAGREEMENT test and nothing more:
//     the telecine question is already decided by repeated fields above, so this only has to
//     catch a source whose field_order says interlaced while the pixels show essentially
//     nothing interlaced at all. Measured on a real interlaced fixture: 50 of 50.
const (
	cadenceSampleFrames    = 500
	cadenceMinFrames       = 10
	cadenceTelecineRatio   = 0.10
	cadenceInterlacedShare = 0.20
)

// idetRepeatedRe and idetDetectionRe read ffmpeg's idet summary, which it prints once per
// filter instance when the run ends:
//
//	[Parsed_idet_0 @ 0x…] Repeated Fields: Neither: 72 Top: 24 Bottom: 24
//	[Parsed_idet_0 @ 0x…] Multi frame detection: TFF: 120 BFF: 0 Progressive: 0 Undetermined: 0
//
// Both are matched over the WHOLE output and the richest occurrence wins, because ffmpeg
// prints the block more than once (an initialised-but-unused filter instance reports zeros)
// and a reader that took the first would classify every source off an empty sample.
var (
	idetRepeatedRe = regexp.MustCompile(
		`Repeated Fields:\s*Neither:\s*(\d+)\s*Top:\s*(\d+)\s*Bottom:\s*(\d+)`)
	idetDetectionRe = regexp.MustCompile(
		`Multi frame detection:\s*TFF:\s*(\d+)\s*BFF:\s*(\d+)\s*Progressive:\s*(\d+)\s*Undetermined:\s*(\d+)`)
)

// Cadence classifies f's interlacing by DECODING a bounded sample of it through ffmpeg's
// idet filter, which is the only way to tell telecine from interlacing: both carry the same
// field_order tag, and the difference is a pattern in the pixels.
//
// Every failure is CadenceUndetermined with a reason, never a guess. A missing ffmpeg, a
// file it cannot decode, an output this build cannot read and a sample too small to judge
// are four different reasons and one verdict, because the caller's next move is the same
// for all of them: leave the file alone.
func (p *Prober) Cadence(ctx context.Context, f string) Cadence {
	cmd := exec.CommandContext(ctx, p.FFmpeg, "-hide_banner", "-nostdin", "-v", "info",
		"-i", f, "-map", "0:v:0", "-vf", "idet", "-an", "-sn", "-dn",
		"-frames:v", strconv.Itoa(cadenceSampleFrames), "-f", "null", "-")
	// The summary is on stderr with the rest of ffmpeg's narration, and a non-zero exit is
	// NOT automatically fatal to the reading: what matters is whether the summary is there.
	// A run killed by the frame bound, or one that ended on a trailing error after
	// classifying a full sample, has still measured what this asks.
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return Cadence{Class: CadenceUndetermined, Why: "the cadence probe was cancelled before it answered"}
	}
	text := string(out)

	neither, top, bottom, okRepeated := richestRepeated(text)
	tff, bff, _, _, okDetect := richestDetection(text)
	if !okRepeated || !okDetect {
		why := "ffmpeg's interlace detector printed no summary this build could read"
		if err != nil {
			why += " (ffmpeg exited with an error: " + truncateLine(err.Error(), 120) + ")"
		}
		return Cadence{Class: CadenceUndetermined, Why: why}
	}

	frames := neither + top + bottom
	c := Cadence{Frames: frames, Repeated: top + bottom, Interlaced: tff + bff}
	if frames < cadenceMinFrames {
		c.Class = CadenceUndetermined
		c.Why = "only " + strconv.Itoa(frames) + " frame(s) were classified, which cannot establish a " +
			"five-frame pulldown pattern either way"
		return c
	}
	switch {
	case float64(c.Repeated)/float64(frames) >= cadenceTelecineRatio:
		c.Class = CadenceTelecined
	case float64(c.Interlaced)/float64(frames) >= cadenceInterlacedShare:
		c.Class = CadenceInterlaced
	default:
		c.Class = CadenceUndetermined
		c.Why = "the container reports an interlaced field order and the detector found interlacing in " +
			strconv.Itoa(c.Interlaced) + " of " + strconv.Itoa(frames) + " frame(s), so the two do not agree " +
			"about what this source is"
	}
	return c
}

// richestRepeated returns the repeated-field counts of the summary block that classified
// the most frames.
func richestRepeated(text string) (neither, top, bottom int, ok bool) {
	for _, m := range idetRepeatedRe.FindAllStringSubmatch(text, -1) {
		n, t, b := atoi(m[1]), atoi(m[2]), atoi(m[3])
		if !ok || n+t+b > neither+top+bottom {
			neither, top, bottom, ok = n, t, b, true
		}
	}
	return neither, top, bottom, ok
}

// richestDetection returns the multi-frame detection counts of the block that classified
// the most frames.
func richestDetection(text string) (tff, bff, progressive, undetermined int, ok bool) {
	for _, m := range idetDetectionRe.FindAllStringSubmatch(text, -1) {
		a, b, c, d := atoi(m[1]), atoi(m[2]), atoi(m[3]), atoi(m[4])
		if !ok || a+b+c+d > tff+bff+progressive+undetermined {
			tff, bff, progressive, undetermined, ok = a, b, c, d, true
		}
	}
	return tff, bff, progressive, undetermined, ok
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// truncateLine bounds a message that rides into a log line, on the same rule the rest of
// this program keeps: a record carries shapes and identifiers, never an unbounded payload.
func truncateLine(s string, n int) string {
	s = strings.TrimSpace(strings.SplitN(s, "\n", 2)[0])
	if len(s) > n {
		return s[:n]
	}
	return s
}
