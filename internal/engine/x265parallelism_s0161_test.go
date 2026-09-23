package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/cpuquota"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
)

// cgroupFixture builds a cgroup mount out of files. The cgroup filesystem is outside the
// boundary of what is under test; the reading and the argv built from it run for real.
func cgroupFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// notTheHostCount is a whole-CPU quota unequal to this host's logical CPU count, so a
// figure read from the quota and one read from the host cannot agree by accident.
func notTheHostCount() int {
	if runtime.NumCPU() == 3 {
		return 2
	}
	return 3
}

// argValue is the value following flag in args, and "" where flag is absent.
func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

// encodeArgv runs the real production encoder once and returns the argv it built.
func encodeArgv(t *testing.T, enc FFmpegEncoder, src, out string) ([]string, error) {
	t.Helper()
	var argv []string
	enc.argvObserver = func(args []string) { argv = append([]string(nil), args...) }
	err := enc.Encode(context.Background(), src, out, nil)
	if argv == nil {
		t.Fatalf("no argv was built for %s (Encode: %v)", src, err)
	}
	return argv, err
}

// x265Report replays a production argv with libx265's and ffmpeg's logging raised to
// info, and returns what libx265 itself says it built: the thread count of the pool it
// created and the frame threads it runs, plus every line in which ffmpeg's libx265
// wrapper refused a parameter. That wrapper only WARNS on an unknown -x265-params key, so
// a clean exit alone would not prove the key was understood.
func x265Report(t *testing.T, ffmpeg string, argv []string) (pool, frames int, complaints []string) {
	t.Helper()
	replay := append([]string(nil), argv...)
	for i := 0; i+1 < len(replay); i++ {
		switch replay[i] {
		case "-loglevel":
			replay[i+1] = "info"
		case "-x265-params":
			replay[i+1] = strings.Replace(replay[i+1], "log-level=error", "log-level=info", 1)
		}
	}
	replay[len(replay)-1] = filepath.Join(t.TempDir(), "replay"+filepath.Ext(replay[len(replay)-1]))
	out, err := exec.Command(ffmpeg, replay...).CombinedOutput()
	if err != nil {
		t.Fatalf("replaying the production argv at info level failed: %v\n%s", err, out)
	}
	pool, frames = -1, -1
	for _, line := range strings.Split(string(out), "\n") {
		if _, err := fmt.Sscanf(strings.TrimSpace(line), "x265 [info]: Thread pool created using %d threads", &pool); err == nil {
			continue
		}
		if _, rest, ok := strings.Cut(line, "frame threads / pool features"); ok {
			if _, v, ok := strings.Cut(rest, ":"); ok {
				_, _ = fmt.Sscanf(strings.TrimSpace(v), "%d", &frames)
			}
		}
		if strings.Contains(line, "Unknown option") || strings.Contains(line, "Invalid value") {
			complaints = append(complaints, line)
		}
	}
	return pool, frames, complaints
}

// TestS0161_AC1_TheFigureIsTheQuotaRoundedDownAndNeverTheHostCPUCount grades AC-1's
// derivation: quota over period, rounded down, floored at 1, from either cgroup layout,
// and a figure that follows the quota where the host's own CPU count differs from it.
func TestS0161_AC1_TheFigureIsTheQuotaRoundedDownAndNeverTheHostCPUCount(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  int
	}{
		{"v2 the measured 24", map[string]string{"cpu.max": "2400000 100000\n"}, 24},
		{"v2 7.5 CPUs rounds down", map[string]string{"cpu.max": "750000 100000\n"}, 7},
		{"v2 just under 2 rounds down", map[string]string{"cpu.max": "199999 100000\n"}, 1},
		{"v2 half a CPU floors at 1", map[string]string{"cpu.max": "50000 100000\n"}, 1},
		{"v2 a shorter period", map[string]string{"cpu.max": "150000 50000\n"}, 3},
		{"v1 3.5 CPUs", map[string]string{"cpu/cpu.cfs_quota_us": "350000", "cpu/cpu.cfs_period_us": "100000"}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := DeriveX265(0, cgroupFixture(t, tc.files))
			if plan.Err != nil {
				t.Fatalf("DeriveX265: %v", plan.Err)
			}
			if plan.Source != X265FromCgroup {
				t.Errorf("source = %q, want %q", plan.Source, X265FromCgroup)
			}
			if want := encoder.X265ParallelismFor(tc.want); plan.Parallelism != want {
				t.Errorf("parallelism = %+v, want %+v", plan.Parallelism, want)
			}
		})
	}

	n := notTheHostCount()
	plan := DeriveX265(0, quotaRoot(t, n))
	if plan.Parallelism.CPUs != n || plan.Parallelism.Pools != n {
		t.Errorf("a quota of %d CPUs on a %d-CPU host derived %+v: the figure must come from the quota",
			n, runtime.NumCPU(), plan.Parallelism)
	}
}

