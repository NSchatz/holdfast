// Package audio is what an encode may do to a source's audio tracks, as pure decisions over
// probed facts: which tracks qualify for a re-encode, which codec carries which layout at
// which sample rate and bitrate, the stereo downmix, the two-pass EBU R128 loudness filters,
// and the acceptance checks an output's audio is held to (docs/design/audio.md). The engine
// derives one Plan per job from these and builds the command line and the gates from it.
//
// Nothing here reads the configuration package: config validates its keys against the
// tables in this package, so the codecs, layouts and bitrates a key accepts and the ones a
// plan can be built from are one list.
package audio

import (
	"strings"
)

// The codecs an audio re-encode targets, in the configuration's spelling. Each is the
// pinned ffmpeg's own encoder of that format: the native aac, ac3 and eac3 encoders and
// libopus. libfdk_aac is never used: the pinned build is configured --disable-libfdk-aac,
// and it is not free software (research-streams-hdr.md section 2.1).
const (
	CodecAAC  = "aac"
	CodecAC3  = "ac3"
	CodecEAC3 = "eac3"
	CodecOpus = "opus"
)

// Codecs is every codec a re-encode may target, in the order a refusal names them.
var Codecs = []string{CodecAAC, CodecAC3, CodecEAC3, CodecOpus}

// KnownCodec reports whether c is a codec a re-encode may target.
func KnownCodec(c string) bool {
	for _, k := range Codecs {
		if k == c {
			return true
		}
	}
	return false
}

// Encoder is the ffmpeg encoder that produces codec c, and "" for a codec this build does
// not target.
func Encoder(c string) string {
	switch c {
	case CodecAAC, CodecAC3, CodecEAC3:
		return c
	case CodecOpus:
		return "libopus"
	}
	return ""
}

// Family is a channel family the codec and layout matrix is written over, named by its
// channel count. A track in no family is copied, never re-encoded.
type Family int

// The four families. 5.1 and 5.1(side) are one family: the six channels are the same
// speakers either side of the listener, and each codec writes the family in its own one
// spelling (see OutputLayout).
const (
	Mono       Family = 1
	Stereo     Family = 2
	Surround51 Family = 6
	Surround71 Family = 8
)

// Families is every family, in the order the per-layout bitrate keys are spelled.
var Families = []Family{Mono, Stereo, Surround51, Surround71}

// String is the family as an operator writes a layout.
func (f Family) String() string {
	switch f {
	case Mono:
		return "mono"
	case Stereo:
		return "stereo"
	case Surround51:
		return "5.1"
	case Surround71:
		return "7.1"
	}
	return "unknown"
}

// FamilyOf is the family a source's ffprobe channel_layout belongs to, and false for every
// layout outside the matrix: 7.1(wide), 6.1, quad, a layout ffprobe could only count ("6
// channels"), and none at all. Those are copied with a recorded reason rather than handed
// to an encoder that would re-order or fold their channels silently.
func FamilyOf(layout string) (Family, bool) {
	switch strings.TrimSpace(layout) {
	case "mono":
		return Mono, true
	case "stereo":
		return Stereo, true
	case "5.1", "5.1(side)":
		return Surround51, true
	case "7.1":
		return Surround71, true
	}
	return 0, false
}

// OutputLayout is THE CODEC AND LAYOUT MATRIX: the channel layout codec writes family
// in, and false where the codec cannot carry it. The layout is named on the command line
// (-ch_layout) so ffmpeg never negotiates one, and it is the layout the gate then requires
// ffprobe to read back from the output - each spelling below is the one the pinned ffprobe
// reads back from both Matroska and MP4 (measured 2026-10-01).
//
//   - aac and libopus: mono, stereo, 5.1, 7.1. A 5.1(side) source is written 5.1: the side
//     pair is mapped to the back pair at unity gain by swresample, and each encoder writes
//     only the back form (libopus refuses 5.1(side) outright).
//   - ac3 and eac3: mono, stereo, 5.1(side), and NOT 7.1. The pinned build's encoders list
//     layouts up to 5.1 only (ff_ac3_ch_layouts, libavcodec/ac3enc.c:152-190) and, with no
//     -ch_layout, fold a 7.1 source to 5.1 with exit 0 and no warning
//     (verify-streams-hdr.md claim 8). A 5.1 source in either spelling reads back 5.1(side).
//
// Sources: https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 libavcodec/ac3enc.c, read
// 2026-10-01.
func OutputLayout(codec string, f Family) (string, bool) {
	switch f {
	case Mono, Stereo:
		if KnownCodec(codec) {
			return f.String(), true
		}
	case Surround51:
		switch codec {
		case CodecAAC, CodecOpus:
			return "5.1", true
		case CodecAC3, CodecEAC3:
			return "5.1(side)", true
		}
	case Surround71:
		switch codec {
		case CodecAAC, CodecOpus:
			return "7.1", true
		}
	}
	return "", false
}

// sampleRates are the rates each encoder takes, from `ffmpeg -h encoder=<x>` on the pinned
// build (2026-10-01): aac lists 96000 to 7350, ac3 and eac3 48000, 44100 and 32000
// (ff_ac3_sample_rate_tab, libavcodec/ac3tab.c:96), libopus 48000, 24000, 16000, 12000 and
// 8000.
var sampleRates = map[string][]int{
	CodecAAC:  {96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350},
	CodecAC3:  {48000, 44100, 32000},
	CodecEAC3: {48000, 44100, 32000},
	CodecOpus: {48000, 24000, 16000, 12000, 8000},
}

