package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// gateFixture is one run of the whole pipeline over a single source with the VMAF
// gate on and its measurement supplied by the seam, so a test can pin ONE property
// of the gate's arithmetic without paying for a second real libvmaf pass - and,
// more importantly, without depending on which numbers real content happens to
// produce. (The numbers themselves are proven against real libvmaf in
// internal/vmaf/chroma_test.go; this file proves what the GATE does with them.)
//
// It returns everything a caller needs to assert the no-loss contract: the source's
// md5 before the run, the recorded terminal row, and the directory.
type gateResult struct {
	dir        string
	src        string
	md5Before  string
	md5After   string
	status     store.Status
	outcome    store.Outcome
	seenReq    vmaf.Request
	scoreCalls int
}

func runGate(t *testing.T, mutate func(*config.Config), score func(vmaf.Request) (vmaf.Result, error)) gateResult {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	res := gateResult{dir: dir, src: src, md5Before: md5f(t, src)}
	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
		c.VmafMinPool = 60
		c.VmafMinChroma = 30
		if mutate != nil {
			mutate(c)
		}
	})
	ts := eng.Store.(*testStore)
	eng.vmafScore = func(_ context.Context, req vmaf.Request) (vmaf.Result, error) {
		res.seenReq = req
		res.scoreCalls++
		return score(req)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	res.md5After = md5f(t, src)

	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path == src {
			res.status, res.outcome = r.Status, r.Outcome
		}
	}
	return res
}

// passing is a measurement that clears every shipped floor.
func passing() vmaf.Result {
	return vmaf.Result{
		HarmonicMean: 98.4, Min: 96.1,
		ChromaMin: 41.2, ChromaMetric: vmaf.ChromaMetricName, PixelFormat: "yuv420p10le",
	}
}

// TestVmafGate_ChromaFloorRejectsAndNamesTheMetric is GATE-4's second acceptance
// criterion at the GATE: an output that clears both luma statistics and fails the
// chroma floor is REJECTED, the source survives byte for byte, and the recorded
// reason names the metric and the floor that rejected it.
//
// The luma numbers are deliberately GOOD. If the fixture failed a luma floor too,
// a rejection would prove nothing about the chroma floor - the old gate would have
// caught it. (That the numbers below are what real chroma-only damage actually
// measures is proven separately, on real libvmaf, in
// vmaf.TestChromaOnlyDamageIsInvisibleToTheLumaGate.)
func TestVmafGate_ChromaFloorRejectsAndNamesTheMetric(t *testing.T) {
	damaged := vmaf.Result{
		HarmonicMean: 98.97, Min: 96.86, // clears min_vmaf=95 and vmaf_min_pool=60
		ChromaMin: 26.21, ChromaMetric: vmaf.ChromaMetricName, PixelFormat: "yuv420p10le",
	}
	got := runGate(t, nil, func(vmaf.Request) (vmaf.Result, error) { return damaged, nil })

	if got.status != store.Failed {
		t.Fatalf("status = %q, want %q - chroma damage that clears both luma floors must still "+
			"REJECT, or the source is deleted for an encode whose colour is wrong", got.status, store.Failed)
	}
	// The no-loss contract: nothing was swapped and nothing was touched.
	if got.md5After != got.md5Before {
		t.Error("the source changed on a chroma rejection - it must be byte-for-byte intact")
	}
	if codecOf(t, envOr("HOLDFAST_FFPROBE", "ffprobe"), got.src) != "h264" {
		t.Error("the source was swapped for the temp on a chroma rejection")
	}
	if n := nTemp(t, got.dir); n != 0 {
		t.Errorf("%d temp file(s) left behind after a chroma rejection", n)
	}
	// The reason names the metric AND the floor. "rejected" alone sends an operator
	// to the logs to find out which of three gates fired.
	for _, want := range []string{vmaf.ChromaMetricName, "vmaf_min_chroma", "26.21", "30.00"} {
		if !strings.Contains(got.outcome.Reason, want) {
			t.Errorf("recorded reason must contain %q; got: %s", want, got.outcome.Reason)
		}
	}
	// And the proof that rejected it is persisted, not thrown away.
	if got.outcome.VmafChroma == nil || *got.outcome.VmafChroma != damaged.ChromaMin {
		t.Errorf("VmafChroma = %v, want the measured %v recorded on the rejected row",
			got.outcome.VmafChroma, damaged.ChromaMin)
	}
	if got.outcome.VmafChromaMetric != vmaf.ChromaMetricName {
		t.Errorf("VmafChromaMetric = %q, want %q", got.outcome.VmafChromaMetric, vmaf.ChromaMetricName)
	}
	if got.outcome.VmafPixFmt != "yuv420p10le" {
		t.Errorf("VmafPixFmt = %q, want the comparison format recorded on the rejected row too",
			got.outcome.VmafPixFmt)
	}
}

