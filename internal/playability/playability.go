// Package playability is the playability gate: a replacement replaces nothing until it decodes
// when a player starts it COLD at a keyframe other than the first, and when its video is remuxed
// into the fragmented MP4 a media server streams and each fragment is decoded on its own
// (docs/design/playability.md#playability-gate).
//
// The gates before it measure faithfulness: the decode-integrity gate decodes the output once,
// linearly, from its first frame, so every picture is decoded with every picture before it
// already in the decoder. A player that seeks, a segmenter that cuts at a keyframe and a client
// that joins a stream mid-way start somewhere else, with nothing before that keyframe decoded,
// and an output whose pictures after a keyframe reference pictures before it decodes cleanly in
// the first case and not in the second (issue #200). This package runs the second case.
package playability

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// The sampling and the windows. Together they are the gate's cost bound, which is independent of
// the output's length: at most SpreadSamples+LeadingSamples cold starts of ColdStartFrames frames
// each, the same number of fragments of at most FragmentFrames frames each, and - where a hardware
// decoder is configured - the cold starts again on it plus one opening window
// (docs/design/playability.md#cost).
const (
	// SpreadSamples is how many keyframes, spread evenly over the output after its first, are
	// started cold. The first keyframe is the file's start, which the decode-integrity gate has
	// already decoded from cold.
	SpreadSamples = 16
	// LeadingSamples is how many MORE keyframes are started cold from among those followed by
	// leading pictures (a picture after the keyframe in decode order that is shown before it):
	// the ones a cold start can fail at, which an even spread over a film with thousands of
	// keyframes would mostly miss (issue #200: 7% of 2,236 keyframes).
	LeadingSamples = 16
	// ColdStartFrames is how many frames each cold start decodes. Leading pictures follow their
	// keyframe in decode order within one reorder window, which is a few frames (x265's bframes
	// is at most 16), so the window holds every one of them and the trailing pictures that
	// reference them.
	ColdStartFrames = 48
	// FragmentFrames is the most frames decoded of one sampled fragment: a fragment is one
	// keyframe interval, which the encoder sets (x265's default keyint is 250), and a fragment
	// longer than this is decoded for this many frames.
	FragmentFrames = 250
	// landingTolerance is how far, in seconds, the first packet a cold start reads may be from
	// the keyframe it was aimed at and still be that keyframe: half a millisecond, under the
	// millisecond Matroska stores a timestamp in and far under any frame interval.
	landingTolerance = 0.0005
	// seekPastKeyframe is how far past a keyframe's timestamp the seek is aimed, so the seek
	// that finds the last keyframe at or before its target finds this one and not the one
	// before it: one millisecond, under any frame interval.
	seekPastKeyframe = 0.001
	// delaySeekCompensation is what fftools subtracts from an input -ss on a file that has a
	// video stream with a reorder delay (has_b_frames), so its seek lands before the target
	// rather than after it: 3*AV_TIME_BASE/23 (fftools/ffmpeg_demux.c, the dts_heuristic in
	// ifile_open). It is added back so the seek lands on the keyframe it was aimed at, and the
	// landing check below proves it did.
	delaySeekCompensation = 3.0 / 23.0
	// stderrCap is how much of a failed ffmpeg's error output is kept for the reason.
	stderrCap = 4096
)

// Fragmented-MP4 movflags: a new fragment at every keyframe, the moov written first with no
// samples, and every fragment's data offsets relative to its own moof, so the init segment and
// one fragment are a stream on their own - the shape HLS and DASH serve.
const fragmentFlags = "frag_keyframe+empty_moov+default_base_moof"

// Keyframe is one keyframe of the gated video stream.
type Keyframe struct {
	// Ordinal is its position among the stream's keyframes, from 0.
	Ordinal int
	// PTS is its presentation timestamp, in seconds.
	PTS float64
	// Leading says a packet after it in decode order, and before the next keyframe, is
	// presented before it: a leading picture, which a decoder starting at this keyframe must
	// either decode from this keyframe alone (RADL) or drop (RASL).
	Leading bool
}

