package encoder

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/version"
)

// standIn is an EncodeFunc for this package's own tests: a plain software encode at the
// probe's pixel format. It is NOT the probe production runs - that is engine.ProbeEncode,
// the job's own derivation and command line, graded in internal/engine - so what these
// tests grade is Available's reading of an output, not the argv a job builds.
func standIn(ffmpeg string) EncodeFunc {
	return func(ctx context.Context, spec Spec, pixelFormat, src, out string) error {
		return exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
			"-i", src, "-c:v", spec.FFmpegCodec, "-pix_fmt", pixelFormat, "--", out).Run()
	}
}

// recording counts the probe's encodes and the formats it asked for.
type recording struct {
	mu      sync.Mutex
	formats []string
}

func (r *recording) wrap(inner EncodeFunc) EncodeFunc {
	return func(ctx context.Context, spec Spec, pixelFormat, src, out string) error {
		r.mu.Lock()
		r.formats = append(r.formats, pixelFormat)
		r.mu.Unlock()
		return inner(ctx, spec, pixelFormat, src, out)
	}
}

func TestAvailable_ProbesAt8And10BitsFromALosslessClipOfThatFormat(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, _ := Lookup("cpu")
	var rec recording
	var sources []string
	check := func(ctx context.Context, spec Spec, pixelFormat, src, out string) error {
		sources = append(sources, src)
		// The clip handed over is exactly the format the probe asks for, losslessly.
		got, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=codec_name,pix_fmt", "-of", "csv=p=0", src).Output()
		if err != nil {
			t.Fatalf("probing the probe clip: %v", err)
		}
		if want := "ffv1," + pixelFormat; strings.TrimSpace(string(got)) != want {
			t.Errorf("probe clip = %q, want %q", strings.TrimSpace(string(got)), want)
		}
		return standIn(ffmpeg)(ctx, spec, pixelFormat, src, out)
	}
	c := Available(context.Background(), ffmpeg, ffprobe, spec, rec.wrap(check))
	if strings.Join(rec.formats, " ") != "yuv420p yuv420p10le" {
		t.Errorf("probe formats = %v, want [yuv420p yuv420p10le]", rec.formats)
	}
	if !c.EightBit || !c.TenBit || c.Reason != "" || c.Key != "cpu" {
		t.Errorf("Available(cpu) = %+v", c)
	}
	for _, src := range sources {
		if _, err := os.Stat(src); err == nil {
			t.Errorf("the probe left %s behind", src)
		}
	}
}

// The failure the old probe could not see: a VAAPI command line that uploads 10-bit frames as
// 8-bit surfaces writes a real HEVC file, so a codec-only check passed it. The 10-bit probe
// reads the output's depth and refuses it.
func TestAvailable_AnEncodeThatCutsTenBitsToEightFailsTheTenBitProbe(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, _ := Lookup("cpu")
	cutsDepth := func(ctx context.Context, spec Spec, pixelFormat, src, out string) error {
		return standIn(ffmpeg)(ctx, spec, "yuv420p", src, out)
	}
	c := Available(context.Background(), ffmpeg, ffprobe, spec, cutsDepth)
	if !c.EightBit || c.TenBit {
		t.Fatalf("Available = %+v, want 8-bit only", c)
	}
	if !c.Usable() {
		t.Error("an encoder usable at 8 bits is not Usable")
	}
	for _, want := range []string{"10-bit", `"yuv420p"`, "not 10-bit"} {
		if !strings.Contains(c.Reason, want) {
			t.Errorf("Reason %q does not name %q", c.Reason, want)
		}
	}
	if strings.Contains(c.Reason, "8-bit:") {
		t.Errorf("Reason %q names the depth that passed", c.Reason)
	}
}

func TestAvailable_AnOutputOfAnotherCodecIsNotTheEncoder(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, _ := Lookup("cpu")
	wrongCodec := func(ctx context.Context, _ Spec, pixelFormat, src, out string) error {
		av1, _ := Lookup("svtav1")
		return standIn(ffmpeg)(ctx, av1, pixelFormat, src, out)
	}
	c := Available(context.Background(), ffmpeg, ffprobe, spec, wrongCodec)
	if c.Usable() {
		t.Fatalf("Available = %+v, want unusable", c)
	}
	if !strings.Contains(c.Reason, `codec is "av1", not hevc`) {
		t.Errorf("Reason = %q", c.Reason)
	}
}

