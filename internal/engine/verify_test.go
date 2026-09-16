package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

// TestVmafGate_TheScoredStreamIsRecordedOnPassAndAbsentWhenNothingWasCompared carries
// the stream the comparison was made against along the same path the comparison format
// already travels, and - the half that matters - stops it at the same place.
//
// A job whose gate could not compare anything compared no stream, and the row must say so.
// The temptation here is sharper than it was for the format: the stream is a CONSTANT this
// build always scores, so writing it unconditionally would look like completing the record
// while actually claiming a measurement nobody took. Either input having no video stream,
// ffmpeg refusing the graph, libvmaf missing and a log that came back incomplete all reach
// the recording site the same way - through an empty proof - and all four are driven here.
func TestVmafGate_TheScoredStreamIsRecordedOnPassAndAbsentWhenNothingWasCompared(t *testing.T) {
	// 1. The pass path. A complete measurement records the stream beside the format.
	measured := vmaf.Result{
		HarmonicMean: 98.4, Min: 96.1,
		ChromaMin: 41.2, ChromaMetric: vmaf.ChromaMetricName, PixelFormat: "yuv420p10le",
		Stream: vmaf.ScoredStream,
	}
	got := runGate(t, nil, func(vmaf.Request) (vmaf.Result, error) { return measured, nil })
	if got.status != store.Done {
		t.Fatalf("status = %q, want %q - a measurement clearing every floor must pass the gate",
			got.status, store.Done)
	}
	if got.outcome.VmafStream != vmaf.ScoredStream {
		t.Errorf("a done row records vmaf_stream %q, want %q - which stream was compared is a fact "+
			"about the measurement, and it travels with the score",
			got.outcome.VmafStream, vmaf.ScoredStream)
	}

	// 2. A FLOOR rejection still records it, exactly as it records the format: the
	// numbers that rejected an encode are the ones an operator most wants to see, and
	// they are uninterpretable without the stream they were measured on.
	damaged := vmaf.Result{
		HarmonicMean: 98.97, Min: 96.86, ChromaMin: 26.21,
		ChromaMetric: vmaf.ChromaMetricName, PixelFormat: "yuv420p10le", Stream: vmaf.ScoredStream,
	}
	got = runGate(t, nil, func(vmaf.Request) (vmaf.Result, error) { return damaged, nil })
	if got.status != store.Failed {
		t.Fatalf("status = %q, want %q on a chroma-floor rejection", got.status, store.Failed)
	}
	if got.outcome.VmafStream != vmaf.ScoredStream {
		t.Errorf("a floor-rejected row records vmaf_stream %q, want %q - the proof is carried on the "+
			"reject path too", got.outcome.VmafStream, vmaf.ScoredStream)
	}

	// 3. And every way the comparison can fail to happen at all records NO stream.
	cases := []struct {
		name string
		err  error
		// partial is what the scorer returns ALONGSIDE the error - including, on
		// purpose, a Stream the engine must not reach in and take.
		partial vmaf.Result
	}{
		{
			name:    "an input has no video stream",
			err:     errors.New("vmaf: ffmpeg failed: exit status 1: Stream specifier 'v:0' in filtergraph description matches no streams"),
			partial: vmaf.Result{Stream: vmaf.ScoredStream},
		},
		{
			name:    "ffmpeg refused the graph",
			err:     errors.New("vmaf: ffmpeg failed: exit status 1: Error initializing complex filters"),
			partial: vmaf.Result{Stream: vmaf.ScoredStream},
		},
		{
			name:    "libvmaf unavailable",
			err:     vmaf.ErrUnavailable,
			partial: vmaf.Result{},
		},
		{
			name: "the log came back without every pooled statistic",
			err: errors.New("vmaf: log is missing a pooled statistic (harmonic_mean present=true, " +
				"min present=true, psnr_cb present=false, psnr_cr present=true)"),
			partial: vmaf.Result{HarmonicMean: 99.1, Min: 97.0, PixelFormat: "yuv420p10le", Stream: vmaf.ScoredStream},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runGate(t, nil, func(vmaf.Request) (vmaf.Result, error) { return tc.partial, tc.err })
			if got.status != store.Failed {
				t.Fatalf("status = %q, want %q - an encode whose quality was never measured must be "+
					"REJECTED, never reported as a pass", got.status, store.Failed)
			}
			if got.md5After != got.md5Before {
				t.Error("the source changed on a rejection - it must be byte-for-byte intact")
			}
			if codecOf(t, envOr("HOLDFAST_FFPROBE", "ffprobe"), got.src) != "h264" {
				t.Error("the source was swapped for the temp despite a rejection")
			}
			if n := nTemp(t, got.dir); n != 0 {
				t.Errorf("%d temp file(s) left behind after a rejection", n)
			}
			if got.outcome.VmafStream != "" {
				t.Errorf("a row whose gate compared nothing recorded vmaf_stream %q - absent means "+
					"absent, and a constant specifier is not a measurement", got.outcome.VmafStream)
			}
			if got.outcome.VmafMean != nil || got.outcome.VmafMin != nil || got.outcome.VmafChroma != nil {
				t.Errorf("a row whose gate compared nothing recorded a score: %+v", got.outcome)
			}
			if !strings.Contains(got.outcome.Reason, "refusing to accept an unmeasured encode") {
				t.Errorf("the recorded reason must say the encode was unmeasured; got: %s", got.outcome.Reason)
			}
		})
	}
}

