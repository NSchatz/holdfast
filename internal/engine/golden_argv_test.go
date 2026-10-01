package engine

// THE GOLDEN ARGV: the whole ffmpeg command line this build assembles, for every encoder in
// the registry and every option combination the existing fixtures drive, written down from
// the build BEFORE the encode plan changed how a job's command line is derived.
//
// It is the proof that refactor owes: behaviour-preserving means an existing configuration
// produces the command line it produced before, byte for byte, and the only way to say that
// about hundreds of configurations is to have recorded them first. The files under
// testdata/golden-argv were written by the build before the refactor and are read back
// unchanged after it. A difference is a finding, never an update: regenerating a golden file
// is a deliberate change to what holdfast runs, made in its own commit that says why.
//
// Two layers, because there are two ways a command line is reached:
//
//   - the ENCODER layer drives FFmpegEncoder directly, the way an exported caller does: with
//     or without a library profile handed in, with or without an intended stream map, with or
//     without a probe snapshot, for every output container this build names;
//   - the ENGINE layer drives a whole pass (RunOneshot) over a library holding one source, so
//     what is graded is the command line the ENGINE's own resolution produced - the profile of
//     the root, its rules and encode profiles, the guards' snapshot, the intended stream map
//     and the working path it chose.
//
// The encoder layer runs every case for every registry encoder. The engine layer runs every
// case for the default encoder and its core cases - the ones whose resolution meets an
// encoder-specific part of the command line: the codec, the quality shape, the VAAPI device
// and its filter chain, the libx265 parameters - for every registry encoder too. A pass costs
// a store and a probe of its source, and the combinations the engine resolves differently
// per encoder are those; the per-encoder shape of every other combination is the encoder
// layer's, built from the same profile, stream map and snapshot the engine hands over.
//
// No encode runs. The encoder's binary is a stand-in that exits 0 without writing anything;
// the probes are the real, pinned ffprobe reading real synthetic sources, because every
// derivation that decides the command line reads the source through them. The command line
// is taken at the encoder's own argvObserver seam, so it is the argv the production encoder
// assembled and would have executed - not a re-derivation of it.
//
// Regenerate (only as a deliberate, reviewed change):
//
//	go test ./internal/engine -run TestGoldenArgv -update-golden-argv

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
)

var updateGoldenArgv = flag.Bool("update-golden-argv", false,
	"rewrite internal/engine/testdata/golden-argv from the current build instead of comparing against it")

// goldenArgvDir is where the golden files live, one per layer per registry encoder.
const goldenArgvDir = "testdata/golden-argv"

// goldenArgvRecorder collects every invocation an encoder made, in the order it made them.
type goldenArgvRecorder struct {
	mu    sync.Mutex
	calls [][]string
}

func (r *goldenArgvRecorder) record(args []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, append([]string(nil), args...))
}

func (r *goldenArgvRecorder) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.calls...)
}

// goldenStub writes the stand-in for the encoder's ffmpeg: it exits 0 and writes nothing, so
// the command line is assembled and executed exactly as production would and nothing is
// encoded.
func goldenStub(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffmpeg-stand-in")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write the ffmpeg stand-in: %v", err)
	}
	return p
}

// goldenPaths replaces the run-specific parts of a path with stable placeholders, so a golden
// file records the shape of a command line and not the temp directory one run happened to
// get. Longest first, so a directory inside another is replaced before its parent.
type goldenPaths [][2]string

func (g goldenPaths) with(from, to string) goldenPaths {
	out := append(goldenPaths(nil), g...)
	out = append(out, [2]string{from, to})
	sort.SliceStable(out, func(i, j int) bool { return len(out[i][0]) > len(out[j][0]) })
	return out
}

func (g goldenPaths) apply(s string) string {
	for _, p := range g {
		if p[0] != "" {
			s = strings.ReplaceAll(s, p[0], p[1])
		}
	}
	return s
}

// goldenRenderArgv renders one invocation as one line: each element as it is, or Go-quoted where it
// is empty or carries a space, a quote or a backslash, so the line reads back unambiguously.
func goldenRenderArgv(args []string, paths goldenPaths) string {
	parts := make([]string, len(args))
	for i, a := range args {
		a = paths.apply(a)
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\") {
			a = strconv.Quote(a)
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// goldenBlocks is one golden file: each case's lines, by case name, and the order they are
// written in.
type goldenBlocks struct {
	order []string
	lines map[string][]string
}

func newGoldenBlocks() *goldenBlocks { return &goldenBlocks{lines: map[string][]string{}} }

func (b *goldenBlocks) add(t *testing.T, name string, lines []string) {
	t.Helper()
	if _, dup := b.lines[name]; dup {
		t.Fatalf("golden case %q is defined twice", name)
	}
	b.order = append(b.order, name)
	b.lines[name] = lines
}

const goldenCaseMark = "== "

func (b *goldenBlocks) format() []byte {
	var out bytes.Buffer
	for _, name := range b.order {
		out.WriteString(goldenCaseMark + name + "\n")
		for _, l := range b.lines[name] {
			out.WriteString(l + "\n")
		}
	}
	return out.Bytes()
}

func parseGoldenBlocks(data []byte) (*goldenBlocks, error) {
	b := newGoldenBlocks()
	var cur string
	for i, l := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if strings.HasPrefix(l, goldenCaseMark) {
			cur = strings.TrimPrefix(l, goldenCaseMark)
			if _, dup := b.lines[cur]; dup {
				return nil, fmt.Errorf("line %d: case %q appears twice", i+1, cur)
			}
			b.order = append(b.order, cur)
			b.lines[cur] = []string{}
			continue
		}
		if cur == "" {
			return nil, fmt.Errorf("line %d precedes the first case", i+1)
		}
		b.lines[cur] = append(b.lines[cur], l)
	}
	return b, nil
}

// checkGolden compares got against the golden file at path, or writes it under
// -update-golden-argv. Every case must be in both, with identical lines: a case the build no
// longer produces is as much a difference as a command line that moved.
func checkGolden(t *testing.T, path string, got *goldenBlocks) {
	t.Helper()
	if *updateGoldenArgv {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(path, got.format(), 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the golden file %s: %v (a golden file that is missing is a failure, never a pass)", path, err)
	}
	want, err := parseGoldenBlocks(data)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, name := range got.order {
		w, ok := want.lines[name]
		if !ok {
			t.Errorf("%s: case %q is not in the golden file", path, name)
			continue
		}
		g := got.lines[name]
		if len(g) != len(w) {
			t.Errorf("%s: case %q produced %d line(s), the golden file records %d:\n  got  %q\n  want %q",
				path, name, len(g), len(w), g, w)
			continue
		}
		for i := range g {
			if g[i] != w[i] {
				t.Errorf("%s: case %q line %d moved:\n  got  %s\n  want %s\n  %s",
					path, name, i+1, g[i], w[i], firstDifference(g[i], w[i]))
			}
		}
	}
	for _, name := range want.order {
		if _, ok := got.lines[name]; !ok {
			t.Errorf("%s: the golden file records case %q, which this build no longer produces", path, name)
		}
	}
}

// firstDifference names the first element two rendered lines disagree on, so a failure
// points at the option that moved rather than at two 600-character lines.
func firstDifference(got, want string) string {
	g, w := strings.Fields(got), strings.Fields(want)
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return fmt.Sprintf("first difference at element %d: got %q, want %q", i, g[i], w[i])
		}
	}
	return fmt.Sprintf("one line is a prefix of the other (%d against %d elements)", len(g), len(w))
}

// goldenEncoderKeys is every key the registry ships, in a stable order. The golden files
// are walked from the registry rather than from a list, so an encoder added later is ungraded
// here the moment it exists and the coverage test below says so.
func goldenEncoderKeys() []string {
	keys := encoder.Known()
	sort.Strings(keys)
	return keys
}

// goldenSource is one fixture with its probe answers taken once: the snapshot the engine's
// guards would read (its lazy parts forced, so every case reads the same answers without
// paying for them again), the stream list and the codec an intended stream map is derived
// from. The encoder layer hands the snapshot over exactly as the engine hands its own.
type goldenSource struct {
	path    string
	props   *probe.VideoProps
	streams []probe.Stream
	codec   string
}