// Report is what one pass of the gate measured.
type Report struct {
	// Keyframes is how many keyframes the stream has, and Leading how many of them are followed
	// by leading pictures.
	Keyframes, Leading int
	// Sampled is the ordinals started cold, and fragment-decoded.
	Sampled []int
	// Hardware says the cold starts were repeated on the configured hardware decoder.
	Hardware bool
}

// Rejection is the gate's verdict that the output does not play: a decode started cold failed.
// It is a property of the output's bytes, so the same settings produce it again.
type Rejection struct {
	// Stage is "cold start", "hardware cold start" or "fragment".
	Stage string
	// Keyframe is where the failed decode started, and Keyframes how many the stream has.
	Keyframe  Keyframe
	Keyframes int
	// Detail is the decoder's first error line.
	Detail string
}

func (r *Rejection) Error() string {
	where := fmt.Sprintf("keyframe %d of %d (at %.3fs", r.Keyframe.Ordinal, r.Keyframes, r.Keyframe.PTS)
	if r.Keyframe.Leading {
		where += ", followed by leading pictures"
	}
	where += ")"
	what := "a decode started cold at " + where
	switch r.Stage {
	case StageHardware:
		what = "a hardware decode started cold at " + where
	case StageFragment:
		what = "the fragment that starts at " + where + " of the output remuxed to fragmented MP4, decoded on its own,"
	}
	return fmt.Sprintf("%s failed (%s): a player that seeks there or starts a stream there cannot decode it", what, r.Detail)
}

// The rejection stages.
const (
	StageColdStart = "cold start"
	StageHardware  = "hardware cold start"
	StageFragment  = "fragment"
)

// Unmeasured is the gate's verdict that it could not take its measurement: the packets could not
// be listed, a seek did not land on the keyframe it was aimed at, the remux did not complete, or
// the hardware decoder did not decode the output's opening frames. It says nothing about the
// output, and the output is refused all the same.
type Unmeasured struct {
	What string
	Err  error
}

func (u *Unmeasured) Error() string {
	if u.Err == nil {
		return u.What
	}
	return u.What + ": " + u.Err.Error()
}

func (u *Unmeasured) Unwrap() error { return u.Err }

// Checker runs the gate.
type Checker struct {
	FFmpeg, FFprobe string
	// Hardware is the global options that put a decode on the job's hardware decoder, and nil
	// where none is configured; HardwareFFmpeg is the binary they run through ("" is FFmpeg).
	Hardware       []string
	HardwareFFmpeg string
}

// Check gates stream (an absolute stream index) of file. It returns nil, a *Rejection or an
// *Unmeasured (or the context's error).
func (c Checker) Check(ctx context.Context, file string, stream int) (Report, error) {
	var rep Report
	kfs, firstKey, err := c.keyframes(ctx, file, stream)
	if err != nil {
		return rep, err
	}
	sample := Sample(kfs)
	rep.Keyframes = len(kfs)
	for _, k := range kfs {
		if k.Leading {
			rep.Leading++
		}
	}
	for _, k := range sample {
		rep.Sampled = append(rep.Sampled, k.Ordinal)
	}
	// A stream with one keyframe samples nothing: no seek is computed and no fragment decoded,
	// so its start time is not probed. The hardware opening decode below still runs.
	var base SeekBase
	if len(sample) > 0 {
		if base, err = c.seekBase(ctx, file); err != nil {
			return rep, err
		}
	}
	for _, k := range sample {
		if err := c.coldStart(ctx, c.FFmpeg, nil, StageColdStart, file, stream, k, len(kfs), base); err != nil {
			return rep, err
		}
	}
	if err := c.fragments(ctx, file, stream, kfs, firstKey, sample); err != nil {
		return rep, err
	}
	if len(c.Hardware) == 0 {
		return rep, nil
	}
	rep.Hardware = true
	bin := c.HardwareFFmpeg
	if bin == "" {
		bin = c.FFmpeg
	}
	if err := c.hardwareOpens(ctx, bin, file, stream); err != nil {
		return rep, err
	}
	for _, k := range sample {
		if err := c.coldStart(ctx, bin, c.Hardware, StageHardware, file, stream, k, len(kfs), base); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// PacketArgs is the ffprobe command line that lists the stream's packets in decode order, each
// as its presentation time and its flags. It reads the container and decodes nothing.
func PacketArgs(file string, stream int) []string {
	return []string{"-v", "error", "-select_streams", strconv.Itoa(stream),
		"-show_entries", "packet=pts_time,flags", "-of", "csv=p=0", "--", file}
}

func (c Checker) keyframes(ctx context.Context, file string, stream int) ([]Keyframe, bool, error) {
	cmd := exec.CommandContext(ctx, c.FFprobe, PacketArgs(file, stream)...)
	var stderr capped
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, &Unmeasured{What: "the output's packets could not be listed", Err: err}
	}
	if err := cmd.Start(); err != nil {
		return nil, false, &Unmeasured{What: "the output's packets could not be listed", Err: err}
	}
	kfs, firstKey, perr := ReadKeyframes(out)
	if perr != nil {
		_, _ = io.Copy(io.Discard, out)
	}
	werr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, false, &Unmeasured{What: "the gate was interrupted", Err: ctx.Err()}
	}
	if werr != nil {
		return nil, false, &Unmeasured{What: "the output's packets could not be listed", Err: withStderr(werr, &stderr)}
	}
	if perr != nil {
		return nil, false, &Unmeasured{What: "the output's packet list could not be read", Err: perr}
	}
	if len(kfs) == 0 {
		return nil, false, &Unmeasured{What: "the output's video stream has no keyframe"}
	}
	return kfs, firstKey, nil
}

