package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/version"
)

// hardwareRefusingFFmpeg is an ffmpeg stand-in in which every hardware encoder fails without
// writing anything (no device), and which logs each hardware command line it was handed.
// Everything else runs on the real binary, so the probe clip is made and software encoders
// work. No device is ever opened.
func hardwareRefusingFFmpeg(t *testing.T) (stub, log string) {
	t.Helper()
	real, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	dir := t.TempDir()
	stub, log = filepath.Join(dir, "ffmpeg"), filepath.Join(dir, "hardware.log")
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  case \"$a\" in\n" +
		"    hevc_nvenc|av1_nvenc|hevc_qsv|hevc_vaapi|hevc_amf) printf '%s\\n' \"$*\" >> " + log + "; exit 1 ;;\n" +
		"  esac\ndone\nexec " + real + " \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return stub, log
}

func hardwareConfig(t *testing.T, body string) (path, state string) {
	t.Helper()
	lib := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(t.TempDir(), "state")
	path = filepath.Join(t.TempDir(), "config.yaml")
	full := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\nvmaf_enable: false\n" + body
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, state
}

func dispatchWith(t *testing.T, ffmpeg string, args ...string) (int, string) {
	t.Helper()
	t.Setenv("HOLDFAST_FFMPEG", ffmpeg)
	var out, errOut bytes.Buffer
	code := dispatch(args, &out, &errOut)
	return code, errOut.String() + out.String()
}

// P3, approved at Checkpoint T: in the container image `encoder: amf` is refused at start,
// before anything is probed or opened, with the reason named (AMD's EULA grants no
// redistribution; use vaapi). `holdfast validate` still accepts the key, and outside the
// image the same configuration is probed as it always was.
func TestPreflight_AMFInTheImageIsRefusedAtStartWithTheNamedReason(t *testing.T) {
	requireWorkingEncoder(t)
	stub, log := hardwareRefusingFFmpeg(t)
	orig := version.Packaging
	t.Cleanup(func() { version.Packaging = orig })
	version.Packaging = version.PackagingImage

	cfgPath, state := hardwareConfig(t, "encoder: amf\n")
	if code, said := dispatchWith(t, stub, "validate", "--config", cfgPath); code != 0 {
		t.Fatalf("validate refused encoder: amf (it must still accept the key):\n%s", said)
	}
	code, said := dispatchWith(t, stub, "run", "--config", cfgPath)
	if code == 0 {
		t.Fatalf("run started with encoder: amf in the image:\n%s", said)
	}
	for _, want := range []string{`encoder "amf"`, "container image", "AMDGPU PRO EULA", "redistribute",
		`"encoder: vaapi"`} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not name %q:\n%s", want, said)
		}
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("amf was probed in the image, though its refusal needs no probe:\n%s", b)
	}
	if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
		t.Error("a refused run left a job store behind")
	}

	// Control: outside the image, amf is probed (twice: 8 and 10 bits) and refused for being
	// unavailable on this host, never for the licence.
	version.Packaging = ""
	code, said = dispatchWith(t, stub, "run", "--config", cfgPath)
	if code == 0 || strings.Contains(said, "EULA") || !strings.Contains(said, "not available") {
		t.Errorf("outside the image: exit %d:\n%s", code, said)
	}
	b, _ := os.ReadFile(log)
	if n := strings.Count(string(b), "hevc_amf"); n != 2 {
		t.Errorf("outside the image amf was probed %d times, want 2:\n%s", n, b)
	}
}

// An encoder that needs a render node and got none is refused with the reason beside the
// probe's own: a VAAPI failure that does not say the container has no /dev/dri sends an
// operator to the wrong place. This host has none (the suite never sees a render node).
func TestPreflight_AVAAPIRefusalSaysWhyTheEncoderHasNoRenderNode(t *testing.T) {
	requireWorkingEncoder(t)
	if _, err := os.Stat("/dev/dri"); err == nil {
		t.Skip("this host has /dev/dri; the no-node reason cannot be shown here")
	}
	stub, log := hardwareRefusingFFmpeg(t)
	cfgPath, _ := hardwareConfig(t, "encoder: vaapi\n")
	code, said := dispatchWith(t, stub, "run", "--config", cfgPath)
	if code == 0 {
		t.Fatalf("run started with an unavailable vaapi:\n%s", said)
	}
	for _, want := range []string{`encoder "vaapi"`, "not available", "render node: no render node under /dev/dri"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, said)
		}
	}
	// The probe ran the job's own command line: the default node, opened over DRM only.
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "-vaapi_device /dev/dri/renderD128,connection_type=drm") {
		t.Errorf("the VAAPI probe did not open its node with connection_type=drm:\n%s", b)
	}
}

