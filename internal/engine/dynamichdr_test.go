package engine

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/dynhdr"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// DYNAMIC HDR THROUGH THE ENGINE (docs/design/dynamic-hdr.md). The tools' arithmetic and the
// gates' real-file fixtures live in internal/dynhdr, beside the code; these are the few real
// encodes that prove the ENGINE path - guard, pre-pass, plan, command line, gate, swap - and
// the decisions that must not move.
//
// The sources are synthetic: a lavfi pattern, 320x240, 24 frames, an RPU from `dovi_tool
// generate` injected and re-encoded with `-dolbyvision 1` into Matroska, HDR10+ from a
// one-scene JSON (the recipe of proposal-crop-dv.md, "Test plan").
//
// THE CODEC MASK. Every Dolby Vision profile 7 and 8 source is HEVC, and the cpu encoder's
// target is HEVC, so the already-at-target-codec guard - which runs before the HDR guard -
// skips every such source before dynamic HDR is ever asked about (docs/design/dynamic-hdr.md#reach).
// To drive the carriage at all, the snapshot probe the guards read is answered by a stand-in
// ffprobe that reports the source's codec as h264 and passes every other probe to the real
// ffprobe unchanged: the plan, the pre-pass, the encode and every gate read the real file.

const dvMasterDisplay = "G(13250,34500)B(7500,3000)R(34000,16000)WP(15635,16450)L(10000000,1)"

// dvTools are the two dynamic-HDR tools, failing the test where either is missing.
func dvTools(t *testing.T) (dovi, plus string) {
	t.Helper()
	dovi, plus = envOr(dynhdr.EnvDoviTool, dynhdr.DefaultDoviTool), envOr(dynhdr.EnvHDR10PlusTool, dynhdr.DefaultHDR10PlusTool)
	for _, b := range []string{dovi, plus} {
		if _, err := exec.LookPath(b); err != nil {
			t.Fatalf("%q not found - the dynamic-HDR proof requires dovi_tool and hdr10plus_tool (set %s/%s): %v",
				b, dynhdr.EnvDoviTool, dynhdr.EnvHDR10PlusTool, err)
		}
	}
	return dovi, plus
}

// dvColour is the fixtures' PQ colour description and static metadata in x265's spelling.
const dvColour = "log-level=error:pools=2:colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:master-display=" +
	dvMasterDisplay + ":max-cll=1000,400:hdr10-opt=1:repeat-headers=1"

const dvHDR10PlusJSON = `{"JSONInfo":{"HDR10plusProfile":"B","Version":"1.0"},"SceneInfo":[{"BezierCurveData":` +
	`{"Anchors":[256,512,768,1023,1023,1023,1023,1023,1023],"KneePointX":0,"KneePointY":0},"LuminanceParameters":` +
	`{"AverageRGB":1000,"LuminanceDistributions":{"DistributionIndex":[1,5,10,25,50,75,90,95,99],` +
	`"DistributionValues":[0,100,200,300,400,500,600,700,800]},"MaxScl":[4000,4000,4000]},"NumberOfWindows":1,` +
	`"TargetedSystemDisplayMaximumLuminance":400,"SceneFrameIndex":0,"SceneId":0,"SequenceFrameIndex":0}],` +
	`"SceneInfoSummary":{"SceneFirstFrameIndex":[0],"SceneFrameNumbers":[24]},"ToolInfo":{"Tool":"test","Version":"0"}}`

