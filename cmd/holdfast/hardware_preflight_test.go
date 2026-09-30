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
