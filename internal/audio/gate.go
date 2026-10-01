package audio

import (
	"fmt"
	"math"
	"strings"
)

// THE AUDIO GATES' arithmetic (docs/design/audio.md#audio-gates): pure checks over what the
// probe and a full decode established about an output track, against what the plan declares
// for it. Packet counts are not compared: they are a function of each codec's frame size, not
// of the content (verify-streams-hdr.md claim 9).

// frameSamples is the samples one frame of each codec carries at its own rate: aac 1024
// (libavcodec/aacenc.c:1574), ac3 and eac3 six blocks of 256, 1536 (libavcodec/ac3enc.c:2502),
// and libopus 20 ms by default, 960 at 48 kHz (libavcodec/libopusenc.c:278 and :548). Sources
// at https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-10-01.
func frameSeconds(codec string, rate int) float64 {
	switch codec {
	case CodecAAC:
		return 1024 / float64(rate)
	case CodecAC3, CodecEAC3:
		return 1536 / float64(rate)
	case CodecOpus:
		return 0.020
	}
	return 0
}

// DurationTolerance is how far a transformed track's decoded length may sit from its source
// track's: TWO frames of the output codec. One for the last frame, which an encoder pads out
// to its frame size, and one for the encoder's priming, which each of them states and which
// is at most a frame - aac 1024 samples (aacenc.c:1575), ac3 and eac3 256 (ac3enc.c:2503),
// libopus its lookahead, 312 samples here (libopusenc.c:431; ASSUMED at most one frame for any
// libopus build). At 48 kHz that is 43 ms for aac, 64 ms for ac3 and eac3 and 40 ms for opus:
// a truncated encode is out by far more, and a faithful one, measured on every codec here, is
// out by none (the demuxers trim the priming; research-streams-hdr.md section 2.6).
func DurationTolerance(codec string, rate int) float64 {
	if rate <= 0 {
		return 0
	}
	return 2 * frameSeconds(codec, rate)
}

// CheckDuration is the duration gate over one transformed track.
func CheckDuration(o Op, source, output float64) error {
	tol := DurationTolerance(o.Codec, o.SampleRate)
	if math.IsNaN(source) || math.IsNaN(output) || math.Abs(source-output) > tol {
		return fmt.Errorf("audio a:%d decodes to %.3fs and its source stream %d to %.3fs, more than the %.3fs "+
			"two %s frames allow", o.Output, output, o.Source.Index, source, tol, o.Codec)
	}
	return nil
}

// Observed is what the probe read off one output audio stream.
type Observed struct {
	Codec      string
	Channels   int
	Layout     string
	SampleRate int
}

// CheckLayout is the channel gate over one transformed track: the codec, the channel count
// AND the layout equal to the plan. The count catches a fold the layout name alone might not
// (ac3 writing a 7.1 source as 5.1), and the layout a re-ordering the count cannot see.
func CheckLayout(o Op, got Observed) error {
	var bad []string
	if got.Codec != OutputCodecName(o.Codec) {
		bad = append(bad, fmt.Sprintf("codec %q, not %q", got.Codec, OutputCodecName(o.Codec)))
	}
	if got.Channels != o.Channels {
		bad = append(bad, fmt.Sprintf("%d channels, not %d", got.Channels, o.Channels))
	}
	if got.Layout != o.Layout {
		bad = append(bad, fmt.Sprintf("channel layout %q, not %q", got.Layout, o.Layout))
	}
	if len(bad) > 0 {
		return fmt.Errorf("audio a:%d (%s of source stream %d) carries %s", o.Output, o.Action, o.Source.Index,
			strings.Join(bad, ", "))
	}
	return nil
}

// CheckSampleRate is the sample-rate gate over one transformed track.
func CheckSampleRate(o Op, got Observed) error {
	if got.SampleRate != o.SampleRate {
		return fmt.Errorf("audio a:%d (%s of source stream %d) is at %d Hz, not the %d Hz its plan declares",
			o.Output, o.Action, o.Source.Index, got.SampleRate, o.SampleRate)
	}
	return nil
}
