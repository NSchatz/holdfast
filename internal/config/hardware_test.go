package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hwRoots makes two library roots and a state directory for a configuration under test.
func hwRoots(t *testing.T) (a, b, state string) {
	t.Helper()
	base := t.TempDir()
	a, b, state = filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "state")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return a, b, state
}

// `encoder: auto` is accepted wherever an encoder is (the top level, a library root, an
// encode profile), and hw_fallback at the top level and per library root, each resolved per
// root with skip as the default.
func TestHardware_AutoAndHWFallbackAreAcceptedAndResolvedPerRoot(t *testing.T) {
	a, b, state := hwRoots(t)
	c := loadYAML(t, "state_dir: "+state+"\nencoder: auto\nhw_fallback: software\nlibrary_roots:\n"+
		"  - "+a+"\n"+
		"  - path: "+b+"\n    encoder: vaapi\n    hw_fallback: skip\n"+
		"encode_profiles:\n  - name: films\n    match: '**/Films/**'\n    encoder: auto\n")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	ra, rb := rootByPath(t, c, a), rootByPath(t, c, b)
	if ra.Profile.Encoder != "auto" || ra.Profile.HWFallbackMode() != HWFallbackSoftware {
		t.Errorf("root a = %q %q, want auto software (inherited)", ra.Profile.Encoder, ra.Profile.HWFallbackMode())
	}
	if rb.Profile.Encoder != "vaapi" || rb.Profile.HWFallbackMode() != HWFallbackSkip {
		t.Errorf("root b = %q %q, want vaapi skip (its own)", rb.Profile.Encoder, rb.Profile.HWFallbackMode())
	}
	if ts := c.TranscodeIn(ra.Profile, filepath.Join(a, "Films", "x.mkv")); ts.Encoder != "auto" || ts.Profile != "films" {
		t.Errorf("the encode profile's auto = %+v", ts)
	}
}

func TestHardware_HWFallbackDefaultsToSkip(t *testing.T) {
	a, _, state := hwRoots(t)
	c := loadYAML(t, "state_dir: "+state+"\nlibrary_roots:\n  - "+a+"\n")
	if c.HWFallback != HWFallbackSkip {
		t.Errorf("the top-level default = %q, want skip", c.HWFallback)
	}
	if got := rootByPath(t, c, a).Profile.HWFallbackMode(); got != HWFallbackSkip {
		t.Errorf("a root's default = %q, want skip", got)
	}
	if got := (Profile{}).HWFallbackMode(); got != HWFallbackSkip {
		t.Errorf("a Profile assembled in Go = %q, want skip", got)
	}
	if got := (Profile{HWFallback: HWFallbackSoftware}).HWFallbackMode(); got != HWFallbackSoftware {
		t.Errorf("an explicit software = %q", got)
	}
	if got := c.TopLevelProfile().HWFallback; got != HWFallbackSkip {
		t.Errorf("TopLevelProfile().HWFallback = %q, want the top level's skip", got)
	}
}

func TestHardware_AnUnknownHWFallbackIsRefusedByName(t *testing.T) {
	a, b, state := hwRoots(t)
	for name, body := range map[string]string{
		"top level": "hw_fallback: cpu\nlibrary_roots:\n  - " + a + "\n",
		"a root":    "library_roots:\n  - " + a + "\n  - path: " + b + "\n    hw_fallback: fallback\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, "state_dir: "+state+"\n"+body)
			if err == nil {
				err = c.Validate()
			}
			if err == nil {
				t.Fatal("an unknown hw_fallback was accepted")
			}
			for _, want := range []string{"hw_fallback", "skip", "software"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not name %q", err, want)
				}
			}
		})
	}
	if err := validateHWFallback(""); err != nil {
		t.Errorf("the empty value (a Profile in Go) refused: %v", err)
	}
}