// goldenSources probes every fixture once.
func goldenSources(t *testing.T, prober *probe.Prober, fixtures map[string]string) map[string]*goldenSource {
	t.Helper()
	ctx := context.Background()
	out := make(map[string]*goldenSource, len(fixtures))
	for name, path := range fixtures {
		props := prober.VideoProps(ctx, path)
		_ = props.SideData()
		_ = props.FrameSideData()
		_, _ = props.VideoStreams()
		_, _ = props.AllStreams()
		streams, ok := prober.Streams(ctx, path)
		if !ok {
			t.Fatalf("ffprobe could not enumerate the streams of the fixture %s", name)
		}
		out[name] = &goldenSource{path: path, props: props, streams: streams, codec: prober.VideoCodec(ctx, path)}
	}
	return out
}

// goldenFixtures makes every synthetic source the cases read, once, into dir. Each is a
// lavfi-generated file of the SHAPE a case needs: a colour description, a pixel format, a
// scan type, a size, a stream layout or a container. The property each case relies on is the
// property the fixture was built to have; the existing fixture builders are reused where one
// already builds it.
func goldenFixtures(t *testing.T, ffmpeg, ffprobe, dir string) map[string]string {
	t.Helper()
	clip := func(name string, extra ...string) {
		args := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
			"-i", "testsrc2=duration=1:size=320x240:rate=10", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M"}
		args = append(args, extra...)
		ff(t, ffmpeg, append(args, "--", filepath.Join(dir, name))...)
	}
	clip("sdr.mkv", "-pix_fmt", "yuv420p")
	for _, ext := range []string{"mp4", "ts", "m2ts", "avi", "mov", "m4v", "flv", "wmv"} {
		clip("sdr."+ext, "-pix_fmt", "yuv420p")
	}
	clip("bt709.mkv", "-pix_fmt", "yuv420p", "-colorspace", "bt709", "-color_primaries", "bt709",
		"-color_trc", "bt709", "-color_range", "tv")
	clip("full-range.mkv", "-pix_fmt", "yuvj420p")
	clip("10bit.mkv", "-pix_fmt", "yuv420p10le", "-profile:v", "high10")
	clip("422.mkv", "-pix_fmt", "yuv422p")
	clip("444.mkv", "-pix_fmt", "yuv444p")
	// A 12-bit 4:2:0 source: the pinned build's libx264 is 8/10-bit, so it is written
	// losslessly with ffv1, which carries yuv420p12le. Its plan keeps 12 bits, which only
	// some encoders list.
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10", "-c:v", "ffv1", "-pix_fmt", "yuv420p12le",
		"--", filepath.Join(dir, "12bit.mkv"))
	// The pinned build's libx264 writes only the matrix of -color_primaries, -color_trc and
	// -colorspace into the stream (the other two probe as unknown, which is the shape the
	// existing bt709 fixture has), so a source that really signals its primaries and transfer
	// is written with the h264_metadata bitstream filter, into the stream's own VUI, which is
	// where a real PQ or HLG source carries them: bt2020 primaries (9), bt2020nc (9), and the
	// PQ (16) or HLG (18) transfer, with no mastering-display block.
	vui := func(prim, trc, matrix int) []string {
		return []string{"-bsf:v", fmt.Sprintf("h264_metadata=colour_primaries=%d:transfer_characteristics=%d:"+
			"matrix_coefficients=%d:video_full_range_flag=0", prim, trc, matrix)}
	}
	clip("bt709-vui.mkv", append([]string{"-pix_fmt", "yuv420p"}, vui(1, 1, 1)...)...)
	clip("hlg.mkv", append([]string{"-pix_fmt", "yuv420p10le", "-profile:v", "high10"}, vui(9, 18, 9)...)...)
	clip("pq.mkv", append([]string{"-pix_fmt", "yuv420p10le", "-profile:v", "high10"}, vui(9, 16, 9)...)...)
	mkH264HDR10(t, ffmpeg, filepath.Join(dir, "hdr10.mkv"), "8M")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10", "-c:v", "ffv1", "-pix_fmt", "yuv411p",
		"--", filepath.Join(dir, "exotic.mkv"))
	mkInterlacedLong(t, ffmpeg, filepath.Join(dir, "interlaced.mkv"), "8M")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=640x480:rate=10", "-c:v", "libx264", "-preset", "ultrafast",
		"-b:v", "8M", "-pix_fmt", "yuv420p", "--", filepath.Join(dir, "tall.mkv"))
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=8:size=640x480:rate=25", "-vf", "interlace=scan=tff",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", "-flags", "+ilme+ildct",
		"--", filepath.Join(dir, "tall-interlaced.mkv"))
	mkMP4WithCoverArt(t, ffmpeg, ffprobe, filepath.Join(dir, "cover.mp4"), "8M")
	// An MP4 whose cover art is BMP: the one MP4 cover codec no Matroska attachment mimetype
	// reads back as a picture, so carrying it into Matroska is refused.
	bmp := filepath.Join(t.TempDir(), "cover.bmp")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=160x120",
		"-frames:v", "1", "-c:v", "bmp", "-f", "image2", "--", bmp)
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=10", "-i", bmp, "-map", "0:v", "-map", "1:v",
		"-c:v:0", "libx264", "-preset", "ultrafast", "-b:v:0", "8M", "-pix_fmt", "yuv420p",
		"-c:v:1", "copy", "-disposition:v:1", "attached_pic", "--", filepath.Join(dir, "cover-bmp.mp4"))
	assertVideoStreamShape(t, ffprobe, filepath.Join(dir, "cover-bmp.mp4"), []bool{false, true})
	mkShortMatroska(t, ffmpeg, filepath.Join(dir, "cover.mkv"), true)
	mkReportMatroska(t, ffmpeg, ffprobe, filepath.Join(dir, "pictures.mkv"), reportShape{pictures: true})
	mkReportMatroska(t, ffmpeg, ffprobe, filepath.Join(dir, "pictures-font.mkv"), reportShape{pictures: true, font: true})
	mkSourceWithStreams(t, ffmpeg, filepath.Join(dir, "streams.mkv"),
		audioStream("eng"), audioStream("jpn"), commentaryAudio("eng"),
		subtitleStream("eng"), subtitleStream("fre"), subtitleStream(""))
	mkSourceWithStreams(t, ffmpeg, filepath.Join(dir, "jpn-only.mkv"), audioStream("jpn"), subtitleStream("jpn"))
	mkMP4WithSubs(t, ffmpeg, filepath.Join(dir, "subs.mp4"), "8M")

	// Sources in every codec family and outside them, for the codec-family guard: an H.264
	// target skips an HEVC or AV1 source, an HEVC target an AV1 one, and a codec this build
	// does not write (MPEG-4 Part 2, FFV1) is re-encoded by every encoder. The 10-bit source
	// outside the families is FFV1 tagged bt2020/PQ on its frames (setparams, since -color_*
	// does not reach them), so an H.264 target has a 10-bit PQ source it does not skip.
	lavfi := []string{"-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=duration=1:size=320x240:rate=10"}
	ff(t, ffmpeg, append(lavfi, "-c:v", "mpeg4", "-q:v", "2", "-pix_fmt", "yuv420p",
		"--", filepath.Join(dir, "mpeg4.mkv"))...)
	ff(t, ffmpeg, append(lavfi, "-c:v", "libx265", "-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error",
		"--", filepath.Join(dir, "hevc.mkv"))...)
	ff(t, ffmpeg, append(lavfi, "-c:v", "libsvtav1", "-pix_fmt", "yuv420p10le",
		"--", filepath.Join(dir, "av1.mkv"))...)
	ff(t, ffmpeg, append(lavfi, "-vf", "setparams=color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc:range=tv",
		"-c:v", "ffv1", "-pix_fmt", "yuv420p10le", "--", filepath.Join(dir, "ffv1-pq.mkv"))...)

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the fixture directory: %v", err)
	}
	out := map[string]string{}
	for _, e := range entries {
		out[e.Name()] = filepath.Join(dir, e.Name())
	}
	return out
}

// goldenCPUs are the whole-CPU budgets the cases size a libx265 parallelism from, as the run
// derives one: the budgets the existing fixtures use (1, 2, 3, 5, 24 and 64).
var goldenCPUs = []int{1, 2, 3, 5, 24, 64}

// goldenPresetWords are the preset spellings the configuration accepts, plus the two a
// mapping has to fall back on: the empty string and a word nobody configured.
var goldenPresetWords = []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium",
	"slow", "slower", "veryslow", "placebo", "", "unrecognised"}

// goldenDeinterlaceApplied are the deinterlace values this build resolves to a filter it runs;
// goldenDeinterlaceRefused are the ones it refuses before anything runs.
var (
	goldenDeinterlaceApplied = []string{"yadif", "bwdif", "yadif=send_frame", "yadif=send_frame_nospatial",
		"bwdif=send_frame", "bwdif=send_frame_nospatial"}
	goldenDeinterlaceRefused = []string{"yadif=send_field", "yadif=send_field_nospatial", "bwdif=send_field",
		"bwdif=send_field_nospatial", "kerndeint"}
)

