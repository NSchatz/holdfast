package dynhdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

// LEVEL 5: the active area a Dolby Vision RPU names, as offsets of the picture from each edge
// of the frame (libavutil/dovi_meta.h AVDOVIDmLevel5, "Active area definition"). A crop that
// leaves it alone leaves the replacement describing bars that are gone (proposal P5, finding
// 2), so a Dolby Vision source is cropped only to the rectangle its own L5 names, with L5
// zeroed in the pre-pass and gated on the output (docs/design/crop.md#dolby-vision).
//
// L5 is read with `dovi_tool extract-rpu` and then `export -l level5 -f json`, which writes one
// record per frame that HAS an L5 block. `export -d level5` is never used: it writes a frame
// with no L5 as 0/0/0/0, exactly like a zeroed one (dovi_tool 2.3.4 src/dovi/exporter.rs lines
// 154 and 165, https://github.com/quietvoid/dovi_tool/tree/2.3.4 , read for P5 on
// 2026-09-29; the difference re-run on the pinned 2.3.4 on 2026-10-01), so a dropped L5 would
// read as a zeroed one.

// L5Record is one frame's L5 as `export -l level5 -f json` writes it.
type L5Record struct {
	Frame  int `json:"frame"`
	Left   int `json:"active_area_left_offset"`
	Right  int `json:"active_area_right_offset"`
	Top    int `json:"active_area_top_offset"`
	Bottom int `json:"active_area_bottom_offset"`
}

// Zero reports whether the record names no bars.
func (r L5Record) Zero() bool { return r.Left == 0 && r.Right == 0 && r.Top == 0 && r.Bottom == 0 }

// ParseL5 reads the export. Anything that is not the export's shape - not a JSON array of
// records, a negative frame index, a frame recorded twice, a negative offset - is an error,
// never an empty reading.
func ParseL5(data []byte) ([]L5Record, error) {
	var recs []L5Record
	if err := json.Unmarshal(data, &recs); err != nil {
		return nil, fmt.Errorf("the L5 export does not parse: %w", err)
	}
	if recs == nil {
		return nil, fmt.Errorf("the L5 export is not a list of records")
	}
	seen := map[int]bool{}
	for _, r := range recs {
		if r.Frame < 0 || seen[r.Frame] || r.Left < 0 || r.Right < 0 || r.Top < 0 || r.Bottom < 0 {
			return nil, fmt.Errorf("the L5 export carries an impossible record %+v", r)
		}
		seen[r.Frame] = true
	}
	return recs, nil
}

// L5Args are the two dovi_tool invocations ReadL5 runs: the RPU of every frame extracted from
// the Annex B stream on its stdin to rpu, then its L5 exported as JSON to out.
//
// The stream is piped from ffmpeg (annexB) rather than named: dovi_tool picks its demuxer from
// the file's extension, and the engine's working file ends in `.holdfast-part`, which it reads
// as raw HEVC and refuses ("Invalid PPS index", observed on the pinned 2.3.4 on 2026-10-01).
func L5Args(rpu, out string) [][]string {
	return [][]string{
		{"extract-rpu", "-", "-o", rpu},
		{"export", "-i", rpu, "-l", "level5=" + out, "-f", "json"},
	}
}

// ReadL5 reads every frame's L5 out of f (a source, or an output for the gate), through the
// two working files rpu and out, which the caller owns and removes.
func ReadL5(ctx context.Context, t Tools, f, rpu, out string) ([]L5Record, error) {
	steps := L5Args(rpu, out)
	if err := pipeline(annexB(ctx, t.FFmpeg, f), exec.CommandContext(ctx, t.DoviTool, steps[0]...)); err != nil {
		return nil, fmt.Errorf("reading the Dolby Vision L5: %w", err)
	}
	if _, err := run(exec.CommandContext(ctx, t.DoviTool, steps[1]...)); err != nil {
		return nil, fmt.Errorf("reading the Dolby Vision L5: %w", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return nil, fmt.Errorf("reading the Dolby Vision L5 export: %w", err)
	}
	return ParseL5(data)
}

// GateDoviL5 is the L5 gate's member of the engine's gate vocabulary.
const GateDoviL5 = "dolby-vision-l5"

// CheckZeroL5 is the L5 gate: an output whose source was cropped to its RPU's own active area
// carries exactly one L5 record per frame, frames 0 to frames-1, and every one is 0/0/0/0. The
// count is what keeps a DROPPED L5 from passing as a zeroed one: a frame without L5 has no
// record at all. frames is the gate's own count of the output's decoded frames.
func CheckZeroL5(recs []L5Record, frames int) error {
	if frames <= 0 {
		return &GateError{Gate: GateDoviL5, Why: "the output's frames could not be counted for its L5"}
	}
	if len(recs) != frames {
		return &GateError{Gate: GateDoviL5, Why: fmt.Sprintf("%d of the output's %d frames carry an L5 record: "+
			"a cropped Dolby Vision output carries a zeroed L5 on every frame", len(recs), frames)}
	}
	for _, r := range recs {
		switch {
		case r.Frame >= frames:
			return &GateError{Gate: GateDoviL5, Why: fmt.Sprintf("an L5 record names frame %d of %d", r.Frame, frames)}
		case !r.Zero():
			return &GateError{Gate: GateDoviL5, Why: fmt.Sprintf("frame %d's L5 is %d/%d/%d/%d (left/right/top/bottom), "+
				"not zero: it describes bars the crop removed", r.Frame, r.Left, r.Right, r.Top, r.Bottom)}
		}
	}
	return nil
}

// ZeroL5Args is the dovi_tool invocation that rewrites a source's RPU with L5 zeroed, reading
// Annex B HEVC on stdin and writing raw HEVC to out: the global `-c` ("Set active area offsets
// to 0 (meaning no letterbox bars)", `dovi_tool --help` on the pinned 2.3.4) with mode 0
// ("Parses the RPU, rewrites it untouched") for profile 8.1, and with mode 2 and the
// enhancement layer discarded for a profile 7 source converted to 8.1 - the conversion
// ConvertProfile7 runs, plus the one flag. `--edit-config` is never paired with them: any
// edit config switches -m and -c off (src/main.rs lines 84-88 at 2.3.4, proposal P5 finding 4).
func ZeroL5Args(in Intent, out string) []string {
	if in.Convert {
		return []string{"-m", "2", "-c", "convert", "--discard", "-", "-o", out}
	}
	return []string{"-m", "0", "-c", "convert", "-", "-o", out}
}

// ConvertArgs is the dovi_tool invocation the pre-pass writes its raw stream with: ZeroL5Args
// where the job crops, else the profile 7 conversion.
func ConvertArgs(in Intent, out string) []string {
	if in.ZeroL5 {
		return ZeroL5Args(in, out)
	}
	return []string{"-m", "2", "convert", "--discard", "-", "-o", out}
}
