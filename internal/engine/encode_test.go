package engine

// Direct proof that FFmpegEncoder.Encode assembles the args TRANSCODE-3 promises
// (colour propagation, derived pix_fmt, -fps_mode passthrough) — asserted against
// the actual argv ffmpeg receives, via a capturing fake "ffmpeg" wrapper script.
// This is a stronger anti-advisory proof for -fps_mode passthrough specifically
// than an end-to-end packet-count comparison: this build of ffmpeg already
// defaults to passthrough-like behaviour for many inputs, so a packet-count
// fixture alone would not reliably RED if the flag were dropped — inspecting the
// literal command line does.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
)

// captureFFmpeg writes a fake "ffmpeg" shell script to dir that logs its argv to
// argvLog (one arg per line, invocations separated by "---") and then delegates to
// the real ffmpeg so the encode still produces a valid output file (verifyOutput
// isn't exercised by these tests, but a real output keeps the fixture honest).
func captureFFmpeg(t *testing.T, dir, realFFmpeg string) (fakeFFmpeg, argvLog string) {
	t.Helper()
	fakeFFmpeg = filepath.Join(dir, "fake-ffmpeg.sh")
	argvLog = filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\n" +
		"{\n" +
		"  for a in \"$@\"; do printf '%s\\n' \"$a\"; done\n" +
		"  printf -- '---\\n'\n" +
		"} >> \"" + argvLog + "\"\n" +
		"exec \"" + realFFmpeg + "\" \"$@\"\n"
	if err := os.WriteFile(fakeFFmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return fakeFFmpeg, argvLog
}

func readArgv(t *testing.T, argvLog string) []string {
	t.Helper()
	b, err := os.ReadFile(argvLog)
	if err != nil {
		t.Fatalf("read argv log: %v", err)
	}
	// Only the first invocation matters for these tests.
	first := strings.SplitN(string(b), "---\n", 2)[0]
	var args []string
	for _, l := range strings.Split(first, "\n") {
		if l != "" {
			args = append(args, l)
		}
	}
	return args
}

func hasArgPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestEncode_FpsModePassthroughWired proves -fps_mode passthrough is present on
// every CPU encode invocation. REDS if that flag is ever dropped from
// FFmpegEncoder.Encode — a VFR source would otherwise silently be forced to CFR.
func TestEncode_FpsModePassthroughWired(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, realFFmpeg, src, "3M")

	fakeFFmpeg, argvLog := captureFFmpeg(t, d, realFFmpeg)
	cfg := baseCfg(d)
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: fakeFFmpeg, Cfg: cfg, Probe: prober}

	out := filepath.Join(d, "out.mkv")
	if err := enc.Encode(context.Background(), src, out, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	args := readArgv(t, argvLog)
	if !hasArgPair(args, "-fps_mode", "passthrough") {
		t.Errorf("ffmpeg argv missing -fps_mode passthrough: %v", args)
	}
}

// TestEncode_ColorArgsWiredForHDR10 proves DeriveColorArgs' output actually reaches
// the ffmpeg command line (both the -color_* flags and the x265-params colour
// suffix) for an HDR10 source. REDS if the colour-args wiring in Encode is removed
// or the x265Color suffix stops being appended.
func TestEncode_ColorArgsWiredForHDR10(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264HDR10(t, realFFmpeg, src, "3M")

	fakeFFmpeg, argvLog := captureFFmpeg(t, d, realFFmpeg)
	cfg := baseCfg(d)
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: fakeFFmpeg, Cfg: cfg, Probe: prober}

	out := filepath.Join(d, "out.mkv")
	if err := enc.Encode(context.Background(), src, out, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	args := readArgv(t, argvLog)
	if !hasArgPair(args, "-color_primaries", "bt2020") {
		t.Errorf("ffmpeg argv missing -color_primaries bt2020: %v", args)
	}
	if !hasArgPair(args, "-color_trc", "smpte2084") {
		t.Errorf("ffmpeg argv missing -color_trc smpte2084: %v", args)
	}
	found := false
	for _, a := range args {
		if strings.Contains(a, "master-display=") && strings.Contains(a, "max-cll=") {
			found = true
		}
	}
	if !found {
		t.Errorf("ffmpeg argv missing x265-params master-display/max-cll: %v", args)
	}
}

