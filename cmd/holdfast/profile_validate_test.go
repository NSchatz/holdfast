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
