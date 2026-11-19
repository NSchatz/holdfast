package engine

import (
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
)

// x265_open_gop (docs/design/encode-plan.md#gop). The golden argv files pin the command line
// for every encoder; these prove the declaration behind it and what libx265 does with it.

// TestGOPFor_OnlyLibx265IsToldAndOnlyWhereSet: the plan's GOP is the root's x265_open_gop on
// libx265 and nothing anywhere else, so an encoder that reads no x265 parameter is never
// declared to have been told one, and a root that sets nothing declares nothing.
func TestGOPFor_OnlyLibx265IsToldAndOnlyWhereSet(t *testing.T) {
	no, yes := false, true
	for _, c := range []struct {
		key  string
		v    *bool
		want GOP
		frag string
	}{
		{"cpu", nil, GOP{}, ""},
		{"cpu", &no, GOP{Set: true, Open: false}, ":open-gop=0"},
		{"cpu", &yes, GOP{Set: true, Open: true}, ":open-gop=1"},
		{"svtav1", &no, GOP{}, ""},
		{"x264", &yes, GOP{}, ""},
		{"nvenc", &no, GOP{}, ""},
		{"vaapi", &yes, GOP{}, ""},
	} {
		got := gopFor(mustLookup(c.key), config.Profile{X265OpenGOP: c.v})
		if got != c.want {
			t.Errorf("gopFor(%s, %v) = %+v, want %+v", c.key, show265(c.v), got, c.want)
		}
		if f := got.X265Params(); f != c.frag {
			t.Errorf("gopFor(%s, %v).X265Params() = %q, want %q", c.key, show265(c.v), f, c.frag)
		}
	}
}

func show265(v *bool) string {
	if v == nil {
		return "unset"
	}
	return strconv.FormatBool(*v)
}

// TestX265OpenGOP_AForgedGOPIsRefused: a plan whose GOP is not what its derivation declares is
// refused before any command line is built, so a GOP nobody configured never reaches an encode.
func TestX265OpenGOP_AForgedGOPIsRefused(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	src := filepath.Join(t.TempDir(), "movie.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p", "--", src)
	prober := probe.New(ffmpeg, ffprobe)
	no := false
	for _, c := range []struct {
		name  string
		key   string
		prof  config.Profile
		forge GOP
	}{
		{"unset/told-closed", "cpu", config.Profile{}, GOP{Set: true}},
		{"closed/told-open", "cpu", config.Profile{X265OpenGOP: &no}, GOP{Set: true, Open: true}},
		{"closed/told-nothing", "cpu", config.Profile{X265OpenGOP: &no}, GOP{}},
		{"svtav1/told-closed", "svtav1", config.Profile{X265OpenGOP: &no}, GOP{Set: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, err := deriveEncodePlan(planInputs{
				settings: config.Transcode{Encoder: c.key, CRF: 22, Preset: "slow", PixelFormat: "auto"},
				prof:     c.prof, source: src, output: filepath.Join(t.TempDir(), "out.mkv"),
				snapshot: func() (*probe.VideoProps, error) { return prober.VideoProps(context.Background(), src), nil },
			})
			if err != nil {
				t.Fatalf("derive: %v", err)
			}
			if _, _, err := p.args(encoder.X265Parallelism{}); err != nil {
				t.Fatalf("the derived plan does not build: %v", err)
			}
			p.Video.GOP = c.forge
			var unbuildable *UnbuildablePlanError
			if _, _, err := p.args(encoder.X265Parallelism{}); err == nil || !asUnbuildable(err, &unbuildable) ||
				!strings.Contains(err.Error(), "GOP") {
				t.Errorf("a forged GOP built: %v", err)
			}
		})
	}
}

// TestX265OpenGOP_ReachesTheBitstream encodes one source with the real pinned libx265 under
// each state of the key and reads the NAL unit types the output carries. A source longer than
// libx265's default keyframe interval gets a second keyframe; under open GOPs (libx265's own
// default, so also the unset key's) that keyframe is a CRA with RASL pictures leading it, the
// pictures a player or segmenter that starts decoding there must drop. Under x265_open_gop:
// false it is an IDR with none. This is the proof the key does what the documentation
// recommends it for; the command line alone would pass on a parameter libx265 ignored.
func TestX265OpenGOP_ReachesTheBitstream(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	src := filepath.Join(t.TempDir(), "movie.mkv")
	// 12 s at 25 fps is 300 frames, past libx265's default keyint of 250.
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=12:size=320x240:rate=25", "-c:v", "libx264", "-preset", "ultrafast",
		"-pix_fmt", "yuv420p", "--", src)
	prober := probe.New(ffmpeg, ffprobe)
	no, yes := false, true
	for _, c := range []struct {
		name     string
		v        *bool
		wantOpen bool
	}{
		{"unset", nil, true},
		{"false", &no, false},
		{"true", &yes, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := config.Config{Encoder: "cpu", CRF: 30, Preset: "ultrafast", PixelFormat: "yuv420p",
				ContainerExt: "source", X265OpenGOP: c.v}
			out := filepath.Join(t.TempDir(), "out.mkv")
			enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}.ForProfile(cfg.TopLevelProfile())
			if err := enc.(ProgressEncoder).EncodeWithProgress(context.Background(), src, out, nil, nil); err != nil {
				t.Fatalf("encode: %v", err)
			}
			nal := hevcNALTypes(t, ffmpeg, out)
			cra, rasl := nal[21], nal[8]+nal[9]
			idr := nal[19] + nal[20]
			if c.wantOpen {
				if cra == 0 || rasl == 0 {
					t.Errorf("open GOP: %d CRA and %d RASL picture(s), want both (NAL types %v)", cra, rasl, nal)
				}
				return
			}
			if cra != 0 || rasl != 0 {
				t.Errorf("x265_open_gop: false carried %d CRA and %d RASL picture(s), want none (NAL types %v)", cra, rasl, nal)
			}
			if idr < 2 {
				t.Errorf("x265_open_gop: false carried %d IDR picture(s), want the first and the one at the "+
					"keyframe interval (NAL types %v)", idr, nal)
			}
		})
	}
}

var nalTypeLine = regexp.MustCompile(`nal_unit_type\s+\d+\s+=\s+(\d+)`)

// hevcNALTypes counts the NAL unit types of path's HEVC video, read with the trace_headers
// bitstream filter.
func hevcNALTypes(t *testing.T, ffmpeg, path string) map[int]int {
	t.Helper()
	out, err := exec.Command(ffmpeg, "-hide_banner", "-i", path, "-map", "0:v:0", "-c", "copy",
		"-bsf:v", "trace_headers", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("trace_headers %s: %v\n%s", path, err, out)
	}
	counts := map[int]int{}
	for _, m := range nalTypeLine.FindAllStringSubmatch(string(out), -1) {
		n, _ := strconv.Atoi(m[1])
		counts[n]++
	}
	if len(counts) == 0 {
		t.Fatalf("trace_headers printed no NAL unit type for %s", path)
	}
	return counts
}
