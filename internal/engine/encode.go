package engine

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/NSchatz/transcode/internal/config"
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
//
// Scope note (TRANSCODE-1): colour/HDR propagation is deliberately NOT wired here —
// that is TRANSCODE-3 (HDR/source guards). Hardware encoders (NVENC/QSV/VAAPI/AMF)
// and SVT-AV1 are TRANSCODE-6. This is the archival CPU path only.
type FFmpegEncoder struct {
	FFmpeg string
	Cfg    config.Config
}

// Encode runs ffmpeg. It returns an error if ffmpeg exits non-zero (the engine
// then discards the temp and leaves the source untouched).
func (e FFmpegEncoder) Encode(ctx context.Context, in, out string) error {
	if e.Cfg.Encoder != "cpu" {
		return fmt.Errorf("unknown encoder %q (TRANSCODE-1 supports cpu; nvenc/av1 arrive in TRANSCODE-6)", e.Cfg.Encoder)
	}
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", in,
		"-map", "0", "-map", "-0:d?",
		"-c", "copy", "-c:v", "libx265",
		"-preset", e.Cfg.Preset,
		"-crf", fmt.Sprintf("%d", e.Cfg.CRF),
		"-pix_fmt", e.Cfg.PixelFormat,
		"-x265-params", "log-level=error",
		"--", out,
	}
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