// TestS0161_AC1_EveryLibx265EncodeOfTheRunCarriesTheFigure: one oneshot run over two
// files, and the argv the production encoder built for EACH of them carries the figure
// derived from the quota rather than anything sized from the host.
func TestS0161_AC1_EveryLibx265EncodeOfTheRunCarriesTheFigure(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	n := notTheHostCount()
	plan := DeriveX265(0, quotaRoot(t, n))

	root := filepath.Join(t.TempDir(), "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	srcs := []string{filepath.Join(root, "a.mkv"), filepath.Join(root, "b.mkv")}
	for _, s := range srcs {
		mkH264(t, ffmpeg, s, "8M")
	}
	cfg := profileCfg(t, "library_roots:\n  - "+root+"\nencoder: cpu\npreset: ultrafast\n"+
		"pixel_format: yuv420p10le\ncontainer_ext: mkv\nvmaf_enable: false\nmin_bitrate_kbps: 0\n")

	log := newArgvLog()
	prober := probe.New(ffmpeg, ffprobe)
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober, X265: plan.Parallelism, argvObserver: log.record}
	if err := New(cfg, prober, enc, newTestStore(t, root), discardLogger()).RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	want := fmt.Sprintf("log-level=error:pools=%d:frame-threads=1", n)
	for _, s := range srcs {
		if got := argValue(log.forSource(t, s), "-x265-params"); got != want {
			t.Errorf("the encode of %s carried -x265-params %q, want %q", filepath.Base(s), got, want)
		}
	}
}

// TestS0161_AC2_X265BuildsThePoolAndFrameCountItWasGiven: the figures reach the argv as a
// worker-pool size and a frame-thread count, and libx265 itself - asked on the same argv -
// builds exactly that pool and runs exactly that many frame threads, with no parameter
// refused. An out-of-range frame count fails the encode outright, so a clean encode here is
// also libx265 accepting the count.
func TestS0161_AC2_X265BuildsThePoolAndFrameCountItWasGiven(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	for _, cpus := range []int{1, 5, 24, 64} {
		t.Run(fmt.Sprintf("%d CPUs", cpus), func(t *testing.T) {
			p := encoder.X265ParallelismFor(cpus)
			enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: baseCfg(d), Probe: probe.New(ffmpeg, ffprobe), X265: p}
			argv, err := encodeArgv(t, enc, src, filepath.Join(d, fmt.Sprintf("out-%d.mkv", cpus)))
			if err != nil {
				t.Fatalf("Encode with %+v: %v", p, err)
			}
			if got, want := argValue(argv, "-x265-params"), "log-level=error"+p.Params(); got != want {
				t.Errorf("-x265-params = %q, want %q", got, want)
			}
			if p.FrameThreads < 0 || p.FrameThreads > 16 {
				t.Errorf("frame-threads=%d is outside the 0..16 libx265 accepts", p.FrameThreads)
			}
			pool, frames, complaints := x265Report(t, ffmpeg, argv)
			if pool != p.Pools {
				t.Errorf("libx265 built a pool of %d threads, want %d", pool, p.Pools)
			}
			if frames != p.FrameThreads {
				t.Errorf("libx265 runs %d frame threads, want %d", frames, p.FrameThreads)
			}
			if len(complaints) > 0 {
				t.Errorf("libx265 refused a parameter:\n%s", strings.Join(complaints, "\n"))
			}
		})
	}
}