// TestEncode_PixFmtAutoDerivesFromSource proves the "auto" PixelFormat sentinel
// actually derives the output -pix_fmt from the source (not a fixed default). REDS
// if PixelFormatAuto()'s wiring into Encode is removed.
func TestEncode_PixFmtAutoDerivesFromSource(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Chroma422(t, realFFmpeg, src, "3M")

	fakeFFmpeg, argvLog := captureFFmpeg(t, d, realFFmpeg)
	cfg := baseCfg(d)
	cfg.PixelFormat = "auto"
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: fakeFFmpeg, Cfg: cfg, Probe: prober}

	out := filepath.Join(d, "out.mkv")
	if err := enc.Encode(context.Background(), src, out, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	args := readArgv(t, argvLog)
	if !hasArgPair(args, "-pix_fmt", "yuv422p10le") {
		t.Errorf("ffmpeg argv missing -pix_fmt yuv422p10le (auto-derived from 4:2:2 source): %v", args)
	}
}

// TestEncode_ExoticPixFmtRefusesToEncode is the encoder-side defence-in-depth
// backstop: even if the engine's skip guard were bypassed, Encode itself must
// refuse an unrecognized/exotic source pix_fmt rather than silently subsampling.
func TestEncode_ExoticPixFmtRefusesToEncode(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	// yuv411p (4:1:1, via ffv1 — libx264/libx265 don't support it, so ffv1 is the
	// only way to actually land this pix_fmt on disk) is not 4:2:0/4:2:2/4:4:4 ->
	// DerivePixFmt returns ok=false.
	ff(t, realFFmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10",
		"-c:v", "ffv1", "-pix_fmt", "yuv411p", "--", src)

	cfg := baseCfg(d)
	cfg.PixelFormat = "auto"
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: realFFmpeg, Cfg: cfg, Probe: prober}

	out := filepath.Join(d, "out.mkv")
	err := enc.Encode(context.Background(), src, out, nil)
	if err == nil {
		t.Fatal("Encode succeeded on an exotic pix_fmt source — should have refused")
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("Encode wrote an output despite refusing the exotic pix_fmt")
	}
}

// TestEncode_UnknownEncoderErrors proves Encode refuses an unrecognized
// Cfg.Encoder rather than silently falling back to any default codec.
func TestEncode_UnknownEncoderErrors(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, realFFmpeg, src, "3M")

	cfg := baseCfg(d)
	cfg.Encoder = "not_a_real_encoder"
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: realFFmpeg, Cfg: cfg, Probe: prober}

	out := filepath.Join(d, "out.mkv")
	if err := enc.Encode(context.Background(), src, out, nil); err == nil {
		t.Fatal("Encode succeeded with an unknown encoder key — should have refused")
	}
}

// TestEncode_SVTAV1WiresCodecColorAndPreset proves the SVT-AV1 path selects the
// correct ffmpeg codec, still applies the universal -color_*/-fps_mode passthrough
// args, uses the numeric preset mapping, and — critically — does NOT append
// x265-params (that mechanism is libx265-only; AV1 HDR10 static-metadata carriage
// is out of scope, see encode.go's buildArgs doc comment).
func TestEncode_SVTAV1WiresCodecColorAndPreset(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264SDR(t, realFFmpeg, src, "3M")

	fakeFFmpeg, argvLog := captureFFmpeg(t, d, realFFmpeg)
	cfg := baseCfg(d)
	cfg.Encoder = "svtav1"
	cfg.Preset = "fast" // -> numeric 10
	cfg.CRF = 30
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: fakeFFmpeg, Cfg: cfg, Probe: prober}

	out := filepath.Join(d, "out.mkv")
	if err := enc.Encode(context.Background(), src, out, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	args := readArgv(t, argvLog)
	if !hasArgPair(args, "-c:v", "libsvtav1") {
		t.Errorf("ffmpeg argv missing -c:v libsvtav1: %v", args)
	}
	if !hasArgPair(args, "-preset", "10") {
		t.Errorf("ffmpeg argv missing -preset 10 (mapped from Preset=fast): %v", args)
	}
	if !hasArgPair(args, "-crf", "30") {
		t.Errorf("ffmpeg argv missing -crf 30: %v", args)
	}
	if !hasArgPair(args, "-colorspace", "bt709") {
		t.Errorf("ffmpeg argv missing universal -colorspace bt709: %v", args)
	}
	if !hasArgPair(args, "-fps_mode", "passthrough") {
		t.Errorf("ffmpeg argv missing universal -fps_mode passthrough: %v", args)
	}
	for _, a := range args {
		if a == "-x265-params" {
			t.Errorf("ffmpeg argv unexpectedly includes -x265-params for an svtav1 encode: %v", args)
		}
	}
}

