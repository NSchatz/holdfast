package engine

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
)

// Encoder produces the re-encoded output file. It is an interface so tests can
// inject deterministic fakes (simulating an encode error, a corrupt output, a
// too-large output, a truncated encode, or a dropped track) without depending on
// codec/compression luck — mirroring the bash suite's TRANSCODER_TEST_HOOKS seam.
// An Encoder must write its result to out and return nil only if it believes the
// encode succeeded; the engine independently VERIFIES the output before any swap,
// so an Encoder is never trusted on its word.
//
// props is the source's probe snapshot the engine already took for its skip guards
// (TRANSCODE-PERF), threaded in so a production encoder reuses it instead of
// re-spawning ffprobe for pix_fmt and the colour tags. It may be nil (a direct caller
// — chiefly the tests — that did not pre-probe); FFmpegEncoder then takes its own
// snapshot of the same source, so behaviour is identical, just an extra probe this
// path avoids when the engine supplies one.
type Encoder interface {
	Encode(ctx context.Context, in, out string, props *probe.VideoProps) error
}

// EncoderFunc adapts a plain function to Encoder (used by tests).
type EncoderFunc func(ctx context.Context, in, out string, props *probe.VideoProps) error

// Encode calls the wrapped function.
func (f EncoderFunc) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	return f(ctx, in, out, props)
}

// FFmpegEncoder is the production encoder. It carries every stream but data
// streams (-map 0 -map -0:d?), stream-copies audio/subtitle/attachment, and
// re-encodes video per the configured Cfg.Encoder (internal/encoder.Lookup) at the
// configured CRF/preset/pixel format. Colour/HDR propagation (TRANSCODE-3) derives
// -color_* flags from the source via internal/hdr and applies to EVERY encoder
// (they are encoder-agnostic primaries/transfer/matrix/range tags); the x265
// colour params (HDR10 static-metadata master-display/max-cll) are libx265-only —
// see buildArgs. Probe is required to do the colour/pixel-format derivation, so it
// must be set in production (a nil Probe is a programmer error).
//
// CPU libx265 is the archival default; SVT-AV1 and the hardware encoders (NVENC/
// QSV/VAAPI/AMF, TRANSCODE-6) are opt-in and gated behind a runtime capability
// check (internal/encoder.Available) BEFORE the engine ever calls Encode — see
// cmd/holdfast's cmdRun. Encode itself never re-derives availability; it trusts
// the caller checked, and simply builds and runs the command for whatever Spec the
// configured Cfg.Encoder resolves to.
type FFmpegEncoder struct {
	FFmpeg string
	Cfg    config.Config
	Probe  *probe.Prober
}

// Encode runs ffmpeg. It returns an error if the configured encoder is unknown, if
// ffmpeg exits non-zero, or if colour/pixel-format derivation fails (the engine
// then discards the temp and leaves the source untouched).
func (e FFmpegEncoder) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	spec, ok := encoder.Lookup(e.Cfg.Encoder)
	if !ok {
		return fmt.Errorf("unknown encoder %q (known: %v)", e.Cfg.Encoder, encoder.Known())
	}
	if e.Probe == nil {
		return fmt.Errorf("FFmpegEncoder.Probe is nil (required to derive colour/pixel-format args from the source)")
	}
	// The engine threads in the snapshot it already probed for its skip guards
	// (TRANSCODE-PERF). A direct caller that did not pre-probe passes nil, so take one
	// here — it snapshots the same source file the guards read, so a fresh snapshot is
	// equivalent; this only avoids a duplicate probe when the engine supplies one.
	if props == nil {
		props = e.Probe.VideoProps(ctx, in)
	}

	pixFmt := e.Cfg.PixelFormat
	if e.Cfg.PixelFormatAuto() {
		derived, ok := hdr.DerivePixFmt(props.PixFmt())
		if !ok {
			// The engine's pix_fmt guard runs before Encode and should already have
			// skipped an exotic source — this is a defence-in-depth backstop so the
			// encoder itself never silently subsamples if that guard is ever bypassed.
			return fmt.Errorf("cannot derive an output pixel format for %q (unrecognized/exotic source pix_fmt)", in)
		}
		pixFmt = derived
	}

	// Colour/HDR propagation: carry the source's primaries/transfer/matrix/range
	// forward instead of letting the encode silently drop them. These -color_* flags
	// are encoder-agnostic and applied to every Spec. x265Color (HDR10 static
	// master-display/max-cll) is libx265-only — see buildArgs. DV/HDR10+ were
	// already detected-and-skipped upstream by the engine.
	colorArgs, x265Color := hdr.DeriveColorArgsFrom(
		props.Color("color_primaries"),
		props.Color("color_transfer"),
		props.Color("color_space"),
		props.Color("color_range"),
		props.SideData(),
	)

	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y"}
	if spec.Key == "vaapi" {
		// -vaapi_device is a GLOBAL option that must precede -i so the hwupload
		// filter (added below by buildArgs) has a device to target. Every other
		// Spec has no such ordering requirement.
		args = append(args, "-vaapi_device", "/dev/dri/renderD128")
	}
	args = append(args, "-i", in,
		"-map", "0", "-map", "-0:d?",
		"-c", "copy", "-c:v", spec.FFmpegCodec,
	)
	args = append(args, buildArgs(spec, e.Cfg, pixFmt, colorArgs, x265Color)...)
	args = append(args, "--", out)

	cmd := exec.CommandContext(ctx, e.FFmpeg, args...)
	if outb, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ffmpeg encode: %w: %s", err, truncate(string(outb), 500))
	}
	return nil
}

