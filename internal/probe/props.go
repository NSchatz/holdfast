package probe

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
)

// VideoProps is a single snapshot of a source file's stream-level properties,
// fetched once by VideoProps and reused by every skip guard AND the encoder
// (TRANSCODE-PERF). On the auto path a single encode-bound file used to spawn ~15
// separate ffprobe/ffmpeg processes, most re-fetching the same fields (codec,
// pix_fmt, field_order, codec_tag, the four colour tags; the side data fetched 3+
// times across Classify → the guards → the HDR-incomplete check → the encoder). A
// snapshot collapses those to a handful of probes taken ONCE, up front.
//
// Behaviour-preserving by construction: every accessor returns exactly what the
// corresponding single-field Prober method returns, run through the SAME
// normalisation helpers (normColorValue / normFieldOrder / resolveBitrate), and the
// side-data strings are the byte-identical `flat=s=.` output the standalone
// frameSideDataFlat/streamSideDataFlat produce — so the HDR/DV classifier, which
// substring-matches that flat text, cannot see any difference. The costly whole-file
// checks (DecodeOK, packet count, output duration/stream counts) are deliberately NOT
// here: they run in verifyOutput against the encoded temp, not the source.
//
// Only the ONE cheap scalar probe is eager. The side-data probes (a first-frame
// decode + a stream probe) and the bit_rate container fallback are LAZY — computed on
// first access and memoised — so a file that skips at an early guard (already-target-
// codec, low-bitrate, interlaced) never pays for data a later stage would have needed:
// the encode-bound path collapses ~15 probes to a handful, and the common already-
// target-codec skip stays the single probe it always was. All access is on one
// worker's goroutine, but the sync.Once guards keep it safe and each probe single-flight.
type VideoProps struct {
	p   *Prober
	ctx context.Context // stored so the lazy probes run under the caller's cancellation
	f   string

	fields  map[string]string // scalar stream entries, verbatim ffprobe values (eager)
	bitrate int               // resolved kbps (stream, else format fallback); 0 = unknown

	brOnce   sync.Once
	sideOnce sync.Once
	frameSD  string // frame-level side data, flat=s=. (== frameSideDataFlat)
	streamSD string // stream-level side data, flat=s=. (== streamSideDataFlat)
}

// scalarStreamEntries are every scalar video-stream field a source skip-guard or the
// encoder reads. Fetched in one ffprobe call instead of one call per field.
const scalarStreamEntries = "codec_name,bit_rate,field_order,codec_tag_string,pix_fmt," +
	"color_primaries,color_transfer,color_space,color_range"

// VideoProps takes one snapshot of f's source properties. The constructor runs a
// single ffprobe (all scalar stream fields at once); the side-data probes and the
// bit_rate container fallback are fetched lazily on first access (see the accessors).
// Across a full encode-bound pass this is a handful of probes in place of the ~15 the
// per-field methods spawned when called across the guards and the encoder; a file that
// skips at an early guard runs fewer still. Never nil: an unreadable file yields a
// snapshot whose accessors all report the same "unknown" values the single-field
// methods would (Codec() == "" then drives the engine's unreadable-source skip).
func (p *Prober) VideoProps(ctx context.Context, f string) *VideoProps {
	// Eager: the one scalar probe (it replaces the old codec probe the earliest guard
	// needs anyway). The bit_rate container fallback and the side-data probes are
	// deferred to first access — an already-target-codec file returns at the codec
	// guard having paid exactly this one probe.
	return &VideoProps{p: p, ctx: ctx, f: f, fields: p.scalarFields(ctx, f)}
}

// scalarFields fetches every scalar stream entry in one ffprobe call and parses the
// `key=value` lines into a map. Uses `-of default=nw=1` (no section wrappers, keys
// kept) so each value is byte-identical to what the single-field `default=nw=1:nk=1`
// probes returned — the same ffprobe formatting, just batched. A field the stream
// does not carry is simply absent from the map (lookup yields ""), matching a
// single-field probe's empty result on a non-zero exit / unknown value.
func (p *Prober) scalarFields(ctx context.Context, f string) map[string]string {
	m := map[string]string{}
	out, err := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream="+scalarStreamEntries, "-of", "default=nw=1", "--", f).Output()
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m
}