// TestBuildArgs_HardwareEncoderShapes is a unit test (no real ffmpeg invocation
// needed — pure function) proving buildArgs assembles the documented arg shape
// for each hardware Spec. These encoders are gated behind capability detection
// and cannot be run end-to-end in this container (no GPU/device), so this is the
// direct proof that the arg-builder logic itself is wired as designed; combined
// with TestHardwareEncoders_AvailabilityTable's honest runtime skip, both the
// "what would we send" and "do we ever send it without checking" halves of the
// hardware story are covered.
func TestBuildArgs_HardwareEncoderShapes(t *testing.T) {
	cfg := config.Config{CRF: 23, Preset: "slow"}

	nvencSpec, _ := encoder.Lookup("nvenc")
	nvencArgs := buildArgs(nvencSpec, cfg, "yuv420p10le", nil, "")
	if !hasArgPair(nvencArgs, "-cq", "23") || !hasArgPair(nvencArgs, "-rc", "vbr") {
		t.Errorf("nvenc buildArgs missing -rc vbr / -cq 23: %v", nvencArgs)
	}

	av1NvencSpec, _ := encoder.Lookup("av1_nvenc")
	av1NvencArgs := buildArgs(av1NvencSpec, cfg, "yuv420p10le", nil, "")
	if !hasArgPair(av1NvencArgs, "-cq", "23") {
		t.Errorf("av1_nvenc buildArgs missing -cq 23: %v", av1NvencArgs)
	}

	qsvSpec, _ := encoder.Lookup("qsv")
	qsvArgs := buildArgs(qsvSpec, cfg, "yuv420p10le", nil, "")
	if !hasArgPair(qsvArgs, "-global_quality", "23") {
		t.Errorf("qsv buildArgs missing -global_quality 23: %v", qsvArgs)
	}

	vaapiSpec, _ := encoder.Lookup("vaapi")
	vaapiArgs := buildArgs(vaapiSpec, cfg, "nv12", nil, "")
	if !hasArgPair(vaapiArgs, "-qp", "23") {
		t.Errorf("vaapi buildArgs missing -qp 23: %v", vaapiArgs)
	}
	foundHwupload := false
	for _, a := range vaapiArgs {
		if strings.Contains(a, "hwupload") {
			foundHwupload = true
		}
	}
	if !foundHwupload {
		t.Errorf("vaapi buildArgs missing hwupload filter: %v", vaapiArgs)
	}

	amfSpec, _ := encoder.Lookup("amf")
	amfArgs := buildArgs(amfSpec, cfg, "yuv420p10le", nil, "")
	if !hasArgPair(amfArgs, "-qp_i", "23") || !hasArgPair(amfArgs, "-qp_p", "23") {
		t.Errorf("amf buildArgs missing -qp_i/-qp_p 23: %v", amfArgs)
	}

	// Every hardware Spec still gets the universal args (pix_fmt/fps_mode), and
	// none of them get x265-params (libx265-only).
	for _, args := range [][]string{nvencArgs, av1NvencArgs, qsvArgs, vaapiArgs, amfArgs} {
		if !hasArgPair(args, "-fps_mode", "passthrough") {
			t.Errorf("hardware buildArgs missing universal -fps_mode passthrough: %v", args)
		}
		for _, a := range args {
			if a == "-x265-params" {
				t.Errorf("hardware buildArgs unexpectedly includes -x265-params: %v", args)
			}
		}
	}
}