// FallbackRate is the rate a track is written at where its codec does not take the source's
// own: every codec here takes it.
const FallbackRate = 48000

// SampleRateFor is the rate codec writes a source of rate src at: the source's own where the
// codec takes it, and FallbackRate otherwise. The plan declares it, the command line names it
// (-ar, and the aresample after a loudness filter), and the gate requires it.
func SampleRateFor(codec string, src int) int {
	for _, r := range sampleRates[codec] {
		if r == src {
			return src
		}
	}
	return FallbackRate
}

// AC3Bitrates are the only bitrates an AC-3 stream can carry, in kb/s
// (ff_ac3_bitrate_tab, libavcodec/ac3tab.c:99-102 at
// https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-10-01). The encoder snaps any
// other request to the nearest of them without a word (libavcodec/ac3enc.c:2300-2315), so a
// configured AC-3 bitrate must be one of them, and 640 is the maximum.
var AC3Bitrates = []int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}

// AC3MaxKbps is AC-3's largest bitrate.
const AC3MaxKbps = 640

// ValidAC3Bitrate reports whether kbps is one of AC3Bitrates.
func ValidAC3Bitrate(kbps int) bool {
	for _, b := range AC3Bitrates {
		if b == kbps {
			return true
		}
	}
	return false
}

// defaultBitrates are the bitrate each codec is given per family where the configuration
// sets none, in kb/s.
//
//   - opus: stereo 128, 5.1 256, 7.1 450 - the top of the Xiph music-storage ranges ("96 -
//     128", "128 - 256", "256 - 450"; "Opus at 128 KB/s (VBR) is pretty much transparent"),
//     https://wiki.xiph.org/Opus_Recommended_Settings , read 2026-10-01. Mono 64: ASSUMED
//     (half the stereo figure; the page gives no mono music figure).
//   - ac3: mono 96 and stereo 192 are the pinned encoder's own defaults
//     (libavcodec/ac3enc.c:2242-2246 at https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 ,
//     read 2026-10-01); 5.1 640, AC-3's maximum: ASSUMED (a lossless source deserves the
//     ceiling).
//   - eac3: ASSUMED, the AC-3 figures.
//   - aac: ASSUMED, 64 kb/s per channel (the AAC page of the FFmpeg wiki was unreachable,
//     2026-10-01).
var defaultBitrates = map[string]map[Family]int{
	CodecAAC:  {Mono: 64, Stereo: 128, Surround51: 384, Surround71: 512},
	CodecAC3:  {Mono: 96, Stereo: 192, Surround51: 640},
	CodecEAC3: {Mono: 96, Stereo: 192, Surround51: 640},
	CodecOpus: {Mono: 64, Stereo: 128, Surround51: 256, Surround71: 450},
}

// DefaultBitrateKbps is codec's bitrate for family where none is configured, and 0 where the
// codec does not carry the family.
func DefaultBitrateKbps(codec string, f Family) int { return defaultBitrates[codec][f] }

// MaxBitrateKbps is the largest bitrate codec writes channels at rate without changing it
// silently, in kb/s, read from the pinned encoders
// (https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-10-01):
//
//   - aac clamps a frame to 6144 bits per channel (libavcodec/aacenc.c:1364-1365), so 6
//     bits per sample per channel;
//   - ac3 640 (AC3MaxKbps);
//   - eac3 refuses more than 2048 words of 16 bits per one-block frame of 256 samples
//     (libavcodec/ac3enc.c:2262-2274), 128 bits per sample;
//   - libopus refuses more than 256000 b/s per channel (libavcodec/libopusenc.c:399-402).
func MaxBitrateKbps(codec string, channels, rate int) int {
	switch codec {
	case CodecAAC:
		return 6 * rate * channels / 1000
	case CodecAC3:
		return AC3MaxKbps
	case CodecEAC3:
		return 128 * rate / 1000
	case CodecOpus:
		return 256 * channels
	}
	return 0
}

// OutputCodecName is what ffprobe names codec's output: libopus writes "opus".
func OutputCodecName(c string) string { return c }

// Lossless reports whether a source track is one a re-encode replaces: TrueHD, DTS-HD Master
// Audio (codec dts with a "DTS-HD MA" profile, which is what tells it from a lossy DTS core),
// any PCM, and FLAC (docs/design/audio.md#reencode). Every other track is copied.
func Lossless(codec, profile string) bool {
	switch {
	case codec == "truehd", codec == "flac":
		return true
	case strings.HasPrefix(codec, "pcm_"):
		return true
	case codec == "dts":
		return strings.HasPrefix(strings.TrimSpace(profile), "DTS-HD MA")
	}
	return false
}

// SupportedContainer reports whether an output container (its extension, lower case) is one
// an audio transformation is written into: Matroska and MP4, the two whose read-back of
// every codec and layout above was measured. Any other output copies its audio, with the
// reason recorded.
func SupportedContainer(ext string) bool {
	switch strings.ToLower(ext) {
	case "mkv", "mp4":
		return true
	}
	return false
}