// A hardware encoder with no device can exit 0 and write nothing; one that fails writes
// nothing either. Neither is available, and the reason says which.
func TestAvailable_NoOutputIsUnavailableWhateverTheExitCode(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, _ := Lookup("nvenc")
	silent := func(context.Context, Spec, string, string, string) error { return nil }
	c := Available(context.Background(), ffmpeg, ffprobe, spec, silent)
	if c.Usable() || !strings.Contains(c.Reason, "wrote no output") {
		t.Errorf("exit-0-no-output: %+v", c)
	}
	failing := func(context.Context, Spec, string, string, string) error {
		return errors.New("\n  Device creation failed: -5\nmore detail")
	}
	c = Available(context.Background(), ffmpeg, ffprobe, spec, failing)
	if c.Usable() || !strings.Contains(c.Reason, "the encode failed: Device creation failed: -5") ||
		strings.Contains(c.Reason, "more detail") {
		t.Errorf("failing: %+v", c)
	}
	empty := func(_ context.Context, _ Spec, _ string, _ string, out string) error {
		return os.WriteFile(out, nil, 0o644)
	}
	if c = Available(context.Background(), ffmpeg, ffprobe, spec, empty); c.Usable() {
		t.Errorf("an empty output file is available: %+v", c)
	}
}

func TestAvailable_AProbeClipThatCannotBeMadeIsUnavailable(t *testing.T) {
	_, ffprobe := tools(t)
	spec, _ := Lookup("cpu")
	var rec recording
	c := Available(context.Background(), "/nonexistent/ffmpeg", ffprobe, spec,
		rec.wrap(func(context.Context, Spec, string, string, string) error { return nil }))
	if c.Usable() || !strings.Contains(c.Reason, "cannot make the probe clip") || len(rec.formats) != 0 {
		t.Errorf("Available with no ffmpeg = %+v, encodes %v", c, rec.formats)
	}
}

// P3, approved at Checkpoint T: `amf` in the image is refused, before anything runs, with
// the reason named; outside the image it is probed as before. It is never read as `vaapi`.
func TestAvailable_AMFInTheImageIsRefusedWithTheReasonAndNeverProbed(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	amf, _ := Lookup("amf")
	orig := version.Packaging
	t.Cleanup(func() { version.Packaging = orig })

	version.Packaging = version.PackagingImage
	var rec recording
	c := Available(context.Background(), ffmpeg, ffprobe, amf,
		rec.wrap(func(context.Context, Spec, string, string, string) error { return nil }))
	if c.Usable() || len(rec.formats) != 0 {
		t.Fatalf("amf in the image: %+v, probed %v", c, rec.formats)
	}
	for _, want := range []string{`"amf"`, "container image", "AMDGPU PRO EULA", "redistribute",
		`"encoder: vaapi"`, "docs/design/hardware.md#amf"} {
		if !strings.Contains(c.Reason, want) {
			t.Errorf("reason %q does not name %q", c.Reason, want)
		}
	}
	_, _, err := RequireAvailable(context.Background(), ffmpeg, ffprobe, "amf",
		rec.wrap(func(context.Context, Spec, string, string, string) error { return nil }))
	if !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "AMDGPU PRO EULA") || len(rec.formats) != 0 {
		t.Errorf("RequireAvailable(amf) in the image = %v, probed %v", err, rec.formats)
	}
	// Only the AMF encoders: every other encoder is probed in the image as anywhere else, and
	// each AMF encoder's refusal names the VAAPI encoder of its own codec.
	for _, key := range Known() {
		spec, _ := Lookup(key)
		if spec.API != APIAMF && RefusedInImage(spec) != "" {
			t.Errorf("%s is refused in the image", key)
		}
	}
	for key, sibling := range map[string]string{"h264_amf": "h264_vaapi", "av1_amf": "av1_vaapi"} {
		spec, _ := Lookup(key)
		why := RefusedInImage(spec)
		if !strings.Contains(why, `encoder "`+key+`"`) || !strings.Contains(why, `"encoder: `+sibling+`"`) ||
			!strings.Contains(why, "AMDGPU PRO EULA") {
			t.Errorf("RefusedInImage(%s) in the image = %q, want its key, the EULA and %s", key, why, sibling)
		}
	}
	// amf is still a key: it resolves to itself (hevc_amf), never to vaapi.
	if spec, ok := Lookup("amf"); !ok || spec.Key != "amf" || spec.FFmpegCodec != "hevc_amf" {
		t.Errorf("Lookup(amf) = %+v, %v", spec, ok)
	}
	if spec, _ := Lookup("hevc_amf"); spec.Key != "amf" {
		t.Errorf("Lookup(hevc_amf) = %+v", spec)
	}

	version.Packaging = ""
	rec = recording{}
	c = Available(context.Background(), ffmpeg, ffprobe, amf,
		rec.wrap(func(context.Context, Spec, string, string, string) error { return nil }))
	if len(rec.formats) != 2 || strings.Contains(c.Reason, "EULA") {
		t.Errorf("amf outside the image was not probed: %+v, probed %v", c, rec.formats)
	}
	if RefusedInImage(amf) != "" {
		t.Error("amf refused outside the image")
	}
}