// TestEncode_VaapiDeviceFlagPrecedesInput proves the -vaapi_device global option
// is placed BEFORE -i in the assembled argv. This matters: -vaapi_device
// establishes the hardware device context that the hwupload filter (added by
// buildArgs) needs, so if it landed after -i (as it would from a naive
// buildArgs-only assembly) ffmpeg would reject the command. Untestable via a real
// encode in this container (no VAAPI device), so this inspects the argv directly
// — the same "capture what ffmpeg would receive" technique the other Encode wiring
// tests use, applied to a fake ffmpeg that just echoes argv (no real run needed
// since a real VAAPI encode would fail here regardless).
func TestEncode_VaapiDeviceFlagPrecedesInput(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, realFFmpeg, src, "3M")

	// A fake ffmpeg that just logs argv and exits 0 without touching real VAAPI —
	// this test proves ARG ORDER, not that vaapi actually encodes (which needs a
	// real device this container doesn't have).
	fakeFFmpeg := filepath.Join(d, "fake-ffmpeg-noop.sh")
	argvLog := filepath.Join(d, "argv.log")
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do printf '%s\\n' \"$a\"; done >> \"" + argvLog + "\"\n" +
		"printf -- '---\\n' >> \"" + argvLog + "\"\n" +
		"exit 0\n"
	if err := os.WriteFile(fakeFFmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}

	cfg := baseCfg(d)
	cfg.Encoder = "vaapi"
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: fakeFFmpeg, Cfg: cfg, Probe: prober}

	out := filepath.Join(d, "out.mkv")
	if err := enc.Encode(context.Background(), src, out, nil); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	args := readArgv(t, argvLog)
	deviceIdx, inputIdx := -1, -1
	for i, a := range args {
		if a == "-vaapi_device" {
			deviceIdx = i
		}
		if a == "-i" {
			inputIdx = i
		}
	}
	if deviceIdx == -1 {
		t.Fatalf("ffmpeg argv missing -vaapi_device: %v", args)
	}
	if inputIdx == -1 {
		t.Fatalf("ffmpeg argv missing -i: %v", args)
	}
	if deviceIdx > inputIdx {
		t.Errorf("-vaapi_device (index %d) must precede -i (index %d): %v", deviceIdx, inputIdx, args)
	}
}

// --- live progress collection (S0030) ----------------------------------------
//
// The claim under test is that collecting progress is ADDITIVE. holdfast deletes a
// source once its replacement is judged faithful, and the error EncodeWithProgress
// returns is what the engine turns into a failed job's reason — so a collector that
// swallowed a non-zero exit would hand a failed encode to the verify gate as a
// candidate, and one that took stdout for itself would empty the failure text an
// operator reads. Every unhappy path below is driven for real against a fake ffmpeg.

// collectProgress is a concurrency-safe ProgressSink that records every report.
type collectProgress struct {
	mu  sync.Mutex
	got []Progress
}

func (c *collectProgress) sink(p Progress) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got = append(c.got, p)
}

func (c *collectProgress) all() []Progress {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Progress, len(c.got))
	copy(out, c.got)
	return out
}

