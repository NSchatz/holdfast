package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/crop"
	"github.com/NSchatz/holdfast/internal/dynhdr"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// THE DOLBY VISION CROP THROUGH THE ENGINE (docs/design/crop.md#dolby-vision, P5 option (c)).
// The decision's refusals are proven on real files in internal/crop and the L5 reading, the
// zeroing and the L5 gate in internal/dynhdr; these are the few real encodes that prove the
// engine path: the L5 read before the pre-pass, the zeroing pre-pass, the cropped command
// line from the raw stream, the L5 gate, the row - and the fallbacks that keep the Dolby
// Vision and the whole frame. Like every Dolby Vision engine case, the snapshot is masked so
// the HEVC source is not skipped as already at the target codec (docs/design/dynamic-hdr.md#reach).

// mkDVLetterbox writes a profile 8.1 letterbox (320x160 picture, 40 px bars) whose every frame
// carries L5 top/bottom offsets of l5top/l5bottom, with a FLAC track.
func mkDVLetterbox(t *testing.T, path string, l5top, l5bottom int) {
	t.Helper()
	ffmpeg, _ := tools(t)
	dovi, _ := dvTools(t)
	dir := t.TempDir()
	base, rpu, inj := filepath.Join(dir, "b.hevc"), filepath.Join(dir, "r.bin"), filepath.Join(dir, "i.hevc")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=s=320x160:r=24:d=1,format=yuv420p10le,pad=320:240:0:40:black",
		"-pix_fmt", "yuv420p10le", "-c:v", "libx265", "-threads", "2", "-x265-params", dvColour, base)
	gen := filepath.Join(dir, "g.json")
	body := `{"cm_version":"V40","length":24,"level5":{"active_area_top_offset":` + itoaT(l5top) +
		`,"active_area_bottom_offset":` + itoaT(l5bottom) + `,"active_area_left_offset":0,"active_area_right_offset":0},` +
		`"level6":{"max_display_mastering_luminance":1000,"min_display_mastering_luminance":1,` +
		`"max_content_light_level":1000,"max_frame_average_light_level":400}}`
	if err := os.WriteFile(gen, []byte(body), 0o644); err != nil {
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
		"-x265-params", dvColour+":vbv-maxrate=20000:vbv-bufsize=20000", "--", path)
}

func itoaT(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoaT(n/10) + string(rune('0'+n%10))
}

// recordingDoviTool is a dovi_tool that appends its arguments to a log and then runs the real
// one; drop names an argument it leaves out (the bite: "-c").
func recordingDoviTool(t *testing.T, real, drop string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin, log = filepath.Join(dir, "dovi_tool"), filepath.Join(dir, "argv.log")
	script := "#!/bin/sh\necho \"$*\" >> " + log + "\n"
	if drop != "" {
		script += "set -- $(for a in \"$@\"; do [ \"$a\" = \"" + drop + "\" ] || printf '%s\\n' \"$a\"; done)\n"
	}
	script += "exec \"" + real + "\" \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

// requireZeroL5 holds a file to the L5 gate's own reading.
func requireZeroL5(t *testing.T, path string) {
	t.Helper()
	_, ffprobe := tools(t)
	dovi, _ := dvTools(t)
	ctx := context.Background()
	c, err := dynhdr.Count(ctx, ffprobe, path)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	recs, err := dynhdr.ReadL5(ctx, dynhdr.Tools{DoviTool: dovi, FFmpeg: envOr("HOLDFAST_FFMPEG", "ffmpeg")}, path, filepath.Join(dir, "r"), filepath.Join(dir, "l5.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := dynhdr.CheckZeroL5(recs, c.Frames); err != nil {
		t.Fatalf("the replacement's L5 is not zero on every frame: %v", err)
	}
}

// dvCropEngine builds the masked engine dynamicRun builds, with the crop key on, for a case
// that runs more than one pass over it.
func dvCropEngine(t *testing.T, root string, mutate func(*config.Config), doviTool string) (*Engine, *testStore, *goldenArgvRecorder) {
	t.Helper()
	ffmpeg, realProbe := tools(t)
	cfg := baseCfg(root)
	cfg.CRF, cfg.PixelFormat, cfg.Crop = 30, "auto", crop.Auto
	if mutate != nil {
		mutate(&cfg)
	}
	st := newTestStore(t, root)
	prober := probe.New(ffmpeg, codecMaskingFFprobe(t, realProbe))
	rec := &goldenArgvRecorder{}
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober, argvObserver: rec.record}, st, discardLogger())
	eng.DoviTool, eng.HDR10PlusTool = dvTools(t)
	if doviTool != "" {
		eng.DoviTool = doviTool
	}
	return eng, st, rec
}

