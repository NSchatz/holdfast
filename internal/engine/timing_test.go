package engine

// The timestamp half of the length gate (docs/design/swap.md#timing-parity) and the encode
// argument it holds (docs/design/encode-plan.md#timestamps), on real encodes with the pinned
// ffmpeg. The source is variable-frame-rate: an encode that rounds its frames onto one rate
// passes the duration, packet-count, decode and perceptual gates exactly as a faithful one
// does, so these cases are the only ones that can see it.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/timing"
)

// timingCase is one whole pass of the timing cases: a source in container from, written into
// container to (container_ext: source where they are the same), by the encoder named, from the
// source codec given (the fixture's H.264 where nil).
type timingCase struct {
	from, to, encoder string
	codec             []string
}

// timingCases are the containers holdfast writes, each with a source it encodes: Matroska, MP4,
// MOV and FLV written back into themselves, WebM (VP9 into AV1, the codecs it carries), MP4
// into Matroska, and MPEG-TS and M2TS written from Matroska. A transport stream is only ever an
// output here because no pass encodes one as a source today: the video-stream probe
// (probe.VideoStreams) cannot read the program listing ffprobe adds for a transport stream, so
// its guard skips the file and the encoder refuses it before a command line exists. AVI and
// ASF, which store no presentation timestamp, have their own case below.
var timingCases = []timingCase{
	{"mkv", "mkv", "cpu", nil}, {"mp4", "mp4", "cpu", nil}, {"mov", "mov", "cpu", nil}, {"flv", "flv", "cpu", nil},
	{"webm", "webm", "svtav1", []string{"-c:v", "libvpx-vp9", "-deadline", "realtime", "-cpu-used", "8", "-b:v", "2M"}},
	{"mp4", "mkv", "cpu", nil}, {"mkv", "ts", "cpu", nil}, {"mkv", "m2ts", "cpu", nil},
}

// configure is the case's configuration change: its encoder, its container and its source's
// extension among the scanned ones.
func (tc timingCase) configure(c *config.Config) {
	c.Encoder, c.ContainerExt = tc.encoder, "source"
	if tc.to != tc.from {
		c.ContainerExt = tc.to
	}
	c.VideoExts = []string{tc.from}
}

func (tc timingCase) String() string { return tc.from + "-to-" + tc.to + "-" + tc.encoder }

// mkVFR writes the variable-frame-rate source every timing case starts from: H.264 at 6 s of
// 24 fps, then 1 s of 60 fps and 1 s of 30 fps, 234 frames, in the container path's extension
// names. The long 24 fps lead is what makes the demuxer of every container in
// timingCases guess the stream at 24 fps, the rate the encoder's time base defaulted to
// before -enc_time_base: on that grid the 60 and 30 fps frames cannot all be placed, and some
// collide.
//
// codec replaces the H.264 encode where a case needs another source codec.
func mkVFR(t *testing.T, ffmpeg, path string, codec ...string) {
	t.Helper()
	if len(codec) == 0 {
		codec = []string{"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p"}
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=6",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=60:duration=1",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=30:duration=1",
		"-filter_complex", "[0:v][1:v][2:v]concat=n=3:v=1:a=0[v]", "-map", "[v]"}
	args = append(append(args, codec...), "-fps_mode", "passthrough", "--", path)
	ff(t, ffmpeg, args...)
}

// vfrFixture writes the source into a fresh root and checks it is the fixture the cases need:
// guessed at 24 fps, and presented at three rates, so its timestamps do not sit on that grid.
func vfrFixture(t *testing.T, ffmpeg, ffprobe, ext string, codec ...string) (root, src string, times probe.Timestamps) {
	t.Helper()
	root = t.TempDir()
	src = filepath.Join(root, "movie."+ext)
	mkVFR(t, ffmpeg, src, codec...)
	if rate := rFrameRate(t, ffprobe, src); rate != "24/1" {
		t.Fatalf("fixture drifted: the %s source's guessed rate is %q, want 24/1 - the cases below would not "+
			"exercise the grid an encode rounded onto", ext, rate)
	}
	times = timesOf(t, ffprobe, src)
	if len(times.Packets) != 234 {
		t.Fatalf("fixture drifted: the %s source carries %d video packets, want 234", ext, len(times.Packets))
	}
	return root, src, times
}

