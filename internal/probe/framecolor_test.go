package probe

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestParseFrameColors(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want Colors
		ok   bool
	}{
		{"all four", "color_range=tv\ncolor_space=bt2020nc\ncolor_primaries=bt2020\ncolor_transfer=smpte2084\n",
			Colors{Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv"}, true},
		{"non-values normalised", "color_range=unknown\ncolor_space=reserved\ncolor_primaries=N/A\ncolor_transfer=\n",
			Colors{}, true},
		{"first frame wins", "color_range=tv\ncolor_primaries=bt709\ncolor_range=pc\ncolor_primaries=bt2020\n",
			Colors{Primaries: "bt709", Range: "tv"}, true},
		{"other keys and junk ignored", "side_data_type=x\nnot a pair\n  color_space=bt709  \n",
			Colors{Matrix: "bt709"}, true},
		{"nothing printed", "", Colors{}, false},
		{"no colour key", "pix_fmt=yuv420p\n", Colors{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseFrameColors(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("parseFrameColors = %+v, %v; want %+v, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestFirstFrameColors_ReadsTheBitstreamNotTheContainer runs the pinned ffmpeg: an HEVC
// stream whose VUI says bt2020/PQ (written by libx265's own parameters), in Matroska written
// straight from a generated source whose frames carry no primaries or transfer - so the
// container's colour elements for those two are left unset, because the pinned ffmpeg takes
// them from the frames and not from -color_primaries/-color_trc - reads back the VUI's tags
// per frame while the stream-level fields report none; an unreadable file is not ok. The gate
// reads both levels because they disagree exactly like this.
func TestFirstFrameColors_ReadsTheBitstreamNotTheContainer(t *testing.T) {
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
		"-c:v", "libx265", "-x265-params", "log-level=error:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc",
		"--", f).CombinedOutput()
	if err != nil {
		t.Fatalf("make fixture: %v\n%s", err, out)
	}
	p := New(ffmpeg, ffprobe)
	ctx := context.Background()
	got, ok := p.FirstFrameColors(ctx, f)
	want := Colors{Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv"}
	if !ok || got != want {
		t.Fatalf("FirstFrameColors = %+v, %v; want %+v, true", got, ok, want)
	}
	if prim := p.ColorField(ctx, f, "color_primaries"); prim != "" {
		t.Fatalf("the stream-level primaries are %q: the fixture no longer shows the two levels disagreeing", prim)
	}
	if _, ok := p.FirstFrameColors(ctx, filepath.Join(t.TempDir(), "missing.mkv")); ok {
		t.Fatal("FirstFrameColors on a missing file reported ok")
	}
}
