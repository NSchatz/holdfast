package dynhdr

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// Request is one job's pre-pass: its intent, its source, and the working paths the pre-pass
// may write. Every path is the engine's, beside the job's working file and under its temp
// marker, and the engine removes each on every way out of the job.
type Request struct {
	Tools  Tools
	Intent Intent
	Source string
	// JSONPath is where the HDR10+ metadata is extracted to, and it must end in ".json": x265
	// reads dhdr10-info only from a path with that extension, and on any other it prints "Fail
	// open file, extension not valid!" and the encoder process aborts
	// (source/dynamicHDR10/JsonHelper.cpp lines 124-131,
	// https://bitbucket.org/multicoreware/x265_git/src/master/source/dynamicHDR10/JsonHelper.cpp ,
	// read 2026-10-01; the abort observed on the pinned ffmpeg the same day). RawPath is where a converted profile
	// 7 stream is written; HeadPath where the head of a profile 7 source's RPU is extracted to
	// so its enhancement-layer type can be logged.
	JSONPath, RawPath, HeadPath string
}

// Prepared is what the pre-pass established, and the part of the encode plan the derivation
// declares from it.
type Prepared struct {
	Intent Intent
	// VBV is the ceiling a Dolby Vision encode runs under (zero for HDR10+ alone).
	VBV VBV
	// HDR10PlusJSON is the validated metadata file, "" where HDR10+ is not carried.
	HDR10PlusJSON string
	// RawVideo is the converted profile 8.1 stream and FrameRate the exact rate it is read at,
	// both empty where nothing is converted.
	RawVideo  string
	FrameRate Rate
	// Frames is the source's video frame (packet) count.
	Frames int
	// ELType is what dovi_tool reports of a converted profile 7 source's enhancement layer
	// ("FEL", "MEL", or both), "" where nothing is converted or it could not be read.
	ELType string
}

// facts are the source facts the pre-pass reads in one ffprobe: its video's size, its two
// frame rates, its start, and its packet count (counted by demuxing, never decoding).
type facts struct {
	width, height int
	rate, avg     string
	start         string
	frames        int
	ok            bool
}

func probeFacts(ctx context.Context, ffprobe, src string) facts {
	out, err := run(exec.CommandContext(ctx, ffprobe, "-v", "error", "-select_streams", "v:0", "-count_packets",
		"-show_entries", "stream=width,height,r_frame_rate,avg_frame_rate,start_time,nb_read_packets",
		"-of", "default=nw=1", "--", src))
	if err != nil {
		return facts{}
	}
	return factsFrom(string(out))
}

// factsFrom reads probeFacts' output. The facts are ok only where the size and the packet
// count parse; the rates and the start are left to the steps that need them.
func factsFrom(out string) facts {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, found := strings.Cut(strings.TrimSpace(line), "="); found {
			if _, seen := m[k]; !seen {
				m[k] = v
			}
		}
	}
	f := facts{rate: m["r_frame_rate"], avg: m["avg_frame_rate"], start: m["start_time"]}
	var e1, e2, e3 error
	f.width, e1 = strconv.Atoi(m["width"])
	f.height, e2 = strconv.Atoi(m["height"])
	f.frames, e3 = strconv.Atoi(m["nb_read_packets"])
	f.ok = e1 == nil && e2 == nil && e3 == nil && f.width > 0 && f.height > 0 && f.frames > 0
	return f
}

