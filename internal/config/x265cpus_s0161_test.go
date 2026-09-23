package config

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestS0161_AC9_AnX265CPUsOutsideItsRangeRefusesToLoad grades AC-9's loading half: a value
// outside 0..1024, or one that is not a whole number, fails Load itself - before anything
// is built from the configuration - with a message naming the key, from the file and from
// the HOLDFAST_* environment layer alike. Validate refuses the same range for a Config
// assembled by hand.
func TestS0161_AC9_AnX265CPUsOutsideItsRangeRefusesToLoad(t *testing.T) {
	for _, bad := range []string{"-1", "1025", "99999", "2.5", "lots", "true", "", "[4]"} {
		t.Run("file "+bad, func(t *testing.T) {
			_, err := load(t, "library_roots:\n  - /mnt/tv\nx265_cpus: "+bad+"\n")
			if err == nil {
				t.Fatalf("Load with x265_cpus: %q = nil, want a refusal", bad)
			}
			if !strings.Contains(err.Error(), "x265_cpus") {
				t.Errorf("the refusal does not name the key: %v", err)
			}
		})
	}
	for _, bad := range []string{"-3", "1025", "1.5", "many"} {
		t.Run("env "+bad, func(t *testing.T) {
			t.Setenv("HOLDFAST_X265_CPUS", bad)
			_, err := load(t, "library_roots:\n  - /mnt/tv\n")
			if err == nil || !strings.Contains(err.Error(), "x265_cpus") {
				t.Errorf("Load with HOLDFAST_X265_CPUS=%q = %v, want a refusal naming x265_cpus", bad, err)
			}
		})
	}

	// Anti-vacuity: the bounds themselves, the default and the environment layer all load.
	if c := loadYAML(t, "library_roots:\n  - /mnt/tv\n"); c.X265CPUs != 0 {
		t.Errorf("an absent x265_cpus resolved to %d, want the built-in default of 0", c.X265CPUs)
	}
	if v, ok := defaultLayer()["x265_cpus"]; !ok || v != 0 {
		t.Errorf("defaultLayer() carries x265_cpus = %v (present %v), want 0", v, ok)
	}
	for _, good := range []int{0, 1, 1024} {
		c := loadYAML(t, "library_roots:\n  - /mnt/tv\nx265_cpus: "+strconv.Itoa(good)+"\n")
		if c.X265CPUs != good {
			t.Errorf("x265_cpus: %d loaded as %d", good, c.X265CPUs)
		}
	}
	t.Setenv("HOLDFAST_X265_CPUS", "6")
	if c := loadYAML(t, "library_roots:\n  - /mnt/tv\nx265_cpus: 2\n"); c.X265CPUs != 6 {
		t.Errorf("HOLDFAST_X265_CPUS=6 over a file's 2 resolved to %d, want 6", c.X265CPUs)
	}

	for _, n := range []int{-1, 1025} {
		c := Config{LibraryRoots: []string{"/mnt/tv"}, X265CPUs: n}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "x265_cpus") {
			t.Errorf("Validate with X265CPUs=%d = %v, want a refusal naming x265_cpus", n, err)
		}
	}
}

// TestS0161_AC10_X265CPUsDescribesTheProcessNotALibrary grades AC-10: inside a
// library_roots entry the key is refused as one describing the process, not as a typo; it
// is not a profile knob; and setting it moves no root's profile digest, so no terminal
// ledger row becomes re-decidable because a parallelism figure changed.
func TestS0161_AC10_X265CPUsDescribesTheProcessNotALibrary(t *testing.T) {
	_, err := load(t, "library_roots:\n  - path: /mnt/tv\n    x265_cpus: 4\n")
	if err == nil {
		t.Fatal("Load with x265_cpus inside a library_roots entry = nil, want a refusal")
	}
	for _, want := range []string{"x265_cpus", "describes the process, not a library"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "typo") {
		t.Errorf("a daemon-level key must not be reported as a typo: %v", err)
	}

	if slices.Contains(ProfileKnobs(), "x265_cpus") || isProfileKnob("x265_cpus") {
		t.Error("x265_cpus is a profile knob, so editing it would move every root's digest")
	}
	without := rootByPath(t, loadYAML(t, "library_roots:\n  - /mnt/tv\n"), "/mnt/tv").Profile.Digest()
	with := rootByPath(t, loadYAML(t, "library_roots:\n  - /mnt/tv\nx265_cpus: 8\n"), "/mnt/tv").Profile.Digest()
	if with != without {
		t.Errorf("setting x265_cpus moved the root's profile digest from %s to %s", without, with)
	}
	// Anti-vacuity: a real knob at the same level does move it, so the equality above is
	// about the key and not about a digest that never moves.
	crf := rootByPath(t, loadYAML(t, "library_roots:\n  - /mnt/tv\ncrf: 30\n"), "/mnt/tv").Profile.Digest()
	if crf == without {
		t.Error("changing crf did not move the digest either, so the comparison above proves nothing")
	}
}
