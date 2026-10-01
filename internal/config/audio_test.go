package config

import (
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/audio"
)

// With no audio key set, or every one at its default, a profile digests to what it digested
// before the keys existed, and its settings derive nothing; each key away from its default
// moves the digest.
func TestAudioConfig_KeysAreDigestSilentAtTheirDefaults(t *testing.T) {
	base := Profile{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "mkv"}
	if got := base.Digest(); got != digestBeforeHWFallback {
		t.Fatalf("the default's digest = %s, want %s (the pre-key digest)", got, digestBeforeHWFallback)
	}
	no := false
	explicit := base
	explicit.AudioReencode, explicit.AudioDownmix, explicit.AudioLoudness = AudioOff, AudioOff, AudioOff
	explicit.KeepOriginalAudio = &no
	if explicit.Digest() != base.Digest() {
		t.Error("the explicit defaults digest apart from the unset keys")
	}
	if s := base.AudioSettings(); s.Active() || s.Loudness || s.KeepOriginal || s.BitratesKbps != nil || s.Codec != "" {
		t.Errorf("the default settings = %+v", s)
	}
	yes := true
	seen := map[string]string{base.Digest(): "default"}
	for name, mut := range map[string]func(*Profile){
		"audio_reencode":      func(p *Profile) { p.AudioReencode = AudioReencodeOn },
		"audio_codec":         func(p *Profile) { p.AudioCodec = "aac" },
		"audio_mono_kbps":     func(p *Profile) { p.AudioMonoKbps = 64 },
		"audio_stereo_kbps":   func(p *Profile) { p.AudioStereoKbps = 64 },
		"audio_51_kbps":       func(p *Profile) { p.Audio51Kbps = 64 },
		"audio_71_kbps":       func(p *Profile) { p.Audio71Kbps = 64 },
		"keep_original_audio": func(p *Profile) { p.KeepOriginalAudio = &yes },
		"audio_downmix":       func(p *Profile) { p.AudioDownmix = AudioDownmixStereo },
		"audio_loudness":      func(p *Profile) { p.AudioLoudness = AudioLoudnessR128 },
	} {
		p := base
		mut(&p)
		d := p.Digest()
		if other, dup := seen[d]; dup {
			t.Errorf("%s digests like %s", name, other)
		}
		seen[d] = name
	}
	for _, k := range audioKnobs {
		if !isProfileKnob(k) || !knownKeys[k] {
			t.Errorf("%s is not a root knob and a known key", k)
		}
		if _, ok := audioDefaults()[k]; !ok {
			t.Errorf("%s has no default", k)
		}
	}
	if silent, ok := audioDigestSilent("crf", "0"); silent || ok {
		t.Error("a non-audio knob is ruled on as audio")
	}
}

func TestAudioConfig_SettingsCarryEveryKey(t *testing.T) {
	yes := true
	p := Profile{AudioReencode: AudioReencodeOn, AudioCodec: "opus", AudioMonoKbps: 48, Audio71Kbps: 400,
		KeepOriginalAudio: &yes, AudioDownmix: AudioDownmixStereo, AudioLoudness: AudioLoudnessR128}
	s := p.AudioSettings()
	if !s.Reencode || s.Codec != "opus" || !s.KeepOriginal || !s.Downmix || !s.Loudness ||
		len(s.BitratesKbps) != 2 || s.BitratesKbps[audio.Mono] != 48 || s.BitratesKbps[audio.Surround71] != 400 {
		t.Errorf("AudioSettings = %+v", s)
	}
	p.AudioStereoKbps, p.Audio51Kbps = 160, 320
	s = p.AudioSettings()
	if s.BitratesKbps[audio.Stereo] != 160 || s.BitratesKbps[audio.Surround51] != 320 {
		t.Errorf("AudioSettings = %+v", s)
	}
	vals := strings.Join(p.audioValues(), " ")
	if vals != "on opus 48 160 320 400 true stereo ebu_r128" {
		t.Errorf("audioValues = %s", vals)
	}
}