func goldenLabel(s string) string {
	if s == "" {
		return "empty"
	}
	return s
}

// ---- the encoder layer ----------------------------------------------------------------

// encoderArgvCase is one direct call of the production encoder.
type encoderArgvCase struct {
	name string
	// source is the fixture the encode reads; out is the output's name, whose extension is
	// what names the container.
	source, out string
	// cfg mutates the configuration the encoder is built with. Its top-level values are the
	// profile the encoder builds from; handProfile hands that profile in explicitly, which is
	// what the engine does.
	cfg         func(*config.Config)
	handProfile bool
	// streamPlan hands the encoder an intended stream map derived from the source's own probe
	// and the profile, which is what the engine does; without it the encoder builds the map a
	// direct caller gets.
	streamPlan bool
	// probesItself hands the encoder no snapshot, so it takes its own, as a direct caller's
	// encoder does; every other case hands it the snapshot of its source, as the engine does.
	probesItself bool
	x265         encoder.X265Parallelism
	progress     bool
}

// encoderArgvCases is every case the encoder layer runs for one registry encoder.
func encoderArgvCases() []encoderArgvCase {
	var cs []encoderArgvCase
	add := func(c encoderArgvCase) {
		if c.source == "" {
			c.source = "sdr.mkv"
		}
		if c.out == "" {
			c.out = "film.mkv"
		}
		cs = append(cs, c)
	}
	both := func(fs ...func(*config.Config)) func(*config.Config) {
		return func(c *config.Config) {
			for _, f := range fs {
				f(c)
			}
		}
	}
	crf := func(n int) func(*config.Config) { return func(c *config.Config) { c.CRF = n } }
	preset := func(w string) func(*config.Config) { return func(c *config.Config) { c.Preset = w } }
	pixFmt := func(p string) func(*config.Config) { return func(c *config.Config) { c.PixelFormat = p } }
	bitrate := func(k int) func(*config.Config) { return func(c *config.Config) { c.BitrateKbps = k } }
	deint := func(f string) func(*config.Config) { return func(c *config.Config) { c.Deinterlace = f } }
	ceiling := func(h int) func(*config.Config) {
		return func(c *config.Config) { c.MaxHeight, c.DownscaleAck = h, boolPtr(true) }
	}
	remux := func(c *config.Config) { c.RemuxOnly = boolPtr(true) }
	langs := func(audio, subs []string) func(*config.Config) {
		return func(c *config.Config) { c.AudioLanguages, c.SubtitleLanguages = audio, subs }
	}
	noCommentary := func(c *config.Config) { c.KeepCommentary = boolPtr(false) }
	x265 := encoder.X265ParallelismFor

	// The base shape, and each way a caller can hand it: a snapshot or none, the engine's own
	// profile, its own map, and a progress channel.
	add(encoderArgvCase{name: "base"})
	add(encoderArgvCase{name: "base/probes-itself", probesItself: true})
	add(encoderArgvCase{name: "base/profile-handed", handProfile: true})
	add(encoderArgvCase{name: "base/stream-plan-handed", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "base/progress", progress: true})
	add(encoderArgvCase{name: "base/progress/stream-plan-handed", handProfile: true, streamPlan: true, progress: true})

	// Rate control and preset.
	for _, n := range []int{23, 24, 26, 28, 29, 30, 31, 32, 33, 40} {
		add(encoderArgvCase{name: "crf-" + strconv.Itoa(n), cfg: crf(n)})
	}
	for _, k := range []int{1500, 3000, 8000} {
		add(encoderArgvCase{name: "bitrate-" + strconv.Itoa(k) + "k", cfg: bitrate(k)})
	}
	for _, w := range goldenPresetWords {
		add(encoderArgvCase{name: "preset-" + goldenLabel(w), cfg: preset(w)})
		add(encoderArgvCase{name: "preset-" + goldenLabel(w) + "/bitrate-8000k", cfg: both(preset(w), bitrate(8000))})
	}
	add(encoderArgvCase{name: "crf-28/preset-ultrafast", cfg: both(crf(28), preset("ultrafast"))})
	add(encoderArgvCase{name: "crf-30/preset-fast/pixel-format-yuv420p", cfg: both(crf(30), preset("fast"), pixFmt("yuv420p"))})

	// Pixel format: forced, and derived from each source shape the guards let through.
	for _, p := range []string{"yuv420p10le", "yuv420p", "nv12"} {
		add(encoderArgvCase{name: "pixel-format-" + p, cfg: pixFmt(p)})
	}
	for _, src := range []string{"full-range", "10bit", "422", "444"} {
		add(encoderArgvCase{name: "source-" + src, source: src + ".mkv"})
		add(encoderArgvCase{name: "source-" + src + "/pixel-format-yuv420p10le", source: src + ".mkv", cfg: pixFmt("yuv420p10le")})
	}
	add(encoderArgvCase{name: "source-exotic", source: "exotic.mkv"})
	add(encoderArgvCase{name: "source-exotic/pixel-format-yuv420p10le", source: "exotic.mkv", cfg: pixFmt("yuv420p10le")})

	// The explicit pixel format: every chroma and depth a plan can carry, derived and forced,
	// in its planar and semi-planar spellings. Each encoder is handed the format it lists for
	// that chroma and depth, or - where it lists none - the plan is refused, never left to
	// ffmpeg's silent auto-selection.
	add(encoderArgvCase{name: "source-12bit", source: "12bit.mkv"})
	for _, p := range []string{"yuv420p12le", "yuv422p", "yuv422p10le", "yuv422p12le", "yuv444p", "yuv444p10le",
		"yuv444p12le", "yuvj420p", "p010le", "nv16", "p410le", "yuv420p10be", "gray"} {
		add(encoderArgvCase{name: "pixel-format-" + p, cfg: pixFmt(p)})
	}
	add(encoderArgvCase{name: "pixel-format-yuv422p10le/bitrate-8000k", cfg: both(pixFmt("yuv422p10le"), bitrate(8000))})
	add(encoderArgvCase{name: "pixel-format-yuv420p/bitrate-8000k", cfg: both(pixFmt("yuv420p"), bitrate(8000))})

	// Per-encoder quality. Every hardware encoder's quality.<key> set to a value of its own,
	// so each file shows its encoder reading its OWN entry and no other; the software
	// encoders, which have none, keep crf. Then each scale's two edges, the value off each
	// scale (refused by the derivation, which validate would have refused first), the crf
	// that is off NVENC's and VAAPI's scales inherited by them, a target bitrate (which
	// passes no quality at all), and the encoder named by its ffmpeg alias.
	qualityAll := func(c *config.Config) {
		c.Quality = map[string]int{"nvenc": 24, "av1_nvenc": 40, "qsv": 25, "vaapi": 26, "amf": 27,
			"h264_nvenc": 28, "h264_qsv": 29, "h264_vaapi": 31, "h264_amf": 32,
			"av1_qsv": 33, "av1_vaapi": 120, "av1_amf": 130}
	}
	qualityEdge := func(pick func(encoder.QualityScale) int) func(*config.Config) {
		return func(c *config.Config) {
			c.Quality = map[string]int{}
			for _, k := range encoder.QualityKeys() {
				spec, _ := encoder.Lookup(k)
				c.Quality[k] = pick(spec.Quality)
			}
		}
	}
	add(encoderArgvCase{name: "quality-set", cfg: qualityAll})
	add(encoderArgvCase{name: "quality-set/crf-30", cfg: both(qualityAll, crf(30))})
	add(encoderArgvCase{name: "quality-set/bitrate-8000k", cfg: both(qualityAll, bitrate(8000))})
	add(encoderArgvCase{name: "quality-set/stream-plan-handed", cfg: qualityAll, handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "quality-set/alias", cfg: both(qualityAll, func(c *config.Config) {
		if spec, ok := encoder.Lookup(c.Encoder); ok {
			c.Encoder = spec.FFmpegCodec
		}
	})})
	add(encoderArgvCase{name: "quality-min", cfg: qualityEdge(func(q encoder.QualityScale) int { return q.Min })})
	add(encoderArgvCase{name: "quality-max", cfg: qualityEdge(func(q encoder.QualityScale) int { return q.Max })})
	add(encoderArgvCase{name: "quality-below-scale", cfg: qualityEdge(func(q encoder.QualityScale) int { return q.Min - 1 })})
	add(encoderArgvCase{name: "quality-above-scale", cfg: qualityEdge(func(q encoder.QualityScale) int { return q.Max + 1 })})
	add(encoderArgvCase{name: "crf-0", cfg: crf(0)})
	add(encoderArgvCase{name: "crf-0/bitrate-8000k", cfg: both(crf(0), bitrate(8000))})
	add(encoderArgvCase{name: "crf-51", cfg: crf(51)})

	// Colour, and HDR10 static metadata.
	for _, src := range []string{"bt709", "bt709-vui", "hlg", "pq", "hdr10"} {
		n := "colour-" + src
		add(encoderArgvCase{name: n, source: src + ".mkv"})
		add(encoderArgvCase{name: n + "/probes-itself", source: src + ".mkv", probesItself: true})
		add(encoderArgvCase{name: n + "/bitrate-8000k", source: src + ".mkv", cfg: bitrate(8000)})
		add(encoderArgvCase{name: n + "/pixel-format-yuv420p10le", source: src + ".mkv", cfg: pixFmt("yuv420p10le")})
		add(encoderArgvCase{name: n + "/x265-3cpu", source: src + ".mkv", x265: x265(3)})
		add(encoderArgvCase{name: n + "/x265-3cpu/bitrate-1500k", source: src + ".mkv", x265: x265(3), cfg: bitrate(1500)})
	}

	// libx265 parallelism.
	for _, n := range goldenCPUs {
		add(encoderArgvCase{name: "x265-" + strconv.Itoa(n) + "cpu", x265: x265(n)})
	}
	add(encoderArgvCase{name: "x265-3cpu/bitrate-1500k", x265: x265(3), cfg: bitrate(1500)})
	add(encoderArgvCase{name: "x265-3cpu/pixel-format-yuv420p10le", x265: x265(3), cfg: pixFmt("yuv420p10le")})

	// Picture operations, handed the way the engine hands them: through the profile.
	for _, f := range goldenDeinterlaceApplied {
		add(encoderArgvCase{name: "deinterlace-" + f, source: "interlaced.mkv", cfg: deint(f), handProfile: true})
	}
	add(encoderArgvCase{name: "deinterlace-off", source: "interlaced.mkv", cfg: deint("off"), handProfile: true})
	for _, f := range goldenDeinterlaceRefused {
		add(encoderArgvCase{name: "deinterlace-" + f, source: "interlaced.mkv", cfg: deint(f), handProfile: true})
	}
	add(encoderArgvCase{name: "deinterlace-yadif/progressive-source", cfg: deint("yadif"), handProfile: true})
	add(encoderArgvCase{name: "deinterlace-yadif/not-handed", source: "interlaced.mkv", cfg: deint("yadif")})
	add(encoderArgvCase{name: "deinterlace-yadif/probes-itself", source: "interlaced.mkv", cfg: deint("yadif"),
		handProfile: true, probesItself: true})
	add(encoderArgvCase{name: "deinterlace-yadif/bitrate-8000k", source: "interlaced.mkv",
		cfg: both(deint("yadif"), bitrate(8000)), handProfile: true})
	add(encoderArgvCase{name: "deinterlace-yadif/remux-only", source: "interlaced.mkv", handProfile: true,
		streamPlan: true, cfg: both(deint("yadif"), remux)})
	for _, h := range []int{220, 240, 360} {
		add(encoderArgvCase{name: "max-height-" + strconv.Itoa(h), source: "tall.mkv", cfg: ceiling(h), handProfile: true})
	}
	add(encoderArgvCase{name: "max-height-480/at-the-ceiling", source: "tall.mkv", cfg: ceiling(480), handProfile: true})
	add(encoderArgvCase{name: "max-height-240/not-handed", source: "tall.mkv", cfg: ceiling(240)})
	add(encoderArgvCase{name: "max-height-240/probes-itself", source: "tall.mkv", cfg: ceiling(240), handProfile: true,
		probesItself: true})
	add(encoderArgvCase{name: "max-height-240/bitrate-8000k", source: "tall.mkv", cfg: both(ceiling(240), bitrate(8000)),
		handProfile: true})
	add(encoderArgvCase{name: "max-height-240/remux-only", source: "tall.mkv", cfg: both(ceiling(240), remux),
		handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "max-height-240/deinterlace-yadif", source: "tall-interlaced.mkv",
		cfg: both(ceiling(240), deint("yadif")), handProfile: true})
	add(encoderArgvCase{name: "max-height-240/deinterlace-bwdif/bitrate-8000k", source: "tall-interlaced.mkv",
		cfg: both(ceiling(240), deint("bwdif"), bitrate(8000)), handProfile: true})

	// The intended stream map: none handed, one selecting everything, and each selection the
	// keys make.
	add(encoderArgvCase{name: "streams/no-plan", source: "streams.mkv"})
	add(encoderArgvCase{name: "streams/no-plan/probes-itself", source: "streams.mkv", probesItself: true})
	add(encoderArgvCase{name: "streams/plan-everything", source: "streams.mkv", handProfile: true, streamPlan: true})
	sel := []struct {
		name string
		cfg  func(*config.Config)
	}{
		{"audio-eng", langs([]string{"eng"}, nil)},
		{"audio-ENG-fre", langs([]string{"ENG", "fre"}, nil)},
		{"subtitles-eng", langs(nil, []string{"eng"})},
		{"audio-eng/subtitles-eng", langs([]string{"eng"}, []string{"eng"})},
		{"audio-jpn/subtitles-fre", langs([]string{"jpn"}, []string{"fre"})},
		{"commentary-dropped", noCommentary},
		{"commentary-dropped/audio-eng/subtitles-eng", both(noCommentary, langs([]string{"eng"}, []string{"eng"}))},
	}
	for _, s := range sel {
		add(encoderArgvCase{name: "streams/" + s.name, source: "streams.mkv", handProfile: true, streamPlan: true, cfg: s.cfg})
	}
	add(encoderArgvCase{name: "streams/no-audio-left-fallback", source: "jpn-only.mkv", handProfile: true,
		streamPlan: true, cfg: langs([]string{"eng"}, []string{"eng"})})
	add(encoderArgvCase{name: "streams/mov-text", source: "subs.mp4", out: "film.mp4", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "streams/mov-text/no-plan", source: "subs.mp4", out: "film.mp4"})
	add(encoderArgvCase{name: "streams/mov-text/into-mkv", source: "subs.mp4", handProfile: true, streamPlan: true})

	// Remux-only.
	add(encoderArgvCase{name: "remux-only", handProfile: true, streamPlan: true, cfg: remux})
	add(encoderArgvCase{name: "remux-only/audio-eng", source: "streams.mkv", handProfile: true, streamPlan: true,
		cfg: both(remux, langs([]string{"eng"}, nil))})
	add(encoderArgvCase{name: "remux-only/cover-mkv", source: "cover.mkv", handProfile: true, streamPlan: true, cfg: remux})
	add(encoderArgvCase{name: "remux-only/cover-mp4", source: "cover.mp4", out: "film.mp4", handProfile: true,
		streamPlan: true, cfg: remux})
	add(encoderArgvCase{name: "remux-only/pictures-font", source: "pictures-font.mkv", handProfile: true,
		streamPlan: true, cfg: remux})
	add(encoderArgvCase{name: "remux-only/progress", handProfile: true, streamPlan: true, cfg: remux, progress: true})
	add(encoderArgvCase{name: "remux-only/bitrate-8000k", handProfile: true, streamPlan: true, cfg: both(remux, bitrate(8000))})

	// Attached pictures: carried through the map and pinned to copy, or - into Matroska - as
	// attachments each copied out of the source first.
	add(encoderArgvCase{name: "cover-mp4/no-plan", source: "cover.mp4", out: "film.mp4"})
	add(encoderArgvCase{name: "cover-mp4/plan", source: "cover.mp4", out: "film.mp4", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "cover-mp4/plan/into-mkv", source: "cover.mp4", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "cover-mkv/no-plan", source: "cover.mkv"})
	add(encoderArgvCase{name: "cover-mkv/no-plan/probes-itself", source: "cover.mkv", probesItself: true})
	add(encoderArgvCase{name: "cover-mkv/plan", source: "cover.mkv", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "cover-mkv/plan/into-mp4", source: "cover.mkv", out: "film.mp4", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "cover-mkv/plan/progress", source: "cover.mkv", handProfile: true, streamPlan: true, progress: true})
	add(encoderArgvCase{name: "cover-mkv/plan/max-height-120", source: "cover.mkv", handProfile: true, streamPlan: true,
		cfg: ceiling(120)})
	add(encoderArgvCase{name: "pictures/plan", source: "pictures.mkv", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "pictures/no-plan", source: "pictures.mkv"})
	add(encoderArgvCase{name: "pictures-font/plan", source: "pictures-font.mkv", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "pictures-font/plan/audio-eng-fre", source: "pictures-font.mkv", handProfile: true,
		streamPlan: true, cfg: langs([]string{"eng", "fre"}, nil)})
	add(encoderArgvCase{name: "pictures/plan/spaces-in-name", source: "pictures.mkv", out: "A Synthetic Film (2025).mkv",
		handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "pictures/plan/percent-d-in-name", source: "pictures.mkv", out: "The 100%d Club (2025).mkv",
		handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "pictures/plan/long-working-name", source: "pictures.mkv",
		out: strings.Repeat("x", 230) + "." + TempMarker + ".mkv" + TempSuffix, handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "pictures/plan/long-scratch-name", source: "pictures.mkv",
		out: strings.Repeat("x", 220) + ".0123456789ab." + TempMarker + ".mkv", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "cover-bmp-mp4/plan", source: "cover-bmp.mp4", out: "film.mp4", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "cover-bmp-mp4/plan/into-mkv", source: "cover-bmp.mp4", handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "cover-bmp-mp4/no-plan/into-mkv", source: "cover-bmp.mp4"})

	// An MPEG-TS source: ffprobe answers its video-stream question with a program section
	// ahead of the stream, so without an intended stream map the encoder cannot establish
	// whether one of its video streams is a picture, and refuses.
	add(encoderArgvCase{name: "ts-source/no-plan", source: "sdr.ts", out: "film.ts"})
	add(encoderArgvCase{name: "ts-source/plan", source: "sdr.ts", out: "film.ts", handProfile: true, streamPlan: true})

	// Every container this build names, the working names it writes, and the names it refuses.
	for _, ext := range knownContainerExts() {
		add(encoderArgvCase{name: "container-" + ext, out: "film." + ext})
	}
	add(encoderArgvCase{name: "container-mkv/upper-case", out: "film.MKV"})
	add(encoderArgvCase{name: "container-m2ts/upper-case", out: "film.M2TS"})
	add(encoderArgvCase{name: "container-mkv/working-name", out: "film." + TempMarker + ".mkv" + TempSuffix})
	add(encoderArgvCase{name: "container-m2ts/working-name", out: "film." + TempMarker + ".m2ts" + TempSuffix})
	add(encoderArgvCase{name: "container-mp4/working-name-numbered", out: "film." + TempMarker + ".1.mp4" + TempSuffix})
	add(encoderArgvCase{name: "container-mp4/earlier-working-name", out: "film." + TempMarker + ".mp4"})
	add(encoderArgvCase{name: "container-mkv/scratch-name", out: "film.0123456789ab." + TempMarker + ".mkv"})
	add(encoderArgvCase{name: "container-xyz", out: "film.xyz"})
	add(encoderArgvCase{name: "container-divx", out: "film.divx"})
	add(encoderArgvCase{name: "container-none", out: "film"})
	add(encoderArgvCase{name: "container-mp4/from-mkv", source: "sdr.mkv", out: "film.mp4"})
	add(encoderArgvCase{name: "container-mkv/from-mp4", source: "sdr.mp4", out: "film.mkv"})

	// Encode profiles: the first matching one's overrides reach the command line.
	add(encoderArgvCase{name: "encode-profile/matched", cfg: func(c *config.Config) {
		c.EncodeProfiles = []config.EncodeProfile{
			{Name: "unmatched", Match: "*.mp4", CRF: intPtrGolden(40)},
			{Name: "films", Match: "*.mkv", CRF: intPtrGolden(30), Preset: strPtrGolden("fast"),
				PixelFormat: strPtrGolden("yuv420p")},
			{Name: "later", Match: "*.mkv", CRF: intPtrGolden(10)},
		}
	}})
	add(encoderArgvCase{name: "encode-profile/matched/bitrate-3000k", cfg: func(c *config.Config) {
		c.EncodeProfiles = []config.EncodeProfile{{Name: "tv", Match: "*.mkv", BitrateKbps: intPtrGolden(3000)}}
	}})
	add(encoderArgvCase{name: "encode-profile/unmatched", cfg: func(c *config.Config) {
		c.EncodeProfiles = []config.EncodeProfile{{Name: "tv", Match: "*.mp4", CRF: intPtrGolden(40)}}
	}})
	add(encoderArgvCase{name: "encode-profile/to-svtav1", cfg: func(c *config.Config) {
		c.EncodeProfiles = []config.EncodeProfile{{Name: "to-av1", Match: "*.mkv", Encoder: strPtrGolden("svtav1")}}
	}})
	add(encoderArgvCase{name: "encode-profile/to-cpu", cfg: func(c *config.Config) {
		c.EncodeProfiles = []config.EncodeProfile{{Name: "to-hevc", Match: "*.mkv", Encoder: strPtrGolden("cpu"),
			CRF: intPtrGolden(26)}}
	}})

	// Hardware decode: each vendor's decode pipeline before the input, and no change at all for
	// a software encoder. With the colour stamp, a deinterlace and a scale on the frames it
	// downloads, a 10-bit HDR10 source, a target bitrate, the 8-bit plan, and a remux (which
	// decodes nothing).
	hwDecode := func(c *config.Config) { c.HWDecode = config.HWDecodeHardware }
	add(encoderArgvCase{name: "hw-decode", cfg: hwDecode})
	add(encoderArgvCase{name: "hw-decode/stream-plan-handed", cfg: hwDecode, handProfile: true, streamPlan: true})
	add(encoderArgvCase{name: "hw-decode/source-hdr10", source: "hdr10.mkv", cfg: hwDecode})
	add(encoderArgvCase{name: "hw-decode/source-ffv1-pq", source: "ffv1-pq.mkv", cfg: hwDecode})
	add(encoderArgvCase{name: "hw-decode/pixel-format-yuv420p", cfg: both(hwDecode, pixFmt("yuv420p"))})
	add(encoderArgvCase{name: "hw-decode/bitrate-8000k", cfg: both(hwDecode, bitrate(8000))})
	add(encoderArgvCase{name: "hw-decode/deinterlace-yadif/max-height-240", source: "tall-interlaced.mkv",
		cfg: both(hwDecode, deint("yadif"), ceiling(240))})
	add(encoderArgvCase{name: "hw-decode/remux-only", cfg: both(hwDecode, remux)})
	add(encoderArgvCase{name: "hw-decode/software", cfg: func(c *config.Config) { c.HWDecode = config.HWDecodeSoftware }})
	return cs
}