// Prepare runs the job's pre-pass: the source facts, the VBV ceiling of a Dolby Vision encode,
// the HDR10+ extraction and its validation, and the profile 7 conversion. Any step that cannot
// complete is a *RefusalError naming the skip, and nothing has been encoded; whatever it wrote
// is at the request's paths, for the caller to remove.
func Prepare(ctx context.Context, req Request) (*Prepared, error) {
	in := req.Intent
	p := &Prepared{Intent: in}
	f := probeFacts(ctx, req.Tools.FFprobe, req.Source)
	if !f.ok {
		reason := ReasonHDR10PlusUnreadable
		if in.DolbyVision {
			reason = ReasonFrameRate
		}
		return nil, refuse(reason, fmt.Errorf("ffprobe did not establish the size and frame count of %q", req.Source))
	}
	p.Frames = f.frames
	if in.DolbyVision {
		rate, err := doviRate(in, f)
		if err != nil {
			return nil, refuse(ReasonFrameRate, err)
		}
		if p.VBV, err = VBVFor(f.width, f.height, rate); err != nil {
			return nil, refuse(ReasonFrameRate, err)
		}
		if in.Rewrites() {
			p.FrameRate = rate
		}
	}
	if in.HDR10Plus {
		if !strings.HasSuffix(req.JSONPath, ".json") {
			return nil, refuse(ReasonHDR10PlusUnreadable, fmt.Errorf("the metadata path %q does not end in .json, "+
				"which x265 refuses", req.JSONPath))
		}
		if err := ExtractHDR10Plus(ctx, req.Tools, req.Source, req.JSONPath, f.frames); err != nil {
			return nil, refuse(ReasonHDR10PlusUnreadable, err)
		}
		p.HDR10PlusJSON = req.JSONPath
	}
	if in.Convert {
		p.ELType = ELType(ctx, req.Tools, req.Source, req.HeadPath)
	}
	if in.Rewrites() {
		if err := rewrite(ctx, req.Tools, in, req.Source, req.RawPath); err != nil {
			return nil, refuse(ReasonConversionFailed, err)
		}
		p.RawVideo = req.RawPath
	}
	return p, nil
}

// doviRate is the rate a Dolby Vision encode's level is chosen at. A profile 8 source is read
// at its r_frame_rate; a profile 7 source to be converted needs one constant rate starting at
// zero, because its converted raw stream is retimed at exactly that rate.
func doviRate(in Intent, f facts) (Rate, error) {
	if !in.Rewrites() {
		r, ok := ParseRate(f.rate)
		if !ok {
			return Rate{}, fmt.Errorf("the video's frame rate could not be established (r_frame_rate %q)", f.rate)
		}
		return r, nil
	}
	r, err := ConstantRate(f.rate, f.avg)
	if err != nil {
		return Rate{}, err
	}
	if !StartsAtZero(f.start) {
		return Rate{}, fmt.Errorf("the video starts at %q, not 0: a converted raw stream is read from 0 and would "+
			"come out shifted against the audio", f.start)
	}
	return r, nil
}

// hdr10PlusJSON is the part of hdr10plus_tool's JSON this package reads: one SceneInfo entry
// per frame, in display order after the tool's reorder step
// (https://github.com/quietvoid/hdr10plus_tool/blob/1.7.2/README.md , read 2026-10-01).
type hdr10PlusJSON struct {
	JSONInfo  *json.RawMessage  `json:"JSONInfo"`
	SceneInfo []json.RawMessage `json:"SceneInfo"`
}

// ExtractHDR10Plus extracts src's HDR10+ metadata to out and validates it: the file parses as
// hdr10plus_tool's JSON and carries exactly frames entries. The form is the tool's documented
// pipe: `ffmpeg ... -c copy -bsf:v hevc_mp4toannexb -f hevc - | hdr10plus_tool extract -o
// <out> -`.
func ExtractHDR10Plus(ctx context.Context, t Tools, src, out string, frames int) error {
	if err := pipeline(annexB(ctx, t.FFmpeg, src),
		exec.CommandContext(ctx, t.HDR10Plus, "extract", "-o", out, "-")); err != nil {
		return fmt.Errorf("extracting the HDR10+ metadata: %w", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		return fmt.Errorf("reading the extracted HDR10+ metadata: %w", err)
	}
	return ValidateHDR10Plus(data, frames)
}

// ValidateHDR10Plus holds extracted metadata to the source: it parses, it is hdr10plus_tool's
// shape, and it has one entry per frame. A file with fewer entries would leave frames without
// their metadata in the output, and one with more would shift every entry after the first
// that did not belong.
func ValidateHDR10Plus(data []byte, frames int) error {
	var m hdr10PlusJSON
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("the HDR10+ metadata does not parse: %w", err)
	}
	if m.JSONInfo == nil || m.SceneInfo == nil {
		return fmt.Errorf("the HDR10+ metadata has no JSONInfo or no SceneInfo")
	}
	if len(m.SceneInfo) != frames {
		return fmt.Errorf("the HDR10+ metadata carries %d frame entries and the source has %d frames", len(m.SceneInfo), frames)
	}
	return nil
}