// Every refusal names the key, the value and what is accepted.
func TestAudioConfig_ValuesAreValidatedWithNamedRefusals(t *testing.T) {
	ok := []Profile{
		{},
		{AudioCodec: "aac"},
		{AudioLoudness: AudioLoudnessR128},
		{AudioReencode: AudioReencodeOn, AudioCodec: "eac3", Audio51Kbps: 1536},
		{AudioDownmix: AudioDownmixStereo, AudioCodec: "opus"},
		{AudioReencode: AudioOff, AudioDownmix: AudioOff, AudioLoudness: AudioOff},
		{AudioCodec: "ac3", AudioMonoKbps: 96, AudioStereoKbps: 192, Audio51Kbps: 640, Audio71Kbps: 32},
	}
	for _, p := range ok {
		if err := p.validateAudio(); err != nil {
			t.Errorf("%+v refused: %v", p, err)
		}
	}
	bad := []struct {
		p     Profile
		parts []string
	}{
		{Profile{AudioReencode: "yes"}, []string{"audio_reencode", `"yes"`, "off", "on"}},
		{Profile{AudioDownmix: "5.1"}, []string{"audio_downmix", `"5.1"`, "off", "stereo"}},
		{Profile{AudioLoudness: "r128"}, []string{"audio_loudness", `"r128"`, "off", "ebu_r128"}},
		{Profile{AudioCodec: "libfdk_aac"}, []string{"audio_codec", `"libfdk_aac"`, "aac, ac3, eac3, opus"}},
		{Profile{AudioCodec: "libopus"}, []string{"audio_codec", `"libopus"`}},
		{Profile{AudioReencode: AudioReencodeOn}, []string{"audio_codec is required", "audio_reencode", "audio_downmix"}},
		{Profile{AudioDownmix: AudioDownmixStereo}, []string{"audio_codec is required"}},
		{Profile{AudioMonoKbps: -1}, []string{"audio_mono_kbps -1"}},
		{Profile{AudioStereoKbps: -5}, []string{"audio_stereo_kbps -5"}},
		{Profile{Audio51Kbps: -5}, []string{"audio_51_kbps -5"}},
		{Profile{Audio71Kbps: -5}, []string{"audio_71_kbps -5"}},
		{Profile{AudioCodec: "ac3", Audio51Kbps: 768}, []string{"audio_51_kbps 768", "AC-3", "640"}},
		{Profile{AudioCodec: "ac3", AudioStereoKbps: 200}, []string{"audio_stereo_kbps 200", "192, 224"}},
	}
	for _, c := range bad {
		err := c.p.validateAudio()
		if err == nil {
			t.Errorf("%+v accepted", c.p)
			continue
		}
		for _, part := range c.parts {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("the refusal of %+v does not name %q: %v", c.p, part, err)
			}
		}
		if verr := c.p.validate(); verr == nil {
			t.Errorf("Profile.validate accepts %+v", c.p)
		}
	}
}

// The keys resolve per root over a top-level default, and a bad value is refused at either
// layer.
func TestAudioConfig_KeysResolvePerRoot(t *testing.T) {
	a, b, state := hwRoots(t)
	c := loadYAML(t, "state_dir: "+state+"\naudio_codec: opus\naudio_loudness: ebu_r128\nlibrary_roots:\n  - "+a+"\n"+
		"  - path: "+b+"\n    audio_reencode: on\n    audio_codec: aac\n    audio_stereo_kbps: 160\n")
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	got := map[string]audio.Settings{}
	for _, r := range c.RootProfiles() {
		got[r.Clean] = r.Profile.AudioSettings()
	}
	if s := got[a]; s.Active() || s.Codec != "opus" || !s.Loudness {
		t.Errorf("root a = %+v, want opus and loudness, inert", s)
	}
	if s := got[b]; !s.Reencode || s.Codec != "aac" || !s.Loudness || s.BitratesKbps[audio.Stereo] != 160 {
		t.Errorf("root b = %+v", s)
	}
	for name, body := range map[string]string{
		"top level": "audio_reencode: on\nlibrary_roots:\n  - " + a + "\n",
		"a root":    "library_roots:\n  - " + a + "\n  - path: " + b + "\n    audio_downmix: stereo\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := load(t, "state_dir: "+state+"\n"+body)
			if err == nil {
				err = c.Validate()
			}
			if err == nil || !strings.Contains(err.Error(), "audio_codec is required") {
				t.Errorf("a re-encode with no codec: %v, want its refusal", err)
			}
		})
	}
}