// TestVerify_RejectsAnOutputMissingAVideoStream is the parity gate at its new width,
// driven end to end because the criterion is about what happens to the FILES: an encoder
// that drops a video stream must be rejected, the source must be byte-for-byte intact
// afterwards and the temp must be gone.
//
// The encoder here is a REAL, faithful HEVC encode of the source's first video stream and
// nothing else - right codec, right duration, smaller - so every gate in front of parity
// passes it and the rejection is attributable to parity alone. Before video joined the
// loop this output was accepted and the source was deleted for it.
func TestVerify_RejectsAnOutputMissingAVideoStream(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mp4")
	mkMP4WithCoverArt(t, ffmpeg, ffprobe, src, "8M")
	before := md5f(t, src)

	var encodes int
	enc := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		encodes++
		cmd := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
			"-i", in, "-map", "0:v:0", "-c:v", "libx265", "-preset", "ultrafast",
			"-x265-params", "log-level=error", "-pix_fmt", "yuv420p10le", "--", out)
		if o, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("fixture encode: %v: %s", err, o)
		}
		return nil
	})

	ts := run(t, ffmpeg, ffprobe, d, enc, func(c *config.Config) { c.ContainerExt = "source" })

	if encodes == 0 {
		t.Fatal("the encoder never ran, so nothing was verified and this proves nothing")
	}
	if !ledgerHas(t, ts, store.Failed, "movie.mp4") {
		t.Fatalf("an output missing a video stream was NOT rejected")
	}
	rows, err := ts.List(context.Background(), []store.Status{store.Failed}, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	var reason string
	for _, r := range rows {
		if r.Path == src {
			reason = r.Outcome.Reason
		}
	}
	if !strings.Contains(reason, "stream-count parity failed (type=v") {
		t.Errorf("the recorded reason must name the TYPE that was dropped; got: %s", reason)
	}
	// The no-loss contract, which is the whole reason the gate exists.
	if md5f(t, src) != before {
		t.Error("the source changed after a video-parity rejection - it must be byte-for-byte intact")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("the source was swapped for an output that had lost a video stream")
	}
	if n := nTemp(t, d); n != 0 {
		t.Errorf("%d temp file(s) left behind after a video-parity rejection", n)
	}
}

