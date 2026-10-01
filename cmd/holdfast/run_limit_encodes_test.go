package main

// `holdfast run --limit-encodes N` at the COMMAND surface (S0174): its refusals, its
// precedence under --file, and the help text a caller discovers it from. What the bound
// counts is graded in the engine (internal/engine/limit_encodes_test.go). Each case names
// the criterion it grades (testing T1).

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/corpus"
)

// TestRunLimitEncodes_AC7_RefusesAValueThatIsNotACount grades AC-7: a --limit-encodes that
// is not an integer of at least 1 exits 2, names the flag and the typed value on stderr, and
// creates nothing under the state directory. A typed empty value is refused too.
func TestRunLimitEncodes_AC7_RefusesAValueThatIsNotACount(t *testing.T) {
	cfgPath, _, state := boundedLayout(t, "")
	for _, value := range []string{"0", "-3", "abc", "1.5", " ", ""} {
		t.Run("--limit-encodes "+strconv.Quote(value), func(t *testing.T) {
			var out, errOut bytes.Buffer
			code := dispatch([]string{"run", "--config", cfgPath, "--limit-encodes", value}, &out, &errOut)
			if code != exitUsage || exitUsage != 2 {
				t.Fatalf("run --limit-encodes %q exited %d, want 2 (stderr: %s)", value, code, errOut.String())
			}
			if !strings.Contains(errOut.String(), "--limit-encodes") {
				t.Errorf("the refusal did not name the flag:\n%s", errOut.String())
			}
			if !strings.Contains(errOut.String(), strconv.Quote(value)) {
				t.Errorf("the refusal did not name the typed value %q:\n%s", value, errOut.String())
			}
			mustNotExist(t, state)
		})
	}
}

// TestRunLimitEncodes_AC9_FileWinsAsItDoesOverLimit grades AC-9: --file with --limit-encodes
// is accepted and carries exactly that one file, the precedence --file has over --limit.
func TestRunLimitEncodes_AC9_FileWinsAsItDoesOverLimit(t *testing.T) {
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg is required here: a bounded run that never ran proves nothing: %v", err)
	}
	cfgPath, lib, _ := boundedLayout(t, "min_bitrate_kbps: 0\nvmaf_enable: false\n")
	target := h264Fixture(t, ffmpeg, filepath.Join(lib, "b_target.mkv"))
	others := []string{
		h264Fixture(t, ffmpeg, filepath.Join(lib, "a_other.mkv")),
		h264Fixture(t, ffmpeg, filepath.Join(lib, "c_other.mkv")),
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--config", cfgPath, "--file", target, "--limit-encodes", "3"}, &out, &errOut); code != exitOK {
		t.Fatalf("run --file with --limit-encodes exited %d, want %d (stderr: %s)", code, exitOK, errOut.String())
	}
	if codec := videoCodec(t, target); codec == "h264" {
		t.Errorf("the named file was not carried to an outcome: %s is still %s", target, codec)
	}
	for _, other := range others {
		if codec := videoCodec(t, other); codec != "h264" {
			t.Errorf("a second file was processed under a --file bound: %s is now %s", other, codec)
		}
	}
}

// proofExample is the runnable proving-pass example the help and the README carry.
const proofExample = "holdfast run --config config.yaml --queue-order smallest --limit-encodes 5"

// TestRunHelp_AC16_ListsLimitEncodesQueueOrderAndTheProvingPass grades AC-16: `run --help`
// prints on stdout and exits 0, lists both new flags with their meanings and every accepted
// order value, carries the proving-pass example, and its code-2 row names both new flags.
func TestRunHelp_AC16_ListsLimitEncodesQueueOrderAndTheProvingPass(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := dispatch([]string{"run", "--help"}, &out, &errOut); code != exitOK {
		t.Fatalf("run --help exited %d, want %d", code, exitOK)
	}
	help := out.String()
	flat := strings.Join(strings.Fields(help), " ")
	for _, want := range []string{
		"-limit-encodes <n>", "reached an encode",
		"-queue-order <order>", "for this run only",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("the help does not carry %q:\n%s", want, help)
		}
	}
	// Every accepted order, read from the key's closed set rather than restated here.
	for _, order := range config.QueueOrders {
		if !strings.Contains(help, order) {
			t.Errorf("the help does not name the accepted order %q:\n%s", order, help)
		}
	}
	if !strings.Contains(help, config.QueueOrderList()) {
		t.Errorf("the help does not render the accepted set %s:\n%s", config.QueueOrderList(), help)
	}
	if !strings.Contains(help, proofExample) {
		t.Errorf("the help does not carry the runnable example %q:\n%s", proofExample, help)
	}
	var usage string
	for _, c := range runExitCodes {
		if c.Code == 2 {
			usage = c.Meaning
		}
	}
	for _, flagName := range []string{"--limit-encodes", "--queue-order"} {
		if !strings.Contains(usage, flagName) {
			t.Errorf("the code-2 row %q does not name %s", usage, flagName)
		}
	}
	if !strings.Contains(help, "2  "+usage) {
		t.Errorf("the help does not carry the code-2 row:\n%s", help)
	}
}

// TestReadme_AC16_CarriesTheProvingPass holds the README's proving-pass example line in step
// with the help's (S0174 T3).
func TestReadme_AC16_CarriesTheProvingPass(t *testing.T) {
	root, err := corpus.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), proofExample) {
		t.Errorf("README.md does not carry the proving-pass example %q", proofExample)
	}
}