// buildArgs assembles the per-encoder ffmpeg args (everything after `-c:v
// <codec>`, before the trailing `-- <out>`). The -pix_fmt, -color_* flags and
// -fps_mode passthrough are UNIVERSAL — every Spec gets them, since they carry
// source fidelity independent of which codec/encoder produces the bytes. Beyond
// that each encoder family has its own quality-knob shape:
//
//   - libx265 (cpu): -preset/-crf plus x265Params (HDR10 static-metadata
//     master-display/max-cll — a libx265-only mechanism).
//   - libsvtav1 (svtav1): -preset (numeric 0-13, mapped from the config Preset
//     word — see svtav1Preset) + -crf. No x265Params: AV1 HDR10 static-metadata
//     carriage would need svt-av1-params mastering-display/content-light options,
//     which is OUT OF SCOPE here (see the package doc / CLAUDE.md) — colour
//     PRIMARIES/TRANSFER/MATRIX/RANGE tags still carry via the universal
//     -color_* flags, just not the mastering-display block. This mirrors the
//     existing NVENC limitation the bash transcoder already documented.
//   - hevc_nvenc/av1_nvenc: -rc vbr -cq <CRF> -b:v 0 (CRF reused as the CQ
//     target) + a preset.
//   - hevc_qsv: -global_quality <CRF>.
//   - hevc_vaapi: -vaapi_device (emitted by Encode, before -i — see Encode's
//     vaapi special case) + -vf format=nv12,hwupload + -qp <CRF>. This is the
//     fiddliest of the set and untestable in this environment (no VAAPI
//     device) — capability detection (internal/encoder.Available) keeps it from
//     ever running unless a real device is present; the arg shape is reasonable
//     but not battle-tested.
//   - hevc_amf: -rc cqp -qp_i <CRF> -qp_p <CRF>.
func buildArgs(spec encoder.Spec, cfg config.Config, pixFmt string, colorArgs []string, x265Color string) []string {
	args := []string{"-pix_fmt", pixFmt}
	args = append(args, colorArgs...)
	args = append(args, "-fps_mode", "passthrough") // a VFR source is not forced to CFR

	switch spec.Key {
	case "cpu":
		args = append(args,
			"-preset", cfg.Preset,
			"-crf", strconv.Itoa(cfg.CRF),
			"-x265-params", "log-level=error"+x265Color,
		)
	case "svtav1":
		args = append(args,
			"-preset", strconv.Itoa(svtav1Preset(cfg.Preset)),
			"-crf", strconv.Itoa(cfg.CRF),
		)
	case "nvenc", "av1_nvenc":
		args = append(args,
			"-rc", "vbr",
			"-cq", strconv.Itoa(cfg.CRF),
			"-b:v", "0",
			"-preset", "p5",
		)
	case "qsv":
		args = append(args, "-global_quality", strconv.Itoa(cfg.CRF))
	case "vaapi":
		// -vaapi_device itself is emitted by Encode (a global option that must
		// precede -i — see Encode's doc comment on the vaapi special case); here we
		// only add the encode-side args that come after -c:v.
		args = append(args,
			"-vf", "format=nv12,hwupload",
			"-qp", strconv.Itoa(cfg.CRF),
		)
	case "amf":
		args = append(args,
			"-rc", "cqp",
			"-qp_i", strconv.Itoa(cfg.CRF),
			"-qp_p", strconv.Itoa(cfg.CRF),
		)
	}
	return args
}

// svtav1Preset maps the config Preset word to SVT-AV1's numeric 0-13 preset scale
// (0 = slowest/best, 13 = fastest/worst — the opposite direction from libx265's
// naming but the same "slower is smaller" intuition). Unrecognized/empty values
// fall back to 8 (SVT-AV1's own default), the same "don't guess extremes" posture
// as picking a documented middle ground rather than silently defaulting to fastest
// or slowest.
func svtav1Preset(p string) int {
	switch p {
	case "veryslow", "placebo":
		return 2
	case "slower":
		return 4
	case "slow":
		return 6
	case "medium":
		return 8
	case "fast":
		return 10
	case "faster", "veryfast":
		return 11
	case "superfast", "ultrafast":
		return 12
	default:
		return 8
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
