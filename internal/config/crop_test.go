package config

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/crop"
)

// crop accepts its two values, and an empty one as the default, and refuses anything else
// naming the key, the value and both accepted values, at the top level and on a root; a root
// may override the top level.
func TestCrop_KeyIsValidatedAtEveryLayer(t *testing.T) {
	for _, v := range []string{"", crop.Off, crop.Auto} {
		if err := validateCrop(v); err != nil {
			t.Errorf("crop %q refused: %v", v, err)
		}
	}
	for _, v := range []string{"on", "true", "Auto", "1920:800:0:140", "letterbox"} {
		err := validateCrop(v)
		if err == nil {
			t.Errorf("crop %q accepted", v)
			continue
		}
		for _, part := range []string{"crop", `"` + v + `"`, "off", "auto"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("the refusal of %q does not name %q: %v", v, part, err)
			}
		}
	}
	a, b, state := hwRoots(t)
	for name, body := range map[string]string{
		"top level": "crop: on\nlibrary_roots:\n  - " + a + "\n",
		"a root":    "library_roots:\n  - " + a + "\n  - path: " + b + "\n    crop: on\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, "state_dir: "+state+"\n"+body)
			if err == nil {
				err = c.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), `crop "on"`) {
				t.Errorf("an unknown crop: %v, want its refusal", err)
			}
		})
	}
	c := loadYAML(t, "state_dir: "+state+"\nlibrary_roots:\n  - "+a+"\n"+
		"  - path: "+b+"\n    crop: auto\n")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := map[string]bool{}
	for _, r := range c.RootProfiles() {
		got[r.Clean] = r.Profile.CropEnabled()
	}
	if got[a] || !got[b] {
		t.Errorf("per-root crop = %v, want %s off (the default) and %s auto", got, a, b)
	}
}

// A root that sets no crop, or sets off, digests as it did before the key existed (I5); a
// root that crops digests apart.
func TestCrop_MovesTheDigestOnlyAwayFromOff(t *testing.T) {
	base := Profile{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "mkv"}
	off, auto := base, base
	off.Crop, auto.Crop = crop.Off, crop.Auto
	if base.Digest() != off.Digest() {
		t.Error("an explicit off digests apart from the default")
	}
	if base.Digest() == auto.Digest() {
		t.Error("auto digests like off")
	}
	if got := base.Digest(); got != digestBeforeHWFallback {
		t.Errorf("the default's digest = %s, want %s (the pre-key digest)", got, digestBeforeHWFallback)
	}
	if base.CropMode() != crop.Off || base.CropEnabled() || !auto.CropEnabled() || off.CropEnabled() {
		t.Error("crop resolves wrongly: unset and off are off, auto is on")
	}
	if !digestSilent(cropKey, crop.Off) || digestSilent(cropKey, crop.Auto) {
		t.Error("crop is digest-silent at the wrong value")
	}
	if !knownKeys[cropKey] || defaultLayer()[cropKey] != crop.Off {
		t.Error("crop is not a known key defaulting to off")
	}
	var top Config
	top.Crop = crop.Auto
	if !top.TopLevelProfile().CropEnabled() {
		t.Error("the top-level value does not reach the top-level profile")
	}
}

// A root that crops earns a notice naming it; a configuration that does not, none.
func TestCrop_NoticeNamesTheRootThatCrops(t *testing.T) {
	a, b, state := hwRoots(t)
	c := loadYAML(t, "state_dir: "+state+"\nlibrary_roots:\n  - "+a+"\n  - path: "+b+"\n    crop: auto\n")
	var hits []string
	for _, n := range c.Notices() {
		if strings.Contains(n, "crop is auto") {
			hits = append(hits, n)
		}
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "library root "+b) || !strings.Contains(hits[0], "NO LONGER THE SAME CONTENT") {
		t.Fatalf("crop notices %q, want one naming %s", hits, b)
	}
	plain := loadYAML(t, "state_dir: "+state+"\nlibrary_roots:\n  - "+a+"\n")
	for _, n := range plain.Notices() {
		if strings.Contains(n, "crop is") {
			t.Errorf("a configuration that does not crop earned %q", n)
		}
	}
	if n := cropNotice(Profile{Crop: crop.Auto}, ""); len(n) != 1 || strings.HasPrefix(n[0], "library root") {
		t.Errorf("a rootless notice is %q", n)
	}
	var top Config
	top.Crop = crop.Auto
	if n := top.cropNotices(); len(n) != 1 {
		t.Errorf("a configuration with no roots and a top-level crop earned %d notices", len(n))
	}
}