// TestVerify_RejectsAnOutputWhoseVideoStreamsCannotBeCounted pins the fail-safe half of
// the same gate: an output this build cannot COUNT the video streams of counts as zero,
// which is below any source's count, so it is rejected and the source is kept.
//
// The condition is produced by an ffprobe that refuses exactly one question - the stream
// count - and only for the TEMP, delegating everything else to the real binary. That is
// deliberate: the temp has to reach gate 5 for this to be about gate 5 at all, so it must
// still probe as the right codec, the right duration and smaller, and a wholly broken
// probe would have failed it long before. A half-installed ffprobe, or one being replaced
// under a running scan, is exactly this shape.
func TestVerify_RejectsAnOutputWhoseVideoStreamsCannotBeCounted(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	// "stream=index" with a csv output IS the stream-count probe; the stream-shape probe
	// the source-shape guard runs asks a different question and still gets its answer.
	fake := delegatingFFprobe(t, t.TempDir(), ffprobe, "stream=index", TempMarker)
	ts := run(t, ffmpeg, fake, d, nil, nil)

	if !ledgerHas(t, ts, store.Failed, "movie.mkv") {
		t.Fatalf("an output whose video streams could not be counted was NOT rejected")
	}
	rows, err := ts.List(context.Background(), []store.Status{store.Failed}, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	var reason string
	for _, r := range rows {
		if r.Path == src {
			reason = r.Outcome.Reason
		}
	}
	if !strings.Contains(reason, "stream-count parity failed (type=v in=1 out=0") {
		t.Errorf("an uncountable output must be treated as carrying ZERO video streams and "+
			"rejected on parity; recorded reason was: %s", reason)
	}
	if md5f(t, src) != before {
		t.Error("the source changed on an uncountable-output rejection")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("the source was swapped on an answer nothing could count")
	}
	if n := nTemp(t, d); n != 0 {
		t.Errorf("%d temp file(s) left behind after an uncountable-output rejection", n)
	}

	// Anti-vacuity: the identical source and the identical encode, under a probe that
	// answers, is SWAPPED. Without this the case above would be satisfied by a gate that
	// rejected everything.
	d2 := t.TempDir()
	src2 := filepath.Join(d2, "movie.mkv")
	mkH264(t, ffmpeg, src2, "8M")
	ts2 := run(t, ffmpeg, ffprobe, d2, nil, nil)
	if !ledgerHas(t, ts2, store.Done, "movie.mkv") {
		t.Fatalf("the control run did not transcode, so the uncountable case proves nothing")
	}
}

// TestVerify_EveryRejectionCarriesTheClassOfItsVerdict drives the gate directly over
// crafted pairs and asserts, per rejection, WHICH KIND of failure it is: one whose
// verdict is a pure function of the source, the configuration and the pinned ffmpeg
// build, or one a later attempt could answer differently.
//
// It is driven at the gate rather than through a scan because that is where the class is
// produced. The engine records what this returns; if the wrong verdict were produced
// here, a scan-level test would only ever confirm that the recording site faithfully
// stored the wrong answer.
//
// Every case also asserts the ERROR TEXT it expected, which is what stops the table from
// passing because some OTHER gate rejected the pair first: the gates run in a fixed order
// and a fixture that failed the codec check would otherwise satisfy a size-increase case.
func TestVerify_EveryRejectionCarriesTheClassOfItsVerdict(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	p := func(name string) string { return filepath.Join(dir, name) }

	// The source most cases are measured against: 2s, 240p, h264, small.
	src := p("src.mkv")
	mkH264(t, ffmpeg, src, "300k")

	// A source carrying an audio track, and a video-only HEVC re-encode of it: smaller,
	// right codec, right duration, one track short. Only stream-count parity sees it.
	srcAudio := p("src-audio.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=2",
		"-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M",
		"-pix_fmt", "yuv420p", "-c:a", "aac", "--", srcAudio)
	outNoAudio := p("out-no-audio.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", srcAudio,
		"-map", "0:v:0", "-c:v", "libx265", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", "--", outNoAudio)

	// A source carrying an attached cover picture as a second VIDEO stream, and an
	// HEVC re-encode of it that kept only v:0: smaller, right codec, right duration, one
	// video stream short. Only stream-count parity sees it, and only now that video is in
	// the loop - the type this tool exists to re-encode was the one type not counted.
	srcCover := p("src-cover.mp4")
	mkMP4WithCoverArt(t, ffmpeg, ffprobe, srcCover, "8M")
	outNoCover := p("out-no-cover.mp4")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", srcCover,
		"-map", "0:v:0", "-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error",
		"-pix_fmt", "yuv420p10le", "--", outNoCover)

	// Same duration, right codec, BIGGER than the source (720p lossless against a 240p
	// source, so it is larger whatever x265 does with the bitrate).
	outBig := p("out-big.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=1280x720:rate=10",
		"-c:v", "libx265", "-x265-params", "lossless=1:log-level=error", "-pix_fmt", "yuv420p", "--", outBig)
	if probe.FileSize(outBig) <= probe.FileSize(src) {
		t.Fatal("fixture drifted: the 'bigger' output is not bigger, so the size gate is not what would reject it")
	}

	// Right codec, a quarter of the length: the truncation the duration check exists for.
	outShort := p("out-short.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=0.5:size=320x240:rate=10",
		"-c:v", "libx265", "-x265-params", "log-level=error", "-pix_fmt", "yuv420p", "--", outShort)

	// The wrong codec entirely.
	outH264 := p("out-h264.mkv")
	mkH264(t, ffmpeg, outH264, "100k")

	// A temp that is there and holds nothing.
	outEmpty := p("out-empty.mkv")
	if err := os.WriteFile(outEmpty, nil, 0o644); err != nil {
		t.Fatalf("write the empty temp: %v", err)
	}

	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)

	cases := []struct {
		name     string
		in, tmp  string
		wantText string
		want     store.FailureClass
		why      string
	}{
		{
			name: "the temp is missing or empty", in: src, tmp: outEmpty,
			wantText: "temp missing or empty", want: store.FailureTransient,
			why: "an empty temp is what a full disk or a killed encoder leaves; the next attempt can differ",
		},
		{
			name: "the output is not the target codec", in: src, tmp: outH264,
			wantText: "output codec is", want: store.FailureDeterministic,
			why: "the configured encoder produces the codec it produces",
		},
		{
			name: "the output is truncated", in: src, tmp: outShort,
			wantText: "duration parity failed", want: store.FailureDeterministic,
			why: "how long this source is, is a property of the file",
		},
		{
			name: "the output is not smaller", in: src, tmp: outBig,
			wantText: "size-increase reject", want: store.FailureDeterministic,
			why: "the same source at the same settings compresses to the same size",
		},
		{
			name: "a track was dropped", in: srcAudio, tmp: outNoAudio,
			wantText: "intended-stream check failed: the output is missing audio", want: store.FailureDeterministic,
			why: "which streams this source has, and which this build maps, is fixed",
		},
		{
			name: "a video stream was dropped", in: srcCover, tmp: outNoCover,
			wantText: "intended-stream check failed: the output is missing video", want: store.FailureDeterministic,
			why: "which video streams this source has, and which this build maps, is fixed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The target codec is resolved exactly as ProcessFile resolves it, from
			// the job's own effective settings, so this table keeps asking the gate
			// the question the engine asks it rather than a run-global one.
			top := eng.Cfg.TopLevelProfile()
			target := targetCodecFor(eng.Cfg.TranscodeIn(top, tc.in).Encoder)
			_, class, err := eng.verifyOutput(context.Background(), tc.in, tc.tmp, top, target,
				planFor(t, eng, tc.in, top))
			if err == nil {
				t.Fatalf("the gate ACCEPTED this pair; the case proves nothing about the class of a rejection")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("a different gate rejected this pair: got %q, want one containing %q", err, tc.wantText)
			}
			if class != tc.want {
				t.Errorf("class = %q, want %q - %s", class, tc.want, tc.why)
			}
		})
	}

	// Packet-count parity is the length check's OTHER half and only runs when neither
	// file reports a duration, which no container-carried fixture above can produce. It
	// is driven at lengthParity, where that verdict is decided.
	t.Run("the output is truncated and neither file reports a duration", func(t *testing.T) {
		rawIn, rawOut := p("raw-in.h264"), p("raw-out.h264")
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "testsrc2=duration=2:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
			"-b:v", "8M", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "--", rawIn)
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "testsrc2=duration=2:size=320x240:rate=10", "-frames:v", "3", "-c:v", "libx264",
			"-preset", "ultrafast", "-bsf:v", "h264_mp4toannexb", "-f", "h264", "--", rawOut)
		class, err := eng.lengthParity(context.Background(), rawIn, rawOut)
		if err == nil {
			t.Fatal("lengthParity accepted a 3-frame encode of a 20-frame source")
		}
		if !strings.Contains(err.Error(), "packet-count parity failed") {
			t.Fatalf("a different rejection: %v", err)
		}
		if class != store.FailureDeterministic {
			t.Errorf("class = %q, want %q - the source's packet count is a property of the file",
				class, store.FailureDeterministic)
		}
	})

	// The anti-vacuity control. Every case above asserts a REJECTION carries a class, and
	// all of them would be satisfied by a gate that rejected everything and called it
	// final. A pair that PASSES must still pass, and must carry no class at all.
	t.Run("a passing pair is not classified as anything", func(t *testing.T) {
		good := p("out-good.mkv")
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
			"-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
			"-pix_fmt", "yuv420p10le", "--", good)
		if probe.FileSize(good) >= probe.FileSize(src) {
			t.Skipf("the control fixture did not come out smaller (%d >= %d), so it cannot pass the size gate",
				probe.FileSize(good), probe.FileSize(src))
		}
		top := eng.Cfg.TopLevelProfile()
		_, class, err := eng.verifyOutput(context.Background(), src, good, top,
			targetCodecFor(eng.Cfg.TranscodeIn(top, src).Encoder), planFor(t, eng, src, top))
		if err != nil {
			t.Fatalf("the gate rejected a faithful smaller HEVC encode: %v", err)
		}
		if class != "" {
			t.Errorf("a passing gate produced class %q; the class belongs to a rejection and "+
				"nothing else reads it", class)
		}
	})
}

