package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/startup"
)

// The two scratch causes this file asserts on, named through the package that
// defines them rather than copied as string literals - so a rename moves the
// assertion with the vocabulary instead of leaving it green against a token nothing
// emits any more.
const (
	startupScratchMissing  = startup.CauseScratchMissing
	startupScratchOverlaps = startup.CauseScratchOverlaps
)

// AC-A11: `holdfast run -h`, `holdfast serve -h` and `holdfast validate -h` list
// EXACTLY the flags the pin lists, so every setting S0079 adds is reachable from the
// config file and its existing HOLDFAST_* environment override, and from nowhere
// else.
//
// "Without CLI overrides" in the origin note is not a preference about ergonomics.
// The configuration is the tool's declared state and the YAML file is the source of
// truth: a flag that could override an encoder, a quality target or a working
// location would be a way to run a library under settings nothing in git records,
// on a tool that deletes originals.
//
// The flag names are read out of the command's OWN help output rather than from a
// list held here, so a flag added to any of the three appears in this comparison the
// moment it exists.
func TestFlags_RunServeAndValidateListExactlyTheFlagsThePinListed(t *testing.T) {
	// The pin's flag set for all three: --config, and nothing else. loadConfig is
	// the single place it is declared, which is why all three agree.
	want := []string{"config"}

	for _, cmd := range []string{"run", "serve", "validate"} {
		t.Run(cmd, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if code := dispatch([]string{cmd, "-h"}, &out, &errOut); code != 0 {
				t.Fatalf("%s -h exited %d (stderr: %s)", cmd, code, errOut.String())
			}
			got := flagNames(errOut.String() + out.String())
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("%s -h lists %v, want exactly %v - a setting this item adds became reachable from the command line\n%s",
					cmd, got, want, errOut.String())
			}
		})
	}

	// The anti-vacuity arm. A parser that found nothing would report an empty set
	// for every command and agree with an empty expectation, so it is shown here
	// finding a flag that really is declared: `export` carries --out beside
	// --config, and the same parser must see both.
	t.Run("the parser finds a flag that is really there", func(t *testing.T) {
		var out, errOut bytes.Buffer
		if code := dispatch([]string{"export", "-h"}, &out, &errOut); code != 0 {
			t.Fatalf("export -h exited %d (stderr: %s)", code, errOut.String())
		}
		got := flagNames(errOut.String() + out.String())
		if strings.Join(got, ",") != "config,out" {
			t.Fatalf("export -h parsed as %v, want [config out] - the parser above cannot see flags", got)
		}
	})
}

// The new settings are reachable from the environment, which is the other half of
// "from the config file and its existing HOLDFAST_* override, and from nowhere
// else": a criterion that only said "no flags" would be satisfied by a setting that
// could not be reached at all.
func TestFlags_TheNewSettingsAreReachableFromTheEnvironment(t *testing.T) {
	t.Setenv("HOLDFAST_BITRATE_KBPS", "4500")
	t.Setenv("HOLDFAST_SCRATCH_MIN_FREE_GB", "7")

	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("library_roots:\n  - "+lib+"\nstate_dir: "+filepath.Join(dir, "state")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d (stderr: %s)", code, errOut.String())
	}
	// A positive bitrate is announced as a note, which is the observable that says
	// the environment override reached the loaded configuration.
	if !strings.Contains(out.String(), "bitrate_kbps") {
		t.Fatalf("HOLDFAST_BITRATE_KBPS did not reach the configuration:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "note:") {
		t.Fatalf("the bitrate announcement is not a note:\n%s", out.String())
	}
}

// AC-B7's second clause: `holdfast validate` reports the SAME cause `run` would
// refuse on, so an operator asking whether their configuration will start gets that
// answer rather than a "config OK" the next run contradicts.
func TestValidate_ReportsTheScratchDirectoryCausesRunWouldRefuseOn(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		full := "library_roots:\n  - " + lib + "\nstate_dir: " + filepath.Join(dir, "state") + "\n" + body
		if err := os.WriteFile(p, []byte(full), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("a scratch directory that does not exist", func(t *testing.T) {
		missing := filepath.Join(dir, "nowhere")
		cfgPath := write(t, "scratch_dir: "+missing+"\n")
		var out, errOut bytes.Buffer
		code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut)
		if code == 0 {
			t.Fatalf("validate exited 0 for a scratch directory that does not exist:\n%s", out.String())
		}
		for _, want := range []string{missing, string(startupScratchMissing), "remedy:"} {
			if !strings.Contains(errOut.String(), want) {
				t.Errorf("the account does not carry %q:\n%s", want, errOut.String())
			}
		}
	})

	t.Run("a scratch directory that overlaps the library root", func(t *testing.T) {
		inside := filepath.Join(lib, "work")
		if err := os.MkdirAll(inside, 0o755); err != nil {
			t.Fatal(err)
		}
		cfgPath := write(t, "scratch_dir: "+inside+"\n")
		var out, errOut bytes.Buffer
		if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code == 0 {
			t.Fatalf("validate exited 0 for a scratch directory inside a library root:\n%s", out.String())
		}
		if !strings.Contains(errOut.String(), string(startupScratchOverlaps)) {
			t.Errorf("the account does not name the overlap cause:\n%s", errOut.String())
		}
	})

	// The control arm: a scratch directory that is fine leaves validate saying
	// exactly what it said before, exit 0 and all.
	t.Run("control: a usable scratch directory validates", func(t *testing.T) {
		ok := filepath.Join(dir, "scratch")
		if err := os.MkdirAll(ok, 0o755); err != nil {
			t.Fatal(err)
		}
		cfgPath := write(t, "scratch_dir: "+ok+"\nscratch_min_free_gb: 0\n")
		var out, errOut bytes.Buffer
		if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
			t.Fatalf("validate exited %d for a usable scratch directory (stderr: %s)", code, errOut.String())
		}
		if !strings.Contains(out.String(), "config OK") {
			t.Fatalf("validate did not report the configuration as OK:\n%s", out.String())
		}
		// And it left nothing behind: the writability probe creates one file and
		// removes it.
		ents, err := os.ReadDir(ok)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != 0 {
			t.Fatalf("validate left %d file(s) in the scratch directory", len(ents))
		}
	})
}

var flagLine = regexp.MustCompile(`(?m)^\s+-([A-Za-z0-9][A-Za-z0-9_.-]*)`)

// flagNames extracts the flag names from a flag package usage dump, sorted.
func flagNames(usage string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range flagLine.FindAllStringSubmatch(usage, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}