// Codec returns codec_name, or "" for an unreadable file / no video stream — the
// exact contract of Prober.VideoCodec (the engine skips on "").
func (vp *VideoProps) Codec() string { return vp.fields["codec_name"] }

// BitrateKbps returns the resolved source bitrate in kbps (0 = unknown), identical to
// Prober.BitrateKbps: the video stream's bit_rate (already in the eager scalar probe),
// falling back to the container's. The fallback probe fires lazily and at most once,
// and only when the stream carries no usable bit_rate — so a file that skipped before
// this guard never triggered it.
func (vp *VideoProps) BitrateKbps() int {
	vp.brOnce.Do(func() {
		vp.bitrate = vp.p.resolveBitrate(vp.ctx, vp.f, vp.fields["bit_rate"])
	})
	return vp.bitrate
}

// FieldOrder returns the normalised field_order (unknown/N/A/"" → ""), identical to
// Prober.FieldOrder.
func (vp *VideoProps) FieldOrder() string { return normFieldOrder(vp.fields["field_order"]) }

// CodecTag returns codec_tag_string verbatim, identical to Prober.CodecTagString.
func (vp *VideoProps) CodecTag() string { return vp.fields["codec_tag_string"] }

// PixFmt returns pix_fmt verbatim, identical to Prober.PixFmt.
func (vp *VideoProps) PixFmt() string { return vp.fields["pix_fmt"] }

// Color returns one normalised colour tag (unknown/reserved/N/A/"" → ""), identical
// to Prober.ColorField for the same field name.
func (vp *VideoProps) Color(field string) string { return normColorValue(vp.fields[field]) }

// loadSideData fetches the frame- and stream-level side data once, on first access.
// Deferred out of the constructor because it is the expensive part (the frame probe
// is a first-frame decode) and only the HDR/DV classification and the encoder — both
// past the cheap early skip guards — ever read it.
func (vp *VideoProps) loadSideData() {
	vp.sideOnce.Do(func() {
		vp.frameSD = vp.p.frameSideDataFlat(vp.ctx, vp.f)
		vp.streamSD = vp.p.streamSideDataFlat(vp.ctx, vp.f)
	})
}

// SideData returns the frame-level side data concatenated with the stream-level side
// data, byte-identical to Prober.SideDataFlat — the input the HDR/DV classifier
// substring-matches.
func (vp *VideoProps) SideData() string {
	vp.loadSideData()
	return vp.frameSD + "\n" + vp.streamSD
}

// FrameSideData returns the first-frame-only side data, byte-identical to
// Prober.FrameSideDataFlat — the input the HDR10-incomplete guard reads.
func (vp *VideoProps) FrameSideData() string {
	vp.loadSideData()
	return vp.frameSD
}

// normColorValue drops the ffprobe non-values ("unknown"/"reserved"/"N/A"/"") to ""
// so a colourspace is never GUESSED onto the output. Shared by Prober.ColorField and
// VideoProps.Color so the two can never drift.
func normColorValue(v string) string {
	switch v {
	case "unknown", "reserved", "N/A", "":
		return ""
	default:
		return v
	}
}

// normFieldOrder drops ffprobe's non-values ("unknown"/"N/A"/"") to "". Shared by
// Prober.FieldOrder and VideoProps.FieldOrder.
func normFieldOrder(v string) string {
	switch v {
	case "unknown", "N/A", "":
		return ""
	default:
		return v
	}
}

// resolveBitrate turns a stream bit_rate value into kbps, falling back to the
// container's format bit_rate when the stream carries none — an UNKNOWN bitrate stays
// 0 (never coerced), so a caller only skips on a known-and-low value. Shared by
// Prober.BitrateKbps and VideoProps so the fallback logic cannot drift.
func (p *Prober) resolveBitrate(ctx context.Context, f, streamBR string) int {
	br := streamBR
	if !intRe.MatchString(br) {
		br = firstLine(ctx, p.FFprobe, "-v", "error",
			"-show_entries", "format=bit_rate", "-of", "default=nw=1:nk=1", "--", f)
	}
	if intRe.MatchString(br) {
		n, _ := strconv.Atoi(br)
		return n / 1000
	}
	return 0
}
