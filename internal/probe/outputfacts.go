package probe

import (
	"context"
	"os/exec"
	"strings"
)

// Colors are the four colour tags at one level of a file, each normalised ("" where it
// signals none; see normColorValue).
type Colors struct {
	Primaries, Transfer, Matrix, Range string
}

// OutputFacts are what the output fidelity gate reads off an encoded output
// (docs/design/encode-plan.md#fidelity): its pixel format, its colour tags at the stream
// level and in its first decoded frame, and its side data at both levels.
type OutputFacts struct {
	// PixFmt is the first video stream's pix_fmt, verbatim.
	PixFmt string
	// Stream are the stream-level colour tags - what a container such as Matroska answers
	// from its own colour elements. Frame are the first decoded frame's - what the decoder
	// reads out of the bitstream itself. The two can disagree.
	Stream, Frame Colors
	// SideData is the first frame's side data followed by the stream's, in ffprobe's flat
	// output (the order Prober.SideDataFlat gives a source's), so the HDR10 readers in
	// internal/hdr read an output exactly as they read a source.
	SideData string
}

// The two probes OutputFacts runs: the stream's scalar fields with its side data, and the
// first frame's colour fields with its side data. Two subprocesses where the snapshot and a
// separate frame-colour probe would take four.
const (
	outputStreamEntries = "stream=pix_fmt,color_primaries,color_transfer,color_space,color_range:stream_side_data_list"
	outputFrameEntries  = "frame=color_primaries,color_transfer,color_space,color_range:frame_side_data_list"
)

// OutputFacts reads f's fidelity facts. A probe that does not run to completion contributes
// nothing - no pixel format, no tags, no side data - and nothing is guessed in its place: the
// gate reads an absent fact as not carried.
func (p *Prober) OutputFacts(ctx context.Context, f string) OutputFacts {
	stream := p.flat(ctx, "-v", "error", "-select_streams", "v:0",
		"-show_entries", outputStreamEntries, "-of", "flat=s=.", "--", f)
	frame := p.flat(ctx, "-v", "error", "-select_streams", "v:0", "-read_intervals", "%+#1",
		"-show_frames", "-show_entries", outputFrameEntries, "-of", "flat=s=.", "--", f)
	return outputFactsFrom(stream, frame)
}

// flat runs ffprobe and returns its output, or "" when it did not run to completion.
func (p *Prober) flat(ctx context.Context, args ...string) string {
	out, err := exec.CommandContext(ctx, p.FFprobe, args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// outputFactsFrom reads the two probes' flat output.
func outputFactsFrom(stream, frame string) OutputFacts {
	s := flatFields(stream, "streams.stream.0.")
	fr := flatFields(frame, "frames.frame.0.")
	return OutputFacts{
		PixFmt:   s["pix_fmt"],
		Stream:   colorsOf(s),
		Frame:    colorsOf(fr),
		SideData: frame + "\n" + stream,
	}
}

func colorsOf(m map[string]string) Colors {
	return Colors{
		Primaries: normColorValue(m["color_primaries"]),
		Transfer:  normColorValue(m["color_transfer"]),
		Matrix:    normColorValue(m["color_space"]),
		Range:     normColorValue(m["color_range"]),
	}
}

// flatFields reads the scalar fields directly under prefix in ffprobe's flat output (not
// those of a nested section such as a side-data list), unquoted. The first value of a key
// wins, so a later section can never overwrite the one asked for.
func flatFields(out, prefix string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		rest, ok := strings.CutPrefix(line, prefix)
		if !ok {
			continue
		}
		k, v, found := strings.Cut(rest, "=")
		if !found || strings.Contains(k, ".") {
			continue
		}
		if _, seen := m[k]; seen {
			continue
		}
		m[k] = strings.TrimSuffix(strings.TrimPrefix(v, `"`), `"`)
	}
	return m
}
