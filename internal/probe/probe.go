// Package probe wraps ffprobe/ffmpeg inspection of media files. Every function
// mirrors a helper in the original bash transcoder (media/transcoder/transcode.sh)
// and preserves its exact fail-safe semantics — most importantly, an UNKNOWN value
// (bitrate, duration) is never coerced to zero, because a wrong zero would make a
// perfectly good encode fail a safety gate.
package probe

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Prober runs ffprobe/ffmpeg against files. FFprobe/FFmpeg are the binary names or
// paths (default "ffprobe"/"ffmpeg"). A single probe is bounded by the caller's ctx.
type Prober struct {
	FFprobe string
	FFmpeg  string
}

// New returns a Prober using the given binaries, defaulting to PATH lookups.
func New(ffmpeg, ffprobe string) *Prober {
	if ffmpeg == "" {
		ffmpeg = "ffmpeg"
	}
	if ffprobe == "" {
		ffprobe = "ffprobe"
	}
	return &Prober{FFprobe: ffprobe, FFmpeg: ffmpeg}
}

// firstLine runs the command and returns the trimmed first line of stdout (stderr
// discarded). A non-zero exit yields "" — callers treat empty as "unknown".
func firstLine(ctx context.Context, name string, args ...string) string {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil {
		return ""
	}
	s := string(out)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

var intRe = regexp.MustCompile(`^[0-9]+$`)
var floatRe = regexp.MustCompile(`^[0-9]+([.][0-9]+)?$`)

// VideoCodec returns the codec_name of the first video stream, or "" if there is
// no readable video stream.
func (p *Prober) VideoCodec(ctx context.Context, f string) string {
	return firstLine(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_name", "-of", "default=nw=1:nk=1", "--", f)
}

// BitrateKbps returns the source video bitrate in kbps, preferring the video
// stream's bit_rate and falling back to the container bit_rate. It returns 0 when
// neither is known — an UNKNOWN bitrate must NOT trigger the low-bitrate skip, so
// callers only skip on a known-and-low value (br > 0 && br < min).
func (p *Prober) BitrateKbps(ctx context.Context, f string) int {
	br := firstLine(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=bit_rate", "-of", "default=nw=1:nk=1", "--", f)
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

// DurationSec returns the container duration (falling back to the video stream's)
// in seconds. ok is false when neither is a real number — ffprobe reports the
// literal "N/A" for some containers (e.g. MPEG-TS), and that must never be coerced
// to 0 (which would fail duration parity on a good encode); callers use the
// packet-count fallback when ok is false.
func (p *Prober) DurationSec(ctx context.Context, f string) (sec float64, ok bool) {
	d := firstLine(ctx, p.FFprobe, "-v", "error",
		"-show_entries", "format=duration", "-of", "default=nw=1:nk=1", "--", f)
	if !floatRe.MatchString(d) {
		d = firstLine(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
			"-show_entries", "stream=duration", "-of", "default=nw=1:nk=1", "--", f)
	}
	if !floatRe.MatchString(d) {
		return 0, false
	}
	v, err := strconv.ParseFloat(d, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// PacketCount returns the number of video packets (~= frames), or ok=false if it
// cannot be counted. Transcoding preserves frame count, so a truncated encode has
// far fewer packets — this is the length check when no duration is available. It
// decodes the whole file, so it is only called on that path.
func (p *Prober) PacketCount(ctx context.Context, f string) (n int, ok bool) {
	s := firstLine(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-count_packets", "-show_entries", "stream=nb_read_packets",
		"-of", "default=nw=1:nk=1", "--", f)
	if !intRe.MatchString(s) {
		return 0, false
	}
	v, _ := strconv.Atoi(s)
	return v, true
}

// DecodeOK fully decodes the primary video stream to null and reports whether it
// decodes cleanly to the end. `-xerror` exits on the first ERROR; `-err_detect
// +explode` promotes concealable decode errors (a corrupt frame the HEVC decoder
// would otherwise silently conceal) to fatal — without it ffmpeg conceals interior
// corruption and exits 0. It is not a complete corruption detector (random-noise
// corruption can still decode "clean"); the parity + size + VMAF (later) gates are
// the complementary layers.
func (p *Prober) DecodeOK(ctx context.Context, f string) bool {
	cmd := exec.CommandContext(ctx, p.FFmpeg, "-hide_banner", "-nostdin", "-v", "error",
		"-xerror", "-err_detect", "+explode", "-i", f, "-map", "0:v:0", "-f", "null", "-")
	return cmd.Run() == nil
}

// StreamCount counts streams of the given ffprobe type specifier: "a"=audio,
// "s"=subtitle, "t"=attachment ("d"=data is intentionally excluded — the encode
// drops data streams). Returns 0 (never negative) when there are none or the file
// is unreadable, so a caller's numeric compare is always well-formed.
func (p *Prober) StreamCount(ctx context.Context, f, typ string) int {
	out, err := exec.CommandContext(ctx, p.FFprobe, "-v", "error",
		"-select_streams", typ, "-show_entries", "stream=index",
		"-of", "csv=p=0", "--", f).Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// FileSize returns the byte size of f, or 0 if it cannot be stat'd.
func FileSize(f string) int64 {
	fi, err := os.Stat(f)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// Fingerprint returns a cheap "size:mtime" content key. A re-downloaded/edited file
// changes size or mtime, so its key changes and it is reconsidered. Returns
// "0:0" if the file cannot be stat'd.
func Fingerprint(f string) string {
	fi, err := os.Stat(f)
	if err != nil {
		return "0:0"
	}
	return strconv.FormatInt(fi.Size(), 10) + ":" + strconv.FormatInt(fi.ModTime().Unix(), 10)
}

// NLink returns the hard-link count of f, or 1 if it cannot be determined (so a
// stat failure never trips the hardlink guard). A count > 1 means the file is an
// active seed / dup: replacing it via rename would break the link and reclaim
// nothing, so the engine skips it.
func NLink(f string) uint64 {
	fi, err := os.Stat(f)
	if err != nil {
		return 1
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}

// ---- colour / HDR + source-property probes (TRANSCODE-3) -------------------

// ColorField returns one colour tag from the video stream: color_primaries,
// color_transfer, color_space, or color_range. Prints "" for an absent/unknown tag
// ("unknown", "reserved", "N/A", or empty) — NEVER guessed, so a wrong colourspace
// is never stamped onto the output. Port of bash probe_color_field.
func (p *Prober) ColorField(ctx context.Context, f, field string) string {
	v := firstLine(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream="+field, "-of", "default=nw=1:nk=1", "--", f)
	switch v {
	case "unknown", "reserved", "N/A", "":
		return ""
	default:
		return v
	}
}

// CodecTagString returns the video stream's codec_tag_string (e.g. "dvh1", "hev1")
// used by the DV classifier.
func (p *Prober) CodecTagString(ctx context.Context, f string) string {
	return firstLine(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=codec_tag_string", "-of", "default=nw=1:nk=1", "--", f)
}

// PixFmt returns the video stream's pix_fmt (e.g. "yuv420p", "yuv420p10le").
func (p *Prober) PixFmt(ctx context.Context, f string) string {
	return firstLine(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=pix_fmt", "-of", "default=nw=1:nk=1", "--", f)
}

// FieldOrder returns the video stream's field_order (e.g. "progressive", "tt",
// "bb", "tb", "bt"), or "" if unknown. Used to skip interlaced sources (this tool
// never deinterlaces).
func (p *Prober) FieldOrder(ctx context.Context, f string) string {
	v := firstLine(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=field_order", "-of", "default=nw=1:nk=1", "--", f)
	switch v {
	case "unknown", "N/A", "":
		return ""
	default:
		return v
	}
}

// frameSideDataFlat returns the flat FIRST-FRAME side-data of the video stream —
// mastering-display, content-light and HDR10+ dynamic metadata surface here. First
// frame only: HDR side data repeats per-frame. Port of bash hdr_sidedata_flat.
func (p *Prober) frameSideDataFlat(ctx context.Context, f string) string {
	out, _ := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-read_intervals", "%+#1", "-show_frames",
		"-show_entries", "frame=side_data_list", "-of", "flat=s=.", "--", f).Output()
	return string(out)
}

// streamSideDataFlat returns the flat STREAM-level side-data of the video stream.
// The Dolby Vision "DOVI configuration record" is stream-level (an ISOBMFF
// dvcC/dvvC box), NOT frame-level, so DV detection must look here too. Port of
// bash hdr_stream_sidedata_flat.
func (p *Prober) streamSideDataFlat(ctx context.Context, f string) string {
	out, _ := exec.CommandContext(ctx, p.FFprobe, "-v", "error",
		"-show_entries", "stream_side_data_list", "-of", "flat=s=.", "--", f).Output()
	return string(out)
}

// SideDataFlat returns the frame-level side data (first frame only) concatenated
// with the stream-level side data, matching how the bash hdr_class feeds its
// classifier: both sources of HDR/DV signal, wherever ffprobe surfaces them.
func (p *Prober) SideDataFlat(ctx context.Context, f string) string {
	return p.frameSideDataFlat(ctx, f) + "\n" + p.streamSideDataFlat(ctx, f)
}

// FrameSideDataFlat exposes the first-frame-only side data (mastering-display /
// content-light live here) — used by the HDR10-incomplete guard, which the bash
// version feeds only frame-level data (hdr_sidedata_flat), not the stream-level
// concatenation used for classification.
func (p *Prober) FrameSideDataFlat(ctx context.Context, f string) string {
	return p.frameSideDataFlat(ctx, f)
}
