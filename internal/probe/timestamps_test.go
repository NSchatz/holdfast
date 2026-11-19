package probe

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestParseTimestamps(t *testing.T) {
	timed := func(pts, dts int64) Packet { return Packet{PTS: pts, DTS: dts, HasPTS: true, HasDTS: true} }
	for _, tc := range []struct {
		name, in string
		want     Timestamps
	}{
		{"every packet timed, in decode order, with its flags",
			"packet|pts=0|dts=-83|flags=K__\npacket|pts=125|dts=-42|flags=___\npacket|pts=42|dts=0|flags=_D_\nstream|time_base=1/1000\n",
			Timestamps{TimeBaseNum: 1, TimeBaseDen: 1000, Packets: []Packet{
				{PTS: 0, DTS: -83, HasPTS: true, HasDTS: true, Key: true}, timed(125, -42),
				{PTS: 42, DTS: 0, HasPTS: true, HasDTS: true, Discard: true}}}},
		{"a transport stream's program listing and trailing empty sections are skipped",
			"packet|pts=133500|dts=126000|flags=KD_|\nprogram|stream|time_base=1/90000\nstream|time_base=1/90000\n",
			Timestamps{TimeBaseNum: 1, TimeBaseDen: 90000, Packets: []Packet{
				{PTS: 133500, DTS: 126000, HasPTS: true, HasDTS: true, Key: true, Discard: true}}}},
		{"a stream group's listing (a video track linked to a timecode track) is skipped",
			"stream_group|stream|time_base=1/16000\n\nstream|time_base=1/16000\npacket|pts=0|dts=0|flags=K__\n",
			Timestamps{TimeBaseNum: 1, TimeBaseDen: 16000, Packets: []Packet{
				{HasPTS: true, HasDTS: true, Key: true}}}},
		{"the stream line comes first, with an empty trailing section, between blank lines",
			"\nstream|time_base=1001/24000|\n\npacket|pts=7|dts=N/A|flags=___\n",
			Timestamps{TimeBaseNum: 1001, TimeBaseDen: 24000, Packets: []Packet{{PTS: 7, HasPTS: true}}}},
		{"packets timed by a decode timestamp alone, and by neither",
			"packet|pts=N/A|dts=25|flags=K__\npacket|pts=N/A|dts=N/A|flags=___\nstream|time_base=1/600\n",
			Timestamps{TimeBaseNum: 1, TimeBaseDen: 600, Packets: []Packet{{DTS: 25, HasDTS: true, Key: true}, {}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTimestamps(strings.NewReader(tc.in))
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseTimestamps = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}

	for _, tc := range []struct{ name, in, want string }{
		{"no time base", "packet|pts=0|dts=0|flags=K__\n", "names no video stream time base"},
		{"no packets", "stream|time_base=1/1000\n", "carries no packets"},
		{"two streams", "stream|time_base=1/1000\nstream|time_base=1/1000\npacket|pts=0|dts=0|flags=K__\n", "more than one video stream"},
		{"a pts that is not a number", "packet|pts=1.5|dts=0|flags=K__\nstream|time_base=1/1000\n", `a packet's pts is not a timestamp: "1.5"`},
		{"a dts that is not a number", "packet|pts=0|dts=x|flags=K__\nstream|time_base=1/1000\n", `a packet's dts is not a timestamp: "x"`},
		{"a packet without its pts", "packet|dts=0|flags=K__\nstream|time_base=1/1000\n", "a packet line carries no pts"},
		{"a packet without its dts", "packet|pts=0|flags=K__\nstream|time_base=1/1000\n", "a packet line carries no dts"},
		{"a packet without its flags", "packet|pts=0|dts=0\nstream|time_base=1/1000\n", "a packet line carries no flags"},
		{"a stream line without a time base", "stream|codec_name=h264\npacket|pts=0|dts=0|flags=K__\n", "the stream line carries no time base"},
		{"a time base of zero ticks", "stream|time_base=0/1000\n", "not a ratio of positive integers"},
		{"a time base over zero", "stream|time_base=1/0\n", "not a ratio of positive integers"},
		{"a negative time base", "stream|time_base=-1/1000\n", "not a ratio of positive integers"},
		{"a time base that is not a ratio", "stream|time_base=1000\n", "not a ratio of positive integers"},
		{"a time base numerator that is not a number", "stream|time_base=a/1000\n", "not a ratio of positive integers"},
		{"a time base denominator that is not a number", "stream|time_base=1/b\n", "not a ratio of positive integers"},
		{"a line this reader does not know", "packet|pts=0|dts=0|flags=K__\nframe|pts=0\nstream|time_base=1/1000\n", "a line this reader does not know"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTimestamps(strings.NewReader(tc.in))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseTimestamps = %+v, %v; want an error containing %q", got, err, tc.want)
			}
			if errors.Is(err, ErrTimestampsUnanswered) {
				t.Errorf("a listing ffprobe printed was reported as unanswered: %v", err)
			}
			if !reflect.DeepEqual(got, Timestamps{}) {
				t.Errorf("a refused listing returned a partial answer: %+v", got)
			}
		})
	}
}

// TestParseTimestamps_ALineTooLongToReadIsUnanswered: a listing the reader could not read to
// its end is not ffprobe's answer about the file.
func TestParseTimestamps_ALineTooLongToReadIsUnanswered(t *testing.T) {
	_, err := parseTimestamps(strings.NewReader("packet|pts=" + strings.Repeat("1", 70000) + "|dts=0|flags=K__\n"))
	if !errors.Is(err, ErrTimestampsUnanswered) {
		t.Fatalf("err = %v, want it to wrap ErrTimestampsUnanswered", err)
	}
}

// ptsOf is the presentation timestamps of a listing's packets that carry one.
func ptsOf(ts Timestamps) []int64 {
	var out []int64
	for _, k := range ts.Packets {
		if k.HasPTS {
			out = append(out, k.PTS)
		}
	}
	return out
}

// TestVideoTimestamps_ReadsWhatFFmpegWrote runs the pinned ffmpeg: a Matroska file at two
// frame rates lists every frame at its own millisecond, an AVI lists decode timestamps alone,
// a raw elementary stream no timestamp at all, and an MP4 cut between keyframes flags the
// packets it keeps from before the cut to be discarded; a cover picture stored first is not
// the stream listed, and a timecode track's stream group does not stop the listing; a file with no video stream is ffprobe's own refusal, and a missing file,
// a broken ffprobe and a cancelled run are unanswered.
func TestVideoTimestamps_ReadsWhatFFmpegWrote(t *testing.T) {
	ffmpeg, err := exec.LookPath(envOrDefault("HOLDFAST_FFMPEG", "ffmpeg"))
	if err != nil {
		t.Fatalf("the pinned ffmpeg is required: %v", err)
	}
	p := realProber()
	dir := t.TempDir()
	vfr, avi, raw := filepath.Join(dir, "vfr.mkv"), filepath.Join(dir, "cfr.avi"), filepath.Join(dir, "cfr.h264")
	audio := filepath.Join(dir, "audio.mka")
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "testsrc2=duration=1:size=64x48:rate=5", "-f", "lavfi", "-i", "testsrc2=duration=1:size=64x48:rate=10",
			"-filter_complex", "[0:v][1:v]concat=n=2:v=1:a=0[v]", "-map", "[v]", "-fps_mode", "passthrough",
			"-c:v", "libx264", "-preset", "ultrafast", "--", vfr},
		{"-f", "lavfi", "-i", "testsrc2=duration=1:size=64x48:rate=5", "-c:v", "libx264", "-preset", "ultrafast", "-bf", "2", "--", avi},
		{"-f", "lavfi", "-i", "testsrc2=duration=1:size=64x48:rate=5", "-c:v", "libx264", "-preset", "ultrafast", "-f", "h264", "--", raw},
		{"-f", "lavfi", "-i", "sine=duration=1", "-c:a", "flac", "--", audio},
	} {
		if out, err := exec.Command(ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("make fixture: %v\n%s", err, out)
		}
	}
	ctx := context.Background()

	got, err := p.VideoTimestamps(ctx, vfr)
	if err != nil {
		t.Fatalf("VideoTimestamps(vfr): %v", err)
	}
	want := []int64{0, 200, 400, 600, 800, 1000, 1100, 1200, 1300, 1400, 1500, 1600, 1700, 1800, 1900}
	if got.TimeBaseNum != 1 || got.TimeBaseDen != 1000 || !slices.Equal(slices.Sorted(slices.Values(ptsOf(got))), want) ||
		!got.Packets[0].Key {
		t.Errorf("vfr = %+v, want 1/1000 presentation ticks %v from a keyframe", got, want)
	}

	got, err = p.VideoTimestamps(ctx, avi)
	if err != nil {
		t.Fatalf("VideoTimestamps(avi): %v", err)
	}
	var dts []int64
	for _, k := range got.Packets {
		if k.HasPTS || !k.HasDTS {
			t.Errorf("an AVI packet %+v carries a presentation timestamp or no decode timestamp", k)
		}
		dts = append(dts, k.DTS)
	}
	if got.TimeBaseNum != 1 || got.TimeBaseDen != 5 || !slices.Equal(dts, []int64{0, 1, 2, 3, 4}) {
		t.Errorf("avi = %+v, want the decode ticks 0..4 of 1/5", got)
	}

	got, err = p.VideoTimestamps(ctx, raw)
	if err != nil {
		t.Fatalf("VideoTimestamps(raw): %v", err)
	}
	if len(got.Packets) != 5 || slices.ContainsFunc(got.Packets, func(k Packet) bool { return k.HasPTS || k.HasDTS }) {
		t.Errorf("an elementary stream = %+v, want 5 packets with no timestamp", got)
	}

	// An MP4 cut between keyframes keeps the packets it needs from the keyframe before and
	// flags them to be discarded.
	cut := filepath.Join(dir, "cut.mp4")
	if out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-ss", "0.5", "-i", avi,
		"-c", "copy", "--", cut).CombinedOutput(); err != nil {
		t.Fatalf("make fixture: %v\n%s", err, out)
	}
	if got, err := p.VideoTimestamps(ctx, cut); err != nil || !got.Packets[0].Key || !got.Packets[0].Discard {
		t.Errorf("an MP4 cut between keyframes = %+v, %v; want its first packet a keyframe to discard", got, err)
	}

	if _, err := p.VideoTimestamps(ctx, audio); err == nil || errors.Is(err, ErrTimestampsUnanswered) {
		t.Errorf("a file with no video stream: %v, want ffprobe's own answer that it lists none", err)
	}
	if _, err := p.VideoTimestamps(ctx, filepath.Join(dir, "missing.mkv")); !errors.Is(err, ErrTimestampsUnanswered) {
		t.Errorf("a missing file: %v, want unanswered", err)
	}
	broken := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nprintf 'stream|time_base=1/1000\\npacket|pts=0|dts=0\\n'\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := New(ffmpeg, broken).VideoTimestamps(ctx, vfr); !errors.Is(err, ErrTimestampsUnanswered) {
		t.Errorf("an ffprobe that printed a listing and then failed: %v, want unanswered", err)
	}
	if _, err := New(ffmpeg, filepath.Join(dir, "no-such-ffprobe")).VideoTimestamps(ctx, vfr); !errors.Is(err, ErrTimestampsUnanswered) {
		t.Errorf("an ffprobe that could not be started: %v, want unanswered", err)
	}
	// A cover picture stored as the first video stream is never the stream timed.
	cover, covered := filepath.Join(dir, "cover.png"), filepath.Join(dir, "covered.mp4")
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "testsrc2=size=64x48:rate=1", "-frames:v", "1", "--", cover},
		{"-i", cover, "-f", "lavfi", "-i", "testsrc2=duration=1:size=64x48:rate=5", "-map", "0", "-map", "1",
			"-c:v:0", "png", "-disposition:v:0", "attached_pic", "-c:v:1", "libx264", "-preset", "ultrafast", "--", covered},
	} {
		if out, err := exec.Command(ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("make fixture: %v\n%s", err, out)
		}
	}
	if got, err := p.VideoTimestamps(ctx, covered); err != nil || len(got.Packets) != 5 {
		t.Errorf("a file whose first video stream is a cover picture = %+v, %v; want its 5-frame moving picture", got, err)
	}

	// A Matroska file carrying a timecode tag, written into MP4: the muxer links the video to a
	// timecode track, and ffprobe lists that stream group beside the stream.
	tagged, timecoded := filepath.Join(dir, "tagged.mkv"), filepath.Join(dir, "timecoded.mp4")
	for _, args := range [][]string{
		{"-f", "lavfi", "-i", "testsrc2=duration=1:size=64x48:rate=5", "-c:v", "libx264", "-preset", "ultrafast",
			"-metadata:s:v", "timecode=01:00:00:00", "--", tagged},
		{"-i", tagged, "-c", "copy", "--", timecoded},
	} {
		if out, err := exec.Command(ffmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("make fixture: %v\n%s", err, out)
		}
	}
	if got, err := p.VideoTimestamps(ctx, timecoded); err != nil || len(got.Packets) != 5 {
		t.Errorf("an MP4 whose video is linked to a timecode track = %+v, %v; want its 5 packets", got, err)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.VideoTimestamps(cancelled, vfr); !errors.Is(err, ErrTimestampsUnanswered) {
		t.Errorf("a cancelled run: %v, want unanswered", err)
	}
}