func rFrameRate(t *testing.T, ffprobe, path string) string {
	t.Helper()
	out, err := exec.Command(ffprobe, "-v", "error", "-select_streams", "v:0", "-show_entries", "stream=r_frame_rate",
		"-of", "default=nw=1:nk=1", "--", path).Output()
	if err != nil {
		t.Fatalf("read the guessed rate of %s: %v", path, err)
	}
	// A transport stream lists its program's copy of the stream after the stream itself.
	first, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(first)
}

// timesOf is the file's video timestamps, read through the probe the gate reads them with.
func timesOf(t *testing.T, ffprobe, path string) probe.Timestamps {
	t.Helper()
	ts, err := probe.New("", ffprobe).VideoTimestamps(context.Background(), path)
	if err != nil {
		t.Fatalf("list the video timestamps of %s: %v", path, err)
	}
	return ts
}

// positions are a listing's presentation times as positions from its first frame, in
// presentation order, in its own ticks.
func positions(ts probe.Timestamps) []int64 {
	var pts []int64
	for _, k := range ts.Packets {
		pts = append(pts, k.PTS)
	}
	s := slices.Sorted(slices.Values(pts))
	out := make([]int64, len(s))
	for i := range s {
		out[i] = s[i] - s[0]
	}
	return out
}

// decodeTimed reports whether a listing carries decode timestamps and no presentation
// timestamp, as AVI and ASF do.
func decodeTimed(ts probe.Timestamps) bool {
	return !slices.ContainsFunc(ts.Packets, func(k probe.Packet) bool { return k.HasPTS || !k.HasDTS })
}

// timingRun runs one oneshot pass over root, its configuration changed by mutate where it is
// not nil, with enc (holdfast's own encoder where nil), and returns the store and the events
// it emitted.
func timingRun(t *testing.T, ffmpeg, ffprobe, root string, enc Encoder, mutate func(*config.Config)) (*testStore, []Event) {
	t.Helper()
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	st := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	eng := New(cfg, prober, enc, st, discardLogger())
	var (
		mu     sync.Mutex
		events []Event
	)
	eng.Observer = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return st, events
}

// TestTiming_AVariableRateSourceKeepsItsTimesThroughTheEncode runs holdfast's own encoder over
// the variable-frame-rate source in each container it writes: the job is done, and the
// replacement presents every frame at the source's time with no two frames at one instant.
// Written in the source's own container it is the same ticks in the same time base, exactly;
// written into another (container_ext) each time is rescaled once into the output's base, which
// the gate's one-tick tolerance takes. Before
// -enc_time_base:v demux the encoder rounded every frame onto the 24 fps grid the demuxer
// guessed, and 29 to 42 of the 234 frames came out sharing a timestamp; with the timing gate
// in place that encode is refused, so this case reds on it either way.
func TestTiming_AVariableRateSourceKeepsItsTimesThroughTheEncode(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range timingCases {
		t.Run(tc.String(), func(t *testing.T) {
			root, _, want := vfrFixture(t, ffmpeg, ffprobe, tc.from, tc.codec...)
			st, _ := timingRun(t, ffmpeg, ffprobe, root, nil, tc.configure)

			row := onlyRow(t, st)
			if row.Status != store.Done {
				t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
			}
			out := filepath.Join(root, "movie."+tc.to)
			if want := map[string]string{"cpu": "hevc", "svtav1": "av1"}[tc.encoder]; codecOf(t, ffprobe, out) != want {
				t.Fatalf("the source was not replaced by %s", want)
			}
			got := timesOf(t, ffprobe, out)
			if err := timing.Check(want, got, nil); err != nil {
				t.Fatalf("the replacement's timing is not the source's: %v", err)
			}
			if tc.to != tc.from {
				return
			}
			if got.TimeBaseNum != want.TimeBaseNum || got.TimeBaseDen != want.TimeBaseDen {
				t.Errorf("time base %d/%d, want the source's %d/%d", got.TimeBaseNum, got.TimeBaseDen,
					want.TimeBaseNum, want.TimeBaseDen)
			}
			if !slices.Equal(positions(got), positions(want)) {
				t.Errorf("the replacement's frames are not at the source's ticks:\n got %v\nwant %v",
					positions(got), positions(want))
			}
		})
	}
}

