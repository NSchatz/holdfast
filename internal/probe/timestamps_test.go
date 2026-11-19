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
	for _, tc := range []struct {
		name, in string
		want     Timestamps
	}{
		{"every packet timed, in decode order", "packet|pts=0|dts=-83\npacket|pts=125|dts=-42\npacket|pts=42|dts=0\nstream|time_base=1/1000\n",
			Timestamps{TimeBaseNum: 1, TimeBaseDen: 1000, Packets: 3, Ticks: []int64{0, 125, 42}}},
		{"a transport stream's program listing is skipped", "packet|pts=133500|dts=126000\nprogram|stream|time_base=1/90000\nstream|time_base=1/90000\n",
			Timestamps{TimeBaseNum: 1, TimeBaseDen: 90000, Packets: 1, Ticks: []int64{133500}}},
		{"the stream line comes first, with an empty trailing section, between blank lines", "\nstream|time_base=1001/24000|\n\npacket|pts=7|dts=N/A\n",
			Timestamps{TimeBaseNum: 1001, TimeBaseDen: 24000, Packets: 1, Ticks: []int64{7}}},
		{"no packet timed for presentation: the decode timestamps", "packet|pts=N/A|dts=0\npacket|pts=N/A|dts=25\nstream|time_base=1/600\n",
			Timestamps{TimeBaseNum: 1, TimeBaseDen: 600, Packets: 2, Ticks: []int64{0, 25}, FromDTS: true}},
		{"no packet timed at all: an elementary stream", "packet|pts=N/A|dts=N/A\npacket|pts=N/A|dts=N/A\nstream|time_base=1/1200000\n",
			Timestamps{TimeBaseNum: 1, TimeBaseDen: 1200000, Packets: 2, Untimed: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTimestamps(strings.NewReader(tc.in))
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseTimestamps = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}

	for _, tc := range []struct{ name, in, want string }{
		{"only some packets timed for presentation", "packet|pts=0|dts=0\npacket|pts=N/A|dts=1\npacket|pts=N/A|dts=2\nstream|time_base=1/1000\n",
			"2 of the video stream's 3 packets carry no presentation timestamp"},
		{"a packet timed by neither", "packet|pts=N/A|dts=0\npacket|pts=N/A|dts=N/A\nstream|time_base=1/1000\n",
			"1 of the video stream's 2 packets carry neither a presentation nor a decode timestamp"},
		{"no time base", "packet|pts=0|dts=0\n", "names no video stream time base"},
		{"no packets", "stream|time_base=1/1000\n", "carries no packets"},
		{"two streams", "stream|time_base=1/1000\nstream|time_base=1/1000\npacket|pts=0|dts=0\n", "more than one video stream"},
		{"a pts that is not a number", "packet|pts=1.5|dts=0\nstream|time_base=1/1000\n", `a packet's pts is not a timestamp: "1.5"`},
		{"a dts that is not a number", "packet|pts=0|dts=x\nstream|time_base=1/1000\n", `a packet's dts is not a timestamp: "x"`},
		{"a packet without its pts", "packet|dts=0\nstream|time_base=1/1000\n", "a packet line carries no pts"},
		{"a packet without its dts", "packet|pts=0\nstream|time_base=1/1000\n", "a packet line carries no dts"},
		{"a stream line without a time base", "stream|codec_name=h264\npacket|pts=0|dts=0\n", "the stream line carries no time base"},
		{"a time base of zero ticks", "stream|time_base=0/1000\n", "not a ratio of positive integers"},
		{"a time base over zero", "stream|time_base=1/0\n", "not a ratio of positive integers"},
		{"a negative time base", "stream|time_base=-1/1000\n", "not a ratio of positive integers"},
		{"a time base that is not a ratio", "stream|time_base=1000\n", "not a ratio of positive integers"},
		{"a time base numerator that is not a number", "stream|time_base=a/1000\n", "not a ratio of positive integers"},
		{"a time base denominator that is not a number", "stream|time_base=1/b\n", "not a ratio of positive integers"},
		{"a line this reader does not know", "packet|pts=0|dts=0\nframe|pts=0\nstream|time_base=1/1000\n", "a line this reader does not know"},
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
	_, err := parseTimestamps(strings.NewReader("packet|pts=" + strings.Repeat("1", 70000) + "|dts=0\n"))
	if !errors.Is(err, ErrTimestampsUnanswered) {
		t.Fatalf("err = %v, want it to wrap ErrTimestampsUnanswered", err)
	}
}

// TestVideoTimestamps_ReadsWhatFFmpegWrote runs the pinned ffmpeg: a Matroska file at two
// frame rates lists every frame at its own millisecond, an AVI (which stores no presentation
// timestamp) is read by its decode timestamps, a raw elementary stream (which stores none at
// all) is untimed, a file with no video stream is ffprobe's own refusal, and a missing file, a broken ffprobe and a cancelled run are unanswered.
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
	if got.TimeBaseNum != 1 || got.TimeBaseDen != 1000 || got.FromDTS || got.Untimed || got.Packets != len(want) ||
		!slices.Equal(slices.Sorted(slices.Values(got.Ticks)), want) {
		t.Errorf("vfr = %+v, want 1/1000 presentation ticks %v", got, want)
	}

	got, err = p.VideoTimestamps(ctx, avi)
	if err != nil {
		t.Fatalf("VideoTimestamps(avi): %v", err)
	}
	if !got.FromDTS || got.TimeBaseNum != 1 || got.TimeBaseDen != 5 || got.Packets != 5 ||
		!slices.Equal(got.Ticks, []int64{0, 1, 2, 3, 4}) {
		t.Errorf("avi = %+v, want the decode ticks 0..4 of 1/5", got)
	}

	got, err = p.VideoTimestamps(ctx, raw)
	if err != nil {
		t.Fatalf("VideoTimestamps(raw): %v", err)
	}
	if !got.Untimed || got.Packets != 5 || len(got.Ticks) != 0 {
		t.Errorf("an elementary stream = %+v, want 5 untimed packets", got)
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
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := p.VideoTimestamps(cancelled, vfr); !errors.Is(err, ErrTimestampsUnanswered) {
		t.Errorf("a cancelled run: %v, want unanswered", err)
	}
}