// TestS0161_AC2_TheQuotadArgvIsPinned is the quota'd case pinned as a whole command line,
// separately from the unquota'd one AC-5 grades, so neither reference is edited to fit the
// other.
func TestS0161_AC2_TheQuotadArgvIsPinned(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	out := filepath.Join(d, "out.mkv")
	plan := DeriveX265(0, quotaRoot(t, 3))
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: baseCfg(d), Probe: probe.New(ffmpeg, ffprobe), X265: plan.Parallelism}
	argv, err := encodeArgv(t, enc, src, out)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := []string{
		"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-i", src,
		"-map", "0", "-map", "-0:d?",
		"-c", "copy", "-c:v", "libx265",
		"-pix_fmt", "yuv420p10le",
		"-color_range", "tv",
		"-fps_mode", "passthrough",
		"-preset", "ultrafast",
		"-crf", "22",
		"-x265-params", "log-level=error:pools=3:frame-threads=1",
		"--", out,
	}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("the quota'd argv changed.\n got: %v\nwant: %v", argv, want)
	}
}

// TestS0161_AC3_TheBitrateTargetedPathCarriesTheSameFigure: a bitrate-targeted encode
// carries the same -x265-params as a quality-targeted one, and libx265 builds the same
// pool for it rather than one sized from the host.
func TestS0161_AC3_TheBitrateTargetedPathCarriesTheSameFigure(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	p := encoder.X265ParallelismFor(notTheHostCount())

	quality := baseCfg(d)
	bitrate := baseCfg(d)
	bitrate.BitrateKbps = 1500
	qArgv, err := encodeArgv(t, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: quality, Probe: probe.New(ffmpeg, ffprobe), X265: p},
		src, filepath.Join(d, "q.mkv"))
	if err != nil {
		t.Fatalf("quality-targeted Encode: %v", err)
	}
	bArgv, err := encodeArgv(t, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: bitrate, Probe: probe.New(ffmpeg, ffprobe), X265: p},
		src, filepath.Join(d, "b.mkv"))
	if err != nil {
		t.Fatalf("bitrate-targeted Encode: %v", err)
	}
	// Anti-vacuity: the second encode really took the bitrate-targeted path.
	if argValue(bArgv, "-b:v") != "1500k" || argValue(bArgv, "-crf") != "" {
		t.Fatalf("the bitrate-targeted argv is not the bitrate path: %v", bArgv)
	}
	want := "log-level=error" + p.Params()
	if got := argValue(bArgv, "-x265-params"); got != want || argValue(qArgv, "-x265-params") != want {
		t.Errorf("-x265-params: bitrate path %q, quality path %q, want both %q",
			got, argValue(qArgv, "-x265-params"), want)
	}
	if pool, frames, _ := x265Report(t, ffmpeg, bArgv); pool != p.Pools || frames != p.FrameThreads {
		t.Errorf("the bitrate-targeted encode built a pool of %d and %d frame threads, want %d and %d",
			pool, frames, p.Pools, p.FrameThreads)
	}
}