func intPtrGolden(n int) *int       { return &n }
func strPtrGolden(s string) *string { return &s }

// runEncoderArgvCase builds the encoder the case describes, runs it, and renders every
// invocation it made, then the error it returned.
func runEncoderArgvCase(t *testing.T, key string, c encoderArgvCase, stub string, prober *probe.Prober,
	fixtures map[string]*goldenSource, paths goldenPaths) []string {
	t.Helper()
	fx, ok := fixtures[c.source]
	if !ok {
		t.Fatalf("case %q reads fixture %q, which was not made", c.name, c.source)
	}
	src := fx.path
	outDir := t.TempDir()
	out := filepath.Join(outDir, c.out)
	paths = paths.with(outDir, "<out>")

	cfg := config.Config{Encoder: key, CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "source"}
	if c.cfg != nil {
		c.cfg(&cfg)
	}
	rec := &goldenArgvRecorder{}
	base := FFmpegEncoder{FFmpeg: stub, Cfg: cfg, Probe: prober, X265: c.x265, argvObserver: rec.record}
	var enc Encoder = base
	prof := cfg.TopLevelProfile()
	if c.handProfile {
		enc = base.ForProfile(prof)
	}
	ctx := context.Background()
	if c.streamPlan {
		enc = enc.(StreamPlanEncoder).ForStreamPlan(DeriveStreamPlan(fx.streams, prof, fx.codec))
	}
	props := fx.props
	if c.probesItself {
		props = nil
	}
	var sink ProgressSink
	if c.progress {
		sink = func(Progress) {}
	}
	err := enc.(ProgressEncoder).EncodeWithProgress(ctx, src, out, props, sink)
	return renderInvocations(rec.all(), err, paths)
}

