package config

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/playability"
)

// The shipped sample is the one the documentation and config.example.yaml state.
func TestPlayability_TheShippedSampleIs16(t *testing.T) {
	if playability.DefaultColdStarts != 16 {
		t.Errorf("playability.DefaultColdStarts = %d; the documents say 16", playability.DefaultColdStarts)
	}
}

// The playability keys (docs/design/swap.md#playability): the gate on by default, both keys
// digested only away from their default, strictly typed wherever written, and a negative
// sample refused.

// TestPlayability_TheDefaultIsOnAndDigestsAsBefore pins the digests this build computed BEFORE
// the keys existed (the literals TestX265OpenGOP_UnsetDigestsAsBefore pins): a configuration
// that writes neither key, or writes each at its default, keeps every terminal row attached to
// the profile that decided it, and the gate runs at its shipped sample with segments on.
func TestPlayability_TheDefaultIsOnAndDigestsAsBefore(t *testing.T) {
	for _, c := range []struct {
		yaml, want string
	}{
		{"library_roots:\n  - /mnt/tv\n", "270a1ea926171dfe"},
		{"library_roots:\n  - /mnt/tv\nplayability_cold_starts: 16\nplayability_segments: true\n", "270a1ea926171dfe"},
		{"library_roots:\n  - path: /mnt/tv\n    crf: 20\n    preset: slow\n    crop: auto\n", "01256238e6145f54"},
		{"library_roots:\n  - path: /mnt/tv\n    crf: 20\n    preset: slow\n    crop: auto\n" +
			"    playability_cold_starts: 16\n    playability_segments: true\n", "01256238e6145f54"},
	} {
		r := rootByPath(t, loadYAML(t, c.yaml), "/mnt/tv")
		if got := r.Profile.PlayabilityColdStartCount(); got != 16 {
			t.Errorf("%q: %d cold starts, want the default 16", c.yaml, got)
		}
		if !r.Profile.PlayabilitySegmentsOn() {
			t.Errorf("%q: the segment check is off, want on", c.yaml)
		}
		if got := r.Profile.Digest(); got != c.want {
			t.Errorf("%q digests to %s, want %s, the digest before the playability keys existed", c.yaml, got, c.want)
		}
	}
	// A Profile assembled in Go is the default too: the gate runs, and the digest is the one
	// the build before the keys computed for the same literal.
	p := Profile{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "mkv"}
	if p.PlayabilityColdStartCount() != playability.DefaultColdStarts || !p.PlayabilitySegmentsOn() {
		t.Errorf("an unset profile: %d cold starts, segments %v", p.PlayabilityColdStartCount(), p.PlayabilitySegmentsOn())
	}
	if got := p.Digest(); got != digestBeforeHWFallback {
		t.Errorf("an unset profile digests %s, want %s", got, digestBeforeHWFallback)
	}
}

// TestPlayability_ValuesAwayFromTheDefaultDigestApart: each value away from the default changes
// what the gate holds an output to, so it is its own profile and its own digest.
func TestPlayability_ValuesAwayFromTheDefaultDigestApart(t *testing.T) {
	digest := func(extra string) string {
		return rootByPath(t, loadYAML(t, "library_roots:\n  - path: /mnt/tv\n"+extra), "/mnt/tv").Profile.Digest()
	}
	seen := map[string]string{}
	for _, extra := range []string{
		"",
		"    playability_cold_starts: 0\n",
		"    playability_cold_starts: 15\n",
		"    playability_cold_starts: 64\n",
		"    playability_segments: false\n",
		"    playability_cold_starts: 0\n    playability_segments: false\n",
	} {
		d := digest(extra)
		if prev, ok := seen[d]; ok {
			t.Errorf("%q and %q digest alike (%s)", prev, extra, d)
		}
		seen[d] = extra
	}
}