func TestRequireAvailable_AnUnusableEncoderNamesTheReason(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, c, err := RequireAvailable(context.Background(), ffmpeg, ffprobe, "nvenc",
		func(context.Context, Spec, string, string, string) error { return errors.New("no CUDA device") })
	if !errors.Is(err, ErrUnavailable) || spec.Key != "nvenc" || c.Usable() {
		t.Fatalf("RequireAvailable = %+v, %+v, %v", spec, c, err)
	}
	if !strings.Contains(err.Error(), `encoder "nvenc"`) || !strings.Contains(err.Error(), "no CUDA device") {
		t.Errorf("error %q does not carry the key and the reason", err)
	}
}

func TestCapability_Carries(t *testing.T) {
	both := Capability{EightBit: true, TenBit: true}
	eight := Capability{EightBit: true}
	ten := Capability{TenBit: true}
	cases := []struct {
		c    Capability
		fmt  string
		want bool
	}{
		{both, "yuv420p", true}, {both, "yuv420p10le", true},
		{eight, "yuv420p", true}, {eight, "yuv420p10le", false}, {eight, "p010le", false},
		{ten, "yuv420p", false}, {ten, "yuv420p10le", true}, {ten, "yuv444p12le", true},
		{ten, "nv12", false}, {eight, "nv12", true},
		{both, "not-a-format", false}, {both, "", false},
	}
	for _, tc := range cases {
		if got := tc.c.Carries(tc.fmt); got != tc.want {
			t.Errorf("%+v.Carries(%q) = %v, want %v", tc.c, tc.fmt, got, tc.want)
		}
	}
	if (Capability{}).Usable() || !eight.Usable() || !ten.Usable() {
		t.Error("Usable is not EightBit || TenBit")
	}
}

// `auto` is a value, not a registry key: Valid accepts it, Lookup does not resolve it, its
// target codec is HEVC, and every encoder it may choose - and the software encoder it falls
// back to - writes HEVC, so a file's target codec does not depend on the host's hardware.
func TestAuto_IsAValueEveryChoiceOfWhichWritesHEVC(t *testing.T) {
	if !Valid(Auto) || !Valid("cpu") || !Valid("hevc_vaapi") || Valid("automatic") || Valid("") {
		t.Error("Valid disagrees with the registry plus auto")
	}
	if _, ok := Lookup(Auto); ok {
		t.Error("Lookup resolves auto, which is not an encoder a job can run")
	}
	if codec, ok := TargetCodecOf(Auto); !ok || codec != "hevc" || AutoTargetCodec != "hevc" {
		t.Errorf("TargetCodecOf(auto) = %q, %v", codec, ok)
	}
	if codec, ok := TargetCodecOf("av1_nvenc"); !ok || codec != "av1" {
		t.Errorf("TargetCodecOf(av1_nvenc) = %q, %v", codec, ok)
	}
	if _, ok := TargetCodecOf("bogus"); ok {
		t.Error("TargetCodecOf(bogus) is ok")
	}
	if strings.Join(AutoOrder, " ") != "nvenc qsv vaapi amf" {
		t.Errorf("AutoOrder = %v", AutoOrder)
	}
	for _, key := range AutoOrder {
		spec, ok := Lookup(key)
		if !ok || !spec.Hardware || spec.TargetCodec != AutoTargetCodec {
			t.Errorf("auto may choose %s (%+v), which is not a hardware HEVC encoder", key, spec)
		}
		if fb := SoftwareFallback(spec); fb.Key != "cpu" {
			t.Errorf("SoftwareFallback(%s) = %s, want cpu", key, fb.Key)
		}
	}
	av1, _ := Lookup("av1_nvenc")
	if fb := SoftwareFallback(av1); fb.Key != "svtav1" {
		t.Errorf("SoftwareFallback(av1_nvenc) = %s, want svtav1", fb.Key)
	}
	for _, key := range []string{"cpu", "svtav1"} {
		spec, _ := Lookup(key)
		if fb := SoftwareFallback(spec); fb.Key != key {
			t.Errorf("SoftwareFallback(%s) = %s, want itself", key, fb.Key)
		}
	}
	all := KnownWithAuto()
	if all[len(all)-1] != Auto || len(all) != len(Known())+1 {
		t.Errorf("KnownWithAuto = %v", all)
	}
}

// A failed encode is not a working encoder even where it left a faithful-looking file behind:
// the probe requires the encode to succeed AND its output to be faithful.
func TestAvailable_AnEncodeThatFailsIsUnavailableEvenWithAGoodOutput(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	spec, _ := Lookup("cpu")
	failsAfterWriting := func(ctx context.Context, spec Spec, pixelFormat, src, out string) error {
		if err := standIn(ffmpeg)(ctx, spec, pixelFormat, src, out); err != nil {
			t.Fatalf("the stand-in encode itself failed: %v", err)
		}
		return errors.New("exit status 1\nthe device went away")
	}
	c := Available(context.Background(), ffmpeg, ffprobe, spec, failsAfterWriting)
	if c.Usable() || !strings.Contains(c.Reason, "the encode failed: exit status 1") {
		t.Errorf("an encode that failed after writing a good file = %+v, want unusable", c)
	}
}
