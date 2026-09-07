package vmaf

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestComparisonFormat pins the rule: the richest chroma subsampling of the two and
// the deeper of the two bit depths, always planar YUV little-endian, refusing
// anything it cannot name.
//
// The direction is the whole safety argument. Converting UP invents no difference
// between two frames - both sides are converted identically - while converting DOWN
// averages chroma samples together, and averaging chroma away is precisely the class
// of damage the chroma floor exists to catch. A comparison made at 4:2:0 or at 8 bits
// can therefore HIDE damage that a comparison at the richer format shows. If anyone
// ever "simplifies" this to a fixed yuv420p, these cases red.
func TestComparisonFormat(t *testing.T) {
	cases := []struct {
		name           string
		ref, dist      string
		want           string
		wantUnnameable bool
	}{
		// The default path: pixel_format auto floors output depth at 10, so an 8-bit
		// source routinely meets a 10-bit output. This is the pair the phase exists
		// for, and the deeper format wins.
		{name: "8-bit source, 10-bit output", ref: "yuv420p", dist: "yuv420p10le", want: "yuv420p10le"},
		{name: "10-bit source, 10-bit output", ref: "yuv420p10le", dist: "yuv420p10le", want: "yuv420p10le"},
		{name: "12-bit source keeps its depth", ref: "yuv420p12le", dist: "yuv420p12le", want: "yuv420p12le"},
		{name: "4:2:2 subsampling is preserved", ref: "yuv422p", dist: "yuv422p10le", want: "yuv422p10le"},
		{name: "4:4:4 subsampling is preserved", ref: "yuv444p", dist: "yuv444p10le", want: "yuv444p10le"},
		// The full-range JPEG spelling is the same pixels; range travels separately.
		{name: "yuvj source normalises", ref: "yuvj420p", dist: "yuv420p10le", want: "yuv420p10le"},
		// Both sides matter, and the choice is the one that discards nothing.
		{name: "richer chroma on the output side wins", ref: "yuv420p10le", dist: "yuv444p10le", want: "yuv444p10le"},
		{name: "richer chroma on the source side wins", ref: "yuv444p10le", dist: "yuv420p10le", want: "yuv444p10le"},
		{name: "deeper depth on the source side wins", ref: "yuv420p12le", dist: "yuv420p10le", want: "yuv420p12le"},
		// Big-endian input is a decoded frame either way; the name is little-endian.
		{name: "big-endian input is named little-endian", ref: "yuv420p10be", dist: "yuv420p10le", want: "yuv420p10le"},
		// Refusals. The caller rejects the encode rather than measure it in a format
		// nobody chose.
		{name: "exotic source is unnameable", ref: "rgb24", dist: "yuv420p10le", wantUnnameable: true},
		{name: "exotic output is unnameable", ref: "yuv420p", dist: "gbrp", wantUnnameable: true},
		{name: "a failed probe is unnameable", ref: "", dist: "yuv420p10le", wantUnnameable: true},
		{name: "deeper than 12 bits is unnameable", ref: "yuv420p16le", dist: "yuv420p10le", wantUnnameable: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ComparisonFormat(tc.ref, tc.dist)
			if tc.wantUnnameable {
				if ok {
					t.Fatalf("ComparisonFormat(%q, %q) = %q, ok - an unnameable pair must be "+
						"refused, never guessed", tc.ref, tc.dist, got)
				}
				return
			}
			if !ok {
				t.Fatalf("ComparisonFormat(%q, %q) refused a nameable pair", tc.ref, tc.dist)
			}
			if got != tc.want {
				t.Errorf("ComparisonFormat(%q, %q) = %q, want %q", tc.ref, tc.dist, got, tc.want)
			}
		})
	}
}

// ComparisonFormat is a PURE function of the pair, which is what makes the recorded
// format a fact rather than a guess: it cannot depend on the host, the ffmpeg build,
// the order files were scanned in, or anything libavfilter would have negotiated.
func TestComparisonFormat_IsDeterministic(t *testing.T) {
	first, ok := ComparisonFormat("yuv420p", "yuv420p10le")
	if !ok {
		t.Fatal("ComparisonFormat refused a nameable pair")
	}
	for i := 0; i < 50; i++ {
		got, ok := ComparisonFormat("yuv420p", "yuv420p10le")
		if !ok || got != first {
			t.Fatalf("call %d returned (%q, %t), want (%q, true)", i, got, ok, first)
		}
	}
}

// TestScore_NamedFormatIsWhatLibvmafCompares is the criterion that cannot be graded
// by reading the filtergraph: it asks whether the format holdfast NAMED is the one
// libvmaf actually received, or whether libavfilter quietly negotiated something else
// on the way in.
//
// So it asks FFMPEG, not the string. It runs the real graph under -loglevel verbose
// and reads ffmpeg's own report of the filters it auto-inserted. A conversion between
// the `format` filter and libvmaf means the named format was NOT what was compared -
// which is exactly what happens if the named format is one libvmaf does not accept.
//
// It is mutation-proof: the same assertion run against a deliberately-unsupported
// format (gbrp, which libvmaf refuses) MUST see the auto-inserted scaler. A grader
// that cannot fail is not evidence.
func TestScore_NamedFormatIsWhatLibvmafCompares(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the comparison-format proof: %v", err)
	}
	dist, ref := mismatchedPair(t, bin)

	// Every format ComparisonFormat can produce. Each must reach libvmaf untouched.
	for _, f := range []string{
		"yuv420p", "yuv422p", "yuv444p",
		"yuv420p10le", "yuv422p10le", "yuv444p10le",
		"yuv420p12le", "yuv422p12le", "yuv444p12le",
	} {
		if n := conversionsAfterFormat(t, bin, Request{
			Distorted: dist, Reference: ref, Subsample: 1,
			Model: "version=vmaf_v0.6.1", PixelFormat: f,
		}); n != 0 {
			t.Errorf("comparison format %q: ffmpeg auto-inserted %d conversion(s) between the "+
				"format filter and libvmaf - libvmaf did NOT compare the format holdfast named, so "+
				"the recorded format would be a lie", f, n)
		}
	}

	// The mutation. gbrp is a format libvmaf does not accept, so ffmpeg MUST insert
	// the conversion the assertion above forbids. If this ever reads 0 the check has
	// stopped being able to detect the failure it exists for.
	if n := conversionsAfterFormat(t, bin, Request{
		Distorted: dist, Reference: ref, Subsample: 1,
		Model: "version=vmaf_v0.6.1", PixelFormat: "gbrp",
	}); n == 0 {
		t.Error("the mutation control saw no auto-inserted conversion for a format libvmaf " +
			"cannot accept - this test can no longer detect a negotiated comparison format")
	}
}