// The VMAF gate's rejections split, and the split is the point: the three FLOORS are
// verdicts about two files that do not change between attempts, while a missing libvmaf
// or a measurement that failed are conditions of the installation and the run. Driving
// vmafGate through the score seam pins each without a second real libvmaf pass.
func TestVmafGate_FloorsAreFinalAndAnUnmeasurableRunIsNot(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	mkH264(t, ffmpeg, src, "8M")

	cases := []struct {
		name     string
		result   vmaf.Result
		err      error
		wantText string
		want     store.FailureClass
		why      string
	}{
		{
			name:     "the average is below the threshold",
			result:   vmaf.Result{HarmonicMean: 80.0, Min: 75.0, ChromaMin: 41.2, ChromaMetric: vmaf.ChromaMetricName, PixelFormat: "yuv420p10le"},
			wantText: "VMAF below threshold", want: store.FailureDeterministic,
			why: "the same two files scored by the same model produce the same number",
		},
		{
			name:     "the worst frame is below the floor",
			result:   vmaf.Result{HarmonicMean: 97.5, Min: 43.0, ChromaMin: 41.2, ChromaMetric: vmaf.ChromaMetricName, PixelFormat: "yuv420p10le"},
			wantText: "VMAF worst-frame below floor", want: store.FailureDeterministic,
			why: "the frame that collapsed collapses again on the same encode",
		},
		{
			name:     "the chroma planes are below the floor",
			result:   vmaf.Result{HarmonicMean: 98.97, Min: 96.86, ChromaMin: 26.21, ChromaMetric: vmaf.ChromaMetricName, PixelFormat: "yuv420p10le"},
			wantText: "chroma below floor", want: store.FailureDeterministic,
			why: "the colour damage is in the encode this configuration produces",
		},
		{
			name:     "libvmaf is not in this ffmpeg build",
			err:      vmaf.ErrUnavailable,
			wantText: "libvmaf is not available", want: store.FailureTransient,
			why: "an operator who installs a libvmaf-capable ffmpeg has changed the thing that rejected",
		},
		{
			name:     "the measurement itself failed",
			err:      errors.New("vmaf: ffmpeg failed: exit status 1"),
			wantText: "VMAF measurement failed", want: store.FailureTransient,
			why: "a measurement that fell over says nothing about the encode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, func(c *config.Config) {
				c.VmafEnable = boolPtr(true)
				c.MinVmaf, c.VmafMinPool, c.VmafMinChroma = 95, 60, 30
			})
			eng.vmafScore = func(context.Context, vmaf.Request) (vmaf.Result, error) {
				if tc.err != nil {
					return tc.result, tc.err
				}
				return tc.result, nil
			}
			_, class, err := eng.vmafGate(context.Background(), src, src, eng.Cfg.TopLevelProfile())
			if err == nil {
				t.Fatal("the gate accepted this measurement; the case proves nothing about a rejection")
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("a different rejection: got %q, want one containing %q", err, tc.wantText)
			}
			if class != tc.want {
				t.Errorf("class = %q, want %q - %s", class, tc.want, tc.why)
			}
		})
	}

	// The anti-vacuity control: a measurement clearing every floor is not a rejection at
	// all, so nothing above is satisfied by a gate that refuses everything.
	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, func(c *config.Config) {
		c.VmafEnable = boolPtr(true)
		c.MinVmaf, c.VmafMinPool, c.VmafMinChroma = 95, 60, 30
	})
	eng.vmafScore = func(context.Context, vmaf.Request) (vmaf.Result, error) { return passing(), nil }
	if _, class, err := eng.vmafGate(context.Background(), src, src, eng.Cfg.TopLevelProfile()); err != nil || class != "" {
		t.Errorf("a passing measurement produced err=%v class=%q, want no rejection and no class", err, class)
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

	proof, class, err := eng.vmafGate(context.Background(), normal, exotic, eng.Cfg.TopLevelProfile())
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
	if _, _, err := eng.vmafGate(context.Background(), normal, normal, eng.Cfg.TopLevelProfile()); err != nil {
		t.Fatalf("the nameable-pair control failed (%v) - the case above proves nothing", err)
	}
	if !called {
		t.Error("the scorer did not run for a nameable pair")
	}
}