// withoutEncTimeBase writes an ffmpeg that runs the real one with `-enc_time_base:v demux`
// taken out of its command line - holdfast's own encode as it was before this argument - and
// logs each removal, so a case can show the argument was there to remove.
func withoutEncTimeBase(t *testing.T, real string) (wrapper, log string) {
	t.Helper()
	dir := t.TempDir()
	wrapper, log = filepath.Join(dir, "ffmpeg-without-enc-time-base.sh"), filepath.Join(dir, "removed.log")
	script := "#!/bin/sh\n" +
		"skip=0\n" +
		"for a in \"$@\"; do\n" +
		"  shift\n" +
		"  if [ \"$skip\" = 1 ]; then skip=0; continue; fi\n" +
		"  if [ \"$a\" = \"-enc_time_base:v\" ]; then skip=1; printf 'removed\\n' >> \"" + log + "\"; continue; fi\n" +
		"  set -- \"$@\" \"$a\"\n" +
		"done\n" +
		"exec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return wrapper, log
}

// TestTiming_TheGateRefusesAnEncodeThatCollapsedTheTimestamps is the regression the gate
// exists for, in each container: holdfast's own command line without -enc_time_base:v demux
// rounds the variable-frame-rate source onto 24 fps. Every other gate passes that output -
// its duration and packet count are the source's and it decodes - and the timing gate refuses
// it, deterministically, under the length gate's token, with the source byte-identical.
func TestTiming_TheGateRefusesAnEncodeThatCollapsedTheTimestamps(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range timingCases {
		t.Run(tc.String(), func(t *testing.T) {
			root, src, _ := vfrFixture(t, ffmpeg, ffprobe, tc.from, tc.codec...)
			before := md5f(t, src)
			old, removed := withoutEncTimeBase(t, ffmpeg)
			prober := probe.New(old, ffprobe)
			cfg := baseCfg(root)
			tc.configure(&cfg)
			st, events := timingRun(t, ffmpeg, ffprobe, root, FFmpegEncoder{FFmpeg: old, Cfg: cfg, Probe: prober}, tc.configure)

			if b, err := os.ReadFile(removed); err != nil || strings.Count(string(b), "removed") != 1 {
				t.Fatalf("the encode's command line did not carry -enc_time_base:v once (%q, %v) - this case "+
					"would not be the encode before the fix", b, err)
			}
			if got := md5f(t, src); got != before {
				t.Fatalf("the source changed (md5 %s -> %s): a rejected encode must leave it byte-identical", before, got)
			}
			if n := nTemp(t, root); n != 0 {
				t.Errorf("%d temp file(s) left behind", n)
			}
			row := onlyRow(t, st)
			if row.Status != store.Failed {
				t.Fatalf("row %s %q, want failed at the timing gate", row.Status, row.Outcome.Reason)
			}
			var failed []Event
			for _, ev := range events {
				if ev.Status == store.Failed {
					failed = append(failed, ev)
				}
			}
			if len(failed) != 1 || failed[0].Gate != GateLength {
				t.Fatalf("failed events %+v, want exactly one at gate %q", failed, GateLength)
			}
			if r := row.Outcome.Reason; !strings.Contains(r, "timing parity failed") ||
				!strings.Contains(r, "presented at the same instant") || !strings.Contains(r, "The source is kept") {
				t.Errorf("the reason does not name collapsed timing: %q", r)
			}
			if row.Outcome.FailureClass != store.FailureDeterministic {
				t.Errorf("failure class %q, want %q", row.Outcome.FailureClass, store.FailureDeterministic)
			}
		})
	}
}