// `encoder: auto` and hw_fallback at start, on a host where no hardware encoder works (every
// hardware codec is refused by the stand-in ffmpeg; no device is opened):
//
//   - under the default (skip), auto has nothing to choose and every file would be skipped,
//     so the run refuses to start and names the lever;
//   - under software, the run starts, and the probe tried every encoder auto may choose.
func TestPreflight_AutoWithNoUsableHardwareFollowsHWFallback(t *testing.T) {
	requireWorkingEncoder(t)
	stub, log := hardwareRefusingFFmpeg(t)

	skip, state := hardwareConfig(t, "encoder: auto\n")
	if code, said := dispatchWith(t, stub, "validate", "--config", skip); code != 0 {
		t.Fatalf("validate refused encoder: auto:\n%s", said)
	}
	code, said := dispatchWith(t, stub, "run", "--config", skip)
	if code == 0 {
		t.Fatalf("run started with encoder: auto, no usable hardware and hw_fallback skip:\n%s", said)
	}
	for _, want := range []string{"encoder: auto found no usable hardware encoder", "hw_fallback: software",
		"nvenc, qsv, vaapi, amf"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, said)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
		t.Error("a refused run left a job store behind")
	}
	b, _ := os.ReadFile(log)
	for _, codec := range []string{"hevc_nvenc", "hevc_qsv", "hevc_vaapi", "hevc_amf"} {
		if n := strings.Count(string(b), codec); n != 2 {
			t.Errorf("auto's probe ran %s %d times, want 2 (8 and 10 bits)", codec, n)
		}
	}

	software, _ := hardwareConfig(t, "encoder: auto\nhw_fallback: software\n")
	if code, said := dispatchWith(t, stub, "run", "--config", software); code != 0 {
		t.Fatalf("run refused encoder: auto under hw_fallback software:\n%s", said)
	}
}

// hw_fallback is per library: a root naming a hardware encoder that does not work refuses the
// start under skip, naming the root and the lever, and starts under software; the root's own
// value beats the top level's.
func TestPreflight_HWFallbackIsDecidedPerLibraryRoot(t *testing.T) {
	requireWorkingEncoder(t)
	stub, _ := hardwareRefusingFFmpeg(t)
	other := filepath.Join(t.TempDir(), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := func(fallback string) string {
		s := "  - path: " + other + "\n    encoder: vaapi\n"
		if fallback != "" {
			s += "    hw_fallback: " + fallback + "\n"
		}
		return s
	}
	write := func(top, root string) string {
		p, _ := hardwareConfig(t, top)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), "\nstate_dir:", "\n"+root+"state_dir:", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	code, said := dispatchWith(t, stub, "run", "--config", write("", entry("")))
	if code == 0 {
		t.Fatalf("a root on an unusable vaapi under the default skip started:\n%s", said)
	}
	for _, want := range []string{"library root " + other, `encoder "vaapi"`, "hw_fallback is skip",
		"hw_fallback: software", "with cpu instead"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, said)
		}
	}
	if code, said := dispatchWith(t, stub, "run", "--config", write("", entry("software"))); code != 0 {
		t.Errorf("the root's own hw_fallback: software did not let the run start:\n%s", said)
	}
	if code, said := dispatchWith(t, stub, "run", "--config", write("hw_fallback: software\n", entry("skip"))); code == 0 {
		t.Errorf("the root's own skip did not beat the top level's software:\n%s", said)
	}
	if code, said := dispatchWith(t, stub, "run", "--config", write("hw_fallback: software\n", entry(""))); code != 0 {
		t.Errorf("a root inheriting the top level's software did not start:\n%s", said)
	}
}

// A resolution rule's hardware encoder runs under its root's hw_fallback (S0165 with this
// goal's fallback): unusable under a skip root it refuses the start naming the root and the
// rule, and under a software root the run starts.
func TestPreflight_ARulesHardwareEncoderFollowsItsRootsHWFallback(t *testing.T) {
	requireWorkingEncoder(t)
	stub, _ := hardwareRefusingFFmpeg(t)
	lib := filepath.Join(t.TempDir(), "banded")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(fallback string) string {
		p, _ := hardwareConfig(t, "")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		entry := "  - path: " + lib + "\n    encoder: cpu\n" + fallback +
			"    rules:\n      - when:\n          min_source_height: 1081\n        encoder: vaapi\n"
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), "\nstate_dir:", "\n"+entry+"state_dir:", 1)), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	code, said := dispatchWith(t, stub, "run", "--config", write(""))
	if code == 0 {
		t.Fatalf("a rule on an unusable vaapi under a skip root started:\n%s", said)
	}
	for _, want := range []string{"library root " + lib + ": rules[0]", `encoder "vaapi"`, "hw_fallback is skip"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, said)
		}
	}
	if code, said := dispatchWith(t, stub, "run", "--config", write("    hw_fallback: software\n")); code != 0 {
		t.Errorf("a rule on an unusable vaapi under a software root did not start:\n%s", said)
	}
}
