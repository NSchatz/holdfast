// Package encoder is the TRANSCODE-6 codec matrix: a registry describing every
// selectable encoder (CPU libx265, SVT-AV1, and the hardware encoders NVENC/QSV/
// VAAPI/AMF) plus a robust runtime capability check. Nothing here assumes an
// encoder works — Available actually exercises it against a tiny real clip and
// inspects the output, because a hardware encoder can exit 0 while writing nothing
// when no device is present (see Available's doc comment).
package encoder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"

	"github.com/NSchatz/transcode/internal/probe"
)

// Spec describes one selectable encoder.
type Spec struct {
	// Key is the config `encoder:` value (e.g. "cpu", "svtav1", "nvenc").
	Key string
	// FFmpegCodec is the ffmpeg -c:v value (e.g. "libx265", "hevc_nvenc").
	FFmpegCodec string
	// TargetCodec is what ffprobe reports codec_name as for the OUTPUT: "hevc" or
	// "av1". Drives the engine's skip-already-target guard and verifyOutput's
	// codec check.
	TargetCodec string
	// Hardware reports whether this encoder needs a GPU/device. Hardware encoders
	// are gated behind Available at run time — never assumed to work.
	Hardware bool
}

// registry is the source of truth for every selectable encoder, keyed by its
// config value. Keyed also by the raw ffmpeg codec name as a convenience alias
// (see init) so `encoder: libsvtav1` works the same as `encoder: svtav1`.
var registry = map[string]Spec{
	"cpu":       {Key: "cpu", FFmpegCodec: "libx265", TargetCodec: "hevc", Hardware: false},
	"svtav1":    {Key: "svtav1", FFmpegCodec: "libsvtav1", TargetCodec: "av1", Hardware: false},
	"nvenc":     {Key: "nvenc", FFmpegCodec: "hevc_nvenc", TargetCodec: "hevc", Hardware: true},
	"av1_nvenc": {Key: "av1_nvenc", FFmpegCodec: "av1_nvenc", TargetCodec: "av1", Hardware: true},
	"qsv":       {Key: "qsv", FFmpegCodec: "hevc_qsv", TargetCodec: "hevc", Hardware: true},
	"vaapi":     {Key: "vaapi", FFmpegCodec: "hevc_vaapi", TargetCodec: "hevc", Hardware: true},
	"amf":       {Key: "amf", FFmpegCodec: "hevc_amf", TargetCodec: "hevc", Hardware: true},
}

// aliases maps the raw ffmpeg -c:v codec name to its registry key, so
// `encoder: libsvtav1` (or `encoder: hevc_nvenc`, etc.) is accepted the same as
// the short key.
var aliases map[string]string

func init() {
	aliases = make(map[string]string, len(registry))
	for key, spec := range registry {
		aliases[spec.FFmpegCodec] = key
	}
}

// Lookup resolves a config `encoder:` value (a registry key or a raw ffmpeg codec
// name alias) to its Spec. ok is false for an unknown key.
func Lookup(key string) (Spec, bool) {
	if spec, ok := registry[key]; ok {
		return spec, true
	}
	if canon, ok := aliases[key]; ok {
		return registry[canon], true
	}
	return Spec{}, false
}

// Known returns every registered encoder key, sorted for stable error messages.
func Known() []string {
	keys := make([]string, 0, len(registry))
	for k := range registry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Available is the ROBUST capability check for spec: it actually encodes a tiny
// real clip with spec.FFmpegCodec to a temp file and ffprobes the RESULT, rather
// than trusting ffmpeg's exit code. This matters specifically for hardware
// encoders: in a container with no GPU/device, `hevc_nvenc -f null -` can exit 0
// while writing nothing at all — exit-code-only detection would report a
// completely unusable encoder as "available" and the engine would then burn a
// full encode attempt (or worse, silently produce nothing) on every file. Writing
// to a real temp file and confirming ffprobe reports a video stream whose
// codec_name matches spec.TargetCodec closes that hole: libx265/libsvtav1 (CPU,
// always available here) return true; hevc_nvenc/av1_nvenc/hevc_qsv/hevc_vaapi/
// hevc_amf return false in this container (no device) exactly as they would on any
// host lacking the matching GPU.
func Available(ctx context.Context, ffmpeg, ffprobe string, spec Spec) bool {
	prober := probe.New(ffmpeg, ffprobe)

	dir, err := os.MkdirTemp("", "transcode-cap-*")
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)

	out := filepath.Join(dir, "probe.mkv")
	args := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=0.2:size=160x120:rate=10",
		"-c:v", spec.FFmpegCodec,
		"--", out,
	}
	cmd := exec.CommandContext(ctx, ffmpeg, args...)
	_ = cmd.Run() // exit code is unreliable for hardware encoders — see doc comment above

	if fi, err := os.Stat(out); err != nil || fi.Size() <= 0 {
		return false
	}
	return prober.VideoCodec(ctx, out) == spec.TargetCodec
}

// ErrUnavailable is the sentinel wrapped by RequireAvailable when the configured
// encoder does not work in this ffmpeg build / on this host (callers can match it
// with errors.Is). Mirrors internal/vmaf's ErrUnavailable style.
var ErrUnavailable = errors.New("encoder not available in this ffmpeg build / on this host")

// RequireAvailable looks up key and confirms it is Available, returning a clear
// error otherwise. It never falls back to another encoder — a configured-but-
// unavailable encoder must fail loud, never silently downgrade to cpu.
func RequireAvailable(ctx context.Context, ffmpeg, ffprobe, key string) (Spec, error) {
	spec, ok := Lookup(key)
	if !ok {
		return Spec{}, fmt.Errorf("unknown encoder %q (known: %v)", key, Known())
	}
	if !Available(ctx, ffmpeg, ffprobe, spec) {
		return Spec{}, fmt.Errorf("encoder %q: %w", key, ErrUnavailable)
	}
	return spec, nil
}
