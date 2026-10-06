// Package queuekey computes the `queue_order: savings_per_hour` ordering key: an estimate of
// the bytes one source's encode will reclaim per hour of the work it costs to produce and
// verify the replacement.
//
// It is a pure function of the facts handed to it - no probe, no file, no clock - so the
// arithmetic is checked against the worked example in docs/design/queue-order.md
// (#savings-per-hour) to the figure, and the engine's only job is to read the facts once per
// candidate and hand them here.
//
// THE KEY ORDERS AND IT IS NEVER PUBLISHED. Nothing here is a prediction an operator is
// shown: README.md states there is deliberately no per-file estimated saving, and an
// ordering key that leaked into a report would become one. What matters about the figure is
// how it RANKS two sources, and the model below is built so that ranking follows the
// operator's case (S0164, the operator's report quoted in the umbrella spec): bytes saved
// rise with the source bitrate in excess of what the encode is expected to produce for that
// picture, and the work rises with pixels times frames.
// measure: a throwaway comment.
package queuekey

import (
	"errors"
	"fmt"
	"math"
)

// Source is what the engine's probe established about one candidate. Every field must be
// established and positive, or the key cannot be read (see Estimate).
type Source struct {
	// VideoKbps is the source's video bitrate in kbit/s: the video stream's bit_rate, else
	// the container's, as the engine's probe resolves it (probe.VideoProps.BitrateKbps).
	VideoKbps int
	// Width and Height are the coded picture dimensions in pixels.
	Width, Height int
	// DurationSec is the container duration in seconds.
	DurationSec float64
}

// Target is what the job that would encode this source is expected to produce, as its
// resolved configuration says.
type Target struct {
	// BitrateKbps is the configured `bitrate_kbps` target of the job (the matching encode
	// profile's, else the top level's); 0 is no target, and the expected output bitrate is
	// then the bits-per-pixel model's.
	BitrateKbps int
	// Width and Height are the OUTPUT picture: the source's own, or what a `max_height`
	// ceiling scales it to. 0 means the source's own.
	Width, Height int
	// RemuxOnly is a job that re-encodes no video: its video bitrate is expected to be the
	// source's, so its estimated saving is zero.
	RemuxOnly bool
}

// Model holds the constants the estimate is built from. Default is the one the engine
// uses; a test builds its own only to show which constant a ranking depends on.
type Model struct {
	// FrameRate is the frames per second assumed for every source. ASSUMED: the probe the
	// engine already takes does not read the frame rate, and adding a field to it would
	// change the one snapshot probe every job takes. Because it is the same for every
	// candidate it scales every key alike and never changes a ranking.
	FrameRate float64
	// BitsPerPixel is the expected output video bits per pixel per frame when the job
	// names no bitrate target. Derived, not measured here: S0164's operator report states a
	// 1080p Blu-ray source at 21.6 Mbps saves about 85%, i.e. comes out near 3.24 Mbps, and
	// 3240 kbit/s over 1920x1080 at FrameRate is 0.0652 bits per pixel, rounded to 0.065.
	// One figure serves every encoder family: no per-family figure is measured here, so
	// none is invented (ASSUMED for av1 and h264).
	BitsPerPixel float64
	// EncodePixelsPerSec, DecodePixelsPerSec and VMAFPixelsPerSec are the throughput of the
	// three passes a replacement costs - the encode, the full decode-integrity read of the
	// output, and the VMAF comparison - in source pixels per second. See Default for where
	// each figure comes from. Each is the same for every candidate, so together they scale
	// every key alike: a ranking depends on them only through the work being proportional
	// to pixels times frames.
	EncodePixelsPerSec float64
	DecodePixelsPerSec float64
	VMAFPixelsPerSec   float64
}

// Default is the model the engine orders by. The three throughputs were measured here
// 2026-10-01 with the pinned ffmpeg, on one synthetic 1920x1080 lavfi clip of 120 frames at
// the shipped defaults (libx265, preset slow, crf 22, yuv420p10le), on the build host's 4
// CPUs; docs/design/queue-order.md#savings-per-hour records the command and the timings.
// Real content encodes at a different speed than a synthetic clip, so the absolute figure
// is not a promise about any library - and it does not need to be one, since a common scale
// changes no ranking.
var Default = Model{
	FrameRate:          24000.0 / 1001.0,
	BitsPerPixel:       0.065,
	EncodePixelsPerSec: encodePixelsPerSec,
	DecodePixelsPerSec: decodePixelsPerSec,
	VMAFPixelsPerSec:   vmafPixelsPerSec,
}