// conversionsAfterFormat runs the REAL scoring filtergraph through ffmpeg at verbose
// level and counts the filters libavfilter auto-inserted immediately before libvmaf.
func conversionsAfterFormat(t *testing.T, bin string, req Request) int {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), "vmaf.json")
	out, err := exec.Command(bin, "-hide_banner", "-nostdin", "-loglevel", "verbose", "-y",
		"-i", req.Distorted, "-i", req.Reference, "-lavfi", BuildFilter(req, logPath),
		"-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("ffmpeg failed for pixel format %q: %v\n%s", req.PixelFormat, err, out)
	}
	n := 0
	for _, ln := range strings.Split(string(out), "\n") {
		if strings.Contains(ln, "auto-inserting") && strings.Contains(ln, "Parsed_libvmaf") {
			n++
		}
	}
	return n
}

// TestScore_RecordsAndHonoursTheNamedFormat is the behavioural half: the SAME pair
// scored twice reports the same named format and the same numbers, and the format it
// reports is the one that produced them.
//
// The anti-vacuity control is the second format. If the named format made no
// difference to the measurement, "the recorded format is a fact about the
// measurement" would be an empty claim - so the test also shows that comparing the
// identical pair at a DIFFERENT named format yields DIFFERENT numbers. An 8-bit
// reference against a 10-bit output is the pair from the default path, and
// downconverting it is not the same measurement as upconverting it.
func TestScore_RecordsAndHonoursTheNamedFormat(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the comparison-format proof: %v", err)
	}
	dist, ref := mismatchedPair(t, bin)

	named, ok := ComparisonFormat(probePixFmt(t, ref), probePixFmt(t, dist))
	if !ok {
		t.Fatal("ComparisonFormat refused the fixture pair")
	}
	if named != "yuv420p10le" {
		t.Fatalf("fixture drifted: comparison format is %q, want yuv420p10le", named)
	}

	base := Request{Distorted: dist, Reference: ref, Subsample: 1, Model: "version=vmaf_v0.6.1", PixelFormat: named}
	first, err := Score(context.Background(), bin, base)
	if err != nil {
		t.Fatalf("Score: %v", err)
	}
	second, err := Score(context.Background(), bin, base)
	if err != nil {
		t.Fatalf("Score (second pass): %v", err)
	}

	if first.PixelFormat != named || second.PixelFormat != named {
		t.Errorf("Score reported formats %q and %q, want %q recorded alongside both scores",
			first.PixelFormat, second.PixelFormat, named)
	}
	if first != second {
		t.Errorf("the same pair scored twice in the same named format gave different results:\n"+
			"  %+v\n  %+v\nthe comparison is not deterministic", first, second)
	}

	// The control: a different named format, same pair, different numbers.
	downcast := base
	downcast.PixelFormat = "yuv420p"
	other, err := Score(context.Background(), bin, downcast)
	if err != nil {
		t.Fatalf("Score (8-bit comparison): %v", err)
	}
	if other.PixelFormat != "yuv420p" {
		t.Errorf("Score reported format %q for an explicit yuv420p comparison", other.PixelFormat)
	}
	if other.HarmonicMean == first.HarmonicMean && other.Min == first.Min && other.ChromaMin == first.ChromaMin {
		t.Errorf("comparing at yuv420p and at yuv420p10le produced identical results "+
			"(%+v) - if the named format cannot change the measurement, recording it proves nothing",
			first)
	}
}

// mismatchedPair builds the pair from holdfast's own default path: an 8-bit source
// and the 10-bit output `pixel_format: auto` produces from it. The two DISAGREE,
// which is the precondition the phase's first criterion is written about.
func mismatchedPair(t *testing.T, bin string) (distorted, reference string) {
	t.Helper()
	dir := t.TempDir()
	reference = filepath.Join(dir, "ref.mkv")
	distorted = filepath.Join(dir, "dist.mkv")
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=24",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "12M", "-pix_fmt", "yuv420p", reference)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", reference,
		"-c:v", "libx265", "-crf", "24", "-preset", "veryfast", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", distorted)
	if got := probePixFmt(t, reference); got != "yuv420p" {
		t.Fatalf("reference pix_fmt = %q, want yuv420p", got)
	}
	if got := probePixFmt(t, distorted); got != "yuv420p10le" {
		t.Fatalf("distorted pix_fmt = %q, want yuv420p10le", got)
	}
	return distorted, reference
}

func probePixFmt(t *testing.T, f string) string {
	t.Helper()
	bin := os.Getenv("HOLDFAST_FFPROBE")
	if bin == "" {
		bin = "ffprobe"
	}
	out, err := exec.Command(bin, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt", "-of", "default=nw=1:nk=1", "--", f).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", f, err)
	}
	return strings.TrimSpace(string(out))
}
