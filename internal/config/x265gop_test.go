package config

import (
	"strings"
	"testing"
)

// x265_open_gop (docs/design/encode-plan.md#gop): unset by default, a boolean where written,
// digested only where set, and refused on a root libx265 never encodes.

// TestX265OpenGOP_UnsetDigestsAsBefore pins the digests this build computed BEFORE the key
// existed, for a root with every default and for one that sets other knobs: a configuration
// that does not write the key must keep every terminal row attached to the profile that
// decided it. The literals were computed by the build before this key.
func TestX265OpenGOP_UnsetDigestsAsBefore(t *testing.T) {
	for _, c := range []struct {
		yaml, want string
	}{
		{"library_roots:\n  - /mnt/tv\n", "270a1ea926171dfe"},
		{"library_roots:\n  - path: /mnt/tv\n    crf: 20\n    preset: slow\n    crop: auto\n", "01256238e6145f54"},
	} {
		r := rootByPath(t, loadYAML(t, c.yaml), "/mnt/tv")
		if r.Profile.X265OpenGOP != nil {
			t.Errorf("an absent x265_open_gop resolved to %v, want unset", *r.Profile.X265OpenGOP)
		}
		if got := r.Profile.OpenGOPMode(); got != X265OpenGOPUnset {
			t.Errorf("an absent x265_open_gop renders %q, want %q", got, X265OpenGOPUnset)
		}
		if got := r.Profile.Digest(); got != c.want {
			t.Errorf("%q digests to %s, want %s, the digest before x265_open_gop existed", c.yaml, got, c.want)
		}
		if got := r.LayerOf(x265OpenGOPKey); got != LayerDefault {
			t.Errorf("an absent x265_open_gop says it came from %s, want %s", got, LayerDefault)
		}
	}
}

// TestX265OpenGOP_SetValuesDigestApart: each written value changes the command line, so unset,
// false and true are three profiles and three digests.
func TestX265OpenGOP_SetValuesDigestApart(t *testing.T) {
	digest := func(extra string) string {
		return rootByPath(t, loadYAML(t, "library_roots:\n  - path: /mnt/tv\n"+extra), "/mnt/tv").Profile.Digest()
	}
	unset, closed, open := digest(""), digest("    x265_open_gop: false\n"), digest("    x265_open_gop: true\n")
	if unset == closed || unset == open || closed == open {
		t.Errorf("digests unset=%s false=%s true=%s, want three different ones", unset, closed, open)
	}
	// The same value written at the top level and inherited is the same profile.
	if top := rootByPath(t, loadYAML(t, "library_roots:\n  - /mnt/tv\nx265_open_gop: false\n"), "/mnt/tv").Profile.Digest(); top != closed {
		t.Errorf("x265_open_gop: false inherited digests %s, written on the root %s; want one digest", top, closed)
	}
}