// renderInvocations is a case's golden lines: one per invocation, in order, then the error
// the encode returned. A case that ran nothing says so, so an empty block is never a pass.
func renderInvocations(calls [][]string, err error, paths goldenPaths) []string {
	var lines []string
	for _, args := range calls {
		lines = append(lines, goldenRenderArgv(args, paths))
	}
	if len(lines) == 0 {
		lines = append(lines, "(no invocation)")
	}
	if err != nil {
		lines = append(lines, "error: "+paths.apply(err.Error()))
	}
	return lines
}

// encoderMiscCases are the direct calls that name no registry encoder, or build an encoder
// without the prober it needs: each is refused, and the refusal is what is recorded.
func encoderMiscCases(t *testing.T, stub string, prober *probe.Prober, fixtures map[string]*goldenSource,
	paths goldenPaths) *goldenBlocks {
	t.Helper()
	blocks := newGoldenBlocks()
	ctx := context.Background()
	src := fixtures["sdr.mkv"].path
	for _, c := range []struct {
		name       string
		key        string
		prober     *probe.Prober
		probeFirst bool
	}{
		{"encoder-unknown", "not_a_real_encoder", prober, false},
		{"encoder-empty", "", prober, false},
		{"prober-missing", "cpu", nil, false},
		{"prober-missing/probed-first", "cpu", nil, true},
	} {
		outDir := t.TempDir()
		rec := &goldenArgvRecorder{}
		cfg := config.Config{Encoder: c.key, CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "source"}
		enc := FFmpegEncoder{FFmpeg: stub, Cfg: cfg, Probe: c.prober, argvObserver: rec.record}
		var props *probe.VideoProps
		if c.probeFirst {
			props = fixtures["sdr.mkv"].props
		}
		err := enc.EncodeWithProgress(ctx, src, filepath.Join(outDir, "film.mkv"), props, nil)
		blocks.add(t, c.name, renderInvocations(rec.all(), err, paths.with(outDir, "<out>")))
	}
	return blocks
}

