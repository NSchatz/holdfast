package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// AC-A6 and AC-A7's unhappy path: the encoder a matching profile selects is the one that
// has to work on this host, and a run whose profiles reach an encoder this host cannot
// run must stop before it touches a file rather than fail those files one at a time.
//
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

// s0165Tree lists every path under dir with its size, so a test can assert that a refused
// start created, renamed and removed nothing under a library root.
func s0165Tree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		fmt.Fprintf(&b, "%s %d\n", p, info.Size())
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return b.String()
}

// [S0165 AC-9] IF a rule names an encoder that is not available on the host, `holdfast run`
// exits non-zero before the job store is opened, naming that encoder, the library root and
// the rule index, leaving no jobs.db and nothing created, renamed or removed under the
// library root - while the root's own encoder and every encode profile's are available.
//
// Graded against the stub ffmpeg the test above uses, in which libsvtav1 alone fails, so no
// real device is involved. The control arm is the same configuration with the rule's
// encoder removed: it starts, which is what proves the refusal came from the rule.
//
// MUTATION: drop the rule loop from buildEngine and the run starts (exit 0) over an encoder
// every file in the band would then fail on.
func TestS0165_AC9_ARuleEncoderUnavailableOnTheHostStopsTheRunBeforeTheStore(t *testing.T) {
	requireWorkingEncoder(t)
	real, err := exec.LookPath(envOr("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Skipf("ffmpeg not on PATH: %v", err)
	}
	stub := filepath.Join(t.TempDir(), "ffmpeg-without-av1")
	script := "#!/bin/sh\nfor a in \"$@\"; do\n  if [ \"$a\" = libsvtav1 ]; then exit 1; fi\ndone\nexec " +
		real + " \"$@\"\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOLDFAST_FFMPEG", stub)

	lib := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "ep.mkv"), []byte("not really a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := s0165Tree(t, lib)

	run := func(t *testing.T, rule string) (int, string, string) {
		t.Helper()
		state := filepath.Join(t.TempDir(), "state")
		p := filepath.Join(t.TempDir(), "config.yaml")
		body := "state_dir: " + state + "\nvmaf_enable: false\nencoder: cpu\n" +
			"encode_profiles:\n  - name: films\n    match: '**/films/**'\n    encoder: cpu\n" +
			"library_roots:\n  - path: " + lib + "\n    rules:\n" +
			"      - when:\n          max_source_height: 576\n        crf: 30\n" + rule
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		var out, errOut bytes.Buffer
		code := dispatch([]string{"run", "--config", p}, &out, &errOut)
		return code, errOut.String() + out.String(), state
	}

	code, said, state := run(t, "      - when:\n          max_source_height: 1080\n        encoder: svtav1\n")
	if code == 0 {
		t.Fatalf("run exited 0 with a rule naming an encoder this host cannot use:\n%s", said)
	}
	for _, want := range []string{`encoder "svtav1"`, "not available", "library root " + lib, "rules[1]"} {
		if !strings.Contains(said, want) {
			t.Errorf("the refusal does not carry %q:\n%s", want, said)
		}
	}
	if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
		t.Error("a refused run left a job store behind")
	}
	if after := s0165Tree(t, lib); after != before {
		t.Errorf("a refused run changed the library root:\nbefore:\n%s\nafter:\n%s", before, after)
	}

	// Control: the same configuration with the rule's encoder taken out starts.
	if code, said, _ := run(t, "      - when:\n          max_source_height: 1080\n        crf: 28\n"); code != 0 ||
		strings.Contains(said, "not available") {
		t.Errorf("the control run exited %d:\n%s", code, said)
	}
}

// [S0165 AC-7, AC-10, AC-16 at the command] The configurations those criteria refuse stop
// `holdfast validate` and `holdfast run` alike with a non-zero exit, and nothing is created,
// renamed or removed under the library root (no jobs.db either). The refusal wording is
// graded in internal/config/rules_test.go; this is the exit code and the untouched tree.
//
// MUTATION: let Validate accept any of them and both commands exit 0.
func TestS0165_AC7_AC10_AC16_ARefusedRuleStopsValidateAndRunAndTouchesNothing(t *testing.T) {
	lib := filepath.Join(t.TempDir(), "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lib, "ep.mkv"), []byte("not really a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := s0165Tree(t, lib)
	for name, tc := range map[string]struct{ root, rule, want string }{
		"AC-7 a VMAF floor in a rule":           {"", "        min_vmaf: 70\n", "min_vmaf"},
		"AC-10 a rule encoder under remux_only": {"    remux_only: true\n", "        encoder: svtav1\n", "remux_only"},
		"AC-16 a ceiling into another codec's band": {"    encoder: cpu\n",
			"        encoder: svtav1\n        max_height: 480\n", "max_height 480"},
	} {
		t.Run(name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			p := filepath.Join(t.TempDir(), "config.yaml")
			body := "state_dir: " + state + "\nvmaf_enable: false\nlibrary_roots:\n  - path: " + lib + "\n" +
				tc.root + "    rules:\n      - when:\n          min_source_height: 481\n" + tc.rule
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, cmd := range []string{"validate", "run"} {
				var out, errOut bytes.Buffer
				code := dispatch([]string{cmd, "--config", p}, &out, &errOut)
				if code == 0 {
					t.Errorf("%s exited 0 over a refused configuration:\n%s%s", cmd, errOut.String(), out.String())
				}
				if !strings.Contains(errOut.String(), tc.want) {
					t.Errorf("%s's refusal does not name %q:\n%s", cmd, tc.want, errOut.String())
				}
			}
			if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
				t.Error("a refused start left a job store behind")
			}
			if after := s0165Tree(t, lib); after != before {
				t.Errorf("a refused start changed the library root:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}