// --- the intended-stream gate (S0088) ----------------------------------------
//
// The gate these cases grade REPLACES the per-type stream count that stood between a
// silently-dropped track and the deletion of the source. So each of them is written to red
// against that count as well as against no gate at all: an output whose per-type counts
// MATCH the source's is the shape a count cannot see, and it is the shape used here.

// TestVerify_RejectsAnOutputMissingAnIntendedStream is [AC-7]: an output missing a stream
// the intended map names is rejected and the source is kept.
//
// The fixture is deliberately one TODAY'S CHECK ACCEPTS. The source carries an English and
// a Japanese audio track; the output carries the English one TWICE. Per-type counts are
// identical on both sides, so a gate comparing counts passes it while the Japanese track is
// gone for ever - and the test asserts that equality itself, so the case cannot quietly
// stop being the one it claims to be.
func TestVerify_RejectsAnOutputMissingAnIntendedStream(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"), audioStream("jpn"))

	// The English track, twice: the counts match, the content does not.
	out := filepath.Join(dir, "out.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-map", "0:v", "-map", "0:a:0", "-map", "0:a:0",
		"-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p10le", "-c:a", "copy", "--", out)

	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	top := eng.Cfg.TopLevelProfile()

	// The premise, asserted rather than assumed: a per-type count cannot tell these apart.
	in, got := countByType(streamsOf(t, eng, src)), countByType(streamsOf(t, eng, out))
	for _, typ := range []string{"video", "audio", "subtitle", "attachment"} {
		if got[typ] < in[typ] {
			t.Fatalf("the fixture drifted: the output has %d %s stream(s) against the source's %d, "+
				"so today's count-based check would already reject it and this case would prove "+
				"nothing about the intended map", got[typ], typ, in[typ])
		}
	}

	_, class, err := eng.verifyOutput(context.Background(), src, out, top,
		targetCodecFor(eng.Cfg.TranscodeIn(top, src).Encoder), planFor(t, eng, src, top))
	if err == nil {
		t.Fatal("the gate ACCEPTED an output that lost the Japanese track: the counts matched, the " +
			"streams did not, and accepting it deletes a source carrying a track the replacement " +
			"does not have")
	}
	if !strings.Contains(err.Error(), "is missing audio (jpn)") {
		t.Fatalf("the rejection must name what is missing; got: %v", err)
	}
	// It names what it counted on BOTH sides: a rejection an operator cannot read is one
	// they cannot act on.
	for _, want := range []string{"Intended:", "Output:", "audio (eng)", "audio (jpn)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the rejection does not name %q on both sides; got: %v", want, err)
		}
	}
	if class != store.FailureDeterministic {
		t.Errorf("class = %q, want %q - which streams this source carries and which this job "+
			"intended are both fixed", class, store.FailureDeterministic)
	}
}

// TestVerify_RejectsAnOutputCarryingAStreamTheMapDoesNotIntend is [AC-7]'s other half, and
// the one that catches a SELECTION THAT DID NOT APPLY.
//
// It is also invisible to a per-type count, and worse than invisible: the count's rule is
// "not fewer than the source", so an output carrying every stream is exactly what it is
// looking for. A build whose selection silently did nothing would pass every gate, and the
// operator would find out when they looked at the file.
func TestVerify_RejectsAnOutputCarryingAStreamTheMapDoesNotIntend(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"), audioStream("jpn"))

	// Everything carried: the selection did not apply.
	out := filepath.Join(dir, "out.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-map", "0", "-map", "-0:d?",
		"-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p10le", "-c:a", "copy", "--", out)

	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	prof := selectionProfile(t, "    audio_languages: [eng]\n")
	plan := planFor(t, eng, src, prof)
	if len(plan.Dropped()) != 1 {
		t.Fatalf("the plan dropped %v, want the jpn track: the case needs a selection to have "+
			"failed to apply", plan.Dropped())
	}

	// The premise: today's count reads this output as carrying everything the source had,
	// which is precisely what it is looking for.
	in, got := countByType(streamsOf(t, eng, src)), countByType(streamsOf(t, eng, out))
	if got["audio"] < in["audio"] {
		t.Fatalf("the fixture drifted: the output carries fewer audio streams (%d) than the source "+
			"(%d), so today's check would reject it too", got["audio"], in["audio"])
	}

	_, class, err := eng.verifyOutput(context.Background(), src, out, prof,
		targetCodecFor(eng.Cfg.TranscodeIn(prof, src).Encoder), plan)
	if err == nil {
		t.Fatal("the gate ACCEPTED an output carrying a stream the map did not intend: a selection " +
			"that silently did not apply has no other check in front of it")
	}
	if !strings.Contains(err.Error(), "carries audio (jpn) this job did not intend") {
		t.Fatalf("the rejection must name the unintended stream; got: %v", err)
	}
	if class != store.FailureDeterministic {
		t.Errorf("class = %q, want %q", class, store.FailureDeterministic)
	}
}

