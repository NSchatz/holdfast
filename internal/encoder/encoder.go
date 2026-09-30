// Package encoder is the TRANSCODE-6 codec matrix: a registry describing every
// selectable encoder (CPU libx265, SVT-AV1, and the hardware encoders NVENC/QSV/
// VAAPI/AMF) plus a robust runtime capability check. Nothing here assumes an
// encoder works — Available actually exercises it against a tiny real clip, through the
// command line a job would run, and inspects the output, because a hardware encoder can
// exit 0 while writing nothing when no device is present (see Available's doc comment).
package encoder

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/NSchatz/holdfast/internal/hdr"
	"github.com/NSchatz/holdfast/internal/probe"
)

// Spec describes one selectable encoder.
type Spec struct {
	// Key is the config `encoder:` value (e.g. "cpu", "svtav1", "nvenc").
	Key string
	// FFmpegCodec is the ffmpeg -c:v value (e.g. "libx265", "hevc_nvenc").
	FFmpegCodec string
	// TargetCodec is what ffprobe reports codec_name as for the OUTPUT: "hevc" or
	// "av1". Drives the engine's skip-already-target guard and verifyAgainst's
	// codec check.
	TargetCodec string
	// Hardware reports whether this encoder needs a GPU/device. Hardware encoders
	// are gated behind Available at run time — never assumed to work.
	Hardware bool
	// PixelFormats is the encoder's "Supported pixel formats:" line from the pinned
	// ffmpeg, verbatim and space-separated (see formats.go for the source and the test
	// that re-reads it from the binary). It is what InputFormat chooses from.
	PixelFormats string
	// UploadFormats is, for an encoder that accepts only hardware surfaces (VAAPI), the
	// software formats this build uploads to it through `format=<fmt>,hwupload`; "" for
	// every encoder that takes software frames directly (see formats.go).
	UploadFormats string
	// Quality is the scale this encoder's quality-targeted rate control is set on: the
	// option, its range and the configuration key that sets it (see quality.go).
	Quality QualityScale
}