// mkDynamicSource writes path: profile 8.1 Dolby Vision (dv), HDR10+ (plus), or both, with a
// FLAC audio track, encoded at a low crf so any honest encode of it is smaller.
func mkDynamicSource(t *testing.T, path string, dv, plus bool) {
	t.Helper()
	ffmpeg, _ := tools(t)
	dovi, _ := dvTools(t)
	dir := t.TempDir()
	params := dvColour
	if plus {
		j := filepath.Join(dir, "src.json")
		if err := os.WriteFile(j, []byte(dvHDR10PlusJSON), 0o644); err != nil {
			t.Fatal(err)
		}
		params += ":dhdr10-info=" + j
	}
	if !dv {
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=320x240:r=24:d=1",
			"-f", "lavfi", "-i", "sine=d=1", "-c:a", "flac", "-pix_fmt", "yuv420p10le", "-c:v", "libx265",
			"-threads", "2", "-crf", "8", "-x265-params", params, "--", path)
		return
	}
	base, rpu, inj := filepath.Join(dir, "b.hevc"), filepath.Join(dir, "r.bin"), filepath.Join(dir, "i.hevc")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=320x240:r=24:d=1",
		"-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-threads", "2", "-x265-params", params, base)
	gen := filepath.Join(dir, "g.json")
	if err := os.WriteFile(gen, []byte(`{"cm_version":"V40","length":24,"level6":{"max_display_mastering_luminance":1000,`+
		`"min_display_mastering_luminance":1,"max_content_light_level":1000,"max_frame_average_light_level":400}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"generate", "-j", gen, "-o", rpu}, {"inject-rpu", "-i", base, "--rpu-in", rpu, "-o", inj}} {
		if out, err := exec.Command(dovi, args...).CombinedOutput(); err != nil {
			t.Fatalf("dovi_tool %v: %v\n%s", args, err, out)
		}
	}
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "hevc", "-framerate", "24", "-i", inj,
		"-f", "lavfi", "-i", "sine=d=1", "-map", "0:v", "-map", "1:a", "-c:a", "flac",
		"-c:v", "libx265", "-dolbyvision", "1", "-threads", "2", "-pix_fmt", "yuv420p10le", "-crf", "8",
		"-x265-params", params+":vbv-maxrate=20000:vbv-bufsize=20000", "--", path)
}

// asDolbyVisionProfile7 rewrites the source's DOVI configuration record to profile 7 with an
// enhancement layer and compatibility id 6 (dovi_tool cannot generate a profile 7 RPU; see
// internal/dynhdr's asProfile7 for the layout).
func asDolbyVisionProfile7(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	p81 := []byte{1, 0, 8 << 1, 1<<3 | 1<<2 | 1, 1 << 4}
	if bytes.Count(data, p81) != 1 {
		t.Fatalf("the source carries %d profile 8.1 records, want 1", bytes.Count(data, p81))
	}
	copy(data[bytes.Index(data, p81):], []byte{1, 0, 7 << 1, 1<<3 | 1<<2 | 1<<1 | 1, 6 << 4})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// codecMaskingFFprobe is the stand-in ffprobe described above: the guards' snapshot probe (the
// one asking for format_name) reports the video codec as h264, and every other invocation is
// the real ffprobe's.
func codecMaskingFFprobe(t *testing.T, real string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffprobe-codec-mask")
	script := "#!/bin/sh\ncase \"$*\" in\n*format=duration,format_name*) \"" + real +
		"\" \"$@\" | sed 's/^codec_name=hevc$/codec_name=h264/'; exit 0;;\nesac\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// dynamicRun is one oneshot pass over a library holding src, through holdfast's own encoder
// (or the ffmpeg binary encFFmpeg where it is not ""), with the snapshot masked. It returns
// the store and every ffmpeg invocation the encoder made.
func dynamicRun(t *testing.T, root string, mutate func(*config.Config), encFFmpeg string, setup func(*Engine)) (*testStore, [][]string) {
	t.Helper()
	return dynamicRunMasked(t, root, mutate, encFFmpeg, setup, true)
}

// dynamicRunMasked is dynamicRun with the codec mask on or off.
func dynamicRunMasked(t *testing.T, root string, mutate func(*config.Config), encFFmpeg string, setup func(*Engine),
	mask bool) (*testStore, [][]string) {
	t.Helper()
	ffmpeg, realProbe := tools(t)
	cfg := baseCfg(root)
	cfg.CRF = 30
	cfg.PixelFormat = "auto"
	if mutate != nil {
		mutate(&cfg)
	}
	st := newTestStore(t, root)
	ffprobe := realProbe
	if mask {
		ffprobe = codecMaskingFFprobe(t, realProbe)
	}
	prober := probe.New(ffmpeg, ffprobe)
	if encFFmpeg == "" {
		encFFmpeg = ffmpeg
	}
	rec := &goldenArgvRecorder{}
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: encFFmpeg, Cfg: cfg, Probe: prober, argvObserver: rec.record}, st, discardLogger())
	eng.DoviTool, eng.HDR10PlusTool = dvTools(t)
	if setup != nil {
		setup(eng)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return st, rec.all()
}

// requireCarried asserts the file at path is a replacement carrying what want declares: the
// gate's own reading of it, so "carried" means what the gate means.
func requireCarried(t *testing.T, path string, want dynhdr.Expectation) {
	t.Helper()
	_, ffprobe := tools(t)
	c, err := dynhdr.Count(context.Background(), ffprobe, path)
	if err != nil {
		t.Fatal(err)
	}
	rec := dynhdr.RecordFrom(probe.New("ffmpeg", ffprobe).OutputFacts(context.Background(), path).SideData)
	if err := dynhdr.Check(want, rec, c); err != nil {
		t.Fatalf("the replacement does not carry what was planned: %v (counts %+v, record %+v)", err, c, rec)
	}
	if c.Frames != 24 {
		t.Errorf("the replacement has %d frames, want 24", c.Frames)
	}
}

func requireNoDynamicTemps(t *testing.T, dir string) {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".dynhdr") || isTempName(e.Name()) {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}

func argvWith(calls [][]string, flag string) []string {
	for _, c := range calls {
		if strings.Contains(strings.Join(c, " "), flag) {
			return c
		}
	}
	return nil
}

// PROFILE 8.1 IS CARRIED by default on the cpu encoder: the job is done, the replacement
// carries profile 8 compatibility id 1 with an RPU on all 24 frames, its command line set
// -dolbyvision 1 and the derived level-2 ceiling, and no pre-pass file is left.
func TestDynamicHDR_Profile81IsCarriedThroughTheEngine(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDynamicSource(t, src, true, false)
	st, calls := dynamicRun(t, root, nil, "", nil)
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
	}
	argv := strings.Join(argvWith(calls, "libx265"), " ")
	for _, want := range []string{"-dolbyvision 1", ":vbv-maxrate=1500:vbv-bufsize=1500", "master-display=" + dvMasterDisplay} {
		if !strings.Contains(argv, want) {
			t.Errorf("the encode's command line lacks %q: %s", want, argv)
		}
	}
	if strings.Contains(argv, "dolby-vision-rpu") || strings.Contains(argv, "dhdr10-info") {
		t.Errorf("the command line carries an option it must not: %s", argv)
	}
	requireCarried(t, src, dynhdr.Expectation{DolbyVision: true, Profile: 8, CompatID: 1})
	requireNoDynamicTemps(t, root)
}

// HDR10+ IS CARRIED by default on the cpu encoder: extracted by hdr10plus_tool, handed to
// libx265 as dhdr10-info, and on every frame of the replacement.
func TestDynamicHDR_HDR10PlusIsCarriedThroughTheEngine(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDynamicSource(t, src, false, true)
	st, calls := dynamicRun(t, root, nil, "", nil)
	if row := rowForFile(t, st, "movie.mkv"); row.Status != store.Done {
		t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
	}
	argv := strings.Join(argvWith(calls, "libx265"), " ")
	if !strings.Contains(argv, ":dhdr10-info=") || !strings.Contains(argv, ".dynhdr0.json") || strings.Contains(argv, "-dolbyvision") {
		t.Errorf("the HDR10+ command line: %s", argv)
	}
	requireCarried(t, src, dynhdr.Expectation{HDR10Plus: true})
	requireNoDynamicTemps(t, root)
}

// PROFILE 7, OPT-IN. Under the default the source is skipped as dolby-vision-profile-7 and the
// row records dolby_vision_p7=skip, so turning conversion on re-opens it; under convert it is
// converted by dovi_tool, encoded from the raw stream at the source's exact rate with the
// audio mapped from the original, and the replacement is profile 8.1 on every frame.
func TestDynamicHDR_Profile7IsSkippedByDefaultAndConvertedWhenOptedIn(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDynamicSource(t, src, true, false)
	asDolbyVisionProfile7(t, src)
	before := md5f(t, src)

	st, calls := dynamicRun(t, root, nil, "", nil)
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Skipped || row.Outcome.Reason != SkipDolbyVisionProfile7 || len(calls) != 0 {
		t.Fatalf("under the default: %s %q with %d encode(s), want skipped %s and none",
			row.Status, row.Outcome.Reason, len(calls), SkipDolbyVisionProfile7)
	}
	if v, ok := row.Outcome.DecisionInputs.Value(InputDolbyVisionP7); !ok || v != config.DolbyVisionP7Skip {
		t.Errorf("the row records %s=%q (%v), want skip", InputDolbyVisionP7, v, ok)
	}
	if got := strings.Join(row.Outcome.DecisionInputs.Keys(), ","); got != InputDolbyVisionP7+","+InputEncoder {
		t.Errorf("the row records %s, want exactly dolby_vision_p7 and encoder", got)
	}
	if md5f(t, src) != before {
		t.Fatal("a skipped source changed")
	}
	convert := baseCfg(root)
	convert.CRF, convert.PixelFormat, convert.DolbyVisionP7 = 30, "auto", config.DolbyVisionP7Convert
	if row.Outcome.DecisionInputs.StillMatches(DecisionInputsFor(convert)) {
		t.Fatal("turning conversion on does not re-open the profile 7 row")
	}

	root2 := t.TempDir()
	src2 := filepath.Join(root2, "movie.mkv")
	if err := os.WriteFile(src2, mustRead(t, src), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, calls2 := dynamicRun(t, root2, func(c *config.Config) { c.DolbyVisionP7 = config.DolbyVisionP7Convert }, "", nil)
	if row := rowForFile(t, st2, "movie.mkv"); row.Status != store.Done {
		t.Fatalf("under convert: %s %q, want done", row.Status, row.Outcome.Reason)
	}
	argv := argvWith(calls2, "libx265")
	joined := strings.Join(argv, " ")
	for _, want := range []string{"-f hevc -framerate 24/1 -i ", ".dynhdr1 -map 1:v:0 -map 0:1", "-dolbyvision 1"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the conversion's command line lacks %q: %s", want, joined)
		}
	}
	requireCarried(t, src2, dynhdr.Expectation{DolbyVision: true, Profile: 8, CompatID: 1})
	requireNoDynamicTemps(t, root2)
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dropDolbyVisionFFmpeg is a stand-in ffmpeg that runs the real one with `-dolbyvision 1`
// taken out of its command line: an encoder that silently drops the RPU, which is the loss
// the gate exists to catch.
func dropDolbyVisionFFmpeg(t *testing.T, real string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ffmpeg-drops-dv")
	script := "#!/bin/sh\nfor a; do shift; if [ \"$skip\" = 1 ]; then skip=; continue; fi; " +
		"if [ \"$a\" = -dolbyvision ]; then skip=1; continue; fi; set -- \"$@\" \"$a\"; done\nexec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// THE GATE IS WIRED: an encode that drops the RPU is refused at the record gate, the job
// fails deterministically there, the source is byte-identical, and nothing is left beside it.
func TestDynamicHDR_AnEncodeThatDropsTheRPUIsRefusedAndTheSourceKept(t *testing.T) {
	ffmpeg, _ := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDynamicSource(t, src, true, false)
	before := md5f(t, src)
	st, _ := dynamicRun(t, root, nil, dropDolbyVisionFFmpeg(t, ffmpeg), nil)
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Failed || !strings.Contains(row.Outcome.Reason, "no DOVI configuration record") ||
		row.Outcome.FailureClass != store.FailureDeterministic {
		t.Fatalf("row %s %q %s, want a deterministic failure at the record gate", row.Status, row.Outcome.Reason, row.Outcome.FailureClass)
	}
	if md5f(t, src) != before {
		t.Fatal("the source changed")
	}
	requireNoDynamicTemps(t, root)
}

// EVERY OTHER ENCODER, AND REMUX-ONLY, SKIPS EXACTLY AS BEFORE: a profile 8.1 source and an
// HDR10+ source under every registry encoder but cpu, and under a remux-only root, are
// skipped dolby-vision and hdr10-plus with nothing encoded. An encoder writing H.264 never
// reaches the HDR guard with these HEVC sources - they are skipped better-codec-family first,
// as they always were - so those encoders are graded unmasked, on the real codec.
func TestDynamicHDR_EveryOtherEncoderAndRemuxStillSkip(t *testing.T) {
	fixtures := t.TempDir()
	dv, plus := filepath.Join(fixtures, "dv.mkv"), filepath.Join(fixtures, "plus.mkv")
	mkDynamicSource(t, dv, true, false)
	mkDynamicSource(t, plus, false, true)
	stub := goldenStub(t)
	cases := map[string]func(*config.Config){}
	h264 := map[string]bool{}
	for _, key := range encoder.Known() {
		if key == "cpu" {
			continue
		}
		k := key
		cases["encoder "+k] = func(c *config.Config) { c.Encoder = k }
		h264["encoder "+k] = targetCodecFor(k) == "h264"
	}
	cases["remux-only cpu"] = func(c *config.Config) { c.RemuxOnly = boolPtr(true) }
	for name, mutate := range cases {
		for fx, want := range map[string]string{dv: SkipDolbyVision, plus: SkipHDR10Plus} {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "movie.mkv"), mustRead(t, fx), 0o644); err != nil {
				t.Fatal(err)
			}
			if h264[name] {
				want = SkipBetterCodecFamily
			}
			st, calls := dynamicRunMasked(t, root, mutate, stub, nil, !h264[name])
			row := rowForFile(t, st, "movie.mkv")
			if row.Status != store.Skipped || row.Outcome.Reason != want || len(calls) != 0 {
				t.Errorf("%s, %s: %s %q with %d encode(s), want skipped %s and none",
					name, filepath.Base(fx), row.Status, row.Outcome.Reason, len(calls), want)
			}
		}
	}
}

// A MISSING TOOL SKIPS, AND IS MUTABLE: an HDR10+ source on a host without hdr10plus_tool is
// skipped dynamic-hdr-tool-missing with nothing encoded, and the next pass that finds the tool
// takes the file again (here a dry run, which records what a run would do).
func TestDynamicHDR_AMissingToolSkipsUntilTheToolIsThere(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDynamicSource(t, src, false, true)
	before := md5f(t, src)
	ffmpeg, realProbe := tools(t)
	cfg := baseCfg(root)
	cfg.PixelFormat = "auto"
	st := newTestStore(t, root)
	prober := probe.New(ffmpeg, codecMaskingFFprobe(t, realProbe))
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: goldenStub(t), Cfg: cfg, Probe: prober}, st, discardLogger())
	eng.HDR10PlusTool = filepath.Join(t.TempDir(), "hdr10plus_tool-not-installed")
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if row := rowForFile(t, st, "movie.mkv"); row.Status != store.Skipped || row.Outcome.Reason != SkipDynamicHDRToolMissing {
		t.Fatalf("without the tool: %s %q", row.Status, row.Outcome.Reason)
	}
	cfg.DryRun = true
	_, plus := dvTools(t)
	eng = New(cfg, prober, FFmpegEncoder{FFmpeg: goldenStub(t), Cfg: cfg, Probe: prober}, st, discardLogger())
	eng.HDR10PlusTool = plus
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	if row := rowForFile(t, st, "movie.mkv"); row.Status != store.WouldTranscode {
		t.Fatalf("with the tool: %s %q, want the file taken again", row.Status, row.Outcome.Reason)
	}
	if md5f(t, src) != before {
		t.Fatal("the source changed")
	}
}

// METADATA THAT DOES NOT VALIDATE SKIPS: an hdr10plus_tool that writes one entry for a
// 24-frame source fails the validation, the file is skipped hdr10-plus-unreadable with
// nothing encoded, and the extracted file is removed.
func TestDynamicHDR_UnvalidatedHDR10PlusSkipsAndLeavesNothing(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDynamicSource(t, src, false, true)
	before := md5f(t, src)
	short := filepath.Join(t.TempDir(), "hdr10plus_tool-short")
	if err := os.WriteFile(short, []byte("#!/bin/sh\ncat >/dev/null; while [ \"$1\" != -o ]; do shift; done; "+
		"printf '{\"JSONInfo\":{},\"SceneInfo\":[{}]}' > \"$2\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, calls := dynamicRun(t, root, nil, "", func(e *Engine) { e.HDR10PlusTool = short })
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Skipped || row.Outcome.Reason != SkipHDR10PlusUnreadable || len(calls) != 0 {
		t.Fatalf("%s %q with %d encode(s), want skipped %s and none", row.Status, row.Outcome.Reason, len(calls), SkipHDR10PlusUnreadable)
	}
	if md5f(t, src) != before {
		t.Fatal("the source changed")
	}
	requireNoDynamicTemps(t, root)
}

// dynamicGoldenCases are the golden command lines of the new paths - profile 8.1, HDR10+,
// both, and profile 7 converted - through the engine with a stand-in encoder binary and the
// codec mask. TestGoldenArgv appends them to the default encoder's engine file, after every
// existing case, so every existing golden line stays byte-identical and these are graded
// exactly as those are.
func dynamicGoldenCases(t *testing.T, stub string, add func(name string, lines []string)) {
	t.Helper()
	fixtures := t.TempDir()
	dv, plus, both := filepath.Join(fixtures, "dv.mkv"), filepath.Join(fixtures, "plus.mkv"), filepath.Join(fixtures, "both.mkv")
	mkDynamicSource(t, dv, true, false)
	mkDynamicSource(t, plus, false, true)
	mkDynamicSource(t, both, true, true)
	p7 := filepath.Join(fixtures, "p7.mkv")
	if err := os.WriteFile(p7, mustRead(t, dv), 0o644); err != nil {
		t.Fatal(err)
	}
	asDolbyVisionProfile7(t, p7)
	for _, c := range []struct {
		name, fixture string
		mutate        func(*config.Config)
	}{
		{"dynamic-hdr/dolby-vision-8.1", dv, nil},
		{"dynamic-hdr/hdr10-plus", plus, nil},
		{"dynamic-hdr/dolby-vision-8.1-and-hdr10-plus", both, nil},
		{"dynamic-hdr/dolby-vision-7-converted", p7, func(c *config.Config) { c.DolbyVisionP7 = config.DolbyVisionP7Convert }},
	} {
		root := t.TempDir()
		src := filepath.Join(root, "film.mkv")
		if err := os.WriteFile(src, mustRead(t, c.fixture), 0o644); err != nil {
			t.Fatal(err)
		}
		st, calls := dynamicRun(t, root, c.mutate, stub, nil)
		paths := goldenPaths{}.with(root, "<library>").with(sourceTag(src), "<source-tag>")
		lines := renderInvocations(calls, nil, paths)
		if out, status, found := outcomeFor(t, st, src); found {
			lines = append(lines, "row: "+string(status)+" "+paths.apply(out.Reason))
		}
		add(c.name, lines)
	}
}

// derivePlanCarrying derives src's plan under cfg with the pre-pass d handed in, as the engine
// does, and returns it with the derivation's error.
func derivePlanCarrying(t *testing.T, cfg config.Config, src, out string, d *dynhdr.Prepared, noMap bool) (*EncodePlan, error) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	prober := probe.New(ffmpeg, ffprobe)
	ctx := context.Background()
	streams, ok := prober.Streams(ctx, src)
	if !ok {
		t.Fatalf("ffprobe could not enumerate the streams of %s", src)
	}
	props := prober.VideoProps(ctx, src)
	prof := cfg.TopLevelProfile()
	plan := DeriveStreamPlan(streams, prof, props.Codec())
	if noMap {
		plan = nil
	}
	return deriveEncodePlan(planInputs{
		settings: cfg.TranscodeIn(prof, src), prof: prof, source: src, output: out, streams: plan,
		snapshot: func() (*probe.VideoProps, error) { return props, nil }, dynamic: d,
	})
}

// THE PLAN DECLARES DYNAMIC HDR ONLY FROM A PRE-PASS, AND ONLY FOR LIBX265, and the builder
// refuses every declaration its derivation could not have made. No encode runs: the source is
// an HDR10 clip (PQ, a mastering display) and the pre-passes are values.
func TestEncodePlan_DynamicHDRIsDeclaredFromThePrePassAndRefusedOtherwise(t *testing.T) {
	ffmpeg, _ := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=s=320x240:r=24:d=1",
		"-f", "lavfi", "-i", "sine=d=1", "-c:a", "flac", "-pix_fmt", "yuv420p10le", "-c:v", "libx265",
		"-threads", "2", "-x265-params", dvColour, "--", src)
	out := filepath.Join(d, "out.mkv")
	cfg := config.Config{Encoder: "cpu", CRF: 22, Preset: "slow", PixelFormat: "auto", ContainerExt: "source"}
	dv := func() *dynhdr.Prepared {
		return &dynhdr.Prepared{Intent: dynhdr.Intent{DolbyVision: true, SourceProfile: 8},
			VBV: dynhdr.VBV{MaxrateKbps: 1500, BufsizeKbit: 1500, Level: "2"}}
	}
	plus := func() *dynhdr.Prepared {
		return &dynhdr.Prepared{Intent: dynhdr.Intent{HDR10Plus: true}, HDR10PlusJSON: "/w/m.dynhdr0.json"}
	}
	conv := func() *dynhdr.Prepared {
		p := dv()
		p.Intent.Convert, p.Intent.SourceProfile, p.RawVideo, p.FrameRate = true, 7, "/w/m.dynhdr1", dynhdr.Rate{Num: 24, Den: 1}
		return p
	}

	// Declared, and built: the parts reach the command line in their places.
	for name, c := range map[string]struct {
		d    *dynhdr.Prepared
		want []string
	}{
		"profile 8.1": {dv(), []string{"-x265-params", ":vbv-maxrate=1500:vbv-bufsize=1500", "-dolbyvision 1"}},
		"HDR10+":      {plus(), []string{`:dhdr10-info=/w/m.dynhdr0.json`}},
		"converted":   {conv(), []string{"-f hevc -framerate 24/1 -i /w/m.dynhdr1 -map 1:v:0 -map 0:1", "-dolbyvision 1"}},
	} {
		job, err := derivePlanCarrying(t, cfg, src, out, c.d, false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if job.Metadata.DolbyVision != c.d.Intent.DolbyVision || job.Metadata.HDR10Plus != c.d.Intent.HDR10Plus {
			t.Errorf("%s: declared %+v", name, job.Metadata)
		}
		_, body, err := job.args(encoder.X265Parallelism{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		joined := strings.Join(body, " ")
		for _, w := range c.want {
			if !strings.Contains(joined, w) {
				t.Errorf("%s: the command line lacks %q: %s", name, w, joined)
			}
		}
		if c.d.Intent.Convert && strings.Contains(joined, "-map 0 ") {
			t.Errorf("%s: the source's own video is still mapped: %s", name, joined)
		}
	}

	// Not derived: another encoder, a remux, a converted stream with no map to place it in.
	svt := cfg
	svt.Encoder = "svtav1"
	remux := cfg
	remux.RemuxOnly = boolPtr(true)
	for name, c := range map[string]struct {
		cfg   config.Config
		d     *dynhdr.Prepared
		noMap bool
		want  string
	}{
		"svtav1":            {svt, dv(), false, "only by libx265"},
		"remux":             {remux, plus(), false, "remux"},
		"converted, no map": {cfg, conv(), true, "intended map"},
	} {
		if _, err := derivePlanCarrying(t, c.cfg, src, out, c.d, c.noMap); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want a refusal naming %q", name, err, c.want)
		}
	}
	// A pre-pass carrying nothing declares nothing.
	if job, err := derivePlanCarrying(t, cfg, src, out, &dynhdr.Prepared{}, false); err != nil || job.Metadata.DolbyVision ||
		job.Metadata.HDR10Plus || job.dynamic != nil {
		t.Errorf("an empty pre-pass: %v %+v", err, job)
	}

	// Built only as derived: each change is one a derivation could not have made.
	for name, c := range map[string]struct {
		d      func() *dynhdr.Prepared
		change func(*EncodePlan)
		want   string
	}{
		"RPU flag dropped":        {dv, func(p *EncodePlan) { p.Metadata.DolbyVision = false }, "disagree"},
		"HDR10+ flag added":       {dv, func(p *EncodePlan) { p.Metadata.HDR10Plus = true }, "HDR10+"},
		"no ceiling":              {dv, func(p *EncodePlan) { p.dynamic.VBV = dynhdr.VBV{} }, "VBV ceiling"},
		"no mastering display":    {dv, func(p *EncodePlan) { p.Metadata.Color.MasterDisplay = "" }, "mastering display"},
		"another encoder":         {dv, func(p *EncodePlan) { p.Video.Encoder, _ = encoder.Lookup("svtav1") }, "not libx265"},
		"a copy":                  {plus, func(p *EncodePlan) { p.Video.Copy = true }, "not libx265"},
		"no metadata file":        {plus, func(p *EncodePlan) { p.dynamic.HDR10PlusJSON = "" }, "metadata file"},
		"converted, no stream":    {conv, func(p *EncodePlan) { p.dynamic.RawVideo = "" }, "profile 7 conversion"},
		"converted, no rate":      {conv, func(p *EncodePlan) { p.dynamic.FrameRate = dynhdr.Rate{} }, "profile 7 conversion"},
		"converted, no map":       {conv, func(p *EncodePlan) { p.Streams = nil }, "profile 7 conversion"},
		"a stream with no intent": {dv, func(p *EncodePlan) { p.dynamic.RawVideo = "/w/x" }, "profile 7 conversion"},
	} {
		job, err := derivePlanCarrying(t, cfg, src, out, c.d(), false)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c.change(job)
		_, _, err = job.args(encoder.X265Parallelism{})
		var unbuildable *UnbuildablePlanError
		if !errors.As(err, &unbuildable) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want an unbuildable plan naming %q", name, err, c.want)
		}
	}
}

// The converted map keeps every stream the intended map carries, in its order, with the
// source's video replaced by the converted stream and its pictures left out only where they
// travel as attachments.
func TestConvertedMapArgs_ReplacesOnlyTheMainVideo(t *testing.T) {
	plan := &StreamPlan{intended: []probe.Stream{
		{Index: 0, Type: probe.TypeVideo}, {Index: 1, Type: probe.TypeAudio}, {Index: 2, Type: probe.TypeSubtitle},
		{Index: 3, Type: probe.TypeVideo, AttachedPicture: true}, {Index: 4, Type: "attachment"},
	}}
	if got := strings.Join(convertedMapArgs(plan, false), " "); got != "-map 1:v:0 -map 0:1 -map 0:2 -map 0:3 -map 0:4" {
		t.Errorf("with pictures mapped: %s", got)
	}
	if got := strings.Join(convertedMapArgs(plan, true), " "); got != "-map 1:v:0 -map 0:1 -map 0:2 -map 0:4" {
		t.Errorf("with pictures as attachments: %s", got)
	}
}

// The room a job reserves, and the names of its pre-pass files.
func TestDynamicRoomAndTempPaths(t *testing.T) {
	if dynamicRoom(100, dynhdr.Intent{}) != 100 || dynamicRoom(100, dynhdr.Intent{HDR10Plus: true, DolbyVision: true}) != 100 {
		t.Error("a job that converts nothing reserves more than its source")
	}
	if dynamicRoom(100, dynhdr.Intent{DolbyVision: true, Convert: true}) != 200 {
		t.Error("a converting job does not reserve room for the converted stream")
	}
	work := "/lib/film.__transcoding__.mkv.holdfast-part"
	for i, want := range []string{work + ".dynhdr0.json", work + ".dynhdr1", work + ".dynhdr2"} {
		got := dynamicTempPath(work, i)
		if got != want {
			t.Errorf("dynamicTempPath(%d) = %s, want %s", i, got, want)
		}
		if ownerKey(got) != work {
			t.Errorf("%s is not owned by its working file (%s)", got, ownerKey(got))
		}
	}
	dir := t.TempDir()
	w := filepath.Join(dir, "a.__transcoding__.mkv.holdfast-part")
	for i := 0; i < 3; i++ {
		if err := os.WriteFile(dynamicTempPath(w, i), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removeDynamicTemps(w)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("left %d pre-pass file(s)", len(entries))
	}
}