// TestIntendedMap_CarriesEveryVideoStreamIncludingAttachedPictures is [AC-8]: with nothing
// selected away the intended map is every source stream but data - VIDEO INCLUDED, and
// including an attached picture - so the multi-video coverage the parity gate already had
// is carried forward by this re-expression rather than lost to it.
func TestIntendedMap_CarriesEveryVideoStreamIncludingAttachedPictures(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mp4")
	mkMP4WithCoverArt(t, ffmpeg, ffprobe, src, "3M")

	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	top := eng.Cfg.TopLevelProfile()
	plan := planFor(t, eng, src, top)

	video := 0
	pictures := 0
	for _, s := range plan.Intended() {
		if s.Type == probe.TypeVideo {
			video++
			if s.AttachedPicture {
				pictures++
			}
		}
	}
	if video != 2 || pictures != 1 {
		t.Fatalf("the intended map carries %d video stream(s), %d of them attached pictures; want "+
			"2 and 1 - the cover art is a video stream and dropping it from the map would take the "+
			"video coverage out of the gate", video, pictures)
	}
	if len(plan.Dropped()) != 0 {
		t.Fatalf("with nothing configured the plan dropped %v, want nothing", plan.Dropped())
	}
	// And the argv still pins the picture back to copy, so the map that carries it does not
	// hand it to the encoder.
	if idx := plan.AttachedPictureIndexes(); len(idx) != 1 || idx[0] != 1 {
		t.Fatalf("the plan pins %v back to copy, want [1] - the attached picture is v:1 of the "+
			"streams this output carries", idx)
	}

	// An output that dropped the cover art is REJECTED, which is the coverage itself rather
	// than a statement about it.
	out := filepath.Join(dir, "out.mp4")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-map", "0:v:0", "-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p10le", "-tag:v", "hvc1", "--", out)
	if _, _, err := eng.verifyOutput(context.Background(), src, out, top,
		targetCodecFor(eng.Cfg.TranscodeIn(top, src).Encoder), plan); err == nil {
		t.Fatal("the gate ACCEPTED an output that dropped the attached picture")
	} else if !strings.Contains(err.Error(), "missing video") {
		t.Fatalf("the rejection must name the missing video stream; got: %v", err)
	}
}

// TestVerify_RejectsAnOutputWhoseStreamsCannotBeEnumerated is [AC-9]'s output half: an
// output whose streams ffprobe will not answer about is rejected and the source is kept. An
// unknown shape is never read as the common one.
func TestVerify_RejectsAnOutputWhoseStreamsCannotBeEnumerated(t *testing.T) {
	ffmpeg, realFFprobe := tools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mkv")
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"))
	out := filepath.Join(dir, "out.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-map", "0", "-map", "-0:d?",
		"-c", "copy", "-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p10le", "--", out)

	// The map is derived with a WORKING probe, so what is blind is only the output's shape.
	real := buildEngine(t, ffmpeg, realFFprobe, dir, nil, nil)
	top := real.Cfg.TopLevelProfile()
	plan := planFor(t, real, src, top)

	blind := buildEngine(t, ffmpeg, blindToStreamList(t, dir, realFFprobe, "out.mkv"), dir, nil, nil)
	_, class, err := blind.verifyOutput(context.Background(), src, out, top,
		targetCodecFor(blind.Cfg.TranscodeIn(top, src).Encoder), plan)
	if err == nil {
		t.Fatal("the gate ACCEPTED an output it could not enumerate: nothing established that the " +
			"replacement carries the streams this job intended, and the source would be deleted")
	}
	if !strings.Contains(err.Error(), "could not be enumerated") {
		t.Fatalf("the rejection must say the output's streams could not be enumerated; got: %v", err)
	}
	if class != store.FailureDeterministic {
		t.Errorf("class = %q, want %q", class, store.FailureDeterministic)
	}

	// Anti-vacuity: the same pair through a WORKING probe passes, so what rejected it above
	// is the blindness and not the fixture.
	if _, _, err := real.verifyOutput(context.Background(), src, out, top,
		targetCodecFor(real.Cfg.TranscodeIn(top, src).Encoder), plan); err != nil {
		t.Fatalf("the control pair was rejected by a working probe (%v), so the case above proves "+
			"nothing about enumeration", err)
	}
}

