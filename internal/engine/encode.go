package engine

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/NSchatz/transcode/internal/config"
	"github.com/NSchatz/transcode/internal/hdr"
	"github.com/NSchatz/transcode/internal/probe"
)

// Encoder produces the re-encoded output file. It is an interface so tests can
// inject deterministic fakes (simulating an encode error, a corrupt output, a
// too-large output, a truncated encode, or a dropped track) without depending on
// codec/compression luck — mirroring the bash suite's TRANSCODER_TEST_HOOKS seam.
// An Encoder must write its result to out and return nil only if it believes the
// encode succeeded; the engine independently VERIFIES the output before any swap,
// so an Encoder is never trusted on its word.
type Encoder interface {
	Encode(ctx context.Context, in, out string) error
}

// EncoderFunc adapts a plain function to Encoder (used by tests).
type EncoderFunc func(ctx context.Context, in, out string) error

// Encode calls the wrapped function.
func (f EncoderFunc) Encode(ctx context.Context, in, out string) error { return f(ctx, in, out) }

// FFmpegEncoder is the production CPU (libx265) encoder. It carries every stream
// but data streams (-map 0 -map -0:d?), stream-copies audio/subtitle/attachment,
// and re-encodes video to libx265 at the configured CRF/preset/pixel format.
// Colour/HDR propagation (TRANSCODE-3) derives -color_* flags and x265 colour
// params from the source via internal/hdr — Probe is required to do that
// derivation, so it must be set in production (a nil Probe is a programmer error).
//
// Hardware encoders (NVENC/QSV/VAAPI/AMF) and SVT-AV1 are TRANSCODE-6. This is the
// archival CPU path only.
type FFmpegEncoder struct {
	FFmpeg string
	Cfg    config.Config
	Probe  *probe.Prober
}

// Encode runs ffmpeg. It returns an error if ffmpeg exits non-zero (the engine
// then discards the temp and leaves the source untouched).
func (e FFmpegEncoder) Encode(ctx context.Context, in, out string) error {
	if e.Cfg.Encoder != "cpu" {
		return fmt.Errorf("unknown encoder %q (TRANSCODE-1 supports cpu; nvenc/av1 arrive in TRANSCODE-6)", e.Cfg.Encoder)
	}
	if e.Probe == nil {
		return fmt.Errorf("FFmpegEncoder.Probe is nil (required to derive colour/pixel-format args from the source)")
	}

	pixFmt := e.Cfg.PixelFormat
	if e.Cfg.PixelFormatAuto() {
		derived, ok := hdr.DerivePixFmt(e.Probe.PixFmt(ctx, in))
		if !ok {
			// The engine's pix_fmt guard runs before Encode and should already have
			// skipped an exotic source — this is a defence-in-depth backstop so the
			// encoder itself never silently subsamples if that guard is ever bypassed.
			return fmt.Errorf("cannot derive an output pixel format for %q (unrecognized/exotic source pix_fmt)", in)
		}
		pixFmt = derived
	}

	// Colour/HDR propagation: carry the source's primaries/transfer/matrix + HDR10
	// static metadata forward instead of letting the encode silently drop them.
	// DV/HDR10+ were already detected-and-skipped upstream by the engine.
	colorArgs, x265Color := hdr.DeriveColorArgs(ctx, e.Probe, in)

	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", in,
		"-map", "0", "-map", "-0:d?",
		"-c", "copy", "-c:v", "libx265",
		"-preset", e.Cfg.Preset,
		"-crf", fmt.Sprintf("%d", e.Cfg.CRF),
		"-pix_fmt", pixFmt,
		"-fps_mode", "passthrough", // a VFR source is not forced to CFR
	}
	args = append(args, colorArgs...)
	args = append(args, "-x265-params", "log-level=error"+x265Color, "--", out)

	cmd := exec.CommandContext(ctx, e.FFmpeg, args...)
	if outb, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg encode: %w: %s", err, truncate(string(outb), 500))
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
