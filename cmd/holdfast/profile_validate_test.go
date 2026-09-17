package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
)

// TestValidate_PrintsTheResolvedProfilePerRoot.
//
// `validate` has to print what the inheritance PRODUCED, not the file as written, and
// that is a different document. A knob may be a built-in default, a top-level choice or
// one root's own profile, and the resolved value is identical in all three cases - so
// reading the YAML back cannot tell an operator what a root will actually do to their
// files. This is the only thing that can, and it matters most on the knobs whose wrong
// value ends in a deleted original.
//
// Three things are asserted, and each is a way the print can be useless: every root is
// named, every knob in the closed set appears under it with the value that root
// resolved to, and each value says WHICH LAYER supplied it.
func TestValidate_PrintsTheResolvedProfilePerRoot(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
library_roots:
  - /mnt/tv
  - path: /mnt/anime
    crf: 18
    vmaf_min_pool: 45
preset: medium
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	got := out.String()

	// Both roots are named, and the whole closed knob set is printed under each.
	tv, anime := sectionFor(t, got, "/mnt/tv"), sectionFor(t, got, "/mnt/anime")
	for _, knob := range config.ProfileKnobs() {
		for name, section := range map[string]string{"/mnt/tv": tv, "/mnt/anime": anime} {
			if !strings.Contains(section, knob) {
				t.Errorf("the printed configuration for %s does not carry the knob %q:\n%s", name, knob, section)
			}
		}
	}

	// The VALUES are what the inheritance produced, per root, and they differ where the
	// profile differs. /mnt/anime overrode crf and the worst-frame floor; /mnt/tv did not.
	assertKnob(t, tv, "crf", "22", string(config.LayerDefault))
	assertKnob(t, anime, "crf", "18", string(config.LayerProfile))
	assertKnob(t, tv, "vmaf_min_pool", "60", string(config.LayerDefault))
	assertKnob(t, anime, "vmaf_min_pool", "45", string(config.LayerProfile))

	// A top-level choice is neither a default nor a profile, and says so under BOTH roots
	// - which is the case a printer that only ever wrote "default" would still pass the
	// assertions above on.
	assertKnob(t, tv, "preset", "medium", string(config.LayerTopLevel))
	assertKnob(t, anime, "preset", "medium", string(config.LayerTopLevel))

	// The digest a terminal row records is printed beside its root, so an operator
	// holding a job's profile_digest can find which root decided it.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, r := range cfg.RootProfiles() {
		if !strings.Contains(got, r.Profile.Digest()) {
			t.Errorf("the printed configuration does not carry %s's digest %s, so a ledger row cannot "+
				"be matched to the root that decided it:\n%s", r.Clean, r.Profile.Digest(), got)
		}
	}
}

// sectionFor returns the printed block for one root: from the line naming it up to the
// next root heading. Splitting matters - an assertion over the whole output would pass on
// a printer that wrote every value under one root and nothing under the other.
func sectionFor(t *testing.T, out, root string) string {
	t.Helper()
	start := strings.Index(out, "library root "+root+" ")
	if start < 0 {
		t.Fatalf("the printed configuration never names the root %s:\n%s", root, out)
	}
	rest := out[start+1:]
	if next := strings.Index(rest, "library root "); next >= 0 {
		return rest[:next]
	}
	return rest
}

// assertKnob finds the line for knob in a root's section and checks both the value the
// inheritance produced and the layer that supplied it.
func assertKnob(t *testing.T, section, knob, wantValue, wantLayer string) {
	t.Helper()
	for _, line := range strings.Split(section, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != knob {
			continue
		}
		if fields[1] != wantValue {
			t.Errorf("%s printed as %q, want the resolved %q (line: %s)", knob, fields[1], wantValue, strings.TrimSpace(line))
		}
		if !strings.Contains(line, wantLayer) {
			t.Errorf("%s does not say it came from %q (line: %s)", knob, wantLayer, strings.TrimSpace(line))
		}
		return
	}
	t.Errorf("no printed line for the knob %q:\n%s", knob, section)
}

// TestValidate_PrintsEachRootsRulesInListOrder is S0089 [AC-21]: `validate` prints each
// root's resolution rules IN LIST ORDER with their band and the knobs each overrides, on
// stdout (cli L2), and prints a root with no rules exactly as it does today.
//
// The ORDER is the whole of why this exists. First match wins and nothing merges, so a rule
// is shadowed by any earlier rule whose band contains its own - a configuration an operator
// can write, cannot see in their file at a glance, and would otherwise discover only as a
// threshold that never applied. The band and the knobs beside it are what make the printed
// line answer "which files, and what changes for them" without re-deriving the layering by
// hand.
//
// stdout and not stderr: this is the report the command exists to print, so a caller can
// read it with stderr discarded.
//
// MUTATION: print the rules from a map, or sort them by band, and the order assertion reds;
// print them on stderr and the whole section disappears from stdout.
func TestValidate_PrintsEachRootsRulesInListOrder(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
library_roots:
  - path: /mnt/tv
    min_bitrate_kbps: 2500
    rules:
      - when:
          min_source_height: 2160
        min_bitrate_kbps: 12000
        crf: 20
      - when:
          max_source_height: 576
        min_bitrate_kbps: 800
      - min_savings_percent: 5
  - /mnt/movies
`), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := dispatch([]string{"validate", "--config", cfgPath}, &out, &errOut); code != 0 {
		t.Fatalf("validate exited %d: %s", code, errOut.String())
	}
	got := out.String()
	tv := sectionFor(t, got, "/mnt/tv")

	// Every rule is printed, in the order the resolver reads it. The assertion is on the
	// POSITIONS of the three bands in the text, so a printer that emitted the right lines
	// in the wrong order still reds.
	first := strings.Index(tv, "2160")
	second := strings.Index(tv, "576")
	third := strings.Index(tv, "min_savings_percent=5")
	if first < 0 || second < 0 || third < 0 {
		t.Fatalf("the printed configuration does not carry all three rules:\n%s", tv)
	}
	if !(first < second && second < third) {
		t.Errorf("the rules are printed out of list order (2160 at %d, 576 at %d, the unconditional "+
			"rule at %d) - the order IS the semantics under first-match:\n%s", first, second, third, tv)
	}

	// Each rule names the knobs it overrides, with the values it supplies.
	for _, want := range []string{"min_bitrate_kbps=12000", "crf=20", "min_bitrate_kbps=800"} {
		if !strings.Contains(tv, want) {
			t.Errorf("the printed rules do not carry %q:\n%s", want, tv)
		}
	}
	// And the bands are named in the spelling the configuration uses, both ways round.
	if !strings.Contains(tv, "source height 2160 to any") {
		t.Errorf("the 2160 rule's band is not printed with its unbounded side:\n%s", tv)
	}
	if !strings.Contains(tv, "source height any to 576") {
		t.Errorf("the 576 rule's band is not printed with its unbounded side:\n%s", tv)
	}

	// A root with NO rules prints exactly what it printed before this item: no rules
	// section at all, and every knob still there.
	movies := sectionFor(t, got, "/mnt/movies")
	if strings.Contains(movies, "rules") {
		t.Errorf("a root with no rules printed a rules section:\n%s", movies)
	}
	assertKnob(t, movies, "min_bitrate_kbps", "2500", string(config.LayerDefault))

	// stdout is where it went, and stderr carries none of it (cli L2).
	if strings.Contains(errOut.String(), "source height") {
		t.Errorf("the rules were narrated on stderr, where a caller reading the report with stderr "+
			"discarded never sees them:\n%s", errOut.String())
	}
}