// TestTiming_AContainerThatStoresNoPresentationTimeIsReadByItsDecodeTimes runs the two
// containers that store no presentation timestamp, AVI (each frame timed by its chunk's slot)
// and ASF (WMV, by a send time), whose demuxers can report a decode timestamp alone.
//
// Read FROM one, in a whole pass: the variable-frame-rate source in AVI with two B-frames, so
// that no packet carries a presentation timestamp, encoded into Matroska. It is not refused for
// what it does not carry: it is read by its decode timestamps, which a decoder shows two frames
// on, and the encode is done; without -enc_time_base:v demux it is refused.
//
// Written INTO them, in a whole pass (container_ext: avi, wmv): the source in Matroska, encoded
// by libsvtav1, done, and refused by the length gate without the argument.
//
// Written into them by libx264, whose output there carries no presentation timestamp at all:
// driven through the encoder and the gate directly, because a pass does not hand libx264 a
// source in a codec it would not re-encode, and every codec ffmpeg can write variable-rate that
// libx264 would re-encode leaves its field order unknown, which the field-order guard skips. The
// gate reads the output by its decode timestamps: the encode passes, and without the argument -
// which retimes the AVI onto 24 fps and moves the WMV's frames by up to 17 ms - it is refused.
func TestTiming_AContainerThatStoresNoPresentationTimeIsReadByItsDecodeTimes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	avi := []string{"-c:v", "libx264", "-preset", "ultrafast", "-bf", "2", "-b:v", "8M", "-pix_fmt", "yuv420p"}
	for _, tc := range []struct {
		name, from, to, encoder string
		codec                   []string
	}{
		{"from avi", "avi", "mkv", "cpu", avi},
		{"into avi", "mkv", "avi", "svtav1", nil},
		{"into wmv", "mkv", "wmv", "svtav1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, before := range []bool{false, true} {
				root, _, want := vfrFixture(t, ffmpeg, ffprobe, tc.from, tc.codec...)
				if tc.from == "avi" && !decodeTimed(want) {
					t.Fatalf("fixture drifted: the AVI source carries presentation timestamps (%+v)", want)
				}
				bin := ffmpeg
				if before {
					bin, _ = withoutEncTimeBase(t, ffmpeg)
				}
				mutate := func(c *config.Config) { c.ContainerExt, c.Encoder = tc.to, tc.encoder }
				cfg := baseCfg(root)
				mutate(&cfg)
				st, events := timingRun(t, ffmpeg, ffprobe, root, FFmpegEncoder{FFmpeg: bin, Cfg: cfg, Probe: probe.New(bin, ffprobe)}, mutate)
				row := onlyRow(t, st)
				if before {
					// Retimed onto 24 fps, the AVI also comes out longer than the source, which the
					// duration half of the same gate sees first.
					refused := false
					for _, ev := range events {
						refused = refused || (ev.Status == store.Failed && ev.Gate == GateLength)
					}
					if row.Status != store.Failed || !refused || !strings.Contains(row.Outcome.Reason, "parity failed") {
						t.Fatalf("the encode without -enc_time_base: row %s %q, want refused by the length gate",
							row.Status, row.Outcome.Reason)
					}
					continue
				}
				if row.Status != store.Done {
					t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
				}
				if err := timing.Check(want, timesOf(t, ffprobe, filepath.Join(root, "movie."+tc.to)), nil); err != nil {
					t.Fatalf("the replacement's timing is not the source's: %v", err)
				}
			}
		})
	}
	for _, to := range []string{"avi", "wmv"} {
		t.Run("libx264 into "+to, func(t *testing.T) {
			hevc := []string{"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", "-b:v", "4M",
				"-pix_fmt", "yuv420p"}
			root, src, _ := vfrFixture(t, ffmpeg, ffprobe, "mkv", hevc...)
			cfg := baseCfg(root)
			cfg.Encoder = "x264"
			prober := probe.New(ffmpeg, ffprobe)
			eng := New(cfg, prober, nil, newTestStore(t, root), discardLogger())
			ctx := context.Background()
			old, _ := withoutEncTimeBase(t, ffmpeg)
			for _, bin := range []string{ffmpeg, old} {
				out := filepath.Join(t.TempDir(), "movie."+to)
				if err := (FFmpegEncoder{FFmpeg: bin, Cfg: cfg, Probe: probe.New(bin, ffprobe)}).Encode(ctx, src, out, nil); err != nil {
					t.Fatalf("encode: %v", err)
				}
				if got := timesOf(t, ffprobe, out); !decodeTimed(got) {
					t.Fatalf("fixture drifted: libx264 into %s carries presentation timestamps (%+v)", to, got)
				}
				class, err := eng.timingParity(ctx, src, out)
				if bin == ffmpeg && err != nil {
					t.Fatalf("the gate refused holdfast's own encode (%s): %v", class, err)
				}
				if bin == old && (err == nil || class != store.FailureDeterministic ||
					!strings.Contains(err.Error(), "the encode moved the source's frame timing")) {
					t.Fatalf("the encode without -enc_time_base: %q %v, want refused as moved", class, err)
				}
			}
		})
	}
}

// leadingSource is one source that starts where a decoder cannot show every packet it stores,
// and the property of its listing that makes it one.
type leadingSource struct {
	name, ext, encoder string
	make               func(t *testing.T, ffmpeg, dir, path string)
	starts             func(ts probe.Timestamps) bool
}