// TestCropDV_AProfile81LetterboxIsCroppedToItsL5WithL5ZeroedAndGated is the feature end to
// end: L5 40/40 over 40 px bars. The pre-pass zeroes L5 (`-m 0 -c convert`), the encode reads
// the raw stream and crops it to 320x160, the perceptual gate scores against the source put
// through the same crop and passes, every output frame carries an RPU and a 0/0/0/0 L5, and
// the row records the rectangle with its L5 zeroed.
func TestCropDV_AProfile81LetterboxIsCroppedToItsL5WithL5ZeroedAndGated(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDVLetterbox(t, src, 40, 40)
	dovi, _ := dvTools(t)
	rdovi, log := recordingDoviTool(t, dovi, "")
	eng, st, rec := dvCropEngine(t, root, func(c *config.Config) { c.VmafEnable, c.MinVmaf, c.CRF = boolPtr(true), 90, 18 }, rdovi)
	ffmpeg, ffprobe := tools(t)
	var mu sync.Mutex
	var req vmaf.Request
	eng.vmafScore = func(ctx context.Context, r vmaf.Request) (vmaf.Result, error) {
		mu.Lock()
		req = r
		mu.Unlock()
		return vmaf.Score(ctx, ffmpeg, r)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
	}
	argv := strings.Join(argvWith(rec.all(), "libx265"), " ")
	for _, want := range []string{"-f hevc -framerate 24/1 -i ", "-map 1:v:0", "-dolbyvision 1", "crop=320:160:0:40:exact=1"} {
		if !strings.Contains(argv, want) {
			t.Errorf("the encode's command line lacks %q: %s", want, argv)
		}
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "-m 0 -c convert - -o ") || strings.Contains(string(calls), "--edit-config") {
		t.Errorf("the pre-pass did not run `-m 0 -c convert`, or paired --edit-config with it:\n%s", calls)
	}
	if !strings.Contains(string(calls), "export -i ") || !strings.Contains(string(calls), "-l level5=") {
		t.Errorf("L5 was not read with export -l level5:\n%s", calls)
	}
	if req.ReferenceFilter != "crop=320:160:0:40:exact=1" || row.Outcome.VmafMean == nil || *row.Outcome.VmafMean < 90 {
		t.Errorf("the perceptual gate scored against %q at %v, want the identically cropped source and a pass",
			req.ReferenceFilter, row.Outcome.VmafMean)
	}
	if w, h, ok := probe.New(ffmpeg, ffprobe).Dimensions(context.Background(), src); !ok || w != 320 || h != 160 {
		t.Errorf("the replacement is %dx%d (%v), want 320x160", w, h, ok)
	}
	requireCarried(t, src, dynhdr.Expectation{DolbyVision: true, Profile: 8, CompatID: 1})
	requireZeroL5(t, src)
	if r := row.Outcome.Crop.Record(); !r.Applied || r.Rect != "320:160:0:40" || !r.L5Zeroed {
		t.Errorf("the row records %+v, want 320:160:0:40 with L5 zeroed", r)
	}
	requireNoDynamicTemps(t, root)
}

// TestCropDV_ARefusedL5EncodesUncroppedWithItsDolbyVision: L5 20/20 over 40 px bars disagrees
// with the picture; the file is encoded uncropped, its RPU (and its L5, untouched) carried, and
// the row says why.
func TestCropDV_ARefusedL5EncodesUncroppedWithItsDolbyVision(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDVLetterbox(t, src, 20, 20)
	eng, st, rec := dvCropEngine(t, root, nil, "")
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
	}
	if r := row.Outcome.Crop.Record(); r.Applied || r.Reason != crop.ReasonL5Disagrees {
		t.Errorf("the row records %+v, want refused %s", r, crop.ReasonL5Disagrees)
	}
	argv := strings.Join(argvWith(rec.all(), "libx265"), " ")
	if strings.Contains(argv, "crop=") || strings.Contains(argv, "-f hevc") || !strings.Contains(argv, "-dolbyvision 1") {
		t.Errorf("the refused crop still cropped, rewrote the stream, or dropped the RPU: %s", argv)
	}
	requireCarried(t, src, dynhdr.Expectation{DolbyVision: true, Profile: 8, CompatID: 1})
	requireNoDynamicTemps(t, root)
}