// ConvertProfile7 writes src's video as a profile 8.1 raw HEVC stream at out: dovi_tool mode
// 2 ("Converts the RPU to be profile 8.1 compatible. Removes luma/chroma mapping for profile 7
// FEL."), never mode 5, with the enhancement layer discarded, in the README's own pipe
// (https://github.com/quietvoid/dovi_tool/blob/2.3.4/README.md , read 2026-10-01). The stream
// is a lossless rewrite of the base layer and the RPU; nothing is decoded.
func ConvertProfile7(ctx context.Context, t Tools, src, out string) error {
	return rewrite(ctx, t, Intent{DolbyVision: true, Convert: true}, src, out)
}

// rewrite writes src's video as the raw stream the intent's encode reads (ConvertArgs): a
// profile 7 conversion, an L5 zeroing, or both.
func rewrite(ctx context.Context, t Tools, in Intent, src, out string) error {
	what := "converting the Dolby Vision profile 7 stream"
	if in.ZeroL5 {
		what = "zeroing the Dolby Vision L5 active area"
	}
	if err := pipeline(annexB(ctx, t.FFmpeg, src),
		exec.CommandContext(ctx, t.DoviTool, ConvertArgs(in, out)...)); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	st, err := os.Stat(out)
	if err != nil || st.Size() == 0 {
		return fmt.Errorf("the converted stream %q is missing or empty", out)
	}
	return nil
}

// elHeadFrames is how many frames of a profile 7 source's RPU are read to name its
// enhancement layer: the layer type is a property of the stream, and reading every frame for
// a log line would read the whole source a second time.
const elHeadFrames = 48

// profileLine is dovi_tool's summary line naming the profile, with the enhancement-layer
// types of a profile 7 stream in brackets: `Profile: 7 (FEL)`, `Profile: 7 (MEL)`, or both
// (dovi_tool 2.3.4, src/dovi/rpu_info.rs lines 240-254, from the el_type of each RPU,
// dolby_vision/src/rpu/rpu_data_nlq.rs lines 16-17 and 188-194,
// https://github.com/quietvoid/dovi_tool/tree/2.3.4 , read 2026-10-01).
var profileLine = regexp.MustCompile(`(?m)^\s*Profiles?: (.*)$`)

// ELType names a profile 7 source's enhancement layer as dovi_tool reports it, from the RPUs
// of its first frames, for the log: the conversion discards it either way. "" where it could
// not be read; that is logged, and does not refuse the conversion, which reads the source
// itself.
func ELType(ctx context.Context, t Tools, src, head string) string {
	if _, err := run(exec.CommandContext(ctx, t.DoviTool, "extract-rpu", "-l", strconv.Itoa(elHeadFrames),
		"-i", src, "-o", head)); err != nil {
		return ""
	}
	out, err := run(exec.CommandContext(ctx, t.DoviTool, "info", "-s", "-i", head))
	if err != nil {
		return ""
	}
	return ELTypeFrom(string(out))
}

// ELTypeFrom reads the enhancement-layer types off dovi_tool's summary: the bracketed part of
// its profile line, or "" where the line names none.
func ELTypeFrom(summary string) string {
	m := profileLine.FindStringSubmatch(summary)
	if m == nil {
		return ""
	}
	open, close := strings.Index(m[1], "("), strings.LastIndex(m[1], ")")
	if open < 0 || close <= open {
		return ""
	}
	return strings.TrimSpace(m[1][open+1 : close])
}

// RewriteFacts are what the crop decision reads before it asks for an L5 zeroing: the source's
// frame count, and why its frame rate is not one constant rate starting at zero - which the
// raw stream an L5 zeroing writes is read at - or nil where it is.
func RewriteFacts(ctx context.Context, ffprobe, src string) (frames int, rate error) {
	f := probeFacts(ctx, ffprobe, src)
	if !f.ok {
		return 0, fmt.Errorf("ffprobe did not establish the size and frame count of %q", src)
	}
	_, err := doviRate(Intent{DolbyVision: true, ZeroL5: true}, f)
	return f.frames, err
}