// TestProcessFile_SkipsASourceWhoseStreamsCannotBeEnumerated is [AC-9]'s source half: a
// source whose streams cannot be enumerated is SKIPPED with a recorded reason and nothing is
// encoded. The intended map is what the argv is built from and what the gate is checked
// against, so a source it cannot be derived from is a file this pass must not encode.
func TestProcessFile_SkipsASourceWhoseStreamsCannotBeEnumerated(t *testing.T) {
	ffmpeg, realFFprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"))
	before := md5f(t, src)

	blind := blindToStreamList(t, t.TempDir(), realFFprobe, "")
	run := runSelection(t, ffmpeg, blind, selectionCfg(t, roots[0], ""), nil)

	row := run.rowFor(t, src)
	if row.Status != store.Skipped {
		t.Fatalf("status = %q, want %q: a source whose stream shape is unknown must not be "+
			"encoded on a guess", row.Status, store.Skipped)
	}
	if row.Outcome.Reason != SkipUnreadableStreamList {
		t.Fatalf("reason = %q, want %q: an operator has to learn WHICH guard held the file",
			row.Outcome.Reason, SkipUnreadableStreamList)
	}
	if md5f(t, src) != before {
		t.Fatal("the source changed on a skip")
	}
	if n := nTemp(t, roots[0]); n != 0 {
		t.Errorf("%d temp file(s) left behind: nothing may be encoded on this path", n)
	}
	if *run.score != 0 {
		t.Errorf("the VMAF gate measured %d time(s) on a file nothing encoded", *run.score)
	}
}

// TestIntendedMap_TheArgvAndTheGateReadOneDerivation is [AC-6]: the map the encode's argv is
// built from and the map the gate checks against are ONE derivation.
//
// The assertion is on IDENTITY and not on content, which is the whole of why this case can
// fail. Two independently-derived maps agree on any fixture anybody writes - that is what
// makes the defect invisible - so a test comparing what they contain would pass against
// exactly the build this criterion exists to refuse. The sub-case below proves the
// comparison says NO to a second derivation of the same source under the same profile,
// which is what a build that derived its own map in either place would produce.
func TestIntendedMap_TheArgvAndTheGateReadOneDerivation(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"), audioStream("jpn"))

	run := runSelection(t, ffmpeg, ffprobe,
		selectionCfg(t, roots[0], "    audio_languages: [eng]\n"), nil)
	run.doneRow(t, src)

	encoded := run.plans.only(t, planStageEncode)
	checked := run.plans.only(t, planStageVerify)
	if !SameDerivation(encoded, checked) {
		t.Fatalf("the argv was built from derivation %d and the gate checked derivation %d: two "+
			"derivations are two answers about whether a track was lost, and the source is deleted "+
			"on the strength of one of them", encoded.ID(), checked.ID())
	}

	t.Run("a second derivation of the same file is caught", func(t *testing.T) {
		// What a build that derived its own map in either place would hand over: the same
		// file, the same profile, the same answer - and a different derivation. Both are
		// taken now, over the file as it stands, so the pair is identical in content by
		// construction and the only thing that can tell them apart is identity.
		prof := selectionProfile(t, "    audio_languages: [eng]\n")
		first := planFor(t, run.eng, src, prof)
		second := planFor(t, run.eng, src, prof)
		if len(first.Intended()) != len(second.Intended()) || len(first.Dropped()) != len(second.Dropped()) {
			t.Fatalf("two derivations of one file disagreed about the streams (%d/%d against %d/%d), "+
				"so this sub-case would pass for the wrong reason",
				len(first.Intended()), len(first.Dropped()), len(second.Intended()), len(second.Dropped()))
		}
		if SameDerivation(first, second) {
			t.Fatal("a SECOND, independently-derived map that AGREES in content was reported as the " +
				"same derivation: the check above would pass against two derivations, which is " +
				"exactly the build this criterion exists to refuse")
		}
	})
}

// --- remux-only (S0088) -------------------------------------------------------

// TestRemuxOnly_SkipsTheVmafGateAndRecordsWhy is [AC-13]: a remux-only output has its video
// established identical to the source's FIRST, and only then is the perceptual gate skipped
// - with the reason on the row and NO VMAF figure of any kind.
//
// The gate is ENABLED in this configuration, and the measurement is supplied through the
// engine's own seam. So "the scorer was never called" is evidence that the gate was SKIPPED,
// rather than evidence that it was switched off.
func TestRemuxOnly_SkipsTheVmafGateAndRecordsWhy(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	// Two audio tracks and one dropped, so the remux is strictly smaller and clears the
	// size gate on its own merits rather than by having it waived.
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"), audioStream("jpn"))
	sourceHash := videoHash(t, ffmpeg, src)

	run := runSelection(t, ffmpeg, ffprobe,
		selectionCfg(t, roots[0], "    remux_only: true\n    audio_languages: [eng]\n"), nil)

	row := run.doneRow(t, src)
	if *run.score != 0 {
		t.Fatalf("the VMAF gate measured %d time(s) on a remux: the whole point of the mode is "+
			"that there is nothing perceptual to measure", *run.score)
	}
	if row.VmafSkipped != VmafSkippedRemuxOnly {
		t.Fatalf("vmaf_skipped = %q, want %q: a row whose gate did not run has to SAY the gate did "+
			"not run, and why", row.VmafSkipped, VmafSkippedRemuxOnly)
	}
	// NO VMAF FIGURE OF ANY KIND. A zero here would be a fabricated measurement of a gate
	// nobody ran, on the row an operator reads after the source is gone.
	if row.VmafMean != nil || row.VmafMin != nil || row.VmafChroma != nil {
		t.Fatalf("the row carries VMAF figures (mean=%v min=%v chroma=%v) for a gate that did not "+
			"run", row.VmafMean, row.VmafMin, row.VmafChroma)
	}
	if row.VmafModel != "" || row.VmafPixFmt != "" || row.VmafStream != "" || row.VmafChromaMetric != "" {
		t.Fatalf("the row names a model (%q), a comparison format (%q), a stream (%q) or a chroma "+
			"metric (%q) for a measurement nobody took",
			row.VmafModel, row.VmafPixFmt, row.VmafStream, row.VmafChromaMetric)
	}
	// The video really was copied, which is what the skip was paid for with.
	if got := videoHash(t, ffmpeg, src); got != sourceHash {
		t.Fatalf("the replacement's video is not the source's bitstream (%s against %s): the "+
			"perceptual gate was skipped on a promise that did not hold", got, sourceHash)
	}
	if n := countByType(streamsOf(t, run.eng, src)); n["audio"] != 1 {
		t.Fatalf("the replacement carries %d audio stream(s), want 1: a remux applies the "+
			"selection like any other job", n["audio"])
	}
}