// registry is the source of truth for every selectable encoder, keyed by its
// config value. Keyed also by the raw ffmpeg codec name as a convenience alias
// (see init) so `encoder: libsvtav1` works the same as `encoder: svtav1`.
var registry = map[string]Spec{
	"cpu": {Key: "cpu", FFmpegCodec: "libx265", TargetCodec: "hevc", Hardware: false,
		PixelFormats: pixFmtsLibx265, Quality: scaleCRF},
	"svtav1": {Key: "svtav1", FFmpegCodec: "libsvtav1", TargetCodec: "av1", Hardware: false,
		PixelFormats: pixFmtsLibsvtav1, Quality: scaleCRF},
	"nvenc": {Key: "nvenc", FFmpegCodec: "hevc_nvenc", TargetCodec: "hevc", Hardware: true,
		PixelFormats: pixFmtsNVENC, Quality: scaleNVENC},
	"av1_nvenc": {Key: "av1_nvenc", FFmpegCodec: "av1_nvenc", TargetCodec: "av1", Hardware: true,
		PixelFormats: pixFmtsNVENC, Quality: scaleAV1NVENC},
	"qsv": {Key: "qsv", FFmpegCodec: "hevc_qsv", TargetCodec: "hevc", Hardware: true,
		PixelFormats: pixFmtsQSV, Quality: scaleQSV},
	"vaapi": {Key: "vaapi", FFmpegCodec: "hevc_vaapi", TargetCodec: "hevc", Hardware: true,
		PixelFormats: pixFmtsVAAPI, UploadFormats: uploadFormatsVAAPI, Quality: scaleVAAPI},
	"amf": {Key: "amf", FFmpegCodec: "hevc_amf", TargetCodec: "hevc", Hardware: true,
		PixelFormats: pixFmtsAMF, Quality: scaleAMF},
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

// TargetCodecs returns every value ffprobe reports as codec_name for an output that
// SOME encoder in this registry could have produced, deduplicated and sorted.
//
// It answers "could this build have written this file", which is a different question
// from Lookup(cfg.Encoder).TargetCodec's "is this file at the codec currently
// configured". A file already on disk was written by whichever encoder was configured
// when it was written, and `encoder:` is an ordinary config key an operator may change
// between runs — so anything deciding the FATE of a file holdfast wrote has to ask the
// first question, never the second.
func TargetCodecs() []string {
	seen := make(map[string]bool, len(registry))
	out := make([]string, 0, len(registry))
	for _, spec := range registry {
		if !seen[spec.TargetCodec] {
			seen[spec.TargetCodec] = true
			out = append(out, spec.TargetCodec)
		}
	}
	sort.Strings(out)
	return out
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

// EncodeFunc encodes src to out with spec, the way a job's encode does: through the same plan
// derivation and the same command-line builder, told the pixel format to encode at ("" leaves
// it to the plan's derivation, as the default `pixel_format: auto` does). The engine provides
// the production one (engine.ProbeEncode); nothing else builds a probe's command line, so a
// probe cannot pass on an argv no job would run (docs/design/hardware.md#probe).
type EncodeFunc func(ctx context.Context, spec Spec, pixelFormat, src, out string) error

// Capability is what one probe of an encoder established on this host: whether it wrote a
// real output of its target codec at each bit depth, and why not where it did not.
type Capability struct {
	Key string
	// EightBit and TenBit report that a 4:2:0 source encoded at 8 and at 10 bits came out as
	// a real file of the encoder's target codec at that same depth.
	EightBit, TenBit bool
	// Reason is why the encoder is not usable at a depth it failed at ("" when it passed at
	// both), or the refusal that stopped the probe before it ran.
	Reason string
}

// Usable reports whether the encoder wrote a faithful output at any depth.
func (c Capability) Usable() bool { return c.EightBit || c.TenBit }

// Carries reports whether the probe showed this encoder writing a plan of pixFmt's depth: an
// 8-bit plan needs the 8-bit probe, a deeper one the 10-bit probe. The probes use 4:2:0; a plan
// of another chroma subsampling is held to the depth its probe showed, and the output fidelity
// gate stays the backstop for the layout (ASSUMED until a hardware report shows 4:2:2 and 4:4:4
// on real devices; brief T43). An unparseable format is carried by nothing.
func (c Capability) Carries(pixFmt string) bool {
	layout, ok := hdr.PixelLayout(pixFmt)
	if !ok {
		return false
	}
	if layout.Depth <= 8 {
		return c.EightBit
	}
	return c.TenBit
}

// probeDepths are the two formats a probe encodes: the forced 8-bit one and the 10-bit one
// every derived plan uses (hdr.DerivePixFmt floors the depth at 10).
var probeDepths = []struct {
	format string
	depth  int
}{{"yuv420p", 8}, {"yuv420p10le", 10}}

// Available is the ROBUST capability check for spec: it encodes a tiny real clip, at 8 and at
// 10 bits, through encode - the job's own derivation and command line, device and upload
// included - and ffprobes each RESULT rather than trusting ffmpeg's exit code. This matters
// for hardware encoders: in a container with no GPU or device, `hevc_nvenc -f null -` can
// exit 0 while writing nothing, so an exit-code check would call an unusable encoder usable.
// Each output must exist, be non-empty, carry spec.TargetCodec and carry the depth it was
// asked for - a VAAPI encode that uploads 10-bit frames as 8-bit surfaces fails the 10-bit
// probe here, where a codec-only check passed it.
//
// `amf` in the container image is refused before anything runs (RefusedInImage).
func Available(ctx context.Context, ffmpeg, ffprobe string, spec Spec, encode EncodeFunc) Capability {
	c := Capability{Key: spec.Key}
	if why := RefusedInImage(spec); why != "" {
		c.Reason = why
		return c
	}
	prober := probe.New(ffmpeg, ffprobe)
	dir, err := os.MkdirTemp("", "holdfast-cap-*")
	if err != nil {
		c.Reason = "cannot make a directory for the probe: " + err.Error()
		return c
	}
	defer os.RemoveAll(dir)

	var reasons []string
	for _, d := range probeDepths {
		ok, why := probeDepth(ctx, ffmpeg, prober, spec, encode, dir, d.format, d.depth)
		if d.depth == 8 {
			c.EightBit = ok
		} else {
			c.TenBit = ok
		}
		if !ok {
			reasons = append(reasons, strconv.Itoa(d.depth)+"-bit: "+why)
		}
	}
	c.Reason = strings.Join(reasons, "; ")
	return c
}

// probeDepth encodes one clip of format and says whether the output is faithful to it.
func probeDepth(ctx context.Context, ffmpeg string, prober *probe.Prober, spec Spec, encode EncodeFunc,
	dir, format string, depth int) (bool, string) {
	src := filepath.Join(dir, "source-"+format+".mkv")
	// The source is lossless (FFV1), so what the encoder is handed is exactly format.
	gen := exec.CommandContext(ctx, ffmpeg, "-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=duration=0.2:size=320x240:rate=10",
		"-pix_fmt", format, "-c:v", "ffv1", "--", src)
	if out, err := gen.CombinedOutput(); err != nil {
		return false, "cannot make the probe clip: " + firstLine(string(out), err)
	}
	out := filepath.Join(dir, "probe-"+format+".mkv")
	encErr := encode(ctx, spec, format, src, out)
	fi, err := os.Stat(out)
	if err != nil || fi.Size() <= 0 {
		if encErr != nil {
			return false, "the encode failed: " + firstLine(encErr.Error(), nil)
		}
		return false, "the encode wrote no output"
	}
	props := prober.VideoProps(ctx, out)
	if got := props.Codec(); got != spec.TargetCodec {
		return false, "the output's codec is " + quoted(got) + ", not " + spec.TargetCodec
	}
	layout, ok := hdr.PixelLayout(props.PixFmt())
	if !ok || layout.Depth != depth {
		return false, "the output's pixel format is " + quoted(props.PixFmt()) + ", not " +
			strconv.Itoa(depth) + "-bit"
	}
	return true, ""
}

func quoted(s string) string { return strconv.Quote(s) }

// firstLine is the first non-empty line of an ffmpeg failure, which names the cause; the
// rest is its call chain.
func firstLine(out string, err error) string {
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	if err != nil {
		return err.Error()
	}
	return "no message"
}

// ErrUnavailable is the sentinel wrapped by RequireAvailable when the configured
// encoder does not work in this ffmpeg build / on this host (callers can match it
// with errors.Is). Mirrors internal/vmaf's ErrUnavailable style.
var ErrUnavailable = errors.New("encoder not available in this ffmpeg build / on this host")

// RequireAvailable looks up key and confirms it is Available, returning a clear error
// otherwise. It never falls back to another encoder: what a configuration does when its
// hardware is missing is the caller's decision (hw_fallback), never this check's.
func RequireAvailable(ctx context.Context, ffmpeg, ffprobe, key string, encode EncodeFunc) (Spec, Capability, error) {
	spec, ok := Lookup(key)
	if !ok {
		return Spec{}, Capability{}, fmt.Errorf("unknown encoder %q (known: %v)", key, Known())
	}
	c := Available(ctx, ffmpeg, ffprobe, spec, encode)
	if !c.Usable() {
		return spec, c, fmt.Errorf("encoder %q: %w: %s", key, ErrUnavailable, c.Reason)
	}
	return spec, c, nil
}
