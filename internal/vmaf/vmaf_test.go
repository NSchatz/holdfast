package vmaf

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func ffmpegBin() string {
	if v := os.Getenv("TRANSCODE_FFMPEG"); v != "" {
		return v
	}
	return "ffmpeg"
}

func TestAvailable_TrueOnRealFfmpeg(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg not found — VMAF proof requires it: %v", err)
	}
	if !Available(context.Background(), bin) {
		t.Fatal("Available() = false on a build that should ship libvmaf")
	}
}

func TestAvailable_FalseWhenFilterMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell stub is POSIX")
	}
	dir := t.TempDir()
	stub := filepath.Join(dir, "ffmpeg")
	// A fake ffmpeg whose -filters output omits libvmaf.
	script := "#!/bin/sh\necho ' .. scale            V->V       Scale the input video size.'\necho ' .. crop             V->V       Crop the input video.'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if Available(context.Background(), stub) {
		t.Error("Available() = true for a build without the libvmaf filter")
	}
}

func mustFF(t *testing.T, bin string, args ...string) {
	t.Helper()
	if out, err := exec.Command(bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, out)
	}
}

// TestScore locks the JSON field mapping the whole gate depends on: a faithful
// encode scores high, a degraded one scores lower, and both pooled fields
// (harmonic_mean, min) are populated in (0,100].
func TestScore(t *testing.T) {
	bin := ffmpegBin()
	if _, err := exec.LookPath(bin); err != nil {
		t.Fatalf("::error:: ffmpeg required for the VMAF proof: %v", err)
	}
	dir := t.TempDir()
	ref := filepath.Join(dir, "ref.mp4")
	good := filepath.Join(dir, "good.mp4")
	bad := filepath.Join(dir, "bad.mp4")
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10", "-c:v", "libx264", "-b:v", "4M", ref)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-c:v", "libx265", "-crf", "22", "-x265-params", "log-level=error", good)
	mustFF(t, bin, "-hide_banner", "-loglevel", "error", "-y", "-i", ref,
		"-vf", "scale=48:36,scale=320:240:flags=neighbor",
		"-c:v", "libx265", "-crf", "45", "-x265-params", "log-level=error", bad)

	g, err := Score(context.Background(), bin, good, ref, 1, "version=vmaf_v0.6.1")
	if err != nil {
		t.Fatalf("Score(good): %v", err)
	}
	for _, v := range []float64{g.HarmonicMean, g.Min} {
		if v <= 0 || v > 100 {
			t.Errorf("pooled VMAF %v out of (0,100] — JSON field mapping likely wrong", v)
		}
	}
	if g.HarmonicMean < 90 {
		t.Errorf("a faithful crf22 encode should score high; got harmonic_mean=%.2f", g.HarmonicMean)
	}
	b, err := Score(context.Background(), bin, bad, ref, 1, "version=vmaf_v0.6.1")
	if err != nil {
		t.Fatalf("Score(bad): %v", err)
	}
	if b.HarmonicMean >= g.HarmonicMean {
		t.Errorf("degraded (%.2f) should score below faithful (%.2f)", b.HarmonicMean, g.HarmonicMean)
	}
}

func TestEscapeFilterValue(t *testing.T) {
	cases := map[string]string{
		"/tmp/a.json":  "/tmp/a.json",
		"/t:mp/a.json": `/t\:mp/a.json`,
		`/a\b`:         `/a\\b`,
		"/a'b[1]":      `/a\'b\[1\]`,
	}
	for in, want := range cases {
		if got := escapeFilterValue(in); got != want {
			t.Errorf("escapeFilterValue(%q) = %q, want %q", in, got, want)
		}
	}
}
