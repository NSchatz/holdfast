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
	ffmpeg = os.Getenv("HOLDFAST_FFMPEG")
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	ffprobe = os.Getenv("HOLDFAST_FFPROBE")
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

// TestTargetCodecs_IsEveryEncodersOutputNotTheConfiguredOne. The set is what the
// engine's record-free hold-back asks against when it decides whether a file at a temp
// path is one holdfast could have written (AC15i). It must cover EVERY encoder in the
// registry: a replacement stranded on disk was written by whichever one was configured
// at the time, and `encoder:` is an ordinary config key an operator may change - so a
// set that tracked the current setting would let an unrelated edit license a deletion.
func TestTargetCodecs_IsEveryEncodersOutputNotTheConfiguredOne(t *testing.T) {
	got := TargetCodecs()

	// Every registered encoder's output is in it, whichever one a build is set to.
	for _, key := range Known() {
		spec, ok := Lookup(key)
		if !ok {
			t.Fatalf("Known() offered %q and Lookup refuses it", key)
		}
		found := false
		for _, c := range got {
			found = found || c == spec.TargetCodec
		}
		if !found {
			t.Errorf("encoder %q writes %q and TargetCodecs() = %v does not carry it", key, spec.TargetCodec, got)
		}
	}
	// Deduplicated and sorted, so callers can compare and report it stably. Five of the
	// seven encoders target hevc, so a set that did not dedupe would be seven long.
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("TargetCodecs() = %v is not sorted-and-deduplicated", got)
		}
	}
	// Every family is really in there - the test above would pass over a one-element
	// set if the registry ever lost an encoder, and this is the case that matters:
	// hevc, av1 and h264 are ALL things this build writes.
	if len(got) != 3 || got[0] != "av1" || got[1] != "h264" || got[2] != "hevc" {
		t.Errorf("TargetCodecs() = %v, want exactly [av1 h264 hevc]", got)
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
	if c := Available(context.Background(), ffmpeg, ffprobe, spec, standIn(ffmpeg)); !c.EightBit || !c.TenBit {
		t.Errorf("Available(cpu/libx265) = %+v, want usable at 8 and 10 bits — libx265 always works", c)
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
	if c := Available(context.Background(), ffmpeg, ffprobe, spec, standIn(ffmpeg)); !c.EightBit || !c.TenBit {
		t.Errorf("Available(svtav1/libsvtav1) = %+v, want usable at 8 and 10 bits — libsvtav1 runs on CPU", c)
	}
}

// TestAvailable_BogusCodecIsFalse proves Available says no for a codec name
// ffmpeg has never heard of — the base case a naive exit-code check would also
// catch, kept here as the negative control for the two real-encoder positives
// above.
func TestAvailable_BogusCodecIsFalse(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec := Spec{Key: "bogus", FFmpegCodec: "definitely_not_a_codec", TargetCodec: "hevc"}
	if c := Available(context.Background(), ffmpeg, ffprobe, spec, standIn(ffmpeg)); c.Usable() {
		t.Errorf("Available(bogus codec) = %+v, want unusable", c)
	}
}

// TestRequireAvailable_UnknownKeyErrors proves an unknown encoder key fails loud
// with a clear error rather than silently doing nothing.
func TestRequireAvailable_UnknownKeyErrors(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	if _, _, err := RequireAvailable(context.Background(), ffmpeg, ffprobe, "not_a_real_encoder", standIn(ffmpeg)); err == nil {
		t.Error("RequireAvailable(unknown key) = nil error, want an error")
	}
}

// TestRequireAvailable_CPUSucceeds proves the happy path: a known, working
// encoder resolves cleanly with no error.
func TestRequireAvailable_CPUSucceeds(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, _, err := RequireAvailable(context.Background(), ffmpeg, ffprobe, "cpu", standIn(ffmpeg))
	if err != nil {
		t.Fatalf("RequireAvailable(cpu): %v", err)
	}
	if spec.TargetCodec != "hevc" {
		t.Errorf("spec.TargetCodec = %q, want hevc", spec.TargetCodec)
	}
}

// TestBetterFamily_RanksH264BelowHEVCBelowAV1: a source in a family ranked above the target is
// better (H.264 < HEVC < AV1, h265 read as hevc); an equal family, a worse one, and a codec
// this build does not write are not - so a codec with no rank is decided as it always was.
func TestBetterFamily_RanksH264BelowHEVCBelowAV1(t *testing.T) {
	for _, c := range []struct {
		codec, target string
		want          bool
	}{
		{"hevc", "h264", true}, {"h265", "h264", true}, {"av1", "h264", true}, {"av1", "hevc", true},
		{"h264", "h264", false}, {"hevc", "hevc", false}, {"av1", "av1", false},
		{"h264", "hevc", false}, {"h264", "av1", false}, {"hevc", "av1", false}, {"h265", "av1", false},
		{"mpeg2video", "h264", false}, {"vp9", "h264", false}, {"vc1", "hevc", false}, {"", "h264", false},
		{"av1", "", false}, {"av1", "vp9", false},
	} {
		if got := BetterFamily(c.codec, c.target); got != c.want {
			t.Errorf("BetterFamily(%q, %q) = %v, want %v", c.codec, c.target, got, c.want)
		}
	}
}

// TestSoftwareFallback_IsTheSoftwareEncoderOfTheSameCodec: every hardware encoder falls back to
// the software encoder that writes its codec - so a fallback never changes a job's target
// codec - and a software encoder is its own.
func TestSoftwareFallback_IsTheSoftwareEncoderOfTheSameCodec(t *testing.T) {
	want := map[string]string{"hevc": "cpu", "av1": "svtav1", "h264": "x264"}
	for _, key := range Known() {
		spec, _ := Lookup(key)
		fb := SoftwareFallback(spec)
		if !spec.Hardware {
			if fb.Key != key {
				t.Errorf("SoftwareFallback(%s) = %s, want itself", key, fb.Key)
			}
			continue
		}
		if fb.Key != want[spec.TargetCodec] || fb.Hardware || fb.TargetCodec != spec.TargetCodec {
			t.Errorf("SoftwareFallback(%s) = %+v, want %s", key, fb, want[spec.TargetCodec])
		}
	}
}

// TestRegistry_T27Encoders: the eight encoders decided by the owner (T27) are registry keys (or,
// for libx264, the alias of one), each writing its codec through its vendor's API, and every
// hardware encoder names an API while no software one does.
func TestRegistry_T27Encoders(t *testing.T) {
	for _, c := range []struct{ name, key, codec, ffmpeg, api string }{
		{"libx264", "x264", "h264", "libx264", ""},
		{"h264_nvenc", "h264_nvenc", "h264", "h264_nvenc", APINVENC},
		{"h264_qsv", "h264_qsv", "h264", "h264_qsv", APIQSV},
		{"h264_vaapi", "h264_vaapi", "h264", "h264_vaapi", APIVAAPI},
		{"h264_amf", "h264_amf", "h264", "h264_amf", APIAMF},
		{"av1_qsv", "av1_qsv", "av1", "av1_qsv", APIQSV},
		{"av1_vaapi", "av1_vaapi", "av1", "av1_vaapi", APIVAAPI},
		{"av1_amf", "av1_amf", "av1", "av1_amf", APIAMF},
	} {
		spec, ok := Lookup(c.name)
		if !ok || !Valid(c.name) {
			t.Errorf("%s is not accepted", c.name)
			continue
		}
		if spec.Key != c.key || spec.TargetCodec != c.codec || spec.FFmpegCodec != c.ffmpeg || spec.API != c.api ||
			spec.Hardware != (c.api != "") {
			t.Errorf("Lookup(%s) = %+v, want key %s codec %s ffmpeg %s api %q", c.name, spec, c.key, c.codec, c.ffmpeg, c.api)
		}
	}
	for _, key := range Known() {
		spec, _ := Lookup(key)
		if spec.Hardware != (spec.API != "") {
			t.Errorf("%s: Hardware %v with API %q", key, spec.Hardware, spec.API)
		}
	}
}