// TestRemuxOnly_RejectsAnOutputWhoseVideoIsNotIdentical is [AC-13]'s unhappy path, and the
// ORDERING is what it grades: identity is established BEFORE the perceptual gate is
// declined, so an output whose video was re-encoded is rejected rather than accepted
// unmeasured.
//
// Every other gate passes this output on purpose. It is the same codec as the source, it is
// smaller, it is the same length, it decodes, and it carries exactly the intended streams -
// so the identity check is the ONLY thing standing between it and the deletion of the
// source. An implementation that skipped VMAF first would swap it.
func TestRemuxOnly_RejectsAnOutputWhoseVideoIsNotIdentical(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"), audioStream("jpn"))
	before := md5f(t, src)

	// An encoder that RE-ENCODES the video to the same codec instead of copying it: what a
	// remux path that quietly re-encoded would produce.
	sneaky := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		return exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
			"-i", in, "-map", "0:v", "-map", "0:a:0", "-c:v", "libx264", "-preset", "ultrafast",
			"-b:v", "200k", "-pix_fmt", "yuv420p", "-c:a", "copy", "--", out).Run()
	})
	run := runSelection(t, ffmpeg, ffprobe,
		selectionCfg(t, roots[0], "    remux_only: true\n    audio_languages: [eng]\n"), sneaky)

	row := run.rowFor(t, src)
	if row.Status != store.Failed {
		t.Fatalf("status = %q, want %q: a remux whose video is not the source's video was accepted "+
			"with no perceptual gate in front of it", row.Status, store.Failed)
	}
	if !strings.Contains(row.Outcome.Reason, "is NOT identical to the source") {
		t.Fatalf("reason = %q, want the identity check's rejection: any other gate rejecting this "+
			"output would mean the identity check never ran", row.Outcome.Reason)
	}
	if row.Outcome.VmafSkipped != "" {
		t.Fatalf("the row records the gate as skipped (%q) on an output that was REJECTED before "+
			"the skip: identity is established first, or the skip is not paid for",
			row.Outcome.VmafSkipped)
	}
	if md5f(t, src) != before {
		t.Fatal("the source changed on a rejection")
	}
	if n := nTemp(t, roots[0]); n != 0 {
		t.Errorf("%d temp file(s) left behind after a rejected remux", n)
	}
}

// TestRemuxOnly_HoldsTheOutputToEveryStructuralGate is [AC-12]: a remux is held to the size
// floor in force for its root, which is neither waived nor lowered for the mode.
//
// The floor is the one an operator can see being waived, and waiving it is the convenient
// choice: a remux that reclaims nothing is the common case. So the case configures a floor
// this remux cannot clear and asserts the source survives.
func TestRemuxOnly_HoldsTheOutputToEveryStructuralGate(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	_, roots := twoRoots(t, "tv")
	src := filepath.Join(roots[0], "ep.mkv")
	mkSourceWithStreams(t, ffmpeg, src, audioStream("eng"), audioStream("jpn"))
	before := md5f(t, src)

	// Dropping one aac track off an 8Mbit video reclaims a fraction of a per cent; a floor
	// of 50% is one this remux cannot clear.
	run := runSelection(t, ffmpeg, ffprobe,
		selectionCfg(t, roots[0],
			"    remux_only: true\n    audio_languages: [eng]\n    min_savings_percent: 50\n"), nil)

	row := run.rowFor(t, src)
	if row.Status != store.Failed {
		t.Fatalf("status = %q, want %q: the size floor is not waived for a remux, and this one "+
			"reclaims far less than the configured 50%%", row.Status, store.Failed)
	}
	if !strings.Contains(row.Outcome.Reason, "size-increase reject") {
		t.Fatalf("reason = %q, want the size gate's rejection", row.Outcome.Reason)
	}
	if !strings.Contains(row.Outcome.Reason, "min_savings=50%") {
		t.Fatalf("reason = %q, want the ROOT's floor named in it: a remux held to the top level's "+
			"floor would be a different gate", row.Outcome.Reason)
	}
	if md5f(t, src) != before {
		t.Fatal("the source changed on a rejected remux")
	}
}
