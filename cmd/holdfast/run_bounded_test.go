package main

// The bounded run at the COMMAND surface (S0100): `holdfast run --file <path>` and
// `holdfast run --limit N`.
//
// What these cases grade is the door, never the pipeline behind it. A bounded run is a
// SMALLER run and not a lighter one, so every case here is about one of three things: the
// refusals the two flags add and the code they leave by, the startup sequence a bounded
// run still pays for in full, and the help text a caller discovers the surface from.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
)

// boundedLayout builds a library root, a state directory that does NOT exist yet, and a
// config naming both. The state directory's absence is load-bearing: a refusal that
// creates nothing is asserted by looking at the directory rather than by reading a log
// line (AC-2, AC-3).
func boundedLayout(t *testing.T, extra string) (cfgPath, lib, state string) {
	t.Helper()
	dir := t.TempDir()
	lib = filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(dir, "state")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, lib, state
}

// touch writes a placeholder file. Every case that uses one refuses BEFORE anything is
// probed or encoded, so the bytes never matter - only that a path exists and carries the
// name it carries.
func touch(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not really a video"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRunFile_RefusesAPathOutsideTheRoots grades AC-2: a `--file` target outside every
// configured library root refuses the WHOLE run before anything is probed, encoded or
// mutated, names the path and the rule that rejected it on stderr, and leaves by a code
// distinct from both the environment/startup code and the invocation-error code.
func TestRunFile_RefusesAPathOutsideTheRoots(t *testing.T) {
	cfgPath, _, state := boundedLayout(t, "")
	outside := touch(t, filepath.Join(filepath.Dir(state), "elsewhere", "film.mkv"))

	var out, errOut bytes.Buffer
	code := dispatch([]string{"run", "--config", cfgPath, "--file", outside}, &out, &errOut)
	if code != exitRefused {
		t.Fatalf("run --file outside the roots exited %d, want %d (stderr: %s)", code, exitRefused, errOut.String())
	}
	// Distinct from BOTH of the codes this command already spends, which is the whole
	// point of the criterion: a caller must be able to tell a policy refusal from a typo
	// and from a broken environment.
	if exitRefused == exitError || exitRefused == exitUsage {
		t.Fatalf("the refusal code %d collides with exitError=%d / exitUsage=%d", exitRefused, exitError, exitUsage)
	}
	if !strings.Contains(errOut.String(), outside) {
		t.Errorf("the refusal did not name the rejected path:\n%s", errOut.String())
	}
	if !strings.Contains(errOut.String(), engine.RuleOutsideRoots) {
		t.Errorf("the refusal did not name the rule that rejected it (%s):\n%s", engine.RuleOutsideRoots, errOut.String())
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("the refused run created the state directory")
	}
}

// TestRunFile_StillRunsTheStartupChecks grades AC-8: a bounded run performs the same
// start-or-refuse startup sequence an unbounded one does and refuses on the same
// conditions with the same code. Two of that sequence's steps are exercised here because
// they are the two a `--file` run could plausibly have been narrowed past: the filesystem
// classification decision (which AC-9 explicitly scopes) and the required-binary check.
func TestRunFile_StillRunsTheStartupChecks(t *testing.T) {
	t.Run("the filesystem classification still refuses", func(t *testing.T) {
		cfgPath, state := nasLayout(t, "")
		target := touch(t, filepath.Join(filepath.Dir(filepath.Dir(state)), "media", "film.mkv"))

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"run", "--config", cfgPath, "--file", target}, &out, &errOut); code != exitError {
			t.Fatalf("a bounded run over a state directory on network storage exited %d, want %d (stderr: %s)",
				code, exitError, errOut.String())
		}
		if _, err := os.Stat(state); err == nil {
			t.Error("the refused bounded run created the state directory")
		}
		if !strings.Contains(errOut.String(), state) {
			t.Errorf("the refusal did not name the path:\n%s", errOut.String())
		}
	})

	t.Run("the required-binary check still refuses", func(t *testing.T) {
		cfgPath, lib, state := boundedLayout(t, "")
		target := touch(t, filepath.Join(lib, "film.mkv"))
		t.Setenv("HOLDFAST_FFMPEG", "holdfast-no-such-encoder-binary")

		var out, errOut bytes.Buffer
		if code := dispatch([]string{"run", "--config", cfgPath, "--file", target}, &out, &errOut); code != exitError {
			t.Fatalf("a bounded run with the encoder binary missing exited %d, want %d (stderr: %s)",
				code, exitError, errOut.String())
		}
		if !strings.Contains(errOut.String(), "holdfast-no-such-encoder-binary") {
			t.Errorf("the refusal did not name the binary it could not find:\n%s", errOut.String())
		}
		if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
			t.Error("the refused bounded run opened the job store")
		}
	})
}
