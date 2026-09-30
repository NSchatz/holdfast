package probe

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	streamFlat = `streams.stream.0.pix_fmt="yuv420p10le"
streams.stream.0.color_range="tv"
streams.stream.0.color_space="bt2020nc"
streams.stream.0.color_transfer="unknown"
streams.stream.0.color_primaries="reserved"
streams.stream.0.side_data_list.side_data.0.side_data_type="Content light level metadata"
streams.stream.0.side_data_list.side_data.0.max_content=1000
`
	frameFlat = `frames.frame.0.color_range="tv"
frames.frame.0.color_space="bt2020nc"
frames.frame.0.color_primaries="bt2020"
frames.frame.0.color_transfer="smpte2084"
frames.frame.0.side_data_list.side_data.0.side_data_type="Mastering display metadata"
frames.frame.1.color_primaries="bt709"
`
)

func TestOutputFactsFrom(t *testing.T) {
	got := outputFactsFrom(streamFlat, frameFlat)
	want := OutputFacts{
		PixFmt:   "yuv420p10le",
		Stream:   Colors{Matrix: "bt2020nc", Range: "tv"},
		Frame:    Colors{Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv"},
		SideData: frameFlat + "\n" + streamFlat,
	}
	if got != want {
		t.Fatalf("outputFactsFrom =\n%+v\nwant\n%+v", got, want)
	}
	if empty := outputFactsFrom("", ""); empty != (OutputFacts{SideData: "\n"}) {
		t.Fatalf("outputFactsFrom of nothing = %+v", empty)
	}
}

func TestFlatFields(t *testing.T) {
	out := "streams.stream.0.a=\"1\"\nstreams.stream.0.b=2\nstreams.stream.0.a=\"9\"\n" +
		"streams.stream.0.side_data_list.side_data.0.c=3\nstreams.stream.1.d=4\nnoise\nstreams.stream.0.e\n"
	got := flatFields(out, "streams.stream.0.")
	if len(got) != 2 || got["a"] != "1" || got["b"] != "2" {
		t.Fatalf("flatFields = %v, want map[a:1 b:2]", got)
	}
}

// TestOutputFacts_ReadsBothLevels runs the pinned ffmpeg: an HEVC stream whose VUI says
// bt2020/PQ (written by libx265's own parameters), in Matroska written straight from a
// generated source whose frames carry no primaries or transfer - so, on the pinned ffmpeg,
// the container leaves those two unset - reads the VUI's tags at the frame level while the
// stream level reports none. That disagreement is why the gate reads both. An unreadable
// file yields no facts at all.
func TestOutputFacts_ReadsBothLevels(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Fatalf("the pinned ffmpeg is required: %v", err)
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Fatalf("the pinned ffprobe is required: %v", err)
	}
	f := filepath.Join(t.TempDir(), "pq.mkv")
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=64x48:rate=5", "-pix_fmt", "yuv420p10le",
		"-color_primaries", "bt2020", "-color_trc", "smpte2084", "-colorspace", "bt2020nc", "-color_range", "tv",
		"-c:v", "libx265", "-x265-params",
		"log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:max-cll=1000,400",
		"--", f).CombinedOutput()
	if err != nil {
		t.Fatalf("make fixture: %v\n%s", err, out)
	}
	p := New(ffmpeg, ffprobe)
	got := p.OutputFacts(context.Background(), f)
	if got.PixFmt != "yuv420p10le" {
		t.Errorf("PixFmt = %q", got.PixFmt)
	}
	if want := (Colors{Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv"}); got.Frame != want {
		t.Errorf("Frame = %+v, want %+v", got.Frame, want)
	}
	if want := (Colors{Matrix: "bt2020nc", Range: "tv"}); got.Stream != want {
		t.Errorf("Stream = %+v, want %+v (the fixture no longer shows the two levels disagreeing)", got.Stream, want)
	}
	if !strings.Contains(got.SideData, `side_data_type="Content light level metadata"`) {
		t.Errorf("SideData carries no content light level block: %q", got.SideData)
	}
	if none := p.OutputFacts(context.Background(), filepath.Join(t.TempDir(), "missing.mkv")); none != (OutputFacts{SideData: "\n"}) {
		t.Errorf("OutputFacts of a missing file = %+v, want nothing", none)
	}
}