// progressFake writes a fake "ffmpeg" that: emits progressPayload on fd 3 IF the caller
// supplied one (so the same script is usable with and without progress collection),
// writes the given stdout/stderr text, and exits with code exitCode. The payload goes
// through a file so no shell quoting can distort the progress stream under test.
func progressFake(t *testing.T, dir, name, progressPayload, stdoutText, stderrText string, exitCode int) string {
	t.Helper()
	payload := filepath.Join(dir, name+".progress")
	if err := os.WriteFile(payload, []byte(progressPayload), 0o644); err != nil {
		t.Fatalf("write progress payload: %v", err)
	}
	script := "#!/bin/sh\n" +
		// `( : >&3 ) 2>/dev/null` is a portable "is fd 3 open for writing?" test: with no
		// -progress option there is no fd 3 and the block is simply skipped.
		"if ( : >&3 ) 2>/dev/null; then cat \"" + payload + "\" >&3; fi\n" +
		"printf '%s' '" + stdoutText + "'\n" +
		"printf '%s' '" + stderrText + "' >&2\n" +
		fmt.Sprintf("exit %d\n", exitCode)
	path := filepath.Join(dir, name+".sh")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

// oneReport is a documented-shape progress report, verbatim in the key set ffmpeg 8.0.1
// was MEASURED to emit (see scanProgressStream's doc comment for the captured sample).
// out_time_ms deliberately carries the SAME microsecond value out_time_us does, because
// that is what the real build emits — reading it as its name suggests is the unit trap
// this fixture exists to keep closed.
func oneReport(us int64, terminator string) string {
	return fmt.Sprintf(
		"frame=27\nfps=0.00\nstream_0_0_q=26.1\nbitrate=   9.4kbits/s\ntotal_size=2942\n"+
			"out_time_us=%d\nout_time_ms=%d\nout_time=%s\ndup_frames=0\ndrop_frames=0\nspeed=   5x\n"+
			"progress=%s\n",
		us, us, clockOf(us), terminator)
}

func clockOf(us int64) string {
	sec := us / 1e6
	return fmt.Sprintf("%02d:%02d:%02d.%06d", sec/3600, (sec/60)%60, sec%60, us%1e6)
}

// TestScanProgressStream_ParsesTheDocumentedShape is the parser proof (AC2/AC3/AC6). The
// documentation guarantees only the line shape and that "progress" is the LAST key of a
// report; every other key here was measured off the real build. So: a report is
// published at its terminator and nowhere else, out_time_us is read as MICROSECONDS, the
// mis-named out_time_ms is never read, and a report whose keys are all unrecognised (or
// which never reaches a terminator) publishes nothing at all.
func TestScanProgressStream_ParsesTheDocumentedShape(t *testing.T) {
	t.Run("two reports, microseconds", func(t *testing.T) {
		var got []float64
		scanProgressStream(strings.NewReader(oneReport(2_500_000, "continue")+oneReport(5_800_000, "end")),
			func(sec float64) { got = append(got, sec) })
		want := []float64{2.5, 5.8}
		if len(got) != len(want) {
			t.Fatalf("got %v reports, want %v", got, want)
		}
		for i := range want {
			if math.Abs(got[i]-want[i]) > 1e-9 {
				t.Errorf("report %d = %vs, want %vs — out_time_us is microseconds", i, got[i], want[i])
			}
		}
	})

	t.Run("out_time_ms is never read as milliseconds", func(t *testing.T) {
		// The real build emits out_time_ms=2500000 at the 2.5s mark. Reading that key as
		// its name suggests would report 2500s, and on a two-hour film would peg every
		// encode near 0% for its whole run.
		var got []float64
		scanProgressStream(strings.NewReader("out_time_ms=2500000\nprogress=continue\n"),
			func(sec float64) { got = append(got, sec) })
		if len(got) != 0 {
			t.Fatalf("out_time_ms alone produced %v — it is mis-named and must not be parsed", got)
		}
	})

	t.Run("out_time is the fallback when out_time_us is absent", func(t *testing.T) {
		var got []float64
		scanProgressStream(strings.NewReader("out_time=01:02:03.500000\nprogress=end\n"),
			func(sec float64) { got = append(got, sec) })
		if len(got) != 1 || math.Abs(got[0]-3723.5) > 1e-6 {
			t.Fatalf("out_time fallback gave %v, want [3723.5]", got)
		}
	})

	t.Run("no terminator publishes nothing", func(t *testing.T) {
		var got []float64
		scanProgressStream(strings.NewReader("out_time_us=9000000\n"), func(sec float64) { got = append(got, sec) })
		if len(got) != 0 {
			t.Fatalf("a report with no progress= terminator published %v — a half-written final block is not a position", got)
		}
	})

	t.Run("unrecognised keys are no progress at all", func(t *testing.T) {
		var got []float64
		scanProgressStream(strings.NewReader("elapsed_ticks=4\nwhat=ever\nprogress=end\n"),
			func(sec float64) { got = append(got, sec) })
		if len(got) != 0 {
			t.Fatalf("an unrecognised key set published %v — it must read as no progress reported", got)
		}
	})

	t.Run("garbage is no progress at all", func(t *testing.T) {
		var got []float64
		scanProgressStream(strings.NewReader("\x00\x01 not even lines "), func(sec float64) { got = append(got, sec) })
		if len(got) != 0 {
			t.Fatalf("garbage published %v", got)
		}
	})
}

// TestEncodeWithProgress_ReportsRealPositionsAgainstTheSource is AC2 end to end against
// the REAL ffmpeg the rest of the safety suite runs on: the option actually reaches the
// command line, the stream actually arrives on its own descriptor, and the positions it
// carries are real positions in the source timeline — measured by the encoder, not
// estimated from elapsed time. This is the test that would red if a future ffmpeg
// renamed or re-united the keys the parser was pinned to.
func TestEncodeWithProgress_ReportsRealPositionsAgainstTheSource(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Long(t, realFFmpeg, src, "3M") // 10s @ 24fps

	fakeFFmpeg, argvLog := captureFFmpeg(t, d, realFFmpeg)
	cfg := baseCfg(d)
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: fakeFFmpeg, Cfg: cfg, Probe: prober}

	var c collectProgress
	out := filepath.Join(d, "out.mkv")
	if err := enc.EncodeWithProgress(context.Background(), src, out, nil, c.sink); err != nil {
		t.Fatalf("EncodeWithProgress: %v", err)
	}

	args := readArgv(t, argvLog)
	if !hasArgPair(args, "-progress", "pipe:3") {
		t.Fatalf("ffmpeg argv missing -progress pipe:3 (and it must be pipe:3, not pipe:1 — stdout is captured error text): %v", args)
	}

	reports := c.all()
	if len(reports) == 0 {
		t.Fatal("a real encode reported no progress at all — the documented -progress stream was not collected")
	}
	srcDur, ok := prober.DurationSec(context.Background(), src)
	if !ok || srcDur <= 0 {
		t.Fatalf("fixture duration unknown (%v, %v)", srcDur, ok)
	}
	last := reports[len(reports)-1].PositionSec
	if last <= 0 {
		t.Errorf("final reported position is %vs — a running encode must report a real position", last)
	}
	// The final report lands at the end of the encode, so it is the source duration to
	// within the container's own rounding. A position wildly off it is a UNIT error.
	if last < srcDur*0.9 || last > srcDur*1.1 {
		t.Errorf("final reported position %vs is not the source duration %vs — the progress unit is wrong", last, srcDur)
	}
	for i, r := range reports {
		if r.PositionSec < 0 {
			t.Errorf("report %d has a negative position %v", i, r.PositionSec)
		}
	}
}