// cutFrom writes a source with args into dir and copies it from at seconds into path, the way a
// clip cut out of a longer file is made.
func cutFrom(at string, args ...string) func(t *testing.T, ffmpeg, dir, path string) {
	return func(t *testing.T, ffmpeg, dir, path string) {
		t.Helper()
		whole := filepath.Join(dir, "whole"+filepath.Ext(args[len(args)-1]))
		ff(t, ffmpeg, append(append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args[:len(args)-1]...), "--", whole)...)
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-ss", at, "-i", whole, "-map", "0:v", "-c", "copy", "--", path)
	}
}

// afterKeyBeforeIt reports whether a packet after the first keyframe is presented before it,
// or carries no presentation timestamp: an open GOP's leading pictures.
func afterKeyBeforeIt(ts probe.Timestamps) bool {
	k := slices.IndexFunc(ts.Packets, func(q probe.Packet) bool { return q.Key })
	return k >= 0 && slices.ContainsFunc(ts.Packets[k+1:], func(q probe.Packet) bool {
		return !q.HasPTS || q.PTS < ts.Packets[k].PTS
	})
}

var leadingSources = []leadingSource{
	{"an MP4 cut between keyframes, its edit list discarding the packets before the cut", "mp4", "cpu",
		cutFrom("2.5", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=10", "-c:v", "libx264", "-preset",
			"ultrafast", "-g", "48", "-bf", "2", "-b:v", "8M", "-pix_fmt", "yuv420p", "whole.mp4"),
		func(ts probe.Timestamps) bool {
			return slices.ContainsFunc(ts.Packets, func(q probe.Packet) bool { return q.Discard })
		}},
	{"H.264 starting at an open GOP's I-frame", "mkv", "cpu",
		cutFrom("4", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=12", "-c:v", "libx264", "-preset",
			"fast", "-x264-params", "open-gop=1:keyint=48:min-keyint=48:scenecut=0:bframes=3", "-b:v", "8M",
			"-pix_fmt", "yuv420p", "whole.mkv"), afterKeyBeforeIt},
	{"MPEG-2 starting at an open GOP", "mkv", "cpu",
		cutFrom("4", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=12", "-c:v", "mpeg2video", "-g", "15",
			"-bf", "2", "-b:v", "8M", "whole.ts"), afterKeyBeforeIt},
	{"HEVC starting at a CRA with leading pictures", "mkv", "svtav1",
		cutFrom("4", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=12", "-c:v", "libx265", "-preset",
			"fast", "-x265-params", "log-level=error:keyint=48:min-keyint=48:scenecut=0:bframes=3", "-b:v", "4M",
			"-pix_fmt", "yuv420p", "whole.mkv"), afterKeyBeforeIt},
	{"a transport stream cut between keyframes, its leading packets copied", "mkv", "cpu",
		func(t *testing.T, ffmpeg, dir, path string) {
			t.Helper()
			whole, cut := filepath.Join(dir, "whole.ts"), filepath.Join(dir, "cut.ts")
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
				"testsrc2=size=320x240:rate=24:duration=12", "-c:v", "libx264", "-preset", "ultrafast", "-g", "48",
				"-bf", "2", "-b:v", "8M", "-pix_fmt", "yuv420p", "--", whole)
			b, err := os.ReadFile(whole)
			if err != nil {
				t.Fatal(err)
			}
			// A cut on a transport packet boundary a fifth of the way in, mid-GOP.
			if err := os.WriteFile(cut, b[len(b)/188/5*188:], 0o644); err != nil {
				t.Fatal(err)
			}
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", cut, "-map", "0:v", "-c", "copy",
				"-copyinkf", "--", path)
		},
		func(ts probe.Timestamps) bool { return !ts.Packets[0].Key }},
}