// Estimate is one candidate's key and every intermediate figure it was computed from, so
// the worked example can be checked line by line.
type Estimate struct {
	// OutputKbps is the video bitrate the encode is expected to produce.
	OutputKbps float64
	// SavedBytes is the bytes the encode is expected to reclaim: the source's video bitrate
	// in excess of OutputKbps, over the duration. Negative where the source is already at
	// or below what the encode is expected to produce.
	SavedBytes float64
	// Frames is the duration times the model's frame rate.
	Frames float64
	// WorkSec is the expected encode plus decode-integrity plus VMAF time, in seconds.
	WorkSec float64
	// BytesPerHour is the ordering key: SavedBytes per hour of WorkSec, rounded down to a
	// whole byte (at least 1 where SavedBytes is positive), and 0 where it is not positive - every such candidate is offered
	// after every candidate with a positive saving, and among themselves on the path.
	BytesPerHour int64
}

// ErrUnestablished is wrapped by every refusal to estimate: a fact the probe did not
// establish. The engine reads it as an unreadable key - the candidate is offered after every
// candidate whose key was read, never dropped.
var ErrUnestablished = errors.New("source facts not established")

// Estimate computes one candidate's savings-per-hour key under m.
//
//	pixels       = Width x Height                                  (source)
//	outPixels    = Target.Width x Target.Height, or pixels when unset
//	OutputKbps   = Source.VideoKbps                     if RemuxOnly
//	             = Target.BitrateKbps                    if > 0
//	             = BitsPerPixel x outPixels x FrameRate / 1000   otherwise
//	SavedBytes   = (VideoKbps - OutputKbps) x 1000 / 8 x DurationSec
//	Frames       = DurationSec x FrameRate
//	WorkSec      = Frames x pixels x (1/Encode + 1/Decode + 1/VMAF pixels per second)
//	BytesPerHour = max(1, floor(SavedBytes / WorkSec x 3600)), or 0 when SavedBytes <= 0
//
// The duration appears in both SavedBytes and WorkSec and cancels out of the ratio: two
// sources of one picture size and bitrate earn one key whatever their lengths, which is the
// point - a long film is not worth more per hour of work than a short one, only more in all.
// It is still required to be established, because a source whose length nobody can read is
// one whose key nobody can read.
func (m Model) Estimate(src Source, t Target) (Estimate, error) {
	if err := established(src); err != nil {
		return Estimate{}, err
	}
	pixels := float64(src.Width) * float64(src.Height)
	outPixels := pixels
	if t.Width > 0 && t.Height > 0 {
		outPixels = float64(t.Width) * float64(t.Height)
	}
	var e Estimate
	switch {
	case t.RemuxOnly:
		e.OutputKbps = float64(src.VideoKbps)
	case t.BitrateKbps > 0:
		e.OutputKbps = float64(t.BitrateKbps)
	default:
		e.OutputKbps = m.BitsPerPixel * outPixels * m.FrameRate / 1000
	}
	e.SavedBytes = (float64(src.VideoKbps) - e.OutputKbps) * 1000 / 8 * src.DurationSec
	e.Frames = src.DurationSec * m.FrameRate
	perPixel := 1/m.EncodePixelsPerSec + 1/m.DecodePixelsPerSec + 1/m.VMAFPixelsPerSec
	e.WorkSec = e.Frames * pixels * perPixel
	if e.SavedBytes > 0 {
		// At least 1: a positive saving too small to reach a whole byte per hour is still a
		// positive saving, and must not tie with the candidates that save nothing.
		e.BytesPerHour = max(1, int64(math.Floor(e.SavedBytes/e.WorkSec*3600)))
	}
	return e, nil
}

// established refuses a Source with any fact missing, zero or not finite, naming the fact.
func established(src Source) error {
	switch {
	case src.VideoKbps <= 0:
		return fmt.Errorf("%w: the video bitrate is unknown or zero", ErrUnestablished)
	case src.Width <= 0 || src.Height <= 0:
		return fmt.Errorf("%w: the picture dimensions are unknown or zero", ErrUnestablished)
	case !(src.DurationSec > 0) || math.IsInf(src.DurationSec, 0):
		return fmt.Errorf("%w: the duration is unknown or zero", ErrUnestablished)
	}
	return nil
}

// The three measured throughputs Default carries, in source pixels per second, each rounded
// to three significant figures. Measured here 2026-10-01 with the pinned ffmpeg
// (N-125875-g5d4d3bdc61) on 4 CPUs of an Intel Xeon E5-2680 v4: 120 frames of 1920x1080
// (248,832,000 pixels) of `testsrc2` with temporal noise took 43.42 s to encode (libx265,
// preset slow, crf 22, yuv420p10le), 2.39 s to decode in full, and 24.06 s to score with
// libvmaf at its default model. docs/design/queue-order.md#savings-per-hour has the commands.
const (
	encodePixelsPerSec = 5_730_000   // 248,832,000 / 43.42 s = 5,730,815
	decodePixelsPerSec = 104_000_000 // 248,832,000 / 2.39 s = 104,113,808
	vmafPixelsPerSec   = 10_300_000  // 248,832,000 / 24.06 s = 10,342,145
)