// TestPlayability_ThreeLayers: the top level's value is inherited, a root's own overrides it,
// each with the layer it came from, and 0 and false turn the two halves off.
func TestPlayability_ThreeLayers(t *testing.T) {
	c := loadYAML(t, `
library_roots:
  - /mnt/tv
  - path: /mnt/films
    playability_cold_starts: 32
    playability_segments: true
playability_cold_starts: 0
playability_segments: false
`)
	tv, films := rootByPath(t, c, "/mnt/tv"), rootByPath(t, c, "/mnt/films")
	if tv.Profile.PlayabilityColdStartCount() != 0 || tv.Profile.PlayabilitySegmentsOn() {
		t.Errorf("/mnt/tv: %d cold starts, segments %v; want the top level's 0 and false",
			tv.Profile.PlayabilityColdStartCount(), tv.Profile.PlayabilitySegmentsOn())
	}
	if films.Profile.PlayabilityColdStartCount() != 32 || !films.Profile.PlayabilitySegmentsOn() {
		t.Errorf("/mnt/films: %d cold starts, segments %v; want its own 32 and true",
			films.Profile.PlayabilityColdStartCount(), films.Profile.PlayabilitySegmentsOn())
	}
	for _, key := range []string{playabilityColdStartsKey, playabilitySegmentsKey} {
		if got := tv.LayerOf(key); got != LayerTopLevel {
			t.Errorf("/mnt/tv's %s came from %s, want %s", key, got, LayerTopLevel)
		}
		if got := films.LayerOf(key); got != LayerProfile {
			t.Errorf("/mnt/films's %s came from %s, want %s", key, got, LayerProfile)
		}
	}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// TestPlayability_Environment: HOLDFAST_PLAYABILITY_* set the top level.
func TestPlayability_Environment(t *testing.T) {
	t.Setenv("HOLDFAST_PLAYABILITY_COLD_STARTS", "4")
	t.Setenv("HOLDFAST_PLAYABILITY_SEGMENTS", "false")
	p := rootByPath(t, loadYAML(t, "library_roots:\n  - /mnt/tv\n"), "/mnt/tv").Profile
	if p.PlayabilityColdStartCount() != 4 || p.PlayabilitySegmentsOn() {
		t.Errorf("the environment resolved %d cold starts, segments %v; want 4 and false",
			p.PlayabilityColdStartCount(), p.PlayabilitySegmentsOn())
	}
}

// TestPlayability_RefusesAValueOfTheWrongType: the weakly typed decoder would read 3 as true,
// 16.5 as 16 and -1 as a sample of nothing; each is refused naming the key, wherever it was
// written.
func TestPlayability_RefusesAValueOfTheWrongType(t *testing.T) {
	for _, c := range []struct{ yaml, want string }{
		{"library_roots:\n  - /mnt/tv\nplayability_segments: 3\n", "playability_segments must be true or false"},
		{"library_roots:\n  - /mnt/tv\nplayability_segments: sometimes\n", "playability_segments must be true or false"},
		{"library_roots:\n  - path: /mnt/tv\n    playability_segments: [true]\n", "playability_segments must be true or false"},
		{"library_roots:\n  - /mnt/tv\nplayability_cold_starts: 16.5\n", "playability_cold_starts must be a whole number"},
		{"library_roots:\n  - /mnt/tv\nplayability_cold_starts: -1\n", "playability_cold_starts must be a whole number"},
		{"library_roots:\n  - /mnt/tv\nplayability_cold_starts: true\n", "playability_cold_starts must be a whole number"},
		{"library_roots:\n  - path: /mnt/tv\n    playability_cold_starts: many\n", "playability_cold_starts must be a whole number"},
		{"library_roots:\n  - path: /mnt/tv\n    playability_cold_starts: -4\n", "playability_cold_starts must be a whole number"},
	} {
		if _, err := load(t, c.yaml); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Load(%q) = %v, want a refusal containing %q", c.yaml, err, c.want)
		}
	}
	t.Setenv("HOLDFAST_PLAYABILITY_SEGMENTS", "yes please")
	if _, err := load(t, "library_roots:\n  - /mnt/tv\n"); err == nil ||
		!strings.Contains(err.Error(), "HOLDFAST_PLAYABILITY_SEGMENTS") {
		t.Errorf("HOLDFAST_PLAYABILITY_SEGMENTS=\"yes please\" loaded (%v), want a refusal naming it", err)
	}
}

// TestPlayability_AnAssembledNegativeSampleIsRefused: a Profile assembled in Go never passed
// the load-time check, and Validate still refuses a negative sample on it.
func TestPlayability_AnAssembledNegativeSampleIsRefused(t *testing.T) {
	neg, zero := -1, 0
	if err := (Profile{PlayabilityColdStarts: &neg}).validate(); err == nil ||
		!strings.Contains(err.Error(), "playability_cold_starts -1 must be 0 or more") {
		t.Errorf("validate = %v, want a refusal naming the key", err)
	}
	if err := (Profile{PlayabilityColdStarts: &zero}).validate(); err != nil {
		t.Errorf("0, which turns cold starts off: %v", err)
	}
}

// TestPlayability_CheckValue covers the reader on its own: the spellings each key accepts.
func TestPlayability_CheckValue(t *testing.T) {
	for _, c := range []struct {
		key string
		raw any
		ok  bool
	}{
		{playabilitySegmentsKey, true, true}, {playabilitySegmentsKey, "false", true},
		{playabilitySegmentsKey, 1, false}, {playabilitySegmentsKey, "on", false},
		{playabilityColdStartsKey, 0, true}, {playabilityColdStartsKey, "12", true},
		{playabilityColdStartsKey, 7.0, true}, {playabilityColdStartsKey, 7.5, false},
		{playabilityColdStartsKey, -2, false}, {playabilityColdStartsKey, "x", false},
		{"crf", "anything", true},
	} {
		if err := checkPlayabilityValue("here", c.key, c.raw); (err == nil) != c.ok {
			t.Errorf("checkPlayabilityValue(%s, %#v) = %v, want ok=%v", c.key, c.raw, err, c.ok)
		} else if err != nil && !strings.HasPrefix(err.Error(), "here: "+c.key) {
			t.Errorf("the refusal %q does not lead with where and the key", err)
		}
	}
}

// TestPlayability_RenderedAheadOfTheHardwareKnobs: the two keys render resolved, in
// profileKnobs order, just ahead of hw_fallback.
func TestPlayability_RenderedAheadOfTheHardwareKnobs(t *testing.T) {
	four, off := 4, false
	vals := Profile{PlayabilityColdStarts: &four, PlayabilitySegments: &off}.values()
	if len(vals) != len(profileKnobs) {
		t.Fatalf("%d values rendered for %d knobs", len(vals), len(profileKnobs))
	}
	at := len(profileKnobs) - 6 // hw_fallback, hw_decode, x265_open_gop and keep_bit_depth follow them
	if profileKnobs[at] != playabilityColdStartsKey || profileKnobs[at+1] != playabilitySegmentsKey ||
		vals[at] != "4" || vals[at+1] != "false" {
		t.Errorf("the playability keys render %v at %v", vals[at:at+2], profileKnobs[at:at+2])
	}
}