// ReadKeyframes reads PacketArgs' output: one packet per line, in decode order, as
// "pts_time,flags". It returns the keyframes, each marked Leading where a packet after it and
// before the next keyframe has an earlier presentation time, and whether the first packet is a
// keyframe. A keyframe without a presentation time cannot be seeked to, and is an error; a
// non-key packet without one is not compared.
func ReadKeyframes(r io.Reader) ([]Keyframe, bool, error) {
	sc := bufio.NewScanner(r)
	var kfs []Keyframe
	firstKey, line := false, 0
	for sc.Scan() {
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		line++
		fields := strings.Split(text, ",")
		if len(fields) < 2 {
			return nil, false, fmt.Errorf("packet line %d %q has no flags", line, text)
		}
		key := strings.HasPrefix(fields[1], "K")
		pts, hasPTS := 0.0, fields[0] != "N/A"
		if hasPTS {
			v, err := strconv.ParseFloat(fields[0], 64)
			if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, false, fmt.Errorf("packet line %d %q has no readable pts", line, text)
			}
			pts = v
		}
		if line == 1 {
			firstKey = key
		}
		switch {
		case key && !hasPTS:
			return nil, false, fmt.Errorf("keyframe %d (packet line %d) has no pts to seek to", len(kfs), line)
		case key:
			kfs = append(kfs, Keyframe{Ordinal: len(kfs), PTS: pts})
		case hasPTS && len(kfs) > 0 && pts < kfs[len(kfs)-1].PTS:
			kfs[len(kfs)-1].Leading = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	return kfs, firstKey, nil
}

// Sample is the keyframes the gate starts cold, in order: SpreadSamples spread evenly over every
// keyframe after the first, and up to LeadingSamples more spread evenly over the keyframes after
// the first that are followed by leading pictures and were not already chosen. A stream with no
// keyframe after its first samples nothing: the decode-integrity gate started it cold already.
func Sample(kfs []Keyframe) []Keyframe {
	if len(kfs) < 2 {
		return nil
	}
	rest := kfs[1:]
	chosen := map[int]bool{}
	var out []Keyframe
	for _, i := range spread(len(rest), SpreadSamples) {
		chosen[rest[i].Ordinal] = true
		out = append(out, rest[i])
	}
	var leading []Keyframe
	for _, k := range rest {
		if k.Leading && !chosen[k.Ordinal] {
			leading = append(leading, k)
		}
	}
	for _, i := range spread(len(leading), LeadingSamples) {
		out = append(out, leading[i])
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Ordinal < out[b].Ordinal })
	return out
}

// spread is want indexes of [0, n) spread evenly, each at the centre of its share, or every
// index where n is no more than want.
func spread(n, want int) []int {
	if n <= 0 || want <= 0 {
		return nil
	}
	if n <= want {
		out := make([]int, n)
		for i := range out {
			out[i] = i
		}
		return out
	}
	out := make([]int, want)
	for i := range out {
		out[i] = int((float64(i) + 0.5) * float64(n) / float64(want))
	}
	return out
}

