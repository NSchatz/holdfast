package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The startup capability preflight, once a profile can name its own encoder.
//
// The check exists because Validate can only confirm an encoder KEY is known: a
// hardware encoder with no matching device, or an ffmpeg build missing a codec, must
// stop the run before any work rather than let every file fail one at a time - or, for
// some hardware encoders, appear to succeed while writing nothing. A preflight that
// asked about the top-level encoder alone would deliver that for part of an operator's
// library and not the rest.
//
// It is graded against a STUB ffmpeg rather than against this host's devices, so the
// answer does not depend on whether the machine running the suite happens to have an
// NVIDIA card. The stub refuses exactly one codec and passes everything else through to
// the real binary, which is what makes the three arms below one experiment: the same
// unavailable encoder, named at the top level, named in a profile, and named nowhere.
func TestPreflight_EveryEncoderTheConfigurationCanReachIsCheckedBeforeAnyWork(t *testing.T) {
	requireWorkingEncoder(t)
	real, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}

	// An ffmpeg in which libsvtav1 does not work: it exits non-zero without writing an
	// output, which is exactly what encoder.Available reads as unavailable (it never
	// trusts the exit code alone - it stats the output and ffprobes its codec).
	stub := filepath.Join(t.TempDir(), "ffmpeg-without-av1")
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = libsvtav1 ]; then exit 1; fi\ndone\nexec " +
		real + " \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		full := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(t.TempDir(), "state") +
			"\nvmaf_enable: false\n" + body
		if err := os.WriteFile(p, []byte(full), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	runWith := func(t *testing.T, cfgPath string) (int, string) {
		t.Helper()
		t.Setenv("HOLDFAST_FFMPEG", stub)
		var out, errOut bytes.Buffer
		code := dispatch([]string{"run", "--config", cfgPath}, &out, &errOut)
		return code, errOut.String() + out.String()
	}

	// The anti-vacuity arm, and it goes first: the stub really does make this encoder
	// unavailable, and the top-level refusal is the one the pin already wrote.
	t.Run("the top-level encoder, refused as it always was", func(t *testing.T) {
		code, said := runWith(t, write(t, "encoder: svtav1\n"))
		if code == 0 {
			t.Fatalf("run exited 0 with an unavailable top-level encoder:\n%s", said)
		}
		for _, want := range []string{`encoder "svtav1"`, "not available"} {
			if !strings.Contains(said, want) {
				t.Errorf("the refusal does not carry %q:\n%s", want, said)
			}
		}
		if strings.Contains(said, "encode_profiles") {
			t.Errorf("a top-level refusal blamed a profile:\n%s", said)
		}
	})

	// The finding itself: the top-level encoder works, so the pin's preflight passes and
	// the run starts - and then every 4K file fails one at a time, hours in.
	t.Run("a profile's encoder stops the run and the account names the profile", func(t *testing.T) {
		code, said := runWith(t, write(t, "encoder: cpu\n"+
			"encode_profiles:\n"+
			"  - name: 4k-av1\n"+
			"    match: '**/4K/**'\n"+
			"    encoder: svtav1\n"))
		if code == 0 {
			t.Fatalf("run exited 0 with a profile naming an encoder this host cannot use; every file "+
				"that profile matches would fail one at a time instead:\n%s", said)
		}
		for _, want := range []string{"encode_profiles", "4k-av1", `encoder "svtav1"`, "not available"} {
			if !strings.Contains(said, want) {
				t.Errorf("the refusal does not carry %q:\n%s", want, said)
			}
		}
		// Nothing was created: the preflight runs before the store is opened, which is
		// the placement the whole fail-early guarantee rests on.
		if _, err := os.Stat(filepath.Join(dir, "state", "jobs.db")); err == nil {
			t.Error("a refused run left a job store behind")
		}
	})

	// The control arm: a profile that overrides no encoder contributes no encoder to
	// check, so the presence of profiles is not what refuses anything.
	t.Run("control: a profile that overrides no encoder refuses nothing", func(t *testing.T) {
		code, said := runWith(t, write(t, "encoder: cpu\n"+
			"encode_profiles:\n"+
			"  - name: small-files\n"+
			"    match: 'small-*.mkv'\n"+
			"    crf: 30\n"))
		if code != 0 {
			t.Fatalf("run exited %d over an empty library root with a working encoder:\n%s", code, said)
		}
		if strings.Contains(said, "not available") {
			t.Errorf("the run reported an unavailable encoder for a profile that names none:\n%s", said)
		}
	})
}