// TestEncodeWithProgress_FailurePathIsByteIdentical is AC4, and it is the blast-radius
// guard for this whole change. The SAME fake ffmpeg is run twice — once with progress
// collection, once without — and the two errors must be the same string: same wrapped
// exit status, same captured text. It also proves stdout is still captured (progress
// went to fd 3, not pipe:1) and that a failure still produces no output file.
func TestEncodeWithProgress_FailurePathIsByteIdentical(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, realFFmpeg, src, "3M")

	const stderrText = "Conversion failed! x265 [error]: encoder blew up"
	const stdoutText = "a line that ffmpeg put on stdout"
	fake := progressFake(t, d, "failing", oneReport(1_000_000, "continue"), stdoutText, stderrText, 3)

	cfg := baseCfg(d)
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{FFmpeg: fake, Cfg: cfg, Probe: prober}

	var c collectProgress
	withErr := enc.EncodeWithProgress(context.Background(), src, filepath.Join(d, "a.mkv"), nil, c.sink)
	withoutErr := enc.Encode(context.Background(), src, filepath.Join(d, "b.mkv"), nil)

	if withErr == nil || withoutErr == nil {
		t.Fatalf("a non-zero ffmpeg exit must be an error (with=%v without=%v)", withErr, withoutErr)
	}
	if withErr.Error() != withoutErr.Error() {
		t.Errorf("progress collection changed the failure text.\n with progress: %q\n without:       %q", withErr.Error(), withoutErr.Error())
	}
	// The exit status itself still surfaces, wrapped, so errors.As still finds it.
	var exitErr *exec.ExitError
	if !errors.As(withErr, &exitErr) {
		t.Fatalf("the ffmpeg exit status no longer surfaces as an *exec.ExitError: %v", withErr)
	}
	if exitErr.ExitCode() != 3 {
		t.Errorf("exit code = %d, want 3", exitErr.ExitCode())
	}
	if !strings.Contains(withErr.Error(), stderrText) {
		t.Errorf("the captured stderr is missing from the failure reason: %q", withErr.Error())
	}
	if !strings.Contains(withErr.Error(), stdoutText) {
		t.Errorf("stdout is no longer captured — the progress stream must not take pipe:1: %q", withErr.Error())
	}
	// And progress really was collected on that failing run, so the equality above is
	// not vacuously true of a run that quietly did nothing.
	if len(c.all()) == 0 {
		t.Error("no progress was collected on the failing run — the equality proof would be vacuous")
	}
}