// TestTiming_ASourceWhoseFirstPacketsADecoderCannotShowIsNotRefused runs whole passes over the
// sources that store more packets than a decoder shows when it starts at their first: an MP4
// edit list's discarded packets, an open GOP's and a CRA's leading pictures, and the packets
// before a cut stream's first keyframe. Each encode is exactly what a player shows of the
// source, and the timing gate passes it: the discarded packets are not counted, and the decode
// of the source's start shows which of its leading packets a decoder drops.
func TestTiming_ASourceWhoseFirstPacketsADecoderCannotShowIsNotRefused(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range leadingSources {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "movie."+tc.ext)
			tc.make(t, ffmpeg, t.TempDir(), src)
			if !tc.starts(timesOf(t, ffprobe, src)) {
				t.Fatal("fixture drifted: the source starts where a decoder shows every packet")
			}
			kept := keepCopy(t, src)
			st, eng := timingRunEngine(t, ffmpeg, ffprobe, root, func(c *config.Config) {
				c.ContainerExt, c.Encoder, c.VideoExts = "source", tc.encoder, []string{tc.ext}
			})
			row := onlyRow(t, st)
			if row.Status != store.Done {
				t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
			}
			if _, err := eng.timingParity(context.Background(), kept, src); err != nil {
				t.Fatalf("the replacement's timing is not what a player shows of the source: %v", err)
			}
		})
	}
}

// keepCopy copies a source aside, so the gate can be asked about it after a swap replaced it.
func keepCopy(t *testing.T, src string) string {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(t.TempDir(), filepath.Base(src))
	if err := os.WriteFile(kept, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return kept
}

// timingRunEngine is timingRun with holdfast's own encoder, returning the engine too.
func timingRunEngine(t *testing.T, ffmpeg, ffprobe, root string, mutate func(*config.Config)) (*testStore, *Engine) {
	t.Helper()
	cfg := baseCfg(root)
	mutate(&cfg)
	st := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, st, discardLogger())
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return st, eng
}

// shownLeading is one source whose first packets include leading pictures a decoder SHOWS: a
// faithful encode carries every one of them, and an output short of them must not pass for one
// whose leading pictures were dropped.
type shownLeading struct {
	name, encoder, codec string
	codecArgs            []string
	make                 func(t *testing.T, ffmpeg, dir, path string)
}

var shownLeadingSources = []shownLeading{
	{"MPEG-2 starting at a closed GOP's leading B-frames", "cpu", "hevc",
		[]string{"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error", "-pix_fmt", "yuv420p10le"},
		func(t *testing.T, ffmpeg, dir, path string) {
			t.Helper()
			// An open-GOP MPEG-2 stream cut at a GOP, its first GOP header then marked closed, so
			// that a decoder shows the B-frames before its first I-frame instead of dropping them -
			// a DVD title's first GOP.
			cut, raw := filepath.Join(dir, "cut.mkv"), filepath.Join(dir, "cut.m2v")
			cutFrom("4", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=25:duration=12", "-c:v", "mpeg2video",
				"-g", "15", "-bf", "2", "-b:v", "8M", "whole.ts")(t, ffmpeg, dir, cut)
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", cut, "-c", "copy", "-f", "mpeg2video", "--", raw)
			b, err := os.ReadFile(raw)
			if err != nil {
				t.Fatal(err)
			}
			gop := bytes.Index(b, []byte{0, 0, 1, 0xb8})
			if gop < 0 {
				t.Fatal("fixture drifted: the stream carries no GOP header")
			}
			b[gop+7] |= 0x40 // closed_gop, the 26th bit after the start code
			if err := os.WriteFile(raw, b, 0o644); err != nil {
				t.Fatal(err)
			}
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-fflags", "+genpts", "-r", "25", "-i", raw,
				"-c", "copy", "--", path)
		}},
	{"HEVC starting at an IDR with RADL pictures", "svtav1", "av1",
		[]string{"-c:v", "libsvtav1", "-preset", "12", "-crf", "40", "-pix_fmt", "yuv420p10le"},
		func(t *testing.T, ffmpeg, dir, path string) {
			t.Helper()
			// The second segment of a closed-GOP stream with two RADL pictures before each IDR,
			// split where an HLS segmenter would: it starts at an IDR whose RADL pictures follow it
			// in decode order and are shown before it, each with its own timestamp.
			whole := filepath.Join(dir, "whole.mkv")
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
				"testsrc2=size=320x240:rate=24:duration=12", "-c:v", "libx265", "-preset", "fast", "-x265-params",
				"log-level=error:keyint=48:min-keyint=48:scenecut=0:bframes=3:open-gop=0:radl=2", "-b:v", "4M",
				"-pix_fmt", "yuv420p", "--", whole)
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", whole, "-map", "0:v", "-c", "copy",
				"-f", "segment", "-segment_times", "4", filepath.Join(dir, "segment%d.mkv"))
			// Remuxed onto a timeline starting just after zero, so the RADL pictures before the
			// IDR keep a timestamp (Matroska reads one before zero back as none; ffmpeg's copy
			// gives the two of them one timestamp between them, which the source then carries and
			// a faithful encode reproduces).
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", filepath.Join(dir, "segment1.mkv"),
				"-c", "copy", "-output_ts_offset", "0.5", "--", path)
		}},
}

