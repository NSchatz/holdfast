package probe

import (
	"bytes"
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

	vsOnce      sync.Once
	videoStream []VideoStream // the file's video streams, in container order
	videoStrOK  bool          // whether ffprobe established that list at all

	allOnce sync.Once
	all     []Stream // every stream the file carries, in container order
	allOK   bool     // whether ffprobe established that list at all

	cadenceOnce sync.Once
	cadence     Cadence // what a bounded decode found the source's interlacing to be

	// demuxErr is what the DEMUXER reported at error level while the eager probe ran, read
	// off the same process's stderr (see ContainerDamage).
	demuxErr ContainerDiagnostic
}

// ContainerDiagnostic is what a source's demuxer reported at error level while the snapshot
// probe read it: the container named, the first message it logged and how many it logged.
type ContainerDiagnostic struct {
	// Demuxer is the demuxer that logged, as ffprobe names it (format_name).
	Demuxer string
	// First is the first message it logged, without ffmpeg's context prefix.
	First string
	// Count is how many error-level lines it logged in all.
	Count int
}

// scalarStreamEntries are every scalar video-stream field a source skip-guard or the
// encoder reads. Fetched in one ffprobe call instead of one call per field.
const scalarStreamEntries = "codec_name,bit_rate,field_order,codec_tag_string,pix_fmt," +
	"width,height," +
	"color_primaries,color_transfer,color_space,color_range"

// snapshotEntries is the single -show_entries argument the snapshot probe issues: the
// scalar stream fields above PLUS the CONTAINER duration.
//
// The duration rides along deliberately (S0030). A live progress figure is meaningless
// without the length it is measured against, and that length has to be known BEFORE the
// encode starts — but a second ffprobe per job would be a real regression on a tool that
// walks a whole library (TRANSCODE-PERF exists because of exactly that). ffprobe answers
// stream and format sections in one call, so this costs zero extra subprocesses.
//
// Only `format=duration` is asked for, never `stream=duration`: with `-of default=nw=1`
// there are no section wrappers, so both would print the same bare `duration=` key and
// the later one would silently overwrite the earlier — inverting the format-then-stream
// preference Prober.DurationSec applies. A container that reports no duration therefore
// leaves the snapshot's duration UNKNOWN rather than guessed, which the reporting
// surface renders as unknown progress (never as zero, never as a fabricated fraction).
//
// `format=format_name` rides along for the same reason: it names the demuxer, which is how a
// line on this process's stderr is attributed to the container rather than to a decoder (see
// ContainerDamage). No stream section prints that key, so it overwrites nothing.
const snapshotEntries = "stream=" + scalarStreamEntries + ":format=duration,format_name"

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
	fields, diag := p.scalarFields(ctx, f)
	return &VideoProps{p: p, ctx: ctx, f: f, fields: fields,
		demuxErr: diag.fromDemuxer(fields["format_name"], fields["codec_name"])}
}

