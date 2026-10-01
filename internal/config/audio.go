package config

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/audio"
)

// THE AUDIO KEYS (docs/design/audio.md). Every one is a library root knob with a top-level
// default, and every one is off until configured: with none of them set, every carried audio
// track is copied, exactly as this build always did.
const (
	audioReencodeKey     = "audio_reencode"
	audioCodecKey        = "audio_codec"
	audioMonoKbpsKey     = "audio_mono_kbps"
	audioStereoKbpsKey   = "audio_stereo_kbps"
	audio51KbpsKey       = "audio_51_kbps"
	audio71KbpsKey       = "audio_71_kbps"
	keepOriginalAudioKey = "keep_original_audio"
	audioDownmixKey      = "audio_downmix"
	audioLoudnessKey     = "audio_loudness"
)

// The values of the three switch keys.
const (
	// AudioOff is the default of audio_reencode, audio_downmix and audio_loudness.
	AudioOff = "off"
	// AudioReencodeOn re-encodes every carried lossless track to audio_codec.
	AudioReencodeOn = "on"
	// AudioDownmixStereo adds a stereo downmix of a surround track (docs/design/audio.md#downmix).
	AudioDownmixStereo = "stereo"
	// AudioLoudnessR128 normalises every re-encoded and added track to EBU R 128 in two passes.
	AudioLoudnessR128 = "ebu_r128"
)

// audioKnobs are the audio keys in the order they are printed and digested.
var audioKnobs = []string{audioReencodeKey, audioCodecKey, audioMonoKbpsKey, audioStereoKbpsKey,
	audio51KbpsKey, audio71KbpsKey, keepOriginalAudioKey, audioDownmixKey, audioLoudnessKey}

// audioKbpsKeys are the per-layout bitrate keys, by the family each sets.
var audioKbpsKeys = map[audio.Family]string{
	audio.Mono: audioMonoKbpsKey, audio.Stereo: audioStereoKbpsKey,
	audio.Surround51: audio51KbpsKey, audio.Surround71: audio71KbpsKey,
}

// audioDefaults are the keys' built-in defaults, as the defaults layer carries them.
func audioDefaults() map[string]any {
	return map[string]any{
		audioReencodeKey: AudioOff, audioCodecKey: "",
		audioMonoKbpsKey: 0, audioStereoKbpsKey: 0, audio51KbpsKey: 0, audio71KbpsKey: 0,
		keepOriginalAudioKey: false, audioDownmixKey: AudioOff, audioLoudnessKey: AudioOff,
	}
}

// AudioSettings is the resolved audio keys of this profile, as the audio plan reads them. A
// Profile assembled in Go with none of them set resolves to every key off.
func (p Profile) AudioSettings() audio.Settings {
	s := audio.Settings{
		Reencode:     p.AudioReencode == AudioReencodeOn,
		Codec:        p.AudioCodec,
		KeepOriginal: p.KeepOriginalAudio != nil && *p.KeepOriginalAudio,
		Downmix:      p.AudioDownmix == AudioDownmixStereo,
		Loudness:     p.AudioLoudness == AudioLoudnessR128,
	}
	for f, v := range p.audioKbps() {
		if v > 0 {
			if s.BitratesKbps == nil {
				s.BitratesKbps = map[audio.Family]int{}
			}
			s.BitratesKbps[f] = v
		}
	}
	return s
}

func (p Profile) audioKbps() map[audio.Family]int {
	return map[audio.Family]int{audio.Mono: p.AudioMonoKbps, audio.Stereo: p.AudioStereoKbps,
		audio.Surround51: p.Audio51Kbps, audio.Surround71: p.Audio71Kbps}
}

// switchValue is a switch key resolved: "" (a Profile assembled in Go) is off.
func switchValue(v string) string {
	if v == "" {
		return AudioOff
	}
	return v
}

// audioValues renders the audio knobs resolved, in audioKnobs order.
func (p Profile) audioValues() []string {
	keep := p.KeepOriginalAudio != nil && *p.KeepOriginalAudio
	return []string{
		switchValue(p.AudioReencode), p.AudioCodec,
		strconv.Itoa(p.AudioMonoKbps), strconv.Itoa(p.AudioStereoKbps),
		strconv.Itoa(p.Audio51Kbps), strconv.Itoa(p.Audio71Kbps),
		strconv.FormatBool(keep), switchValue(p.AudioDownmix), switchValue(p.AudioLoudness),
	}
}

// audioDigestSilent reports whether an audio knob at its rendered value is its default, which
// contributes nothing to the digest: a root that sets none of them digests as it did before
// the keys existed.
func audioDigestSilent(knob, value string) (silent, isAudio bool) {
	switch knob {
	case audioReencodeKey, audioDownmixKey, audioLoudnessKey:
		return value == AudioOff, true
	case audioCodecKey:
		return value == "", true
	case audioMonoKbpsKey, audioStereoKbpsKey, audio51KbpsKey, audio71KbpsKey:
		return value == "0", true
	case keepOriginalAudioKey:
		return value == "false", true
	}
	return false, false
}

// validateAudio refuses every audio value this build cannot honour, naming the key, the value
// and what it accepts. A codec is required where a re-encode or a downmix is asked for; every
// other key may be set alone and is then inert, so a top-level default a root switches on is
// not refused at the top level.
func (p Profile) validateAudio() error {
	for _, k := range []struct{ key, v, a, b string }{
		{audioReencodeKey, p.AudioReencode, AudioOff, AudioReencodeOn},
		{audioDownmixKey, p.AudioDownmix, AudioOff, AudioDownmixStereo},
		{audioLoudnessKey, p.AudioLoudness, AudioOff, AudioLoudnessR128},
	} {
		if k.v != "" && k.v != k.a && k.v != k.b {
			return fmt.Errorf("%s %q is not a value this build accepts (known: %s, %s)", k.key, k.v, k.a, k.b)
		}
	}
	if p.AudioCodec != "" && !audio.KnownCodec(p.AudioCodec) {
		return fmt.Errorf("%s %q is not a codec this build encodes audio to (known: %s)",
			audioCodecKey, p.AudioCodec, strings.Join(audio.Codecs, ", "))
	}
	if p.AudioCodec == "" && (p.AudioReencode == AudioReencodeOn || p.AudioDownmix == AudioDownmixStereo) {
		return fmt.Errorf("%s is required where %s is %s or %s is %s (known: %s)", audioCodecKey,
			audioReencodeKey, AudioReencodeOn, audioDownmixKey, AudioDownmixStereo, strings.Join(audio.Codecs, ", "))
	}
	for _, f := range audio.Families {
		v := p.audioKbps()[f]
		key := audioKbpsKeys[f]
		switch {
		case v < 0:
			return fmt.Errorf("%s %d is negative (0 is the codec's default)", key, v)
		case v > 0 && p.AudioCodec == audio.CodecAC3 && !audio.ValidAC3Bitrate(v):
			// AC-3 snaps any other bitrate to the nearest of its own without a word, so the
			// track would not be at the bitrate the row records (libavcodec/ac3enc.c:2300-2315).
			return fmt.Errorf("%s %d is not a bitrate AC-3 carries (one of %s kb/s, at most %d)", key, v,
				joinInts(audio.AC3Bitrates), audio.AC3MaxKbps)
		}
	}
	return nil
}

func joinInts(v []int) string {
	s := make([]string, len(v))
	for i, n := range v {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ", ")
}
