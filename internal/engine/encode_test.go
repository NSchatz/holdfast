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
	"os"
	"path/filepath"
	"strings"
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
	if err := enc.Encode(context.Background(), src, out); err != nil {
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
	if err := enc.Encode(context.Background(), src, out); err != nil {
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
	if err := enc.Encode(context.Background(), src, out); err != nil {
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
	err := enc.Encode(context.Background(), src, out)
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
	if err := enc.Encode(context.Background(), src, out); err == nil {
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
	if err := enc.Encode(context.Background(), src, out); err != nil {
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
	if err := enc.Encode(context.Background(), src, out); err != nil {
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
