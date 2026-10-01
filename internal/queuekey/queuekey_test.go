package queuekey

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// near reports whether got is within tol of want: the worked example prints its intermediate
// figures rounded, and is checked to the rounding it prints.
func near(got, want, tol float64) bool { return math.Abs(got-want) <= tol }

// TestEstimate_ReproducesTheWorkedExample checks the estimator against the worked example in
// docs/design/queue-order.md#savings-per-hour line by line: every intermediate figure to the
// rounding the document prints it at, and the key itself EXACTLY. The case is S0164's AC-5,
// the operator's own (S0164, the operator's report quoted in the umbrella spec).
func TestEstimate_ReproducesTheWorkedExample(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		src                         Source
		output, saved, frames, work float64
		key                         int64
	}{
		{"1920x1080, 21.6 Mbps, 45 min", Source{21_600, 1920, 1080, 2700},
			3_231.58, 6_199_340_260, 64_735.26, 37_749.96, 591_195_994},
		{"1920x1080, 6.1 Mbps, 180 min", Source{6_100, 1920, 1080, 10_800},
			3_231.58, 3_872_361_039, 258_941.06, 150_999.84, 92_321_289},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := Default.Estimate(tc.src, Target{})
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			if !near(e.OutputKbps, tc.output, 0.005) {
				t.Errorf("OutputKbps = %.4f, the document says %.2f", e.OutputKbps, tc.output)
			}
			if !near(e.SavedBytes, tc.saved, 0.5) {
				t.Errorf("SavedBytes = %.2f, the document says %.0f", e.SavedBytes, tc.saved)
			}
			if !near(e.Frames, tc.frames, 0.005) {
				t.Errorf("Frames = %.4f, the document says %.2f", e.Frames, tc.frames)
			}
			if !near(e.WorkSec, tc.work, 0.005) {
				t.Errorf("WorkSec = %.4f, the document says %.2f", e.WorkSec, tc.work)
			}
			if e.BytesPerHour != tc.key {
				t.Errorf("BytesPerHour = %d, the document says %d", e.BytesPerHour, tc.key)
			}
		})
	}

	// The per-pixel work the document prints: 1/5,730,000 + 1/104,000,000 + 1/10,300,000.
	per := 1/Default.EncodePixelsPerSec + 1/Default.DecodePixelsPerSec + 1/Default.VMAFPixelsPerSec
	if !near(per*1e9, 281.2228, 0.00005) {
		t.Errorf("work per pixel = %.7f ns, the document says 281.2228 ns", per*1e9)
	}
	// And the model figures the document states.
	if Default.FrameRate != 24000.0/1001.0 || Default.BitsPerPixel != 0.065 ||
		Default.EncodePixelsPerSec != 5_730_000 || Default.DecodePixelsPerSec != 104_000_000 ||
		Default.VMAFPixelsPerSec != 10_300_000 {
		t.Errorf("Default = %+v, which is not the model the document works its example with", Default)
	}
}

// TestModel_BitsPerPixelReproducesTheOperatorsSaving: the bits-per-pixel figure is derived
// from the operator's report that a 21.6 Mbps 1080p source saves about 85%. The model must
// still say so.
func TestModel_BitsPerPixelReproducesTheOperatorsSaving(t *testing.T) {
	e, err := Default.Estimate(Source{21_600, 1920, 1080, 2700}, Target{})
	if err != nil {
		t.Fatal(err)
	}
	if saving := 1 - e.OutputKbps/21_600; !near(saving, 0.85, 0.005) {
		t.Errorf("the model saves %.4f of a 21.6 Mbps 1080p source, the operator reported about 0.85", saving)
	}
}

// TestEstimate_RanksAsTheCriteriaRequire is AC-3 and AC-4 at the arithmetic: a higher
// bitrate at one picture size ranks higher, and one bitrate over fewer pixels ranks higher.
// The duration cancels: the same source at two lengths earns one key.
func TestEstimate_RanksAsTheCriteriaRequire(t *testing.T) {
	key := func(s Source, tg Target) int64 {
		t.Helper()
		e, err := Default.Estimate(s, tg)
		if err != nil {
			t.Fatal(err)
		}
		return e.BytesPerHour
	}
	if hi, lo := key(Source{20_000, 1920, 1080, 3600}, Target{}), key(Source{8_000, 1920, 1080, 3600}, Target{}); hi <= lo {
		t.Errorf("20 Mbps keyed %d, 8 Mbps %d: the higher bitrate must rank higher", hi, lo)
	}
	if small, big := key(Source{8_000, 1280, 720, 3600}, Target{}), key(Source{8_000, 1920, 1080, 3600}, Target{}); small <= big {
		t.Errorf("720p keyed %d, 1080p %d at one bitrate: fewer pixels must rank higher", small, big)
	}
	short, long := key(Source{8_000, 1920, 1080, 600}, Target{}), key(Source{8_000, 1920, 1080, 6000}, Target{})
	if d := short - long; d < -1 || d > 1 {
		t.Errorf("one source at 10 and 100 minutes keyed %d and %d: the duration must cancel", short, long)
	}
}

