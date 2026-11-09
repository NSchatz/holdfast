package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/NSchatz/holdfast/internal/playability"
	"github.com/NSchatz/holdfast/internal/store"
)

// playabilityGate holds the output to the playability gate (docs/design/playability.md): its
// first moving video stream - the one a player starts - decodes from a cold start at a sample of
// its keyframes, each sampled fragment of its fragmented-MP4 remux decodes on its own, and, on a
// job that decodes on hardware, the cold starts decode on that hardware too.
//
// A failed decode is DETERMINISTIC: the output's bytes are a pure function of the source, the
// settings and the pinned ffmpeg build, so the same settings fail the same way, and the row is
// parked at once (a change to the settings, such as the encoder's GOP structure, is what re-opens
// it). Everything that only failed to MEASURE - the packets could not be listed, a seek did not
// land on its keyframe, the remux did not complete, the hardware did not decode the opening
// frames - is TRANSIENT, the fail-safe class, and the output is refused all the same.
func (e *Engine) playabilityGate(ctx context.Context, job *EncodePlan) (string, store.FailureClass, error) {
	streams, ok := e.Probe.VideoStreams(ctx, job.Output)
	stream := -1
	for _, s := range streams {
		if !s.AttachedPicture {
			stream = s.Index
			break
		}
	}
	if !ok || stream < 0 {
		return GatePlayability, store.FailureTransient, fmt.Errorf("playability gate: the output's video stream " +
			"could not be established, so its playability could not be measured. The source is kept")
	}
	c := playability.Checker{FFmpeg: e.Probe.FFmpeg, FFprobe: e.Probe.FFprobe}
	if job.Video.HardwareDecode() {
		c.Hardware = playabilityHardwareArgs(job.Video)
		// The hardware cold starts run through the binary the job's hardware decode ran
		// through: the encoder's.
		if fe, ok := e.Enc.(FFmpegEncoder); ok {
			c.HardwareFFmpeg = fe.FFmpeg
		}
	}
	rep, err := c.Check(ctx, job.Output, stream)
	if err != nil {
		var rej *playability.Rejection
		if errors.As(err, &rej) {
			return GatePlayability, store.FailureDeterministic, fmt.Errorf("playability gate: %w. The source is kept", err)
		}
		return GatePlayability, store.FailureTransient, fmt.Errorf("playability gate: %w. The source is kept", err)
	}
	e.Log.Info("playability gate passed", "file", job.Source, "keyframes", rep.Keyframes,
		"with_leading_pictures", rep.Leading, "cold_starts", len(rep.Sampled), "hardware", rep.Hardware)
	return "", "", nil
}

// playabilityHardwareArgs are the global options of a hardware decode of the OUTPUT on the
// job's own decode device: the decode half of hwDecodeArgs, without the encoder's device or the
// upload's filter device, which a decode to null has no use for.
func playabilityHardwareArgs(v VideoPlan) []string {
	switch v.Decode {
	case DecodeCUDA:
		return []string{"-hwaccel", "cuda"}
	case DecodeVAAPI:
		return []string{"-init_hw_device", "vaapi=hfva:" + v.DecodeDevice + ",connection_type=drm",
			"-hwaccel", "vaapi", "-hwaccel_device", "hfva"}
	}
	return nil
}