// TestTiming_LeadingPicturesADecoderShowsAreNotAllowedFor: a source whose first frames are
// leading pictures a decoder shows - a closed GOP's B-frames, an IDR's RADL pictures - is
// encoded faithfully with every one of them, and an output missing two frames, at the start or
// at the end, is refused: the decode of the source's start shows that those pictures are shown,
// so no frame is allowed for.
func TestTiming_LeadingPicturesADecoderShowsAreNotAllowedFor(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, tc := range shownLeadingSources {
		t.Run(tc.name, func(t *testing.T) {
			fixture := filepath.Join(t.TempDir(), "movie.mkv")
			tc.make(t, ffmpeg, t.TempDir(), fixture)
			listing := timesOf(t, ffprobe, fixture)
			if !afterKeyBeforeIt(listing) {
				t.Fatal("fixture drifted: no packet after the first keyframe is presented before it")
			}
			frames := 0
			for _, k := range listing.Packets {
				if !k.Discard {
					frames++
				}
			}
			start, err := probe.New(ffmpeg, ffprobe).VideoStart(context.Background(), fixture, len(listing.Packets))
			if err != nil || len(start.Frames) != frames {
				t.Fatalf("fixture drifted: a decoder shows %d of the source's %d packets (%v), want every one",
					len(start.Frames), frames, err)
			}
			mutate := func(c *config.Config) { c.Encoder = tc.encoder }
			run := func(t *testing.T, damage ...string) (store.Job, string) {
				t.Helper()
				root := t.TempDir()
				src := filepath.Join(root, "movie.mkv")
				b, err := os.ReadFile(fixture)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(src, b, 0o644); err != nil {
					t.Fatal(err)
				}
				var enc Encoder
				if damage != nil {
					enc = EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
						args := append([]string{"-hide_banner", "-nostdin", "-v", "error", "-y", "-i", in, "-map", "0"},
							tc.codecArgs...)
						args = append(append(append(args, timestampArgs...), damage...), "-f", "matroska", "--", out)
						return exec.CommandContext(ctx, ffmpeg, args...).Run()
					})
				}
				st, _ := timingRun(t, ffmpeg, ffprobe, root, enc, mutate)
				return onlyRow(t, st), src
			}

			t.Run("faithful", func(t *testing.T) {
				row, src := run(t)
				if row.Status != store.Done || codecOf(t, ffprobe, src) != tc.codec {
					t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
				}
			})
			for name, damage := range map[string][]string{
				"the first two frames missing": {"-vf", "select='gte(n\\,2)'"},
				"the last two frames missing":  {"-frames:v", strconv.Itoa(frames - 2)},
			} {
				t.Run(name, func(t *testing.T) {
					row, src := run(t, damage...)
					if row.Status != store.Failed || !strings.Contains(row.Outcome.Reason,
						fmt.Sprintf("the output's video shows %d frames and the source's %d", frames-2, frames)) {
						t.Fatalf("row %s %q, want refused for two frames short", row.Status, row.Outcome.Reason)
					}
					if md5f(t, src) != md5f(t, fixture) {
						t.Error("the source changed under a refused encode")
					}
				})
			}
		})
	}
}

