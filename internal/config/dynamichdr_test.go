package config

import (
	"strings"
	"testing"
)

// dolby_vision_p7 accepts its two values, and an empty one as the default, and refuses
// anything else naming the key, the value and both accepted values, at the top level and on
// a root; a root may override the top level.
func TestDolbyVisionP7_KeyIsValidatedAtEveryLayer(t *testing.T) {
	for _, v := range []string{"", DolbyVisionP7Skip, DolbyVisionP7Convert} {
		if err := validateDolbyVisionP7(v); err != nil {
			t.Errorf("dolby_vision_p7 %q refused: %v", v, err)
		}
	}
	for _, v := range []string{"on", "true", "8.1", "Convert", "mode5", "off"} {
		err := validateDolbyVisionP7(v)
		if err == nil {
			t.Errorf("dolby_vision_p7 %q accepted", v)
			continue
		}
		for _, part := range []string{"dolby_vision_p7", `"` + v + `"`, "skip", "convert"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("the refusal of %q does not name %q: %v", v, part, err)
			}
		}
	}
	a, b, state := hwRoots(t)
	for name, body := range map[string]string{
		"top level": "dolby_vision_p7: mode5\nlibrary_roots:\n  - " + a + "\n",
		"a root":    "library_roots:\n  - " + a + "\n  - path: " + b + "\n    dolby_vision_p7: mode5\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, "state_dir: "+state+"\n"+body)
			if err == nil {
				err = c.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), `dolby_vision_p7 "mode5"`) {
				t.Errorf("an unknown dolby_vision_p7: %v, want its refusal", err)
			}
		})
	}
	c := loadYAML(t, "state_dir: "+state+"\nlibrary_roots:\n  - "+a+"\n"+
		"  - path: "+b+"\n    dolby_vision_p7: convert\n")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := map[string]string{}
	for _, r := range c.RootProfiles() {
		got[r.Clean] = r.Profile.DolbyVisionP7Mode()
	}
	if got[a] != DolbyVisionP7Skip || got[b] != DolbyVisionP7Convert {
		t.Errorf("per-root dolby_vision_p7 = %v, want %s skip (the default) and %s convert", got, a, b)
	}
	top := loadYAML(t, "state_dir: "+state+"\ndolby_vision_p7: convert\nlibrary_roots:\n  - "+a+"\n")
	if err := top.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, r := range top.RootProfiles() {
		if r.Profile.DolbyVisionP7Mode() != DolbyVisionP7Convert {
			t.Errorf("a root under a top-level convert resolves to %q", r.Profile.DolbyVisionP7Mode())
		}
	}
}

// A root that sets no dolby_vision_p7, or sets skip, digests as it did before the key existed
// (I5); a root that converts digests apart.
func TestDolbyVisionP7_MovesTheDigestOnlyAwayFromSkip(t *testing.T) {
	base := Profile{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "mkv"}
	skip, convert := base, base
	skip.DolbyVisionP7, convert.DolbyVisionP7 = DolbyVisionP7Skip, DolbyVisionP7Convert
	if base.Digest() != skip.Digest() {
		t.Error("an explicit skip digests apart from the default")
	}
	if base.Digest() == convert.Digest() {
		t.Error("convert digests like skip")
	}
	if got := base.Digest(); got != digestBeforeHWFallback {
		t.Errorf("the default's digest = %s, want %s (the pre-key digest)", got, digestBeforeHWFallback)
	}
	if got := base.DolbyVisionP7Mode(); got != DolbyVisionP7Skip {
		t.Errorf("an unset dolby_vision_p7 resolves to %q, want skip", got)
	}
	if !digestSilent(dolbyVisionP7Key, DolbyVisionP7Skip) || digestSilent(dolbyVisionP7Key, DolbyVisionP7Convert) {
		t.Error("dolby_vision_p7 is digest-silent at the wrong value")
	}
	if !knownKeys[dolbyVisionP7Key] || defaultLayer()[dolbyVisionP7Key] != DolbyVisionP7Skip {
		t.Error("dolby_vision_p7 is not a known key defaulting to skip")
	}
	var top Config
	top.DolbyVisionP7 = DolbyVisionP7Convert
	if top.TopLevelProfile().DolbyVisionP7Mode() != DolbyVisionP7Convert {
		t.Error("the top-level dolby_vision_p7 does not reach the top-level profile")
	}
}
