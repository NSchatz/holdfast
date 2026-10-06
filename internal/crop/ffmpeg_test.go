package crop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

// The fixtures: real, tiny, synthetic (lavfi) sources run through the pinned ffmpeg. A crop
// is a guess about pixels, so the proof of each rule is ffmpeg's own reading of a picture
// built to test it - never a log string written by hand.

// tools finds ffmpeg, failing loud rather than skipping: a skipped safety proof is a false
// green (docs/development.md, "a grader that skips is a false green").
func tools(t *testing.T) string {
	t.Helper()
	ffmpeg := os.Getenv("HOLDFAST_FFMPEG")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: %q not found - the crop fixtures require the pinned ffmpeg (set HOLDFAST_FFMPEG): %v", ffmpeg, err)
	}
	return ffmpeg
}

func ff(t *testing.T, ffmpeg string, args ...string) {
	t.Helper()
	full := append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)
	if out, err := exec.Command(ffmpeg, full...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// The fixture frame: a 320x160 picture in a 320x240 frame, 40 px bars top and bottom.
var (
	sd        = Frame{W: 320, H: 240}
	sdPicture = "320:160:0:40"
)

// mkLetterbox8 writes an 8-bit H.264 letterbox: testsrc2 padded with black bars. vf is
// appended to the source chain, so a fixture can draw into the bars.
func mkLetterbox8(t *testing.T, ffmpeg, path string, seconds int, vf string) {
	t.Helper()
	chain := "testsrc2=duration=" + strconv.Itoa(seconds) + ":size=320x160:rate=24,pad=320:240:0:40:black"
	if vf != "" {
		chain += "," + vf
	}
	ff(t, ffmpeg, "-f", "lavfi", "-i", chain, "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "4M",
		"-pix_fmt", "yuv420p", "--", path)
}

// mkLetterbox10PQ writes a 10-bit PQ (HDR10-tagged) HEVC letterbox through libx265: the case
// where black is 64, not 16, and where compression ringing reaches into the bars.
func mkLetterbox10PQ(t *testing.T, ffmpeg, path string) {
	t.Helper()
	ff(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=duration=4:size=320x160:rate=24,format=yuv420p10le,pad=320:240:0:40:black",
		"-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc",
		"-pix_fmt", "yuv420p10le", "--", path)
}

// TestDetect_Letterbox crops the 8-bit letterbox to its picture, and the bars pass the
// blackness check.
func TestDetect_Letterbox(t *testing.T) {
	ffmpeg := tools(t)
	src := filepath.Join(t.TempDir(), "letterbox.mkv")
	mkLetterbox8(t, ffmpeg, src, 4, "")
	ctx := context.Background()
	c := Detect(ctx, ffmpeg, src, 4, sd)
	d := Decide(Inputs{Frame: sd, PixelFormats: []string{"yuv420p", "yuv420p10le"}, Consensus: c})
	if !d.Applied() || d.Rect.String() != sdPicture {
		t.Fatalf("the letterbox decided %+v (consensus %+v), want %s", d, c, sdPicture)
	}
	if c.Valid != Samples {
		t.Errorf("%d of %d samples valid on a clean letterbox", c.Valid, Samples)
	}
	if err := Blackness(ctx, ffmpeg, src, d.Rect, sd, "yuv420p"); err != nil {
		t.Fatalf("the clean bars failed the blackness check: %v", err)
	}
}

// TestDetect_TenBitPQ_FractionalLimitCropsAndAbsoluteDoesNot: on 10-bit, black is 64. The
// fractional limit finds the bars; an absolute limit of 24 sits below black and finds no bars
// at all - which is why the limit is fractional. The bars then pass the blackness check with
// its bounds scaled to 10 bits, ringing and all.
func TestDetect_TenBitPQ_FractionalLimitCropsAndAbsoluteDoesNot(t *testing.T) {
	ffmpeg := tools(t)
	src := filepath.Join(t.TempDir(), "letterbox10.mkv")
	mkLetterbox10PQ(t, ffmpeg, src)
	ctx := context.Background()
	c := Detect(ctx, ffmpeg, src, 4, sd)
	d := Decide(Inputs{Frame: sd, PixelFormats: []string{"yuv420p10le"}, Consensus: c})
	// libx265's ringing lifts the row touching the picture above the limit on some frames, so
	// the loose consensus keeps it and the rectangle is aligned outward around it: the crop
	// keeps every picture row (40..199) and removes nearly all of each 40-row bar. That is the
	// conservative direction, measured here on the pinned build.
	if !d.Applied() || d.Rect.W != 320 || d.Rect.X != 0 || d.Rect.Y > 40 || d.Rect.Y+d.Rect.H < 200 ||
		d.Rect.Y < 36 || d.Rect.Y+d.Rect.H > 204 {
		t.Fatalf("the 10-bit letterbox decided %+v (consensus %+v), want a crop keeping rows 40..199 and "+
			"removing all but at most 4 rows of each bar", d, c)
	}
	abs := detectWith(ctx, ffmpeg, src, 4, sd, "24")
	if da := Decide(Inputs{Frame: sd, PixelFormats: []string{"yuv420p10le"}, Consensus: abs}); da.Applied() {
		t.Fatalf("an ABSOLUTE limit of 24 cropped the 10-bit source to %v: then this case does not show why the "+
			"limit must be fractional", da.Rect)
	} else if da.Reason != ReasonNoBars {
		t.Errorf("the absolute limit's decision is %s, want %s (black 64 reads as picture)", da.Reason, ReasonNoBars)
	}
	if err := Blackness(ctx, ffmpeg, src, d.Rect, sd, "yuv420p10le"); err != nil {
		t.Fatalf("the 10-bit bars failed the blackness check: %v", err)
	}
}

// TestBlackness_CalibrationOnTheTenBitFixture is where GuardPx comes from: on the 10-bit
// fixture, the rows touching the picture carry ringing bright enough to fail the brightest-
// pixel bound, and leaving GuardPx rows out of it is what lets clean bars pass.
func TestBlackness_CalibrationOnTheTenBitFixture(t *testing.T) {
	ffmpeg := tools(t)
	src := filepath.Join(t.TempDir(), "letterbox10.mkv")
	mkLetterbox10PQ(t, ffmpeg, src)
	ctx := context.Background()
	r := Rect{W: 320, H: 160, Y: 40}
	err := blacknessWith(ctx, ffmpeg, src, r, sd, "yuv420p10le", 0)
	var nb *NotBlackError
	if !errors.As(err, &nb) || nb.Stat != "YMAX" {
		t.Fatalf("with no guard rows the ringing next to the picture did not fail the YMAX bound (%v): the "+
			"fixture no longer shows why GuardPx exists - recalibrate", err)
	}
	if err := blacknessWith(ctx, ffmpeg, src, r, sd, "yuv420p10le", GuardPx); err != nil {
		t.Fatalf("with %d guard rows the clean 10-bit bars still fail: %v", GuardPx, err)
	}
}

// TestBlackness_TextInTheBarIsRefused: text burned into the bottom bar for half a second,
// between two samples. Detection does not see it and would crop through it - which is exactly
// why the blackness check reads the whole source - and the check refuses the crop.
func TestBlackness_TextInTheBarIsRefused(t *testing.T) {
	ffmpeg := tools(t)
	src := filepath.Join(t.TempDir(), "subtitled-bar.mkv")
	// Four glyph-sized white boxes in the bottom bar from 1.25 s to 1.75 s: after the first
	// sample (0.95 s, four frames) and before the second (1.85 s).
	glyphs := ""
	for i := 0; i < 4; i++ {
		if i > 0 {
			glyphs += ","
		}
		glyphs += "drawbox=x=" + strconv.Itoa(100+i*24) + ":y=212:w=12:h=14:color=white:t=fill:enable='between(t,1.25,1.75)'"
	}
	mkLetterbox8(t, ffmpeg, src, 10, glyphs)
	ctx := context.Background()
	c := Detect(ctx, ffmpeg, src, 10, sd)
	d := Decide(Inputs{Frame: sd, PixelFormats: []string{"yuv420p"}, Consensus: c})
	if !d.Applied() || d.Rect.String() != sdPicture {
		t.Fatalf("detection decided %+v: the fixture's text was meant to fall between samples, so this case "+
			"would not show the blackness check catching what sampling missed", d)
	}
	err := Blackness(ctx, ffmpeg, src, d.Rect, sd, "yuv420p")
	var nb *NotBlackError
	if !errors.As(err, &nb) {
		t.Fatalf("the blackness check passed a bar with text in it (%v): the crop would cut it away", err)
	}
	if nb.Side != "bottom" {
		t.Errorf("refused on the %s band, the text is in the bottom one", nb.Side)
	}
	// And it is the TEXT that refused it: the same bars without it pass.
	clean := filepath.Join(t.TempDir(), "clean.mkv")
	mkLetterbox8(t, ffmpeg, clean, 10, "")
	if err := Blackness(ctx, ffmpeg, clean, d.Rect, sd, "yuv420p"); err != nil {
		t.Fatalf("the same bars without the text failed too: %v", err)
	}
}

// TestDetect_MixedAspectRatioIsRefused: bars for the first half of the file and none for the
// second. The samples disagree, and there is no crop.
func TestDetect_MixedAspectRatioIsRefused(t *testing.T) {
	ffmpeg := tools(t)
	src := filepath.Join(t.TempDir(), "mixed.mkv")
	ff(t, ffmpeg, "-f", "lavfi", "-i", "testsrc2=duration=10:size=320x240:rate=24,"+
		"drawbox=x=0:y=0:w=320:h=40:color=black:t=fill:enable='lt(t,5)',"+
		"drawbox=x=0:y=200:w=320:h=40:color=black:t=fill:enable='lt(t,5)'",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "4M", "-pix_fmt", "yuv420p", "--", src)
	c := Detect(context.Background(), ffmpeg, src, 10, sd)
	d := Decide(Inputs{Frame: sd, PixelFormats: []string{"yuv420p"}, Consensus: c})
	if d.Applied() || d.Reason != ReasonSamplesDisagree {
		t.Fatalf("the mixed aspect ratio decided %+v (consensus %+v), want refused %s", d, c, ReasonSamplesDisagree)
	}
}

// TestDetect_AllBlackOpeningIsDiscarded: a source black for its first 4 of 10 seconds. The
// samples there report cropdetect's negative crop and are discarded; the rest crop.
func TestDetect_AllBlackOpeningIsDiscarded(t *testing.T) {
	ffmpeg := tools(t)
	src := filepath.Join(t.TempDir(), "fade-in.mkv")
	mkLetterbox8(t, ffmpeg, src, 10, "drawbox=x=0:y=0:w=320:h=240:color=black:t=fill:enable='lt(t,4)'")
	c := Detect(context.Background(), ffmpeg, src, 10, sd)
	if c.Discarded == 0 || c.Valid < MinValidSamples {
		t.Fatalf("consensus %+v: the black opening's samples were meant to be discarded and the rest kept", c)
	}
	d := Decide(Inputs{Frame: sd, PixelFormats: []string{"yuv420p"}, Consensus: c})
	if !d.Applied() || d.Rect.String() != sdPicture {
		t.Fatalf("decided %+v, want %s", d, sdPicture)
	}
}

// TestDetect_FailuresAreNoCrop: a source ffmpeg cannot read, and an unknown duration, are a
// consensus of no crop with a reason - never a box.
func TestDetect_FailuresAreNoCrop(t *testing.T) {
	ffmpeg := tools(t)
	missing := filepath.Join(t.TempDir(), "missing.mkv")
	if c := Detect(context.Background(), ffmpeg, missing, 10, sd); c.Reason != ReasonTooFewSamples || c.Valid != 0 {
		t.Errorf("an unreadable source gave %+v", c)
	}
	if c := Detect(context.Background(), ffmpeg, missing, 0, sd); c.Reason != ReasonDetectFailed {
		t.Errorf("an unknown duration gave %+v", c)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c := Detect(ctx, ffmpeg, missing, 10, sd); c.Reason != ReasonDetectFailed {
		t.Errorf("a cancelled detection gave %+v", c)
	}
	if err := Blackness(context.Background(), ffmpeg, missing, Rect{W: 320, H: 160, Y: 40}, sd, "yuv420p"); err == nil {
		t.Error("the blackness check of an unreadable source passed")
	}
	if err := Blackness(context.Background(), ffmpeg, missing, Rect{W: 320, H: 160, Y: 40}, sd, "rgb24"); err == nil {
		t.Error("the blackness check with an unknown bit depth passed")
	}
	if err := Blackness(context.Background(), ffmpeg, missing, Rect{W: 320, H: 240}, sd, "yuv420p"); err == nil {
		t.Error("a crop removing nothing passed the blackness check")
	}
	if err := Blackness(context.Background(), ffmpeg, missing, Rect{W: 320, H: 241}, sd, "yuv420p"); err == nil {
		t.Error("a crop outside the frame passed the blackness check")
	}
}