// The anti-vacuity control for the case above: the IDENTICAL pipeline with a chroma
// figure ABOVE the floor must SWAP. Without it, "the chroma floor rejects" could be
// satisfied by a gate that rejects everything.
func TestVmafGate_ChromaAboveTheFloorStillSwaps(t *testing.T) {
	got := runGate(t, nil, func(vmaf.Request) (vmaf.Result, error) { return passing(), nil })
	if got.status != store.Done {
		t.Fatalf("status = %q, want %q - a measurement clearing every floor must pass the gate",
			got.status, store.Done)
	}
	if got.outcome.VmafChroma == nil || *got.outcome.VmafChroma != 41.2 {
		t.Errorf("VmafChroma = %v, want 41.2 recorded on the done row", got.outcome.VmafChroma)
	}
	if got.outcome.VmafPixFmt != "yuv420p10le" || got.outcome.VmafChromaMetric != vmaf.ChromaMetricName {
		t.Errorf("done row must record the comparison format and the chroma metric; got %q / %q",
			got.outcome.VmafPixFmt, got.outcome.VmafChromaMetric)
	}
}

// A chroma floor of 0 disables the floor (config-as-code: the operator's stated
// intent wins), so the same damaged measurement passes. This is what makes the
// rejection above attributable to the FLOOR rather than to the metric being present.
func TestVmafGate_ChromaFloorAtZeroDoesNotReject(t *testing.T) {
	damaged := vmaf.Result{HarmonicMean: 98.97, Min: 96.86, ChromaMin: 26.21,
		ChromaMetric: vmaf.ChromaMetricName, PixelFormat: "yuv420p10le"}
	got := runGate(t, func(c *config.Config) { c.VmafMinChroma = 0 },
		func(vmaf.Request) (vmaf.Result, error) { return damaged, nil })
	if got.status != store.Done {
		t.Fatalf("status = %q, want %q - vmaf_min_chroma: 0 must disable the floor, not reject",
			got.status, store.Done)
	}
	// It is disabled, not unmeasured: the figure is still recorded, so an operator
	// running with the floor off can still SEE what the chroma was.
	if got.outcome.VmafChroma == nil || *got.outcome.VmafChroma != 26.21 {
		t.Errorf("VmafChroma = %v, want 26.21 recorded even with the floor disabled", got.outcome.VmafChroma)
	}
}

// TestVmafGate_UnmeasurableMetricIsARejection is GATE-4's fourth acceptance
// criterion: if ANY configured metric cannot be measured, the output is rejected
// rather than scored on the metrics that did report.
//
// The seam returns the error vmaf.Score returns for an incomplete log. The engine
// must not reach into the partial Result for the statistics it CAN see: an output
// graded on two of three metrics is an output whose third property nobody measured,
// and the swap that follows a pass is irreversible.
func TestVmafGate_UnmeasurableMetricIsARejection(t *testing.T) {
	cases := []struct {
		name string
		err  error
		// partial is what the scorer returns ALONGSIDE the error - a Result that
		// would clear every floor if the gate ever read it.
		partial vmaf.Result
	}{
		{
			name: "chroma plane absent from the log",
			err: fmt.Errorf("vmaf: log is missing a pooled statistic (harmonic_mean present=true, " +
				"min present=true, psnr_cb present=false, psnr_cr present=true)"),
			partial: vmaf.Result{HarmonicMean: 99.1, Min: 97.0},
		},
		{
			name:    "libvmaf unavailable",
			err:     vmaf.ErrUnavailable,
			partial: vmaf.Result{},
		},
		{
			name:    "the measurement failed outright",
			err:     errors.New("vmaf: ffmpeg failed: exit status 1"),
			partial: vmaf.Result{HarmonicMean: 99.1, Min: 97.0, ChromaMin: 44.0},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runGate(t, nil, func(vmaf.Request) (vmaf.Result, error) { return tc.partial, tc.err })
			if got.status != store.Failed {
				t.Fatalf("status = %q, want %q - an unmeasurable metric must REJECT, never be "+
					"scored on the metrics that did report", got.status, store.Failed)
			}
			if got.md5After != got.md5Before {
				t.Error("the source changed on an unmeasurable-metric rejection")
			}
			if codecOf(t, envOr("HOLDFAST_FFPROBE", "ffprobe"), got.src) != "h264" {
				t.Error("the source was swapped despite an unmeasurable metric")
			}
			if n := nTemp(t, got.dir); n != 0 {
				t.Errorf("%d temp file(s) left behind after an unmeasurable-metric rejection", n)
			}
			// NOTHING was measured, so nothing is recorded - an empty proof, never a
			// zeroed one, and never the partial figures the scorer happened to carry.
			if got.outcome.VmafMean != nil || got.outcome.VmafMin != nil || got.outcome.VmafChroma != nil {
				t.Errorf("a partially-reported measurement was persisted as if it were a score: %+v",
					got.outcome)
			}
			if got.outcome.VmafPixFmt != "" || got.outcome.VmafChromaMetric != "" {
				t.Errorf("an unmeasured row recorded a comparison format (%q) or metric (%q)",
					got.outcome.VmafPixFmt, got.outcome.VmafChromaMetric)
			}
			if !strings.Contains(got.outcome.Reason, "refusing to accept an unmeasured encode") {
				t.Errorf("the recorded reason must say the encode was unmeasured; got: %s", got.outcome.Reason)
			}
		})
	}
}