func TestHardware_AnUnknownEncoderStillListsAuto(t *testing.T) {
	a, _, state := hwRoots(t)
	c, err := load(t, "state_dir: "+state+"\nencoder: automatic\nlibrary_roots:\n  - "+a+"\n")
	if err == nil {
		err = c.Validate()
	}
	if err == nil || !strings.Contains(err.Error(), "auto") || !strings.Contains(err.Error(), `"automatic"`) {
		t.Errorf("encoder: automatic = %v", err)
	}
	if err := validateEncoderKey("auto"); err != nil {
		t.Errorf("an encode profile's auto refused: %v", err)
	}
	if err := validateEncoderKey(""); err == nil || !strings.Contains(err.Error(), "auto") {
		t.Errorf("an encode profile's empty encoder = %v", err)
	}
}

// A root that sets nothing, or sets the default, digests as it did before the key existed,
// so no terminal row is detached from the profile that decided it; a root that falls back to
// software digests apart.
func TestHardware_HWFallbackMovesTheDigestOnlyAwayFromTheDefault(t *testing.T) {
	base := Profile{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "mkv"}
	skip, software := base, base
	skip.HWFallback, software.HWFallback = HWFallbackSkip, HWFallbackSoftware
	if base.Digest() != skip.Digest() {
		t.Error("an explicit skip digests apart from the default")
	}
	if base.Digest() == software.Digest() {
		t.Error("software digests like skip")
	}
	// Pinned: the digest of this profile as the build before hw_fallback computed it
	// (bf36b9c, `Profile.Digest` of the same literal).
	if got := base.Digest(); got != digestBeforeHWFallback {
		t.Errorf("the default's digest = %s, want %s (the pre-key digest)", got, digestBeforeHWFallback)
	}
	vals := software.values()
	if vals[len(vals)-1] != HWFallbackSoftware || profileKnobs[len(profileKnobs)-1] != hwFallbackKey {
		t.Errorf("hw_fallback is not the last knob rendered: %v", vals)
	}
}

// WithEncoder resolves the encoder `auto` or a fallback chose, with that encoder's quality.
func TestHardware_WithEncoderTakesTheChosenEncodersQuality(t *testing.T) {
	c := &Config{Quality: map[string]int{"vaapi": 30}}
	ts := Transcode{Encoder: "auto", CRF: 22}
	got := c.WithEncoder(ts, "vaapi")
	if got.Encoder != "vaapi" || !got.EncoderQualitySet || got.EncoderQuality != 30 || got.CRF != 22 {
		t.Errorf("WithEncoder(vaapi) = %+v", got)
	}
	got = c.WithEncoder(ts, "cpu")
	if got.Encoder != "cpu" || got.EncoderQualitySet {
		t.Errorf("WithEncoder(cpu) = %+v", got)
	}
}

// digestBeforeHWFallback is Profile.Digest of the literal in
// TestHardware_HWFallbackMovesTheDigestOnlyAwayFromTheDefault, computed by the goal-start
// build (bf36b9c), before hw_fallback existed.
const digestBeforeHWFallback = "ab5f38d831b37a30"

// A rule may name `encoder: auto` (it is judged by validateEncoderKey), and the S0165
// ceiling check reads auto as HEVC - every choice of it writes HEVC - so a band capped into a
// `cpu` band is one codec and accepted, and into an `svtav1` band is two and refused.
func TestHardware_ARuleMayNameAutoAndItsCeilingCheckReadsHEVC(t *testing.T) {
	cfg := func(root string) string {
		return `
library_roots:
  - path: /mnt/tv
    encoder: ` + root + `
    rules:
      - when:
          min_source_height: 1081
        max_height: 1080
        encoder: auto
`
	}
	c, err := load(t, cfg("cpu"))
	if err == nil {
		err = c.Validate()
	}
	if err != nil {
		t.Errorf("auto capped into a cpu band (both HEVC) was refused: %v", err)
	}
	mentions(t, refusal(t, cfg("svtav1")), "rules[0]", "max_height 1080", "hevc", "av1")
	if got := targetCodecOf("auto"); got != "hevc" {
		t.Errorf("targetCodecOf(auto) = %q, want hevc", got)
	}
}
