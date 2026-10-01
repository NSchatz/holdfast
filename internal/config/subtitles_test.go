package config

import (
	"strings"
	"testing"
)

// subtitle_sidecars accepts its two values, and an empty one as the default, and refuses
// anything else naming the key, the value and both accepted values, at the top level and on
// a root; a root may override the top level.
func TestSubtitle_SidecarsKeyIsValidatedAtEveryLayer(t *testing.T) {
	for _, v := range []string{"", SubtitleSidecarsOff, SubtitleSidecarsText} {
		if err := validateSubtitleSidecars(v); err != nil {
			t.Errorf("subtitle_sidecars %q refused: %v", v, err)
		}
	}
	for _, v := range []string{"on", "true", "srt", "all", "Text", "bitmap"} {
		err := validateSubtitleSidecars(v)
		if err == nil {
			t.Errorf("subtitle_sidecars %q accepted", v)
			continue
		}
		for _, part := range []string{"subtitle_sidecars", `"` + v + `"`, "off", "text"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("the refusal of %q does not name %q: %v", v, part, err)
			}
		}
	}
	a, b, state := hwRoots(t)
	for name, body := range map[string]string{
		"top level": "subtitle_sidecars: srt\nlibrary_roots:\n  - " + a + "\n",
		"a root":    "library_roots:\n  - " + a + "\n  - path: " + b + "\n    subtitle_sidecars: srt\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, "state_dir: "+state+"\n"+body)
			if err == nil {
				err = c.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), `subtitle_sidecars "srt"`) {
				t.Errorf("an unknown subtitle_sidecars: %v, want its refusal", err)
			}
		})
	}
	c := loadYAML(t, "state_dir: "+state+"\nlibrary_roots:\n  - "+a+"\n"+
		"  - path: "+b+"\n    subtitle_sidecars: text\n")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := map[string]string{}
	for _, r := range c.RootProfiles() {
		got[r.Clean] = r.Profile.SubtitleSidecarsMode()
	}
	if got[a] != SubtitleSidecarsOff || got[b] != SubtitleSidecarsText {
		t.Errorf("per-root subtitle_sidecars = %v, want %s off (the default) and %s text", got, a, b)
	}
}

// A root that sets no subtitle_sidecars, or sets off, digests as it did before the key
// existed (I5); a root that writes sidecars digests apart.
func TestSubtitle_SidecarsMoveTheDigestOnlyAwayFromOff(t *testing.T) {
	base := Profile{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "mkv"}
	off, text := base, base
	off.SubtitleSidecars, text.SubtitleSidecars = SubtitleSidecarsOff, SubtitleSidecarsText
	if base.Digest() != off.Digest() {
		t.Error("an explicit off digests apart from the default")
	}
	if base.Digest() == text.Digest() {
		t.Error("text digests like off")
	}
	if got := base.Digest(); got != digestBeforeHWFallback {
		t.Errorf("the default's digest = %s, want %s (the pre-key digest)", got, digestBeforeHWFallback)
	}
	if got := base.SubtitleSidecarsMode(); got != SubtitleSidecarsOff {
		t.Errorf("an unset subtitle_sidecars resolves to %q, want off", got)
	}
	if !digestSilent(subtitleSidecarsKey, SubtitleSidecarsOff) || digestSilent(subtitleSidecarsKey, SubtitleSidecarsText) {
		t.Error("subtitle_sidecars is digest-silent at the wrong value")
	}
	if !knownKeys[subtitleSidecarsKey] || defaultLayer()[subtitleSidecarsKey] != SubtitleSidecarsOff {
		t.Error("subtitle_sidecars is not a known key defaulting to off")
	}
	var top Config
	top.SubtitleSidecars = SubtitleSidecarsText
	if top.TopLevelProfile().SubtitleSidecarsMode() != SubtitleSidecarsText {
		t.Error("the top-level value does not reach the top-level profile")
	}
}