// TestEncodeWithProgress_SilentOrMalformedStreamChangesNothing is AC6: an encoder that
// reports nothing, or nonsense, still finishes exactly as it would with no progress
// collection at all. Silence reads as unknown, never as stalled and never as an error.
func TestEncodeWithProgress_SilentOrMalformedStreamChangesNothing(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, realFFmpeg, src, "3M")
	cfg := baseCfg(d)
	prober := probe.New(realFFmpeg, realFFprobe)

	cases := []struct {
		name    string
		payload string
	}{
		{"silent", ""},
		{"malformed", "\x00\x01garbage with no newline and no keys"},
		{"partial", "out_time_us=1200000\n"}, // no terminator: never a published position
		{"unknown keys", "position_ticks=7\nprogress=end\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := progressFake(t, d, "ok-"+strings.ReplaceAll(tc.name, " ", "-"), tc.payload, "", "", 0)
			enc := FFmpegEncoder{FFmpeg: fake, Cfg: cfg, Probe: prober}
			var c collectProgress
			if err := enc.EncodeWithProgress(context.Background(), src, filepath.Join(d, "out.mkv"), nil, c.sink); err != nil {
				t.Fatalf("a zero-exit encode must succeed regardless of its progress stream: %v", err)
			}
			if got := c.all(); len(got) != 0 {
				t.Errorf("published %v from a %s progress stream — an unusable stream is NO progress, never a guess", got, tc.name)
			}
		})
	}
}

// TestEncodeWithProgress_UnstartableCollectionFallsBackToThePlainEncode is the other
// half of AC6: if the progress channel cannot be opened at all, the option is never
// passed and the encode runs exactly as it did before progress collection existed.
func TestEncodeWithProgress_UnstartableCollectionFallsBackToThePlainEncode(t *testing.T) {
	realFFmpeg, realFFprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, realFFmpeg, src, "3M")

	fakeFFmpeg, argvLog := captureFFmpeg(t, d, realFFmpeg)
	cfg := baseCfg(d)
	prober := probe.New(realFFmpeg, realFFprobe)
	enc := FFmpegEncoder{
		FFmpeg: fakeFFmpeg, Cfg: cfg, Probe: prober,
		newProgressPipe: func() (*os.File, *os.File, error) { return nil, nil, errors.New("no descriptors") },
	}

	var c collectProgress
	out := filepath.Join(d, "out.mkv")
	if err := enc.EncodeWithProgress(context.Background(), src, out, nil, c.sink); err != nil {
		t.Fatalf("an unopenable progress pipe must not fail the encode: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("the encode produced no output: %v", err)
	}
	args := readArgv(t, argvLog)
	for _, a := range args {
		if a == "-progress" {
			t.Fatalf("-progress was passed with no reader for it: %v", args)
		}
	}
	if got := c.all(); len(got) != 0 {
		t.Errorf("progress was reported with no pipe to report over: %v", got)
	}
}

// TestSvtav1Preset_MapsPresetWords proves the config Preset word -> SVT-AV1
// numeric preset mapping matches the documented table, and that an unrecognized
// word falls back to the documented middle-ground default (8) rather than an
// extreme.
func TestSvtav1Preset_MapsPresetWords(t *testing.T) {
	cases := []struct {
		word string
		want int
	}{
		{"placebo", 2},
		{"veryslow", 2},
		{"slower", 4},
		{"slow", 6},
		{"medium", 8},
		{"fast", 10},
		{"faster", 11},
		{"veryfast", 11},
		{"superfast", 12},
		{"ultrafast", 12},
		{"", 8},
		{"not_a_real_preset", 8},
	}
	for _, tc := range cases {
		if got := svtav1Preset(tc.word); got != tc.want {
			t.Errorf("svtav1Preset(%q) = %d, want %d", tc.word, got, tc.want)
		}
	}
}
