package engine

import (
	"context"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hwdevice"
	"github.com/NSchatz/holdfast/internal/probe"
)

// ProbeEncode is the encode a capability probe runs (encoder.EncodeFunc): this build's own
// encoder, deriving the probe's plan from cfg's top level with the encoder set to the one
// probed and the pixel format to the probe's, through the derivation and the command-line
// builder every job's encode goes through - the device options, the upload, the explicit
// pixel format, the quality option and the container included. A probe therefore cannot pass
// on a command line no job would run, nor fail on one no job would run
// (docs/design/hardware.md#probe).
//
// The encode profiles are dropped: they select by path, and a probe's clip is under none.
func ProbeEncode(cfg config.Config, ffmpeg string, prober *probe.Prober, devices hwdevice.Assignment) encoder.EncodeFunc {
	return func(ctx context.Context, spec encoder.Spec, pixelFormat, src, out string) error {
		c := cfg
		c.Encoder = spec.Key
		if pixelFormat != "" {
			c.PixelFormat = pixelFormat
		}
		c.EncodeProfiles = nil
		return FFmpegEncoder{FFmpeg: ffmpeg, Cfg: c, Probe: prober, Devices: devices}.Encode(ctx, src, out, nil)
	}
}
