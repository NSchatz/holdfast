package probe

import (
	"context"
	"os/exec"
	"strings"
)

// Colors are the four colour tags of a video, each normalised ("" where it signals none;
// see normColorValue).
type Colors struct {
	Primaries, Transfer, Matrix, Range string
}

// frameColorEntries are the colour fields of a decoded frame, in the order ffprobe prints
// them for `-show_entries frame=...`.
const frameColorEntries = "frame=color_range,color_space,color_primaries,color_transfer"

// FirstFrameColors returns the colour tags of the FIRST decoded frame of f's first video
// stream: what the decoder reads out of the bitstream itself (an HEVC or H.264 VUI), as
// opposed to the stream-level fields, which a container such as Matroska answers from its
// own colour elements. The two can disagree - a muxer can write one description and the
// encoder another - and a player that trusts either one must see the tags the source had, so
// the output fidelity gate reads both.
//
// ok=false when ffprobe did not run to completion or printed no frame: nothing is guessed,
// and the caller treats every tag as unsignalled.
func (p *Prober) FirstFrameColors(ctx context.Context, f string) (c Colors, ok bool) {
	out, err := exec.CommandContext(ctx, p.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-read_intervals", "%+#1", "-show_frames", "-show_entries", frameColorEntries,
		"-of", "default=nw=1", "--", f).Output()
	if err != nil {
		return Colors{}, false
	}
	return parseFrameColors(string(out))
}

// parseFrameColors reads ffprobe's `default=nw=1` output for one frame's colour fields. It
// takes the first value of each key, so a second frame printed by a build that ignores the
// read interval cannot overwrite the first; ok=false when no key was printed at all.
func parseFrameColors(out string) (c Colors, ok bool) {
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		k, v, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || seen[k] {
			continue
		}
		var dst *string
		switch k {
		case "color_primaries":
			dst = &c.Primaries
		case "color_transfer":
			dst = &c.Transfer
		case "color_space":
			dst = &c.Matrix
		case "color_range":
			dst = &c.Range
		default:
			continue
		}
		seen[k] = true
		*dst = normColorValue(v)
	}
	return c, len(seen) > 0
}