// scalarFields fetches every scalar stream entry (plus the container duration) in one
// ffprobe call and parses the `key=value` lines into a map. Uses `-of default=nw=1` (no
// section wrappers, keys kept) so each value is byte-identical to what the single-field
// `default=nw=1:nk=1` probes returned — the same ffprobe formatting, just batched. A
// field the stream does not carry is simply absent from the map (lookup yields ""),
// matching a single-field probe's empty result on a non-zero exit / unknown value.
//
// It also returns what the same process wrote to stderr, which at `-v error` is every
// error-level line ffmpeg logged while it opened and analysed the file. A process that did
// not run to completion - a non-zero exit, killed by a signal, its context cancelled -
// yields no fields and NO diagnostics: a line a probe wrote before it was stopped is not an
// answer about the file, and reporting it would call a file damaged on the strength of a
// probe nobody let finish.
func (p *Prober) scalarFields(ctx context.Context, f string) (map[string]string, *diagnostics) {
	m := map[string]string{}
	diag := &diagnostics{}
	cmd := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", snapshotEntries, "-of", "default=nw=1", "--", f)
	cmd.Stderr = diag
	out, err := cmd.Output()
	if err != nil {
		return m, &diagnostics{}
	}
	diag.flush()
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		m[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return m, diag
}

// ContainerDamage returns what the source's DEMUXER reported at error level while the
// snapshot probe read it, and whether it reported anything.
//
// It costs no probe: it is read off the stderr of the eager probe every file already pays
// for. A line counts only where ffmpeg attributed it to the demuxer, and ffmpeg names the
// context that logged each line - `[matroska,webm @ 0x...]` for the Matroska demuxer, the
// same string ffprobe reports as format_name. A decoder's line names the DECODER
// (`[h264 @ 0x...]`), and decoders complain about healthy files routinely: a broadcast
// capture that starts mid-GOP prints decoding errors for its first frames and is a perfectly
// good source. So a decoder's line is never read as damage to the container.
//
// Where the demuxer's name is also the name of the decoder of the probed video stream, the
// prefix alone cannot say which of the two logged a line (a raw `.h264` elementary stream is
// read by the `h264` demuxer and decoded by the `h264` decoder), and such a line is not
// attributed to the demuxer. An unestablished answer here sends the file on to the encode and
// the full gate, which is where it went before this signal existed.
func (vp *VideoProps) ContainerDamage() (ContainerDiagnostic, bool) {
	return vp.demuxErr, vp.demuxErr.Count > 0
}

// decoderNamedAsDemuxer maps a video codec to the name its ffmpeg decoder logs under, for
// the codecs whose decoder carries a DEMUXER's name without carrying the codec's own: FLV1
// video is decoded by the decoder named `flv`, which is also the FLV demuxer's name.
var decoderNamedAsDemuxer = map[string]string{"flv1": "flv"}

// maxDiagnosticLine bounds how much of one stderr line is kept, so a line with no newline in
// it cannot grow without limit.
const maxDiagnosticLine = 4096

// diagnostics is what an ffprobe process wrote to stderr, read line by line as it is
// written and kept only as the first message and the line count per logging context. What it
// holds grows with the number of distinct contexts that logged, never with the number of
// lines, so a source that makes ffmpeg complain on every packet costs no more to hold than
// one that complains once.
type diagnostics struct {
	partial []byte
	first   map[string]string
	count   map[string]int
}

// Write takes stderr as ffprobe writes it; it never fails, so the probe is never stopped by
// what it had to say.
func (d *diagnostics) Write(b []byte) (int, error) {
	n := len(b)
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			d.keep(b)
			return n, nil
		}
		d.keep(b[:i])
		d.flush()
		b = b[i+1:]
	}
}

// keep appends to the line being read, up to maxDiagnosticLine.
func (d *diagnostics) keep(b []byte) {
	if room := maxDiagnosticLine - len(d.partial); room > 0 {
		d.partial = append(d.partial, b[:min(len(b), room)]...)
	}
}

// flush records the line being read, if there is one.
func (d *diagnostics) flush() {
	if len(d.partial) == 0 {
		return
	}
	name, msg, ok := logContext(string(d.partial))
	d.partial = d.partial[:0]
	if !ok {
		return
	}
	if d.count == nil {
		d.first, d.count = map[string]string{}, map[string]int{}
	}
	if d.count[name] == 0 {
		d.first[name] = msg
	}
	d.count[name]++
}

// fromDemuxer is what the context named format logged, when that context can only be the
// demuxer (see ContainerDamage), and nothing otherwise.
func (d *diagnostics) fromDemuxer(format, codec string) ContainerDiagnostic {
	if format == "" || format == codec || format == decoderNamedAsDemuxer[codec] || d.count[format] == 0 {
		return ContainerDiagnostic{}
	}
	return ContainerDiagnostic{Demuxer: format, First: d.first[format], Count: d.count[format]}
}