// The gate NAMES the comparison format before it measures anything, and hands that
// name to the scorer. This is the engine-side half of the first acceptance criterion
// - internal/vmaf proves the named format is what libvmaf compares; this proves the
// gate names it at all, from the two files' real pixel formats, on the default path
// where they disagree (an 8-bit source and its 10-bit replacement).
func TestVmafGate_NamesTheComparisonFormatFromBothStreams(t *testing.T) {
	got := runGate(t, nil, func(vmaf.Request) (vmaf.Result, error) { return passing(), nil })
	if got.scoreCalls != 1 {
		t.Fatalf("the scorer ran %d times, want 1", got.scoreCalls)
	}
	if got.seenReq.PixelFormat != "yuv420p10le" {
		t.Errorf("the gate asked for comparison format %q, want yuv420p10le - the source is "+
			"8-bit and pixel_format yuv420p10le makes the output 10-bit, so the comparison must "+
			"happen at the deeper of the two", got.seenReq.PixelFormat)
	}
	if got.seenReq.Model == "" {
		t.Error("the gate passed no resolved model to the scorer")
	}
}

// A pair holdfast cannot name a comparison format for is REJECTED, not measured in
// whatever libavfilter would have negotiated. The scorer must never even be reached:
// there is nothing to measure it in.
//
// This drives vmafGate directly with a real 4:1:1 file, because the scan's own
// exotic-pix_fmt guard skips such a source long before the encoder - this is the belt
// on that brace, and the belt has to be provable on its own.
func TestVmafGate_UnnameableComparisonFormatIsARejection(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	exotic := filepath.Join(dir, "exotic.mkv")
	normal := filepath.Join(dir, "normal.mkv")
	// 4:1:1 is named in hdr.DerivePixFmt's doc as a format this tool never guesses at.
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10",
		"-c:v", "ffv1", "-pix_fmt", "yuv411p", "--", exotic)
	mkH264(t, ffmpeg, normal, "8M")
	if got := probe.New(ffmpeg, ffprobe).PixFmt(context.Background(), exotic); got != "yuv411p" {
		t.Fatalf("fixture drifted: exotic pix_fmt = %q, want yuv411p", got)
	}

	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf, c.VmafMinPool, c.VmafMinChroma = 95, 60, 30
	})
	called := false
	eng.vmafScore = func(context.Context, vmaf.Request) (vmaf.Result, error) {
		called = true
		return passing(), nil
	}

	proof, class, err := eng.vmafGate(context.Background(), normal, exotic)
	if err == nil {
		t.Fatal("vmafGate accepted a pair whose comparison format cannot be named")
	}
	if called {
		t.Error("the scorer ran for a pair whose comparison format could not be named")
	}
	// The pair's own pixel formats decide this, before anything is measured: no retry
	// under this configuration reaches a different answer.
	if class != store.FailureDeterministic {
		t.Errorf("class = %q, want %q - the comparison format is derived from the two "+
			"files' own pix_fmts, so a re-encode cannot change the verdict",
			class, store.FailureDeterministic)
	}
	if !strings.Contains(err.Error(), "comparison pixel format") || !strings.Contains(err.Error(), "yuv411p") {
		t.Errorf("the error must say the comparison format could not be named, and name the "+
			"offending pix_fmt; got: %v", err)
	}
	if proof != (vmafProof{}) {
		t.Errorf("an unmeasured pair produced a non-empty proof: %+v", proof)
	}

	// Anti-vacuity: the SAME gate over a nameable pair reaches the scorer and passes.
	called = false
	if _, _, err := eng.vmafGate(context.Background(), normal, normal); err != nil {
		t.Fatalf("the nameable-pair control failed (%v) - the case above proves nothing", err)
	}
	if !called {
		t.Error("the scorer did not run for a nameable pair")
	}
}