// TestX265OpenGOP_ThreeLayers: the top level's value is inherited, and a root's own overrides
// it, both ways, with the layer each came from.
func TestX265OpenGOP_ThreeLayers(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - /mnt/tv
  - path: /mnt/films
    x265_open_gop: true
x265_open_gop: false
`)
	tv, films := rootByPath(t, c, "/mnt/tv"), rootByPath(t, c, "/mnt/films")
	if open, set := tv.Profile.OpenGOP(); !set || open {
		t.Errorf("/mnt/tv resolved open=%t set=%t, want the top level's false", open, set)
	}
	if got := tv.LayerOf(x265OpenGOPKey); got != LayerTopLevel {
		t.Errorf("/mnt/tv's x265_open_gop came from %s, want %s", got, LayerTopLevel)
	}
	if open, set := films.Profile.OpenGOP(); !set || !open {
		t.Errorf("/mnt/films resolved open=%t set=%t, want its own true", open, set)
	}
	if got := films.LayerOf(x265OpenGOPKey); got != LayerProfile {
		t.Errorf("/mnt/films's x265_open_gop came from %s, want %s", got, LayerProfile)
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestX265OpenGOP_Environment: HOLDFAST_X265_OPEN_GOP sets the top level.
func TestX265OpenGOP_Environment(t *testing.T) {
	t.Setenv("HOLDFAST_X265_OPEN_GOP", "false")
	if open, set := rootByPath(t, loadYAML(t, "library_roots:\n  - /mnt/tv\n"), "/mnt/tv").Profile.OpenGOP(); !set || open {
		t.Errorf("HOLDFAST_X265_OPEN_GOP=false resolved open=%t set=%t, want false", open, set)
	}
}

// TestX265OpenGOP_NullIsUnset: a top-level key written with no value is the absent key.
func TestX265OpenGOP_NullIsUnset(t *testing.T) {
	if _, set := rootByPath(t, loadYAML(t, "library_roots:\n  - /mnt/tv\nx265_open_gop:\n"), "/mnt/tv").Profile.OpenGOP(); set {
		t.Errorf("x265_open_gop written with no value resolved as set, want unset")
	}
}

// TestX265OpenGOP_RefusesANonBoolean: the weakly typed decoder would read 3 as true and "no"
// as false; a GOP structure nobody wrote is refused, naming the key, wherever it was written.
func TestX265OpenGOP_RefusesANonBoolean(t *testing.T) {
	for _, y := range []string{
		"library_roots:\n  - /mnt/tv\nx265_open_gop: 3\n",
		"library_roots:\n  - /mnt/tv\nx265_open_gop: closed\n",
		"library_roots:\n  - path: /mnt/tv\n    x265_open_gop: 0\n",
		"library_roots:\n  - path: /mnt/tv\n    x265_open_gop: [false]\n",
	} {
		if _, err := load(t, y); err == nil || !strings.Contains(err.Error(), "x265_open_gop must be true or false") {
			t.Errorf("Load(%q) = %v, want a refusal naming x265_open_gop", y, err)
		}
	}
	t.Setenv("HOLDFAST_X265_OPEN_GOP", "yes please")
	if _, err := load(t, "library_roots:\n  - /mnt/tv\n"); err == nil ||
		!strings.Contains(err.Error(), "HOLDFAST_X265_OPEN_GOP") {
		t.Errorf("HOLDFAST_X265_OPEN_GOP=\"yes please\" loaded (%v), want a refusal naming it", err)
	}
}

// TestX265OpenGOP_RefusedWhereLibx265NeverEncodes: the key is a libx265 parameter, so a root
// none of whose files libx265 can encode refuses it at start, naming the key and the root; a
// root whose files can reach libx265 by any route - its own encoder, a rule, an encode
// profile, or hw_fallback: software behind a hardware HEVC encoder or auto - accepts it.
func TestX265OpenGOP_RefusedWhereLibx265NeverEncodes(t *testing.T) {
	for _, c := range []struct {
		name, yaml string
		refused    bool
	}{
		{"cpu", "encoder: cpu\nx265_open_gop: false\n", false},
		{"cpu-by-alias", "encoder: libx265\nx265_open_gop: false\n", false},
		{"unset-on-svtav1", "encoder: svtav1\n", false},
		{"svtav1", "encoder: svtav1\nx265_open_gop: false\n", true},
		{"x264", "encoder: x264\nx265_open_gop: true\n", true},
		{"nvenc/fallback-skip", "encoder: nvenc\nx265_open_gop: false\n", true},
		{"nvenc/fallback-software", "encoder: nvenc\nhw_fallback: software\nx265_open_gop: false\n", false},
		{"av1_nvenc/fallback-software", "encoder: av1_nvenc\nhw_fallback: software\nx265_open_gop: false\n", true},
		{"auto/fallback-skip", "encoder: auto\nx265_open_gop: false\n", true},
		{"auto/fallback-software", "encoder: auto\nhw_fallback: software\nx265_open_gop: false\n", false},
		{"svtav1/encode-profile-cpu", "encoder: svtav1\nx265_open_gop: false\nencode_profiles:\n" +
			"  - name: hevc\n    match: \"*.mkv\"\n    encoder: cpu\n", false},
		{"svtav1/encode-profile-x264", "encoder: svtav1\nx265_open_gop: false\nencode_profiles:\n" +
			"  - name: avc\n    match: \"*.mkv\"\n    encoder: x264\n", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := loadYAML(t, "library_roots:\n  - /mnt/tv\n"+c.yaml)
			err := cfg.Validate()
			if !c.refused {
				if err != nil {
					t.Errorf("Validate: %v, want accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate accepted x265_open_gop on a root libx265 never encodes")
			}
			for _, want := range []string{"x265_open_gop", "/mnt/tv", "libx265"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
		})
	}
	// A rule routing a band to libx265 is a route too, and it is the root's own.
	cfg := loadYAML(t, "library_roots:\n  - path: /mnt/tv\n    encoder: svtav1\n    x265_open_gop: false\n"+
		"    rules:\n      - when:\n          max_source_height: 576\n        encoder: cpu\n")
	if err := cfg.Validate(); err != nil {
		t.Errorf("a rule routing a band to cpu: Validate: %v, want accepted", err)
	}
	// Set on one root, it is refused naming that root and not the other.
	cfg = loadYAML(t, "library_roots:\n  - /mnt/tv\n  - path: /mnt/films\n    encoder: svtav1\n    x265_open_gop: true\n")
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "/mnt/films") {
		t.Errorf("Validate = %v, want a refusal naming /mnt/films", err)
	}
}
