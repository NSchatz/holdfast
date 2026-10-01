package dynhdr

import (
	"strconv"
	"strings"
)

// The command-line parts a carried plan adds to a libx265 encode. Each is read off what the
// pre-pass established, and nothing else: the engine's plan holds the Prepared value and the
// argv builder calls these.

// X265Params is the ":k=v" suffix the carriage adds to -x265-params: the VBV ceiling a Dolby
// Vision encode requires, and the HDR10+ metadata file. It is "" for a plan carrying nothing.
//
// x265 never reads `dolby-vision-rpu` here: it is a CLI-only option that ffmpeg's wrapper
// ignores with a warning and exit 0 (verify-streams-hdr.md claim 4), so the RPU is coded by
// ffmpeg's own `-dolbyvision 1` (CodecArgs) and the gate is what proves it.
func (p *Prepared) X265Params() string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	if p.Intent.DolbyVision {
		b.WriteString(":vbv-maxrate=" + itoa(p.VBV.MaxrateKbps) + ":vbv-bufsize=" + itoa(p.VBV.BufsizeKbit))
	}
	if p.Intent.HDR10Plus {
		b.WriteString(":dhdr10-info=" + EscapeX265Value(p.HDR10PlusJSON))
	}
	return b.String()
}

// CodecArgs are the encoder options the carriage adds after -x265-params: `-dolbyvision 1`,
// set EXPLICITLY. Its default, auto, never codes an RPU from the ffmpeg command line: the
// wrapper reads the metadata only from the encoder's global side data, which the command line
// never fills for Dolby Vision (verify-streams-hdr.md claim 3, libavcodec/dovi_rpuenc.c line
// 81 at 5d4d3bdc61).
func (p *Prepared) CodecArgs() []string {
	if p == nil || !p.Intent.DolbyVision {
		return nil
	}
	return []string{"-dolbyvision", "1"}
}

// InputArgs are the second input a converted profile 7 encode reads its video from: the raw
// profile 8.1 stream, which carries no timestamps, read at exactly the source's rate. nil
// where nothing is converted.
func (p *Prepared) InputArgs() []string {
	if p == nil || p.RawVideo == "" {
		return nil
	}
	return []string{"-f", "hevc", "-framerate", p.FrameRate.String(), "-i", p.RawVideo}
}

// Expect is what the gate holds the output to: profile 8 with compatibility id 1 where Dolby
// Vision is carried (profile 7 is converted to exactly that), and HDR10+ where it is.
func (p *Prepared) Expect() Expectation {
	if p == nil {
		return Expectation{}
	}
	e := Expectation{HDR10Plus: p.Intent.HDR10Plus}
	if p.Intent.DolbyVision {
		e.DolbyVision, e.Profile, e.CompatID = true, ProfileCarried, CompatHDR10
	}
	return e
}

// EscapeX265Value makes v one value of ffmpeg's -x265-params, which ffmpeg splits on ':' and
// '=' with av_get_token's rules: a backslash makes the next character literal, a quote
// starts a quoted run, and unescaped leading and trailing whitespace is dropped
// (libavutil/avstring.c av_get_token and libavutil/dict.c av_dict_parse_string at
// 5d4d3bdc61, https://github.com/FFmpeg/FFmpeg/tree/5d4d3bdc61 , read 2026-10-01). Every one
// of those characters is escaped, so a path naming a library directory with a colon in it
// stays one path.
func EscapeX265Value(v string) string {
	var b strings.Builder
	for _, r := range v {
		switch r {
		case '\\', '\'', ':', '=', ' ', '\t':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func itoa(n int) string { return strconv.Itoa(n) }
