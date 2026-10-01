package dynhdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

// Counts are what the dynamic-HDR gate reads off an output: how many video frames it decodes
// to, and how many of them carry a Dolby Vision RPU and HDR10+ metadata.
//
// They are counted by ffprobe on the decoded frames (`-show_frames`), not by the two tools
// that wrote the metadata: the frame side data is what libavcodec's HEVC decoder exports for
// an RPU NAL and a 2094-40 SEI it parsed (libavcodec/hevc/hevcdec.c and h2645_sei.c at
// 5d4d3bdc61), so a count taken there is the metadata a player's decoder sees, measured by a
// second implementation rather than by the tool whose output it checks. It costs one decode
// of the video, as the decode-integrity gate does; frames are counted once each, however many
// copies of one type a frame carries.
type Counts struct {
	Frames, DoviRPU, HDR10Plus int
}

// frameSideData is the part of ffprobe's JSON this package reads.
type frameSideData struct {
	Frames []struct {
		MediaType string `json:"media_type"`
		SideData  []struct {
			Type string `json:"side_data_type"`
		} `json:"side_data_list"`
	} `json:"frames"`
}

// CountsFrom reads ffprobe's `-show_frames -of json` output of one video stream.
func CountsFrom(data []byte) (Counts, error) {
	var fs frameSideData
	if err := json.Unmarshal(data, &fs); err != nil {
		return Counts{}, fmt.Errorf("the per-frame side data does not parse: %w", err)
	}
	var c Counts
	for _, f := range fs.Frames {
		if f.MediaType != "video" {
			continue
		}
		c.Frames++
		var rpu, plus bool
		for _, sd := range f.SideData {
			rpu = rpu || sd.Type == DoviRPUType
			plus = plus || sd.Type == HDR10PlusType
		}
		if rpu {
			c.DoviRPU++
		}
		if plus {
			c.HDR10Plus++
		}
	}
	return c, nil
}

// Count decodes f's first video stream and counts its frames and the metadata they carry.
func Count(ctx context.Context, ffprobe, f string) (Counts, error) {
	out, err := run(exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0", "-show_frames",
		"-show_entries", "frame=media_type:frame_side_data=side_data_type", "-of", "json", "--", f))
	if err != nil {
		return Counts{}, err
	}
	return CountsFrom(out)
}

// The three things the gate checks, as the engine's gate vocabulary names them: the DOVI
// configuration record, the per-frame RPU, and the per-frame HDR10+.
const (
	GateDoviRecord = "dolby-vision-record"
	GateDoviRPU    = "dolby-vision-rpu"
	GateHDR10Plus  = "hdr10-plus"
)

// Expectation is what an output must carry: a record naming profile Profile and compatibility
// id CompatID with an RPU on every frame, where DolbyVision, and HDR10+ on every frame, where
// HDR10Plus.
type Expectation struct {
	DolbyVision       bool
	Profile, CompatID int
	HDR10Plus         bool
}

// GateError is the gate's rejection: which of the three failed, and why.
type GateError struct {
	Gate string
	Why  string
}

func (e *GateError) Error() string {
	return "dynamic HDR: " + e.Why + ". The source is kept"
}

// Check holds an output's record and counts to the expectation. Anything it cannot establish
// - no frames counted, a record that does not parse - is a rejection, never a pass. The
// record is checked first: an output without one is not Dolby Vision to any player, whatever
// its frames carry.
func Check(want Expectation, rec Record, c Counts) error {
	if !want.DolbyVision && !want.HDR10Plus {
		return nil
	}
	if c.Frames <= 0 {
		gate := GateHDR10Plus
		if want.DolbyVision {
			gate = GateDoviRPU
		}
		return &GateError{Gate: gate, Why: "the output's frames could not be counted"}
	}
	if want.DolbyVision {
		switch {
		case !rec.Present:
			return &GateError{Gate: GateDoviRecord, Why: "the output carries no DOVI configuration record"}
		case !rec.Readable:
			return &GateError{Gate: GateDoviRecord, Why: "the output's DOVI configuration record could not be read"}
		case rec.Profile != want.Profile || rec.CompatID != want.CompatID:
			return &GateError{Gate: GateDoviRecord, Why: fmt.Sprintf("the output's DOVI configuration record names "+
				"profile %d compatibility id %d, and the plan declares profile %d compatibility id %d",
				rec.Profile, rec.CompatID, want.Profile, want.CompatID)}
		case !rec.RPU || !rec.BL || rec.EL:
			return &GateError{Gate: GateDoviRecord, Why: fmt.Sprintf("the output's DOVI configuration record flags "+
				"rpu %t, bl %t, el %t, and a single-layer profile %d carries rpu and bl and no el",
				rec.RPU, rec.BL, rec.EL, want.Profile)}
		case c.DoviRPU != c.Frames:
			return &GateError{Gate: GateDoviRPU, Why: fmt.Sprintf("%d of the output's %d frames carry a Dolby Vision RPU",
				c.DoviRPU, c.Frames)}
		}
	}
	if want.HDR10Plus && c.HDR10Plus != c.Frames {
		return &GateError{Gate: GateHDR10Plus, Why: fmt.Sprintf("%d of the output's %d frames carry HDR10+ metadata",
			c.HDR10Plus, c.Frames)}
	}
	return nil
}
