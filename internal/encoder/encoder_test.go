package encoder

import (
	"context"
	"os"
	"os/exec"
	"testing"
)

// tools fails loud (never skips) if ffmpeg is missing — a skip here would be a
// false green for the capability-detection proof, mirroring the engine package's
// own fixture-suite discipline.
func tools(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg = os.Getenv("TRANSCODE_FFMPEG")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	ffprobe = os.Getenv("TRANSCODE_FFPROBE")
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	for _, b := range []string{ffmpeg, ffprobe} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("::error:: %q not found — the capability-detection proof requires real ffmpeg+ffprobe: %v", b, err)
		}
	}
	return ffmpeg, ffprobe
}

func TestLookup_KnownKeys(t *testing.T) {
	for _, tc := range []struct {
		key         string
		ffmpegCodec string
		target      string
		hardware    bool
	}{
		{"cpu", "libx265", "hevc", false},
		{"svtav1", "libsvtav1", "av1", false},
		{"nvenc", "hevc_nvenc", "hevc", true},
		{"av1_nvenc", "av1_nvenc", "av1", true},
		{"qsv", "hevc_qsv", "hevc", true},
		{"vaapi", "hevc_vaapi", "hevc", true},
		{"amf", "hevc_amf", "hevc", true},
	} {
		spec, ok := Lookup(tc.key)
		if !ok {
			t.Fatalf("Lookup(%q): not found", tc.key)
		}
		if spec.FFmpegCodec != tc.ffmpegCodec || spec.TargetCodec != tc.target || spec.Hardware != tc.hardware {
			t.Errorf("Lookup(%q) = %+v, want FFmpegCodec=%q TargetCodec=%q Hardware=%v",
				tc.key, spec, tc.ffmpegCodec, tc.target, tc.hardware)
		}
	}
}

func TestLookup_FFmpegCodecAlias(t *testing.T) {
	spec, ok := Lookup("libsvtav1")
	if !ok || spec.Key != "svtav1" {
		t.Fatalf("Lookup(%q) = %+v, %v; want the svtav1 spec via alias", "libsvtav1", spec, ok)
	}
}

func TestLookup_Unknown(t *testing.T) {
	if _, ok := Lookup("definitely_not_a_key"); ok {
		t.Error("Lookup of an unknown key returned ok=true")
	}
}

// TestAvailable_CPURealEncoderIsTrue proves Available says yes for a real,
// always-encodable CPU spec (libx265). REDS if the temp-file-and-ffprobe check is
// broken in a way that false-negatives a working encoder.
func TestAvailable_CPURealEncoderIsTrue(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, ok := Lookup("cpu")
	if !ok {
		t.Fatal("Lookup(cpu) failed")
	}
	if !Available(context.Background(), ffmpeg, ffprobe, spec) {
		t.Error("Available(cpu/libx265) = false, want true — libx265 always works")
	}
}

// TestAvailable_SVTAV1RealEncoderIsTrue proves Available says yes for the other
// fully-testable-in-this-container encoder, SVT-AV1 (CPU-only, no device needed).
func TestAvailable_SVTAV1RealEncoderIsTrue(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, ok := Lookup("svtav1")
	if !ok {
		t.Fatal("Lookup(svtav1) failed")
	}
	if !Available(context.Background(), ffmpeg, ffprobe, spec) {
		t.Error("Available(svtav1/libsvtav1) = false, want true — libsvtav1 runs on CPU")
	}
}

// TestAvailable_BogusCodecIsFalse proves Available says no for a codec name
// ffmpeg has never heard of — the base case a naive exit-code check would also
// catch, kept here as the negative control for the two real-encoder positives
// above.
func TestAvailable_BogusCodecIsFalse(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec := Spec{Key: "bogus", FFmpegCodec: "definitely_not_a_codec", TargetCodec: "hevc"}
	if Available(context.Background(), ffmpeg, ffprobe, spec) {
		t.Error("Available(bogus codec) = true, want false")
	}
}

// TestRequireAvailable_UnknownKeyErrors proves an unknown encoder key fails loud
// with a clear error rather than silently doing nothing.
func TestRequireAvailable_UnknownKeyErrors(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	if _, err := RequireAvailable(context.Background(), ffmpeg, ffprobe, "not_a_real_encoder"); err == nil {
		t.Error("RequireAvailable(unknown key) = nil error, want an error")
	}
}

// TestRequireAvailable_CPUSucceeds proves the happy path: a known, working
// encoder resolves cleanly with no error.
func TestRequireAvailable_CPUSucceeds(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, err := RequireAvailable(context.Background(), ffmpeg, ffprobe, "cpu")
	if err != nil {
		t.Fatalf("RequireAvailable(cpu): %v", err)
	}
	if spec.TargetCodec != "hevc" {
		t.Errorf("spec.TargetCodec = %q, want hevc", spec.TargetCodec)
	}
}