// TestTiming_ASourceThatCarriesFramesOnOneInstantIsNotRefusedForThem: a source that stores two
// frames on one timestamp itself, re-encoded and remuxed. Either output reproduces the pair and
// nothing more, so the gate, which refuses only frames on one instant beyond the source's own,
// passes both, and each replaces the source.
func TestTiming_ASourceThatCarriesFramesOnOneInstantIsNotRefusedForThem(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, remux := range []bool{false, true} {
		t.Run(map[bool]string{false: "encode", true: "remux"}[remux], func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "movie.mkv")
			// Frame 5 is stamped with frame 4's time; two audio tracks, one for a remux to drop.
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
				"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=24:duration=2",
				"-f", "lavfi", "-i", "sine=frequency=440:duration=2", "-f", "lavfi", "-i", "sine=frequency=880:duration=2",
				"-map", "0:v", "-map", "1:a", "-map", "2:a", "-vf", "setpts='if(eq(N,5),4,N)/24/TB'",
				"-c:v", "libx264", "-preset", "ultrafast", "-bf", "0", "-b:v", "8M", "-fps_mode", "passthrough",
				"-c:a", "aac", "-b:a", "192k", "-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=fre",
				"--", src)
			want := timesOf(t, ffprobe, src)
			if p := positions(want); p[4] != p[5] {
				t.Fatalf("fixture drifted: the source does not carry two frames on one instant (%v)", p)
			}
			st, _ := timingRun(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
				c.AudioLanguages = []string{"eng"}
				c.RemuxOnly = boolPtr(remux)
			})
			if row := onlyRow(t, st); row.Status != store.Done {
				t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
			}
			got := timesOf(t, ffprobe, src)
			if !slices.Equal(positions(got), positions(want)) {
				t.Fatalf("the replacement is not at the source's ticks:\n got %v\nwant %v", positions(got), positions(want))
			}
		})
	}
}

// TestTiming_TheGateHoldsARemux runs a remux-only root over the variable-frame-rate source,
// dropping one of its two audio tracks so the copy is smaller: a stream copy carries every
// timestamp exactly, so the gate, which holds a remux too, passes it and the source is
// replaced by the copy at the source's own ticks.
func TestTiming_TheGateHoldsARemux(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, src, want := vfrFixture(t, ffmpeg, ffprobe, "mkv")
	withAudio := filepath.Join(t.TempDir(), "movie.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-f", "lavfi", "-i", "sine=frequency=440:duration=8", "-f", "lavfi", "-i", "sine=frequency=880:duration=8",
		"-map", "0:v", "-map", "1:a", "-map", "2:a", "-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
		"-metadata:s:a:0", "language=eng", "-metadata:s:a:1", "language=fre", "--", withAudio)
	if err := os.Rename(withAudio, src); err != nil {
		t.Fatal(err)
	}
	if err := timing.Check(want, timesOf(t, ffprobe, src), nil); err != nil {
		t.Fatalf("fixture drifted: adding the audio retimed the video: %v", err)
	}
	st, _ := timingRun(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.RemuxOnly = boolPtr(true)
		c.AudioLanguages = []string{"eng"}
	})
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("row %s %q, want a done remux", row.Status, row.Outcome.Reason)
	}
	got := timesOf(t, ffprobe, src)
	if codecOf(t, ffprobe, src) != "h264" || !slices.Equal(positions(got), positions(want)) {
		t.Fatalf("the remux is not the source's video at the source's ticks (codec %q):\n got %v\nwant %v",
			codecOf(t, ffprobe, src), positions(got), positions(want))
	}
}

// TestTiming_AListingThatCannotBeComparedIsRefusedAndClassified: a timing nobody could read is
// never a match. Where ffprobe answered about the file - here, an output with no video stream to
// list - the refusal is deterministic; where it never answered, it is transient, a condition of
// the run.
func TestTiming_AListingThatCannotBeComparedIsRefusedAndClassified(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, src, _ := vfrFixture(t, ffmpeg, ffprobe, "mkv")
	audioOnly := filepath.Join(t.TempDir(), "audio.mka")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "sine=duration=8",
		"-c:a", "flac", "--", audioOnly)
	broken := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(broken, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name, ffprobe, in, out, want string
		class                        store.FailureClass
	}{
		{"an output with no video stream", ffprobe, src, audioOnly, "the output's video timestamps could not be listed",
			store.FailureDeterministic},
		{"a source with no video stream", ffprobe, audioOnly, src, "the source's video timestamps could not be listed",
			store.FailureDeterministic},
		{"an ffprobe that answers nothing", broken, src, src, "ffprobe did not answer", store.FailureTransient},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eng := New(baseCfg(root), probe.New(ffmpeg, tc.ffprobe), nil, newTestStore(t, root), discardLogger())
			class, err := eng.timingParity(ctx, tc.in, tc.out)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "The source is kept") {
				t.Fatalf("timingParity = %v, want a refusal containing %q", err, tc.want)
			}
			if class != tc.class {
				t.Errorf("class %q, want %q", class, tc.class)
			}
		})
	}
}