// SeekBase is what a cold start's seek is computed from: the file's start time, which fftools
// adds to an input -ss, and whether a video stream has a reorder delay, for which it then
// subtracts delaySeekCompensation.
type SeekBase struct {
	Start   float64
	Delayed bool
}

// SeekTarget is the input -ss that lands on k: its timestamp relative to the file's start, a
// millisecond past it, plus what fftools takes off a file with a reorder delay. fftools seeks
// to the last keyframe at or before that point (avformat_seek_file with a maximum of the
// target), which is k as long as the next keyframe is more than a millisecond later.
func SeekTarget(k Keyframe, start float64, delayed bool) float64 {
	t := k.PTS - start + seekPastKeyframe
	if delayed {
		t += delaySeekCompensation
	}
	return math.Max(t, 0)
}

func (c Checker) seekBase(ctx context.Context, file string) (SeekBase, error) {
	out, err := exec.CommandContext(ctx, c.FFprobe, "-v", "error",
		"-show_entries", "format=start_time:stream=codec_type,has_b_frames", "-of", "json", "--", file).Output()
	if err != nil {
		return SeekBase{}, &Unmeasured{What: "the output's start time could not be read", Err: err}
	}
	return ParseSeekBase(out)
}

// ParseSeekBase reads the ffprobe JSON seekBase asks for.
func ParseSeekBase(out []byte) (SeekBase, error) {
	var doc struct {
		Streams []struct {
			CodecType  string `json:"codec_type"`
			HasBFrames int    `json:"has_b_frames"`
		} `json:"streams"`
		Format struct {
			StartTime string `json:"start_time"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return SeekBase{}, &Unmeasured{What: "the output's start time could not be read", Err: err}
	}
	var b SeekBase
	if doc.Format.StartTime != "" && doc.Format.StartTime != "N/A" {
		v, err := strconv.ParseFloat(doc.Format.StartTime, 64)
		if err != nil {
			return SeekBase{}, &Unmeasured{What: "the output's start time could not be read", Err: err}
		}
		b.Start = v
	}
	for _, s := range doc.Streams {
		if s.CodecType == "video" && s.HasBFrames > 0 {
			b.Delayed = true
		}
	}
	return b, nil
}

// ColdStartArgs is the command line of one cold start: hw (the hardware decode's global options,
// or nil), then an input seek to target with -noaccurate_seek, so nothing before the keyframe the
// seek lands on is read or decoded and nothing after it is dropped, and -copyts, so the landing
// is reported in the stream's own timestamps. Two outputs: ColdStartFrames frames decoded under
// -xerror and -err_detect +explode, exactly as the decode-integrity gate decodes, and the first
// packet copied to framecrc on stdout, which is where the seek landed.
func ColdStartArgs(hw []string, file string, stream int, target float64) []string {
	m := "0:" + strconv.Itoa(stream)
	args := []string{"-hide_banner", "-nostdin", "-v", "error"}
	args = append(args, hw...)
	return append(args, "-xerror", "-err_detect", "+explode",
		"-ss", strconv.FormatFloat(target, 'f', 6, 64), "-noaccurate_seek", "-copyts", "-i", file,
		"-map", m, "-frames:v", strconv.Itoa(ColdStartFrames), "-f", "null", "-",
		"-map", m, "-c", "copy", "-frames:v", "1", "-f", "framecrc", "-")
}

func (c Checker) coldStart(ctx context.Context, bin string, hw []string, stage, file string, stream int,
	k Keyframe, total int, base SeekBase) error {
	target := SeekTarget(k, base.Start, base.Delayed)
	cmd := exec.CommandContext(ctx, bin, ColdStartArgs(hw, file, stream, target)...)
	var stdout bytes.Buffer
	var stderr capped
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return &Unmeasured{What: "the gate was interrupted", Err: ctx.Err()}
	}
	if err != nil {
		return &Rejection{Stage: stage, Keyframe: k, Keyframes: total, Detail: detail(err, &stderr)}
	}
	landed, ok := FirstPacketTime(stdout.String())
	if !ok {
		return &Unmeasured{What: fmt.Sprintf("the %s at keyframe %d read no packet to say where its seek landed", stage, k.Ordinal)}
	}
	if math.Abs(landed-k.PTS) > landingTolerance {
		return &Unmeasured{What: fmt.Sprintf("the %s aimed at keyframe %d (%.6fs) landed at %.6fs, so it did not "+
			"start where it was meant to", stage, k.Ordinal, k.PTS, landed)}
	}
	return nil
}

// FirstPacketTime reads framecrc output: the stream's time base from its "#tb 0: num/den" line
// and the first packet's pts (its third field), in seconds.
func FirstPacketTime(framecrc string) (float64, bool) {
	num, den := int64(0), int64(0)
	for _, line := range strings.Split(framecrc, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "#tb 0:"); ok {
			n, d, found := strings.Cut(strings.TrimSpace(rest), "/")
			if !found {
				return 0, false
			}
			var err1, err2 error
			num, err1 = strconv.ParseInt(n, 10, 64)
			den, err2 = strconv.ParseInt(d, 10, 64)
			if err1 != nil || err2 != nil || num <= 0 || den <= 0 {
				return 0, false
			}
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if den == 0 || len(fields) < 3 {
			return 0, false
		}
		pts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			return 0, false
		}
		return float64(pts) * float64(num) / float64(den), true
	}
	return 0, false
}

// HardwareOpenArgs is the command line that decodes the output's first ColdStartFrames frames on
// the hardware decoder, from the file's start.
func HardwareOpenArgs(hw []string, file string, stream int) []string {
	args := []string{"-hide_banner", "-nostdin", "-v", "error"}
	args = append(args, hw...)
	return append(args, "-xerror", "-err_detect", "+explode", "-i", file,
		"-map", "0:"+strconv.Itoa(stream), "-frames:v", strconv.Itoa(ColdStartFrames), "-f", "null", "-")
}

// hardwareOpens decodes the output's opening frames on the hardware decoder. A failure here is
// the device or the decoder, not a cold start - the same frames decoded in software already
// passed the decode-integrity gate - so it is Unmeasured, and only a cold start that fails
// after this passed is a rejection.
func (c Checker) hardwareOpens(ctx context.Context, bin, file string, stream int) error {
	cmd := exec.CommandContext(ctx, bin, HardwareOpenArgs(c.Hardware, file, stream)...)
	var stderr capped
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return &Unmeasured{What: "the gate was interrupted", Err: ctx.Err()}
		}
		return &Unmeasured{What: "the hardware decoder did not decode the output's opening frames, so its cold " +
			"starts could not be measured", Err: errors.New(detail(err, &stderr))}
	}
	return nil
}

// RemuxArgs is the command line that remuxes the stream to fragmented MP4 on stdout. It copies
// the bitstream and decodes nothing.
func RemuxArgs(file string, stream int) []string {
	return []string{"-hide_banner", "-nostdin", "-v", "error", "-i", file,
		"-map", "0:" + strconv.Itoa(stream), "-c", "copy", "-f", "mp4", "-movflags", fragmentFlags, "-"}
}

// FragmentArgs is the command line that decodes one init segment and one fragment, read from
// stdin, as the decode-integrity gate decodes.
func FragmentArgs() []string {
	return []string{"-hide_banner", "-v", "error", "-xerror", "-err_detect", "+explode",
		"-f", "mp4", "-i", "pipe:0", "-map", "0:v:0", "-frames:v", strconv.Itoa(FragmentFrames), "-f", "null", "-"}
}

// fragments remuxes the stream to fragmented MP4 through a pipe and decodes each sampled
// fragment on its own, the init segment and that fragment and nothing else, as a client that
// joins a stream there is handed them. Nothing is written to disk and one box is held at a time:
// a sampled fragment is streamed into its decoder as it is read.
func (c Checker) fragments(ctx context.Context, file string, stream int, kfs []Keyframe, firstKey bool, sample []Keyframe) error {
	if len(sample) == 0 {
		return nil
	}
	// Fragment n starts at keyframe n, after one fragment more where the stream does not open on
	// a keyframe (frag_keyframe cuts before every keyframe, and the first fragment opens on
	// whatever the first packet is).
	offset := 0
	if !firstKey {
		offset = 1
	}
	byFragment := map[int]Keyframe{}
	for _, k := range sample {
		byFragment[k.Ordinal+offset] = k
	}
	remux := exec.CommandContext(ctx, c.FFmpeg, RemuxArgs(file, stream)...)
	var stderr capped
	remux.Stderr = &stderr
	out, err := remux.StdoutPipe()
	if err != nil {
		return &Unmeasured{What: "the output could not be remuxed to fragmented MP4", Err: err}
	}
	if err := remux.Start(); err != nil {
		return &Unmeasured{What: "the output could not be remuxed to fragmented MP4", Err: err}
	}
	n, rerr := c.walkFragments(ctx, bufio.NewReaderSize(out, 1<<20), byFragment, len(kfs))
	_, _ = io.Copy(io.Discard, out)
	werr := remux.Wait()
	if ctx.Err() != nil {
		return &Unmeasured{What: "the gate was interrupted", Err: ctx.Err()}
	}
	if rerr != nil {
		return rerr
	}
	if werr != nil {
		return &Unmeasured{What: "the output could not be remuxed to fragmented MP4", Err: withStderr(werr, &stderr)}
	}
	if want := len(kfs) + offset; n != want {
		return &Unmeasured{What: fmt.Sprintf("the fragmented MP4 remux has %d fragments where the stream has %d "+
			"keyframes, so its fragments cannot be matched to them", n, len(kfs))}
	}
	return nil
}

// walkFragments reads the remux's top-level boxes, keeps the init segment (ftyp and moov), and
// streams each sampled fragment (its moof and every box up to the next moof) into a decoder of
// its own. It returns how many fragments it saw.
func (c Checker) walkFragments(ctx context.Context, r io.Reader, byFragment map[int]Keyframe, total int) (int, error) {
	var initSeg bytes.Buffer
	frag := -1
	var dec *fragmentDecoder
	finish := func() error {
		if dec == nil {
			return nil
		}
		d := dec
		dec = nil
		if err := d.finish(); err != nil {
			if ctx.Err() != nil {
				return &Unmeasured{What: "the gate was interrupted", Err: ctx.Err()}
			}
			return &Rejection{Stage: StageFragment, Keyframe: d.k, Keyframes: total, Detail: err.Error()}
		}
		return nil
	}
	for {
		h, err := ReadBoxHeader(r)
		if err == io.EOF {
			break
		}
		if err != nil {
			if dec != nil {
				dec.abandon()
			}
			return frag + 1, &Unmeasured{What: "the fragmented MP4 remux could not be read", Err: err}
		}
		body := io.LimitReader(r, h.Size)
		switch h.Type {
		case "ftyp", "moov":
			if frag >= 0 || initSeg.Len()+len(h.Raw)+int(h.Size) > maxInit {
				return frag + 1, &Unmeasured{What: "the fragmented MP4 remux has no usable init segment"}
			}
			initSeg.Write(h.Raw)
			if _, err := io.Copy(&initSeg, body); err != nil {
				return frag + 1, &Unmeasured{What: "the fragmented MP4 remux could not be read", Err: err}
			}
			continue
		case "moof":
			if err := finish(); err != nil {
				_, _ = io.Copy(io.Discard, body)
				return frag + 1, err
			}
			frag++
			if k, ok := byFragment[frag]; ok {
				d, err := c.startFragmentDecoder(ctx, k, initSeg.Bytes())
				if err != nil {
					return frag + 1, err
				}
				dec = d
			}
		case "mfra":
			if err := finish(); err != nil {
				return frag + 1, err
			}
		}
		if dec != nil {
			dec.write(h.Raw)
			if err := dec.copy(body); err != nil {
				dec.abandon()
				return frag + 1, &Unmeasured{What: "the fragmented MP4 remux could not be read", Err: err}
			}
		} else if _, err := io.Copy(io.Discard, body); err != nil {
			return frag + 1, &Unmeasured{What: "the fragmented MP4 remux could not be read", Err: err}
		}
	}
	if err := finish(); err != nil {
		return frag + 1, err
	}
	return frag + 1, nil
}

// maxInit bounds the init segment held in memory: a moov with no samples is a few kilobytes.
const maxInit = 16 << 20

// BoxHeader is one ISO BMFF box header: its type, the size of its body, and its raw bytes.
type BoxHeader struct {
	Type string
	Size int64
	Raw  []byte
}

// ReadBoxHeader reads one box header. It returns io.EOF only at a clean end between boxes. A box
// whose size runs to the end of the file (size 0) is not one the fragmented muxer writes, and is
// refused.
func ReadBoxHeader(r io.Reader) (BoxHeader, error) {
	raw := make([]byte, 8)
	if _, err := io.ReadFull(r, raw); err != nil {
		if err == io.EOF {
			return BoxHeader{}, io.EOF
		}
		return BoxHeader{}, fmt.Errorf("a truncated box header: %w", err)
	}
	size := int64(uint32(raw[0])<<24 | uint32(raw[1])<<16 | uint32(raw[2])<<8 | uint32(raw[3]))
	typ := string(raw[4:8])
	header := int64(8)
	switch size {
	case 0:
		return BoxHeader{}, fmt.Errorf("box %q runs to the end of the stream", typ)
	case 1:
		ext := make([]byte, 8)
		if _, err := io.ReadFull(r, ext); err != nil {
			return BoxHeader{}, fmt.Errorf("a truncated box header: %w", err)
		}
		raw = append(raw, ext...)
		var v uint64
		for _, b := range ext {
			v = v<<8 | uint64(b)
		}
		if v > math.MaxInt64 {
			return BoxHeader{}, fmt.Errorf("box %q is larger than this reader can hold", typ)
		}
		size, header = int64(v), 16
	}
	if size < header {
		return BoxHeader{}, fmt.Errorf("box %q declares %d bytes, less than its own header", typ, size)
	}
	return BoxHeader{Type: typ, Size: size - header, Raw: raw}, nil
}

// fragmentDecoder is one fragment's decoder, fed on its stdin.
type fragmentDecoder struct {
	k      Keyframe
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr capped
	closed bool // the decoder stopped reading (it reached FragmentFrames, or failed)
}

func (c Checker) startFragmentDecoder(ctx context.Context, k Keyframe, initSeg []byte) (*fragmentDecoder, error) {
	d := &fragmentDecoder{k: k}
	d.cmd = exec.CommandContext(ctx, c.FFmpeg, FragmentArgs()...)
	d.cmd.Stderr = &d.stderr
	in, err := d.cmd.StdinPipe()
	if err != nil {
		return nil, &Unmeasured{What: "a fragment decoder could not be started", Err: err}
	}
	d.stdin = in
	if err := d.cmd.Start(); err != nil {
		return nil, &Unmeasured{What: "a fragment decoder could not be started", Err: err}
	}
	d.write(initSeg)
	return d, nil
}

// write feeds the decoder; once it has stopped reading, the rest of its fragment is discarded.
func (d *fragmentDecoder) write(b []byte) {
	if d.closed {
		return
	}
	if _, err := d.stdin.Write(b); err != nil {
		d.closed = true
	}
}

// copy streams a box body into the decoder, and reads it to its end whatever the decoder does.
// The error is the remux's, never the decoder's.
func (d *fragmentDecoder) copy(body io.Reader) error {
	buf := make([]byte, 64<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			d.write(buf[:n])
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// finish closes the decoder's input and returns its verdict: nil where it decoded what it was
// given, or its first error line.
func (d *fragmentDecoder) finish() error {
	_ = d.stdin.Close()
	if err := d.cmd.Wait(); err != nil {
		return errors.New(detail(err, &d.stderr))
	}
	return nil
}

// abandon stops a decoder whose fragment could not be read to its end.
func (d *fragmentDecoder) abandon() {
	_ = d.stdin.Close()
	if d.cmd.Process != nil {
		_ = d.cmd.Process.Kill()
	}
	_ = d.cmd.Wait()
}

// capped keeps the first stderrCap bytes written to it and discards the rest.
type capped struct{ buf bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := stderrCap - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

func (c *capped) String() string { return c.buf.String() }

// detail is a failed ffmpeg's first error line, or its exit status where it printed none.
func detail(err error, stderr *capped) string {
	for _, line := range strings.Split(stderr.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return err.Error()
}

func withStderr(err error, stderr *capped) error {
	return errors.New(detail(err, stderr))
}