// TestCropDV_TheL5GateRefusesAStaleL5AndTheNextAttemptIsUncropped is the L5 gate's bite in the
// engine: a dovi_tool that drops `-c` from the pre-pass leaves L5 at 40/40 on the cropped
// output. The L5 gate refuses it, the source is byte-identical, and the next attempt in the
// same process encodes the file uncropped with its Dolby Vision carried.
func TestCropDV_TheL5GateRefusesAStaleL5AndTheNextAttemptIsUncropped(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDVLetterbox(t, src, 40, 40)
	before := sha256f(t, src)
	dovi, _ := dvTools(t)
	noCrop, _ := recordingDoviTool(t, dovi, "-c")
	eng, st, rec := dvCropEngine(t, root, nil, noCrop)
	var mu sync.Mutex
	var gates []string
	eng.Observer = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		if ev.Status == store.Failed {
			gates = append(gates, ev.Gate)
		}
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Failed || len(gates) != 1 || gates[0] != GateDolbyVisionL5 {
		t.Fatalf("row %s %q at gates %v, want failed at %s", row.Status, row.Outcome.Reason, gates, GateDolbyVisionL5)
	}
	if !strings.Contains(row.Outcome.Reason, "0/0/40/40") || row.Outcome.FailureClass != store.FailureTransient {
		t.Errorf("the refusal %q (%s) does not name the stale L5, or is not transient", row.Outcome.Reason, row.Outcome.FailureClass)
	}
	if sha256f(t, src) != before {
		t.Fatal("the source changed although the L5 gate refused its replacement")
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	row = rowForFile(t, st, "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("the next attempt is %s %q, want done uncropped", row.Status, row.Outcome.Reason)
	}
	if r := row.Outcome.Crop.Record(); r.Applied || r.Reason != crop.ReasonL5GateFailed {
		t.Errorf("the next attempt records %+v, want refused %s", r, crop.ReasonL5GateFailed)
	}
	if argv := strings.Join(argvWith(rec.all()[1:], "libx265"), " "); strings.Contains(argv, "crop=") {
		t.Errorf("the next attempt cropped: %s", argv)
	}
	requireCarried(t, src, dynhdr.Expectation{DolbyVision: true, Profile: 8, CompatID: 1})
}

// TestCropDV_AProfile7SourceIsZeroedByTheModeTwoConversion: an opted-in profile 7 source
// (whose record says 7 and whose RPUs are 8.1, since dovi_tool cannot generate profile 7) with
// L5 40/40 is converted and zeroed in one pass, `-m 2 -c convert --discard`, and cropped.
func TestCropDV_AProfile7SourceIsZeroedByTheModeTwoConversion(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkDVLetterbox(t, src, 40, 40)
	asDolbyVisionProfile7(t, src)
	dovi, _ := dvTools(t)
	rdovi, log := recordingDoviTool(t, dovi, "")
	eng, st, rec := dvCropEngine(t, root, func(c *config.Config) { c.DolbyVisionP7 = config.DolbyVisionP7Convert }, rdovi)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatal(err)
	}
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("row %s %q, want done", row.Status, row.Outcome.Reason)
	}
	calls, _ := os.ReadFile(log)
	if !strings.Contains(string(calls), "-m 2 -c convert --discard - -o ") {
		t.Errorf("the profile 7 pre-pass did not run `-m 2 -c convert --discard`:\n%s", calls)
	}
	if argv := strings.Join(argvWith(rec.all(), "libx265"), " "); !strings.Contains(argv, "crop=320:160:0:40:exact=1") {
		t.Errorf("the profile 7 encode did not crop: %s", argv)
	}
	requireCarried(t, src, dynhdr.Expectation{DolbyVision: true, Profile: 8, CompatID: 1})
	requireZeroL5(t, src)
}