// ---- the engine layer -----------------------------------------------------------------

// engineArgvCase is one whole pass of the engine over a library holding one source.
type engineArgvCase struct {
	name string
	// source is the fixture copied into the library root; as is the name it is given there,
	// the fixture's own name when empty.
	source, as string
	// root is YAML the root's own entry carries, one key per line; top is top-level YAML.
	root, top string
	x265      encoder.X265Parallelism
	progress  bool
	scratch   bool
	// core runs the case for every registry encoder; every other case runs for the default
	// encoder (see the note at the top of this file).
	core bool
}

// goldenDefaultEncoder is the encoder the configuration defaults to, which every engine case
// runs for.
const goldenDefaultEncoder = "cpu"

func engineArgvCases() []engineArgvCase {
	var cs []engineArgvCase
	add := func(c engineArgvCase) {
		if c.source == "" {
			c.source = "sdr.mkv"
		}
		cs = append(cs, c)
	}
	x265 := encoder.X265ParallelismFor
	ceiling := func(h int) string {
		return "max_height: " + strconv.Itoa(h) + "\ndownscale_acknowledged: true"
	}

	add(engineArgvCase{name: "base", core: true})
	add(engineArgvCase{name: "crf-28/preset-ultrafast", root: "crf: 28\npreset: ultrafast"})
	add(engineArgvCase{name: "crf-30/preset-fast/pixel-format-yuv420p/container-mp4",
		root: "crf: 30\npreset: fast\npixel_format: yuv420p\ncontainer_ext: mp4"})
	add(engineArgvCase{name: "pixel-format-yuv420p10le", root: "pixel_format: yuv420p10le"})
	add(engineArgvCase{name: "pixel-format-yuv420p", root: "pixel_format: yuv420p"})
	add(engineArgvCase{name: "progress", progress: true, core: true})
	add(engineArgvCase{name: "bitrate-1500k", top: "bitrate_kbps: 1500", core: true})
	add(engineArgvCase{name: "bitrate-8000k/hdr10", source: "hdr10.mkv", top: "bitrate_kbps: 8000", core: true})
	for _, n := range []int{2, 3, 24} {
		add(engineArgvCase{name: "x265-" + strconv.Itoa(n) + "cpu", x265: x265(n)})
	}
	add(engineArgvCase{name: "x265-3cpu/hdr10", source: "hdr10.mkv", x265: x265(3), core: true})
	add(engineArgvCase{name: "x265-3cpu/bitrate-1500k/progress", x265: x265(3), top: "bitrate_kbps: 1500", progress: true})

	// Source containers kept, and container_ext forcing one.
	for _, ext := range []string{"mp4", "ts", "m2ts", "avi", "mov", "m4v", "flv", "wmv"} {
		add(engineArgvCase{name: "container-source-" + ext, source: "sdr." + ext})
	}
	add(engineArgvCase{name: "container-mkv-to-mp4", root: "container_ext: mp4", core: true})
	add(engineArgvCase{name: "container-mp4-to-mkv", source: "sdr.mp4", root: "container_ext: mkv"})
	add(engineArgvCase{name: "container-mkv-to-m2ts", root: "container_ext: m2ts"})

	// Source shapes: colour, depth, chroma, and the ones the guards refuse.
	for _, src := range []string{"bt709", "bt709-vui", "hlg", "pq", "hdr10", "full-range", "10bit", "422", "444", "exotic"} {
		add(engineArgvCase{name: "source-" + src, source: src + ".mkv"})
	}
	// Every chroma and depth a plan carries, through the engine's own guard, for every
	// encoder: the ones an encoder lists no format for are SKIPPED under
	// exotic-pixel-format before anything is encoded, derived or forced.
	for _, src := range []string{"422", "444", "12bit"} {
		add(engineArgvCase{name: "source-" + src + "/every-encoder", source: src + ".mkv", core: true})
	}
	add(engineArgvCase{name: "pixel-format-yuv422p10le/every-encoder", root: "pixel_format: yuv422p10le", core: true})
	add(engineArgvCase{name: "pixel-format-yuv420p/every-encoder", root: "pixel_format: yuv420p", core: true})
	add(engineArgvCase{name: "source-422/remux-only", source: "422.mkv", root: "remux_only: true", core: true})

	// Per-encoder quality, through the configuration file.
	quality := "quality:\n  nvenc: 24\n  av1_nvenc: 40\n  qsv: 25\n  vaapi: 26\n  amf: 27"
	add(engineArgvCase{name: "quality-set", top: quality, core: true})
	add(engineArgvCase{name: "quality-set/encode-profile-crf-30", top: quality +
		"\nencode_profiles:\n  - name: bulk\n    match: \"*.mkv\"\n    crf: 30", core: true})
	add(engineArgvCase{name: "quality-set/encode-profile-to-nvenc", top: quality +
		"\nencode_profiles:\n  - name: gpu\n    match: \"*.mkv\"\n    encoder: nvenc", core: true})
	add(engineArgvCase{name: "crf-0", root: "crf: 0", core: true})
	add(engineArgvCase{name: "source-hdr10/pixel-format-yuv420p10le", source: "hdr10.mkv", root: "pixel_format: yuv420p10le"})

	// Picture operations.
	for _, f := range []string{"yadif", "bwdif", "yadif=send_frame_nospatial"} {
		add(engineArgvCase{name: "deinterlace-" + f, source: "interlaced.mkv", root: "deinterlace: " + f, core: f == "yadif"})
	}
	add(engineArgvCase{name: "deinterlace-yadif/progressive-source", root: "deinterlace: yadif"})
	for _, h := range []int{220, 240} {
		add(engineArgvCase{name: "max-height-" + strconv.Itoa(h), source: "tall.mkv", root: ceiling(h), core: h == 240})
	}
	add(engineArgvCase{name: "max-height-480/at-the-ceiling", source: "tall.mkv", root: ceiling(480)})
	add(engineArgvCase{name: "max-height-240/deinterlace-yadif", source: "tall-interlaced.mkv",
		root: ceiling(240) + "\ndeinterlace: yadif", core: true})
	add(engineArgvCase{name: "max-height-240/bitrate-8000k", source: "tall.mkv", root: ceiling(240), top: "bitrate_kbps: 8000"})

	// Stream selection and remux.
	add(engineArgvCase{name: "streams/everything", source: "streams.mkv"})
	add(engineArgvCase{name: "streams/audio-eng", source: "streams.mkv", root: "audio_languages: [eng]", core: true})
	add(engineArgvCase{name: "streams/audio-ENG-fre", source: "streams.mkv", root: "audio_languages: [ENG, fre]"})
	add(engineArgvCase{name: "streams/subtitles-eng", source: "streams.mkv", root: "subtitle_languages: [eng]"})
	add(engineArgvCase{name: "streams/commentary-dropped/audio-eng/subtitles-eng", source: "streams.mkv",
		root: "keep_commentary: false\naudio_languages: [eng]\nsubtitle_languages: [eng]"})
	add(engineArgvCase{name: "streams/no-audio-left-fallback", source: "jpn-only.mkv",
		root: "audio_languages: [eng]\nsubtitle_languages: [eng]"})
	add(engineArgvCase{name: "streams/mov-text", source: "subs.mp4"})
	add(engineArgvCase{name: "streams/mov-text/into-mkv", source: "subs.mp4", root: "container_ext: mkv"})
	add(engineArgvCase{name: "remux-only", root: "remux_only: true", core: true})
	add(engineArgvCase{name: "remux-only/audio-eng", source: "streams.mkv", root: "remux_only: true\naudio_languages: [eng]"})
	add(engineArgvCase{name: "remux-only/pictures-font", source: "pictures-font.mkv", root: "remux_only: true"})

	// Attached pictures.
	add(engineArgvCase{name: "cover-mp4", source: "cover.mp4", core: true})
	add(engineArgvCase{name: "cover-mp4/into-mkv", source: "cover.mp4", root: "container_ext: mkv"})
	add(engineArgvCase{name: "cover-mkv", source: "cover.mkv"})
	add(engineArgvCase{name: "cover-mkv/into-mp4", source: "cover.mkv", root: "container_ext: mp4"})
	add(engineArgvCase{name: "pictures", source: "pictures.mkv"})
	add(engineArgvCase{name: "pictures-font", source: "pictures-font.mkv"})
	add(engineArgvCase{name: "pictures-font/audio-eng-fre", source: "pictures-font.mkv", root: "audio_languages: [eng, fre]", core: true})
	add(engineArgvCase{name: "pictures/spaces-in-name", source: "pictures.mkv", as: "A Synthetic Film (2025).mkv"})
	add(engineArgvCase{name: "pictures/percent-d-in-name", source: "pictures.mkv", as: "The 100%d Club (2025).mkv"})
	add(engineArgvCase{name: "pictures/long-stem", source: "pictures.mkv", as: strings.Repeat("x", 230) + ".mkv"})
	add(engineArgvCase{name: "cover-bmp-mp4", source: "cover-bmp.mp4"})
	add(engineArgvCase{name: "cover-bmp-mp4/into-mkv", source: "cover-bmp.mp4", root: "container_ext: mkv"})

	// An extension this build knows no container for, offered through video_exts.
	add(engineArgvCase{name: "container-unknown-divx", source: "sdr.avi", as: "sdr.divx", top: "video_exts: [divx]"})

	// Where the encoder writes.
	add(engineArgvCase{name: "scratch", scratch: true})
	add(engineArgvCase{name: "scratch/pictures", source: "pictures.mkv", scratch: true, core: true})

	// Encode profiles and resolution rules.
	add(engineArgvCase{name: "encode-profile/crf-30", top: "encode_profiles:\n  - name: bulk\n    match: \"*.mkv\"\n    crf: 30", core: true})
	add(engineArgvCase{name: "encode-profile/first-match-wins", top: "encode_profiles:\n" +
		"  - name: films\n    match: \"*.mkv\"\n    encoder: svtav1\n    preset: fast\n    crf: 30\n" +
		"  - name: later\n    match: \"*.mkv\"\n    encoder: cpu\n    crf: 10"})
	add(engineArgvCase{name: "encode-profile/bitrate-3000k", top: "encode_profiles:\n  - name: tv\n    match: \"*.mkv\"\n    bitrate_kbps: 3000"})
	add(engineArgvCase{name: "encode-profile/unmatched", top: "encode_profiles:\n  - name: tv\n    match: \"*.mp4\"\n    crf: 40"})
	add(engineArgvCase{name: "rules/band-crf-31", root: "crf: 20\nrules:\n  - when:\n      max_source_height: 576\n    crf: 31", core: true})
	add(engineArgvCase{name: "rules/unmatched-band", root: "crf: 29\nrules:\n  - when:\n      min_source_height: 2160\n    crf: 33"})

	// The codec families, for every encoder: a source in each family and outside them, so
	// each file shows which sources its encoder re-encodes and which it leaves as already at
	// its target or already in a better family. The 8-bit plan of the MPEG-4 source is the one
	// every H.264 hardware encoder carries; the FFV1 PQ source is the 10-bit PQ source an H.264
	// target does not skip. Then the T27 encoders' own quality keys, set.
	add(engineArgvCase{name: "source-mpeg4/every-encoder", source: "mpeg4.mkv", core: true})
	add(engineArgvCase{name: "source-mpeg4/pixel-format-yuv420p/every-encoder", source: "mpeg4.mkv",
		root: "pixel_format: yuv420p", core: true})
	add(engineArgvCase{name: "source-hevc/every-encoder", source: "hevc.mkv", core: true})
	add(engineArgvCase{name: "source-av1/every-encoder", source: "av1.mkv", core: true})
	add(engineArgvCase{name: "source-ffv1-pq/every-encoder", source: "ffv1-pq.mkv", core: true})
	add(engineArgvCase{name: "source-mpeg4/quality-set-t27", source: "mpeg4.mkv", root: "pixel_format: yuv420p",
		top: "quality:\n  h264_nvenc: 28\n  h264_qsv: 29\n  h264_vaapi: 31\n  h264_amf: 32\n" +
			"  av1_qsv: 33\n  av1_vaapi: 120\n  av1_amf: 130", core: true})

	// Hardware decode through the engine's own resolution: a root's hw_decode, a top-level one
	// a root inherits, one a root turns off, and the picture operations on downloaded frames.
	add(engineArgvCase{name: "hw-decode/source-ffv1-pq/every-encoder", source: "ffv1-pq.mkv",
		root: "hw_decode: hardware", core: true})
	add(engineArgvCase{name: "hw-decode/source-mpeg4/pixel-format-yuv420p/every-encoder", source: "mpeg4.mkv",
		root: "hw_decode: hardware\npixel_format: yuv420p", core: true})
	add(engineArgvCase{name: "hw-decode/top-level/source-hdr10", source: "hdr10.mkv", top: "hw_decode: hardware", core: true})
	add(engineArgvCase{name: "hw-decode/top-level/root-software", top: "hw_decode: hardware",
		root: "hw_decode: software", core: true})
	add(engineArgvCase{name: "hw-decode/max-height-240/deinterlace-yadif", source: "tall-interlaced.mkv",
		root: ceiling(240) + "\ndeinterlace: yadif\nhw_decode: hardware", core: true})
	add(engineArgvCase{name: "hw-decode/remux-only", root: "remux_only: true\nhw_decode: hardware", core: true})
	return cs
}

