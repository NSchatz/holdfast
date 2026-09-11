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
			wantText: "stream-count parity failed (type=a", want: store.FailureDeterministic,
			why: "which streams this source has, and which this build maps, is fixed",
		},
		{
			name: "a video stream was dropped", in: srcCover, tmp: outNoCover,
			wantText: "stream-count parity failed (type=v", want: store.FailureDeterministic,
			why: "which video streams this source has, and which this build maps, is fixed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, class, err := eng.verifyOutput(context.Background(), tc.in, tc.tmp)
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
		_, class, err := eng.verifyOutput(context.Background(), src, good)
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
			_, class, err := eng.vmafGate(context.Background(), src, src)
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
	if _, class, err := eng.vmafGate(context.Background(), src, src); err != nil || class != "" {
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