// logContext splits one line of ffmpeg's log into the name of the context that logged it and
// the message. ffmpeg prefixes a line with `[name @ 0xADDR] ` for the context, preceded by
// the same for its parent where it has one, so the LAST prefix names the context that logged.
// ok is false for a line that names no context at all.
func logContext(line string) (name, msg string, ok bool) {
	rest := line
	for strings.HasPrefix(rest, "[") {
		end := strings.Index(rest, "] ")
		if end < 0 {
			break
		}
		n, _, isContext := strings.Cut(rest[1:end], " @ ")
		if !isContext {
			break
		}
		name, rest, ok = n, rest[end+2:], true
	}
	return name, strings.TrimSpace(rest), ok
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

// DurationSec returns the CONTAINER duration in seconds from the eager snapshot probe,
// with ok=false when the container reports none (ffprobe prints the literal "N/A" for
// some containers — MPEG-TS most notably). An unknown duration is NEVER coerced to 0:
// that is the same fail-safe every other field in this package keeps, and here a zero
// would be read downstream as "a zero-length source", i.e. an instantly-complete encode.
//
// This is deliberately NOT Prober.DurationSec: it takes no subprocess of its own and it
// applies no stream-level fallback, because that fallback would be a second ffprobe on
// exactly the files that lack a container duration. The caller (a live progress figure)
// treats false as "unknown length" and publishes no fraction at all, which is the
// honest answer; the verify gate keeps using Prober.DurationSec, untouched.
func (vp *VideoProps) DurationSec() (sec float64, ok bool) {
	d := vp.fields["duration"]
	if !floatRe.MatchString(d) {
		return 0, false
	}
	v, err := strconv.ParseFloat(d, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// Dimensions returns the coded width and height of the first video stream in pixels, and
// whether the probe established BOTH of them. It rides the eager scalar probe, so a file
// that skips at an early guard pays nothing for it.
//
// established is false - and the two values are 0 - whenever either dimension is missing,
// unparseable or not positive. It is a second return value rather than a zero because 0 is
// not a legal pixel dimension for anything: a caller that read a bare 0 as a height would
// judge a file it could not measure against whichever band admits zero, and on a tool that
// deletes sources the fail-safe direction is to decide nothing at all. The same rule
// DurationSec keeps, for the same reason.
func (vp *VideoProps) Dimensions() (width, height int, established bool) {
	w, wok := positiveInt(vp.fields["width"])
	h, hok := positiveInt(vp.fields["height"])
	if !wok || !hok {
		return 0, 0, false
	}
	return w, h, true
}

// positiveInt reads one verbatim ffprobe scalar as a positive integer.
func positiveInt(s string) (int, bool) {
	if !intRe.MatchString(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// FieldOrder returns the normalised field_order (unknown/N/A/"" → ""), identical to
// Prober.FieldOrder.
func (vp *VideoProps) FieldOrder() string { return normFieldOrder(vp.fields["field_order"]) }

// FieldOrderRaw returns the field_order value VERBATIM, before normalisation, and "" where
// the stream carried none at all.
//
// The normalised form collapses every way of not answering into one empty string, which is
// right for a guard deciding what to do and wrong for the line that tells an operator WHY a
// file was held back: "ffprobe said unknown" and "the field is not in this container at all"
// send them to different places. It is a scalar off the eager snapshot, so it costs nothing.
func (vp *VideoProps) FieldOrderRaw() string { return vp.fields["field_order"] }

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

// VideoStreams returns the source's video streams and whether ffprobe established that
// list, byte-for-byte the contract of Prober.VideoStreams.
//
// Lazy and memoised, like the side data and for the same reason: it is a second ffprobe,
// and only a file that has already cleared the cheap guards (codec, bitrate, field order,
// HDR class, pixel format) is ever asked what its stream shape is. A file that skipped at
// one of those never pays for it.
func (vp *VideoProps) VideoStreams() (streams []VideoStream, established bool) {
	vp.vsOnce.Do(func() {
		vp.videoStream, vp.videoStrOK = vp.p.VideoStreams(vp.ctx, vp.f)
	})
	return vp.videoStream, vp.videoStrOK
}

// AllStreams returns every stream the source carries and whether ffprobe established
// that list, byte-for-byte the contract of Prober.Streams.
//
// Lazy and memoised, like the side data and the video-stream list, and here the laziness
// is the whole reason it is on the snapshot at all: it is a second ffprobe, and only a
// file that has already cleared every cheap guard (codec, bitrate, field order, HDR
// class, pixel format) and is about to be ENCODED is ever asked what its full stream
// shape is. A file that skipped at one of those never pays for it, which is what keeps a
// library walk costing what it cost before stream selection existed.
func (vp *VideoProps) AllStreams() (streams []Stream, established bool) {
	vp.allOnce.Do(func() {
		vp.all, vp.allOK = vp.p.Streams(vp.ctx, vp.f)
	})
	return vp.all, vp.allOK
}

// Cadence returns what a bounded decode found this source's interlacing to BE - real
// interlacing, a telecine pulldown, or something nobody could establish - byte-for-byte the
// contract of Prober.Cadence.
//
// Lazy and memoised, and here the laziness is the whole reason it is on the snapshot: it
// DECODES, where every other field on this type is read from a header. Only a file whose
// container reports an interlaced field order AND whose root asks for a deinterlace is ever
// asked, so no file any existing configuration processes pays for it at all.
func (vp *VideoProps) Cadence() Cadence {
	vp.cadenceOnce.Do(func() {
		vp.cadence = vp.p.Cadence(vp.ctx, vp.f)
	})
	return vp.cadence
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