// engineArgvYAML is the configuration one engine case runs under: the root and its own keys,
// the registry encoder under test, and the settings every case shares - nothing is held back
// for being small and the perceptual gate is off, since the stand-in encodes nothing to score.
func engineArgvYAML(key, root, scratch string, c engineArgvCase) string {
	var b strings.Builder
	b.WriteString("library_roots:\n  - path: " + root + "\n")
	for _, l := range strings.Split(c.root, "\n") {
		if l != "" {
			b.WriteString("    " + l + "\n")
		}
	}
	b.WriteString("encoder: " + key + "\nmin_bitrate_kbps: 0\nvmaf_enable: false\n")
	if scratch != "" {
		b.WriteString("scratch_dir: " + scratch + "\n")
	}
	if c.top != "" {
		b.WriteString(c.top + "\n")
	}
	return b.String()
}

// runEngineArgvCase runs one pass and renders every invocation the production encoder made,
// then the row the pass recorded for the source: a case whose source never reached the
// encoder says why, so it cannot pass by building nothing.
//
// Every case of a layer shares one ledger. Each case's library is its own directory, so its
// source is a path no other case's row names, and nothing a pass reads from the ledger about
// OTHER paths reaches a command line; a ledger per case would cost a store's opening, which
// under the race detector is most of what a case costs.
func runEngineArgvCase(t *testing.T, key string, c engineArgvCase, stub, ffmpeg, ffprobe string,
	fixtures map[string]*goldenSource, st *testStore, paths goldenPaths) []string {
	t.Helper()
	fx, ok := fixtures[c.source]
	if !ok {
		t.Fatalf("case %q reads fixture %q, which was not made", c.name, c.source)
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "library")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	name := c.as
	if name == "" {
		name = c.source
	}
	src := filepath.Join(root, name)
	data, err := os.ReadFile(fx.path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	var scratch string
	if c.scratch {
		scratch = filepath.Join(dir, "scratch")
		if err := os.Mkdir(scratch, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	cfg := profileCfg(t, engineArgvYAML(key, root, scratch, c))

	prober := probe.New(ffmpeg, ffprobe)
	rec := &goldenArgvRecorder{}
	enc := FFmpegEncoder{FFmpeg: stub, Cfg: cfg, Probe: prober, X265: c.x265, argvObserver: rec.record}
	eng := New(cfg, prober, enc, st, discardLogger())
	if c.progress {
		eng.Observer = func(Event) {}
	}
	runErr := eng.RunOneshot(context.Background())

	paths = paths.with(root, "<library>").with(sourceTag(src), "<source-tag>")
	if scratch != "" {
		paths = paths.with(scratch, "<scratch>")
	}
	lines := renderInvocations(rec.all(), runErr, paths)
	if out, status, found := outcomeFor(t, st, src); found {
		lines = append(lines, "row: "+string(status)+" "+paths.apply(out.Reason))
	} else {
		lines = append(lines, "row: none")
	}
	return lines
}

// ---- the tests ------------------------------------------------------------------------

// TestGoldenArgv grades every case of both layers, for every registry encoder, against the
// golden files the build before the encode plan wrote.
func TestGoldenArgv(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	fxDir := t.TempDir()
	prober := probe.New(ffmpeg, ffprobe)
	fixtures := goldenSources(t, prober, goldenFixtures(t, ffmpeg, ffprobe, fxDir))
	stub := goldenStub(t)
	paths := goldenPaths{}.with(fxDir, "<fixtures>")

	t.Run("encoder", func(t *testing.T) {
		for _, key := range goldenEncoderKeys() {
			blocks := newGoldenBlocks()
			for _, c := range encoderArgvCases() {
				blocks.add(t, c.name, runEncoderArgvCase(t, key, c, stub, prober, fixtures, paths))
			}
			// The raw ffmpeg codec name the configuration accepts in place of the key must
			// build exactly what the key builds; its cases sit in the key's own file.
			spec, _ := encoder.Lookup(key)
			for _, c := range []encoderArgvCase{{name: "alias"}, {name: "alias/stream-plan-handed", handProfile: true,
				streamPlan: true}, {name: "alias/probes-itself", probesItself: true}} {
				c.source, c.out = "sdr.mkv", "film.mkv"
				c.name += "-" + spec.FFmpegCodec
				blocks.add(t, c.name, runEncoderArgvCase(t, spec.FFmpegCodec, c, stub, prober, fixtures, paths))
			}
			checkGolden(t, filepath.Join(goldenArgvDir, "encoder-"+key+".txt"), blocks)
		}
		checkGolden(t, filepath.Join(goldenArgvDir, "encoder-none.txt"),
			encoderMiscCases(t, stub, prober, fixtures, paths))
	})

	t.Run("engine", func(t *testing.T) {
		st := newTestStore(t, t.TempDir())
		for _, key := range goldenEncoderKeys() {
			blocks := newGoldenBlocks()
			for _, c := range engineArgvCases() {
				if !c.core && key != goldenDefaultEncoder {
					continue
				}
				blocks.add(t, c.name, runEngineArgvCase(t, key, c, stub, ffmpeg, ffprobe, fixtures, st, paths))
			}
			checkGolden(t, filepath.Join(goldenArgvDir, "engine-"+key+".txt"), blocks)
		}
	})
}

// TestGoldenArgv_CoversEveryRegistryEncoder is the anti-vacuity half: every encoder the
// registry ships has a golden file in both layers, and no golden file stands for an encoder
// the registry does not ship or a file nothing grades. An encoder added to the registry
// without its golden files reds here rather than going ungraded.
func TestGoldenArgv_CoversEveryRegistryEncoder(t *testing.T) {
	want := map[string]bool{"encoder-none.txt": true}
	for _, key := range goldenEncoderKeys() {
		want["encoder-"+key+".txt"] = true
		want["engine-"+key+".txt"] = true
	}
	entries, err := os.ReadDir(goldenArgvDir)
	if err != nil {
		t.Fatalf("read %s: %v", goldenArgvDir, err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
		if !want[e.Name()] {
			t.Errorf("%s/%s grades nothing: it names no encoder the registry ships", goldenArgvDir, e.Name())
		}
	}
	for name := range want {
		if !got[name] {
			t.Errorf("%s/%s is missing: an encoder in the registry has no golden command lines", goldenArgvDir, name)
		}
	}
}

// TestGoldenArgv_EveryArgvNamesItsPixelFormat reads every command line the golden files
// record - which TestGoldenArgv proves is what this build assembles - and holds every one
// that re-encodes the video to the explicit-pixel-format rule: it names the format the encoder
// is handed, and that format is one the encoder lists. For every encoder but VAAPI that is
// exactly one `-pix_fmt X` with X in the encoder's "Supported pixel formats" line (the
// registry's copy, which internal/encoder re-reads from the pinned binary); for VAAPI, which
// lists only hardware surfaces, it is exactly one `format=X,hwupload` chain with X one of the
// formats this build uploads, and NO -pix_fmt, which would contradict the chain. A command
// line that leaves the format to ffmpeg's auto-selection fails here.
//
// It is walked from the files rather than from a list of cases, so a case added later is held
// to the rule the moment it is recorded; and each file must hold at least one re-encode, so
// the test cannot pass by finding nothing to check.
func TestGoldenArgv_EveryArgvNamesItsPixelFormat(t *testing.T) {
	entries, err := os.ReadDir(goldenArgvDir)
	if err != nil {
		t.Fatalf("read %s: %v", goldenArgvDir, err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(goldenArgvDir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		blocks, err := parseGoldenBlocks(data)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		checked := 0
		for _, name := range blocks.order {
			for _, line := range blocks.lines[name] {
				if !strings.HasPrefix(line, "-hide_banner") {
					continue
				}
				args := strings.Fields(line)
				codec := goldenValueAfter(args, "-c:v")
				if codec == "" || codec == "copy" {
					continue // a stream copy hands no encoder anything
				}
				spec, ok := encoder.Lookup(codec)
				if !ok {
					t.Errorf("%s: case %q runs -c:v %q, which is no registry encoder", e.Name(), name, codec)
					continue
				}
				checked++
				if msg := explicitPixelFormatProblem(spec, args); msg != "" {
					t.Errorf("%s: case %q (%s): %s\n  %s", e.Name(), name, spec.FFmpegCodec, msg, line)
				}
			}
		}
		if checked == 0 && e.Name() != "encoder-none.txt" {
			t.Errorf("%s records no re-encode at all: nothing was held to the explicit-pixel-format rule", e.Name())
		}
	}
}

// explicitPixelFormatProblem says what is wrong with one re-encoding command line's pixel
// format, or "" when it names one its encoder lists, exactly once.
func explicitPixelFormatProblem(spec encoder.Spec, args []string) string {
	var pixFmts, uploads []string
	for i, a := range args {
		if a == "-pix_fmt" && i+1 < len(args) {
			pixFmts = append(pixFmts, args[i+1])
		}
		if a == "-vf" && i+1 < len(args) {
			for _, f := range strings.Split(args[i+1], ",") {
				if strings.HasPrefix(f, "format=") {
					uploads = append(uploads, strings.TrimPrefix(f, "format="))
				}
			}
		}
	}
	if spec.Uploads() {
		switch {
		case len(pixFmts) != 0:
			return fmt.Sprintf("a -pix_fmt %v beside an upload chain", pixFmts)
		case len(uploads) != 1:
			return fmt.Sprintf("want exactly one uploaded format, got %v", uploads)
		case !strings.Contains(goldenValueAfter(args, "-vf"), "format="+uploads[0]+",hwupload"):
			return "the uploaded format is not what hwupload receives"
		case !goldenListed(spec.UploadFormats, uploads[0]):
			return fmt.Sprintf("uploads %q, which is not one of %q", uploads[0], spec.UploadFormats)
		}
		return ""
	}
	switch {
	case len(pixFmts) != 1:
		return fmt.Sprintf("want exactly one -pix_fmt, got %v", pixFmts)
	case !goldenListed(spec.PixelFormats, pixFmts[0]):
		return fmt.Sprintf("-pix_fmt %q is not in the encoder's list %q", pixFmts[0], spec.PixelFormats)
	}
	return ""
}

func goldenValueAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func goldenListed(list, f string) bool {
	for _, l := range strings.Fields(list) {
		if l == f {
			return true
		}
	}
	return false
}
