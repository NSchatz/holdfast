package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// A root that sets no keep_bit_depth, or sets it false, digests exactly as it did before the
// key existed, so no terminal row is detached from the profile that decided it; a root that
// keeps the source's depth digests apart, and the knob is the last one rendered.
func TestKeepBitDepth_MovesTheDigestOnlyAwayFromTheDefault(t *testing.T) {
	base := Profile{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "mkv"}
	off, on := base, base
	no, yes := false, true
	off.KeepBitDepth, on.KeepBitDepth = &no, &yes
	if base.Digest() != off.Digest() {
		t.Error("an explicit keep_bit_depth: false digests apart from the default")
	}
	if base.Digest() == on.Digest() {
		t.Error("keep_bit_depth: true digests like the default")
	}
	// Pinned: the digest of this profile before keep_bit_depth (or either hardware key) existed.
	if got := base.Digest(); got != digestBeforeHWFallback {
		t.Errorf("the default's digest = %s, want %s (the pre-key digest)", got, digestBeforeHWFallback)
	}
	if got := off.Digest(); got != digestBeforeHWFallback {
		t.Errorf("keep_bit_depth: false digests %s, want %s (the pre-key digest)", got, digestBeforeHWFallback)
	}
	if base.BitDepthKept() || off.BitDepthKept() || !on.BitDepthKept() {
		t.Errorf("BitDepthKept: unset %v, false %v, true %v; want false, false, true",
			base.BitDepthKept(), off.BitDepthKept(), on.BitDepthKept())
	}
	vals := on.values()
	if vals[len(vals)-1] != "true" || profileKnobs[len(profileKnobs)-1] != keepBitDepthKey {
		t.Errorf("keep_bit_depth is not the last knob rendered: %v", vals)
	}
	if len(vals) != len(profileKnobs) {
		t.Errorf("%d values rendered for %d knobs", len(vals), len(profileKnobs))
	}
}

// keep_bit_depth is a root knob with a top-level default: a root inherits the top level's
// value, overrides it, and every job's settings carry the root's resolved value whatever an
// encode profile says, since no encode profile may name it.
func TestKeepBitDepth_ResolvesPerRootAndReachesTheJob(t *testing.T) {
	state, a, b := t.TempDir(), t.TempDir(), t.TempDir()
	c := loadYAML(t, "state_dir: "+state+"\nkeep_bit_depth: true\nlibrary_roots:\n  - "+a+"\n"+
		"  - path: "+b+"\n    keep_bit_depth: false\n")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := map[string]bool{}
	for _, r := range c.RootProfiles() {
		got[r.Clean] = r.Profile.BitDepthKept()
		ts := c.TranscodeIn(r.Profile, filepath.Join(r.Clean, "film.mkv"))
		if ts.KeepBitDepth != r.Profile.BitDepthKept() {
			t.Errorf("root %s: the job's KeepBitDepth %v, the root's %v", r.Clean, ts.KeepBitDepth, r.Profile.BitDepthKept())
		}
	}
	if !got[a] || got[b] {
		t.Errorf("per-root keep_bit_depth = %v, want %s true (inherited) and %s false", got, a, b)
	}
	if def := loadYAML(t, "state_dir: "+state+"\nlibrary_roots:\n  - "+a+"\n"); def.TopLevelProfile().BitDepthKept() {
		t.Error("an unset keep_bit_depth resolves to true; the shipped default is the 10-bit floor")
	}

	// No encode profile may carry it: the key belongs to the root.
	_, err := load(t, "state_dir: "+state+"\nlibrary_roots:\n  - "+a+"\n"+
		"encode_profiles:\n  - name: films\n    match: \"*.mkv\"\n    keep_bit_depth: true\n")
	if err == nil {
		t.Error("an encode profile carrying keep_bit_depth was accepted")
	}
}

// keep_bit_depth beside a forced pixel_format is refused at start, at the top level and on a
// root, naming both keys: the key changes only how `auto` derives a depth, and a forced format
// would silently win. Beside auto, and set false beside a forced format, it is accepted.
func TestKeepBitDepth_IsRefusedBesideAForcedPixelFormat(t *testing.T) {
	state, a, b := t.TempDir(), t.TempDir(), t.TempDir()
	for name, cfg := range map[string]string{
		"top level":           "keep_bit_depth: true\npixel_format: yuv420p10le\nlibrary_roots:\n  - " + a + "\n",
		"a root":              "library_roots:\n  - " + a + "\n  - path: " + b + "\n    keep_bit_depth: true\n    pixel_format: yuv420p\n",
		"a root's own format": "keep_bit_depth: true\nlibrary_roots:\n  - " + a + "\n  - path: " + b + "\n    pixel_format: yuv420p10le\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, "state_dir: "+state+"\n"+cfg)
			if err == nil {
				err = c.Validate()
			}
			if err == nil {
				t.Fatal("keep_bit_depth: true beside a forced pixel_format was accepted")
			}
			for _, part := range []string{"keep_bit_depth", "pixel_format", "auto"} {
				if !strings.Contains(err.Error(), part) {
					t.Errorf("the refusal %q does not name %q", err, part)
				}
			}
		})
	}
	for name, cfg := range map[string]string{
		"true beside auto":            "keep_bit_depth: true\npixel_format: auto\nlibrary_roots:\n  - " + a + "\n",
		"false beside a forced value": "keep_bit_depth: false\npixel_format: yuv420p\nlibrary_roots:\n  - " + a + "\n",
		"a root opting out":           "keep_bit_depth: true\nlibrary_roots:\n  - " + a + "\n  - path: " + b + "\n    keep_bit_depth: false\n    pixel_format: yuv420p10le\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, "state_dir: "+state+"\n"+cfg)
			if err == nil {
				err = c.Validate()
			}
			if err != nil {
				t.Errorf("refused: %v", err)
			}
		})
	}
}