// TestS0161_AC4_NoOtherEncodersArgvMovesWithTheFigure: for every encoder family that is
// not libx265, on both the quality-targeted and the bitrate-targeted path, the argv built
// with a parallelism figure is byte-identical to the one built without it - which is the
// argv the unedited fixtures of this package pin. The hardware encoders fail to run on a
// host without their device; their argv is observed before the subprocess starts, which
// is all this criterion is about.
func TestS0161_AC4_NoOtherEncodersArgvMovesWithTheFigure(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	p := encoder.X265ParallelismFor(24)
	checked := 0
	for _, key := range encoder.Known() {
		if key == "cpu" {
			continue
		}
		for _, kbps := range []int{0, 1500} {
			t.Run(fmt.Sprintf("%s at %d kbps", key, kbps), func(t *testing.T) {
				cfg := baseCfg(d)
				cfg.Encoder = key
				cfg.BitrateKbps = kbps
				out := filepath.Join(d, fmt.Sprintf("%s-%d.mkv", key, kbps))
				bare := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: probe.New(ffmpeg, ffprobe)}
				sized := bare
				sized.X265 = p
				without, _ := encodeArgv(t, bare, src, out)
				with, _ := encodeArgv(t, sized, src, out)
				if strings.Join(with, "\x00") != strings.Join(without, "\x00") {
					t.Errorf("the %s argv moved with the libx265 figure.\nwith:    %v\nwithout: %v", key, with, without)
				}
				if strings.Contains(strings.Join(with, " "), "pools=") {
					t.Errorf("the %s argv carries a pool instruction: %v", key, with)
				}
				checked++
			})
		}
	}
	if checked < 10 {
		t.Errorf("only %d non-libx265 argv lists were compared; every family on both paths must be", checked)
	}
}

// TestS0161_AC5_NoQuotaLeavesTheLibx265ArgvAtThePinnedBytes: a cgroup that names no limit,
// under either layout, and a host with no bandwidth interface at all, derive no figure and
// no warning, and the libx265 argv is the command line pinned at b7c26ca - the one
// TestEncode_SingleVideoStreamArgvUnchanged holds, restated here rather than read back from
// the changed builder.
func TestS0161_AC5_NoQuotaLeavesTheLibx265ArgvAtThePinnedBytes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "3M")
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"v2 max", map[string]string{"cpu.max": "max 100000\n"}},
		{"v1 -1", map[string]string{"cpu/cpu.cfs_quota_us": "-1", "cpu/cpu.cfs_period_us": "100000"}},
		{"no interface", map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := DeriveX265(0, cgroupFixture(t, tc.files))
			if plan.Source != X265NoQuota || plan.Err != nil || plan.Parallelism.Set() {
				t.Fatalf("plan = %+v, want no figure, no error and source %q", plan, X265NoQuota)
			}
			out := filepath.Join(d, "out.mkv")
			enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: baseCfg(d), Probe: probe.New(ffmpeg, ffprobe), X265: plan.Parallelism}
			argv, err := encodeArgv(t, enc, src, out)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			want := []string{
				"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
				"-i", src,
				"-map", "0", "-map", "-0:d?",
				"-c", "copy", "-c:v", "libx265",
				"-pix_fmt", "yuv420p10le",
				"-color_range", "tv",
				"-fps_mode", "passthrough",
				"-preset", "ultrafast",
				"-crf", "22",
				"-x265-params", "log-level=error",
				"--", out,
			}
			if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
				t.Errorf("the no-quota argv is not the pinned one.\n got: %v\nwant: %v", argv, want)
			}
		})
	}
}

// TestS0161_AC6_ABrokenInterfaceDerivesNoFigureAndNamesTheFile is the reading half of
// AC-6: a bandwidth file that exists and cannot be read or parsed derives no figure, so the
// encoder keeps its own default, and the plan carries an error naming that file - which is
// what the run's warn is made from. The record itself is graded in cmd/holdfast.
func TestS0161_AC6_ABrokenInterfaceDerivesNoFigureAndNamesTheFile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"malformed", map[string]string{"cpu.max": "garbage\n"}},
		{"unreadable", map[string]string{"cpu.max/x": "a directory where the file should be"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := cgroupFixture(t, tc.files)
			plan := DeriveX265(0, root)
			if plan.Err == nil {
				t.Fatalf("plan = %+v: a broken interface must be carried as an error", plan)
			}
			if plan.Source != X265NoQuota || plan.Parallelism.Set() {
				t.Errorf("plan = %+v: a broken interface derives no figure", plan)
			}
			var ie *cpuquota.InterfaceError
			if !errors.As(plan.Err, &ie) || ie.Path != filepath.Join(root, "cpu.max") {
				t.Errorf("the error does not name %s: %v", filepath.Join(root, "cpu.max"), plan.Err)
			}
		})
	}
}