// TestEstimate_TheTargetDecidesTheExpectedOutput: a configured bitrate target replaces the
// bits-per-pixel model, a ceiling shrinks the output picture the model is applied to, and a
// remux-only job is expected to save nothing.
func TestEstimate_TheTargetDecidesTheExpectedOutput(t *testing.T) {
	src := Source{12_000, 3840, 2160, 3600}
	for _, tc := range []struct {
		name   string
		target Target
		output float64
	}{
		{"the model at the source's own picture", Target{}, 0.065 * 3840 * 2160 * (24000.0 / 1001.0) / 1000},
		{"a bitrate target", Target{BitrateKbps: 4_000}, 4_000},
		{"a ceiling", Target{Width: 1920, Height: 1080}, 0.065 * 1920 * 1080 * (24000.0 / 1001.0) / 1000},
		{"a ceiling under a bitrate target: the target", Target{BitrateKbps: 4_000, Width: 1920, Height: 1080}, 4_000},
		{"remux only", Target{RemuxOnly: true, BitrateKbps: 4_000}, 12_000},
		{"a half-written ceiling is no ceiling", Target{Width: 1920}, 0.065 * 3840 * 2160 * (24000.0 / 1001.0) / 1000},
	} {
		e, err := Default.Estimate(src, tc.target)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !near(e.OutputKbps, tc.output, 1e-6) {
			t.Errorf("%s: OutputKbps = %f, want %f", tc.name, e.OutputKbps, tc.output)
		}
	}
	e, _ := Default.Estimate(src, Target{RemuxOnly: true})
	if e.SavedBytes != 0 || e.BytesPerHour != 0 {
		t.Errorf("a remux-only job saved %f bytes, key %d: want 0 and 0", e.SavedBytes, e.BytesPerHour)
	}
	// The work is the SOURCE's pixels whatever the output: the encode reads them all.
	a, _ := Default.Estimate(src, Target{})
	b, _ := Default.Estimate(src, Target{Width: 1920, Height: 1080})
	if a.WorkSec != b.WorkSec {
		t.Errorf("a ceiling moved the work from %f to %f s", a.WorkSec, b.WorkSec)
	}
}

// TestEstimate_ANonPositiveSavingKeysZeroAndAPositiveOneAtLeastOne is AC-6 at the arithmetic.
func TestEstimate_ANonPositiveSavingKeysZeroAndAPositiveOneAtLeastOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  Source
		tg   Target
		want func(int64) bool
	}{
		{"below the model", Source{1_000, 1920, 1080, 3600}, Target{}, func(k int64) bool { return k == 0 }},
		{"exactly the target", Source{4_000, 1920, 1080, 3600}, Target{BitrateKbps: 4_000}, func(k int64) bool { return k == 0 }},
		{"one kbit/s over the target", Source{4_001, 1920, 1080, 3600}, Target{BitrateKbps: 4_000}, func(k int64) bool { return k >= 1 }},
	} {
		e, err := Default.Estimate(tc.src, tc.tg)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !tc.want(e.BytesPerHour) {
			t.Errorf("%s: key %d", tc.name, e.BytesPerHour)
		}
	}
	// A saving too small to reach a whole byte per hour is still positive: key 1, not 0.
	slow := Default
	slow.EncodePixelsPerSec = 1
	e, err := slow.Estimate(Source{4_001, 1920, 1080, 3600}, Target{BitrateKbps: 4_000})
	if err != nil {
		t.Fatal(err)
	}
	if e.SavedBytes <= 0 || e.SavedBytes/e.WorkSec*3600 >= 1 || e.BytesPerHour != 1 {
		t.Errorf("a positive saving under one byte per hour keyed %d (saved %f over %f s), want 1",
			e.BytesPerHour, e.SavedBytes, e.WorkSec)
	}
}

// TestEstimate_AnUnestablishedFactIsRefused is AC-7 at the arithmetic: every missing, zero or
// non-finite fact is refused with ErrUnestablished, naming the fact.
func TestEstimate_AnUnestablishedFactIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  Source
		says string
	}{
		{"no bitrate", Source{0, 1920, 1080, 60}, "bitrate"},
		{"a negative bitrate", Source{-1, 1920, 1080, 60}, "bitrate"},
		{"no width", Source{8000, 0, 1080, 60}, "dimensions"},
		{"no height", Source{8000, 1920, 0, 60}, "dimensions"},
		{"no duration", Source{8000, 1920, 1080, 0}, "duration"},
		{"a negative duration", Source{8000, 1920, 1080, -1}, "duration"},
		{"a NaN duration", Source{8000, 1920, 1080, math.NaN()}, "duration"},
		{"an infinite duration", Source{8000, 1920, 1080, math.Inf(1)}, "duration"},
	} {
		_, err := Default.Estimate(tc.src, Target{})
		if !errors.Is(err, ErrUnestablished) {
			t.Errorf("%s: err = %v, want ErrUnestablished", tc.name, err)
			continue
		}
		if !contains(err.Error(), tc.says) {
			t.Errorf("%s: %q does not name the %s", tc.name, err, tc.says)
		}
	}
	if _, err := Default.Estimate(Source{1, 1, 1, 0.001}, Target{}); err != nil {
		t.Errorf("the smallest established source was refused: %v", err)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
