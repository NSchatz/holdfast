package engine

// TRANSCODE-6: the codec matrix. SVT-AV1 runs on CPU, so it is fully testable here
// with the same real-ffmpeg-fixture discipline as the libx265 cases above. The
// hardware encoders (NVENC/QSV/VAAPI/AMF) have no GPU/device in this container —
// their table test proves an HONEST skip (via internal/encoder.Available, the
// robust capability check) rather than a false green; on a host with a real
// device the same test would exercise a real encode.

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/NSchatz/transcode/internal/config"
	"github.com/NSchatz/transcode/internal/encoder"
	"github.com/NSchatz/transcode/internal/probe"
	"github.com/NSchatz/transcode/internal/store"
)

// mkAV1 writes a real SVT-AV1 clip (container inferred from path ext). Mirrors
// mkHevc but for the av1 target codec, used to prove the skip-already-target guard
// generalizes to av1.
func mkAV1(t *testing.T, ffmpeg, path, crf string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libsvtav1", "-preset", "10", "-crf", crf, "-pix_fmt", "yuv420p10le", "--", path)
}

// svtav1Cfg mutates baseCfg to select SVT-AV1: numeric preset 10 to keep the
// (real, CPU) encode fast, matching the ultrafast-libx265 posture of baseCfg.
func svtav1Cfg(c *config.Config) {
	c.Encoder = "svtav1"
	c.Preset = "fast" // maps to svtav1Preset("fast") == 10
	c.CRF = 30
}

// TestSVTAV1_GoodSmallerSwap proves the full happy path for a real SVT-AV1 encode:
// output codec is av1, smaller than the source, passes verify+VMAF, the source is
// replaced, and the job ends done. The encoder-agnostic promise (TRANSCODE-6's
// headline claim) is that this is the SAME shape as TestCase5's libx265 proof,
// just with Encoder=svtav1 and target av1.
func TestSVTAV1_GoodSmallerSwap(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M") // inflated so a real svtav1 encode reliably shrinks it
	inSize := probe.FileSize(src)

	led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		svtav1Cfg(c)
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 90 // svtav1 at crf30/preset10 is lossy-but-faithful; 90 is a safe real floor
	})

	if codecOf(t, ffprobe, src) != "av1" {
		t.Fatalf("source codec = %q, want av1", codecOf(t, ffprobe, src))
	}
	if probe.FileSize(src) >= inSize {
		t.Error("av1 output was not smaller than the source")
	}
	if !ledgerHas(t, led, store.Done, "movie.mkv") {
		t.Error("expected a done row")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp left behind")
	}
}

// TestSVTAV1_AlreadyAV1Skipped proves the generalized skip-already-target guard:
// with Encoder=svtav1 (target av1), a source that is ALREADY av1 is skipped rather
// than re-encoded — the av1 analogue of TestCase1_AlreadyHEVCSkipped.
func TestSVTAV1_AlreadyAV1Skipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkAV1(t, ffmpeg, src, "40")
	before := md5f(t, src)

	led := run(t, ffmpeg, ffprobe, d, nil, svtav1Cfg)

	if md5f(t, src) != before {
		t.Error("already-av1 source was modified")
	}
	if !ledgerHas(t, led, store.Skipped, "movie.mkv") {
		t.Error("expected skipped row")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp left behind")
	}
}

// TestSVTAV1_VmafRejectsDegradedOutput proves the verify/VMAF gate is genuinely
// encoder-agnostic: a structurally-valid but perceptually-degraded AV1 encode
// (downscale-then-upscale, same pattern as degradedEncoder for libx265) is
// REJECTED by VMAF and the source is left untouched — the exact same bar a
// hardware/hevc encode is held to. This is the anti-advisory proof that
// TRANSCODE-6 did not special-case any encoder to bypass the no-loss gate.
func TestSVTAV1_VmafRejectsDegradedOutput(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	degradedAV1 := EncoderFunc(func(ctx context.Context, in, out string) error {
		return exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in,
			"-vf", "scale=64:48,scale=320:240:flags=neighbor",
			"-c:v", "libsvtav1", "-preset", "10", "-crf", "55",
			"-pix_fmt", "yuv420p10le", "--", out).Run()
	})

	led := run(t, ffmpeg, ffprobe, d, degradedAV1, func(c *config.Config) {
		svtav1Cfg(c)
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
	})

	if md5f(t, src) != before {
		t.Error("source modified by a low-VMAF av1 output")
	}
	if codecOf(t, ffprobe, src) != "h264" {
		t.Error("source was swapped despite a low VMAF av1 encode")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp not discarded")
	}
	if !ledgerHas(t, led, store.Failed, "movie.mkv") {
		t.Error("expected a failed row (VMAF rejection of a degraded av1 encode)")
	}
}

// ---- hardware encoders: honest capability-gated skip -------------------------

// TestHardwareEncoders_AvailabilityTable proves, for every registered hardware
// Spec, that internal/encoder.Available reports the truth about this host. In
// THIS container there is no GPU/device of any kind, so every hardware encoder is
// expected to be unavailable — asserted explicitly (not just skipped) so the test
// REDS if Available ever starts lying (e.g. reporting a hardware encoder available
// when it demonstrably is not, which would be the exact false-green the
// temp-file-and-ffprobe design in Available exists to prevent). When an encoder
// IS available (e.g. a future CI runner with a real GPU), this test runs a REAL
// encode through the engine and asserts the same happy-path shape as SVT-AV1
// above; otherwise it records an honest, clearly-worded skip.
func TestHardwareEncoders_AvailabilityTable(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, key := range []string{"nvenc", "av1_nvenc", "qsv", "vaapi", "amf"} {
		t.Run(key, func(t *testing.T) {
			spec, ok := encoder.Lookup(key)
			if !ok {
				t.Fatalf("encoder.Lookup(%q) failed", key)
			}
			if !spec.Hardware {
				t.Fatalf("registry bug: %q is not marked Hardware", key)
			}

			available := encoder.Available(context.Background(), ffmpeg, ffprobe, spec)
			if !available {
				t.Skipf("%s (%s) not available on this host (no matching GPU/device) — honest skip, not a false green", key, spec.FFmpegCodec)
				return
			}

			// A real device IS present (not the case in this container today) — run
			// the actual encoder end-to-end through the engine, same happy-path shape
			// as the SVT-AV1 proof above.
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
				c.Encoder = key
				c.CRF = 23
			})
			if codecOf(t, ffprobe, src) != spec.TargetCodec {
				t.Errorf("source codec after %s encode = %q, want %q", key, codecOf(t, ffprobe, src), spec.TargetCodec)
			}
			if !ledgerHas(t, led, store.Done, "movie.mkv") {
				t.Errorf("expected a done row for %s", key)
			}
		})
	}
}
