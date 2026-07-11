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

	"github.com/NSchatz/transcode/internal/probe"
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
