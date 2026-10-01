package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/crop"
	"github.com/NSchatz/holdfast/internal/encoder"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// The crop through the engine (docs/design/crop.md): the decision reaching the command line,
// the perceptual gate's reference, the crop gate and the row, and every way a crop is refused
// leaving the file encoded uncropped with the reason recorded. The detection and blackness
// arithmetic is proven in internal/crop, which owns it; these cases prove the wiring, and keep
// to as few real encodes as that needs (the engine package carries most of the gate's clock).

// mkCropLetterbox writes a 320x240 H.264 source whose picture is 320x160 with 40 px black bars
// top and bottom; draw is appended to the source chain so a case can put content in a bar.
func mkCropLetterbox(t *testing.T, ffmpeg, path string, seconds string, draw string) {
	t.Helper()
	chain := "testsrc2=duration=" + seconds + ":size=320x160:rate=24,pad=320:240:0:40:black"
	if draw != "" {
		chain += "," + draw
	}
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i", chain,
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "6M", "-pix_fmt", "yuv420p", "--", path)
}

// textInTheBottomBar is four glyph-sized white boxes in the bottom bar from 1.25 s to 1.75 s of
// a 10 s source: after the first cropdetect sample and before the second, so detection does
// not see them and only the blackness check can.
const textInTheBottomBar = "drawbox=x=100:y=212:w=12:h=14:color=white:t=fill:enable='between(t,1.25,1.75)'," +
	"drawbox=x=124:y=212:w=12:h=14:color=white:t=fill:enable='between(t,1.25,1.75)'," +
	"drawbox=x=148:y=212:w=12:h=14:color=white:t=fill:enable='between(t,1.25,1.75)'"

const lbRect = "320:160:0:40"

func cropAuto(c *config.Config) { c.Crop = crop.Auto }

// cropRun runs one pass over a library holding src, with the crop seams a case sets, and
// returns the store and every event.
func cropRun(t *testing.T, root string, mutate func(*config.Config), seams func(*Engine)) (*testStore, []Event) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	st := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, st, discardLogger())
	var mu sync.Mutex
	var events []Event
	eng.Observer = func(ev Event) {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, ev)
	}
	if seams != nil {
		seams(eng)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return st, events
}

// TestCrop_LetterboxReachesTheArgvTheReferenceTheGatesAndTheRow is the end-to-end case: a
// letterboxed source under `crop: auto` is encoded cropped to its picture, the encoder's -vf
// carries the crop, the perceptual gate scores the output against the source put through the
// SAME crop and passes, the crop gate holds the output to the declared size, and the row
// records the rectangle.
func TestCrop_LetterboxReachesTheArgvTheReferenceTheGatesAndTheRow(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkCropLetterbox(t, ffmpeg, src, "4", "")

	cfg := baseCfg(root)
	cropAuto(&cfg)
	cfg.VmafEnable, cfg.MinVmaf = boolPtr(true), 90
	st := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	var argv []string
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober,
		argvObserver: func(a []string) { argv = append([]string(nil), a...) }}
	eng := New(cfg, prober, enc, st, discardLogger())
	var req vmaf.Request
	eng.vmafScore = func(ctx context.Context, r vmaf.Request) (vmaf.Result, error) {
		req = r
		return vmaf.Score(ctx, ffmpeg, r)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("the cropping job is %q/%q, want done", row.Status, row.Outcome.Reason)
	}
	spec := "crop=" + lbRect + ":exact=1"
	if chain, ok := vfChain(argv); !ok || !strings.HasPrefix(chain, spec) {
		t.Errorf("the encode's -vf %q does not start with the crop %q\nargv: %v", chain, spec, argv)
	}
	if req.ReferenceFilter != spec {
		t.Errorf("the perceptual gate's reference chain is %q, want the same crop %q: a score against the "+
			"uncropped source measures the crop, not the encode", req.ReferenceFilter, spec)
	}
	if req.DistortedFilter != "" {
		t.Errorf("the distorted output was filtered (%q) on a job that only crops", req.DistortedFilter)
	}
	if row.Outcome.VmafMean == nil || *row.Outcome.VmafMean < 90 {
		t.Errorf("VMAF against the cropped reference is %v, want a recorded pass", row.Outcome.VmafMean)
	}
	if row.Outcome.DeinterlaceFilter != "" || (row.Outcome.Deinterlaced != nil && *row.Outcome.Deinterlaced) {
		t.Errorf("the crop was recorded as a deinterlace: %q", row.Outcome.DeinterlaceFilter)
	}
	if w, h := dimsOf(t, ffprobe, src); w != 320 || h != 160 {
		t.Errorf("the replacement is %dx%d, want the declared 320x160", w, h)
	}
	if row.Outcome.OutputWidth == nil || *row.Outcome.OutputHeight != 160 {
		t.Errorf("the row's output size is %s", pixels(row.Outcome.OutputWidth, row.Outcome.OutputHeight))
	}
	rec := row.Outcome.Crop
	if !rec.Recorded() || !rec.Record().Applied || rec.Record().Rect != lbRect || rec.Record().Frame != "320x240" {
		t.Errorf("the row records the crop as %s, want %s of 320x240", rec, lbRect)
	}
}

// TestCrop_TheGateRefusesBarsThatAreNotBlack: text burned into a bar between two samples, and
// the pre-encode blackness check stood aside by its seam, so the plan crops through the text.
// The crop gate - which measures the source again over the declared rectangle - refuses the
// output, and the source is byte-identical.
func TestCrop_TheGateRefusesBarsThatAreNotBlack(t *testing.T) {
	ffmpeg, _ := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkCropLetterbox(t, ffmpeg, src, "10", textInTheBottomBar)
	before := sha256f(t, src)
	st, events := cropRun(t, root, cropAuto, func(e *Engine) {
		e.cropBlackness = func(context.Context, string, crop.Decision, string) error { return nil }
	})
	if after := sha256f(t, src); after != before {
		t.Fatal("the source changed: a crop whose bars were not black reached the swap")
	}
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Failed {
		t.Fatalf("the job is %q/%q, want failed at the crop gate", row.Status, row.Outcome.Reason)
	}
	var gates []string
	for _, ev := range events {
		if ev.Status == store.Failed {
			gates = append(gates, ev.Gate)
		}
	}
	if len(gates) != 1 || gates[0] != GateCrop {
		t.Fatalf("failed at %v, want exactly the crop gate (reason %q)", gates, row.Outcome.Reason)
	}
	if !strings.Contains(row.Outcome.Reason, "not black") || !strings.Contains(row.Outcome.Reason, "bottom band") {
		t.Errorf("the reason does not say the bottom band was not black: %q", row.Outcome.Reason)
	}
	if row.Outcome.FailureClass != store.FailureTransient {
		t.Errorf("class %q: the next attempt checks again before encoding and encodes uncropped, so it is transient",
			row.Outcome.FailureClass)
	}
	if rec := row.Outcome.Crop.Record(); !rec.Applied || rec.Rect != lbRect {
		t.Errorf("the refused job does not record the crop it tried: %s", row.Outcome.Crop)
	}
}

// TestCrop_RefusalsEncodeUncroppedWithTheReason: the pre-encode blackness check refuses a crop
// through text in a bar, and samples that disagree (a mixed aspect ratio) refuse one too; each
// file is still encoded, uncropped, and its row says why.
func TestCrop_RefusalsEncodeUncroppedWithTheReason(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	cases := []struct {
		name, reason string
		make         func(path string)
	}{
		{"text in the bar", crop.ReasonBarsNotBlack, func(p string) { mkCropLetterbox(t, ffmpeg, p, "10", textInTheBottomBar) }},
		{"mixed aspect ratio", crop.ReasonSamplesDisagree, func(p string) {
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi", "-i",
				"testsrc2=duration=10:size=320x240:rate=24,"+
					"drawbox=x=0:y=0:w=320:h=40:color=black:t=fill:enable='lt(t,5)',"+
					"drawbox=x=0:y=200:w=320:h=40:color=black:t=fill:enable='lt(t,5)'",
				"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "6M", "-pix_fmt", "yuv420p", "--", p)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "movie.mkv")
			tc.make(src)
			st, _ := cropRun(t, root, cropAuto, nil)
			row := rowForFile(t, st, "movie.mkv")
			if row.Status != store.Done {
				t.Fatalf("the job is %q/%q: a refused crop still encodes the file uncropped", row.Status, row.Outcome.Reason)
			}
			if w, h := dimsOf(t, ffprobe, src); w != 320 || h != 240 {
				t.Errorf("the replacement is %dx%d, want the whole 320x240 frame", w, h)
			}
			rec := row.Outcome.Crop
			if !rec.Recorded() || rec.Record().Applied || rec.Record().Reason != tc.reason || rec.Record().Detail == "" {
				t.Errorf("the row records %s (%q), want not cropped (%s) with its detail", rec, rec.Record().Detail, tc.reason)
			}
		})
	}
}

// TestCrop_OffByDefaultSamplesNothingAndRecordsNothing (I5): with the key unset no detection
// runs and the row records no crop, so a configuration that does not ask is unchanged.
func TestCrop_OffByDefaultSamplesNothingAndRecordsNothing(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkCropLetterbox(t, ffmpeg, src, "1", "")
	st, _ := cropRun(t, root, nil, func(e *Engine) {
		e.cropDetect = func(context.Context, string, *probe.VideoProps) crop.Consensus {
			t.Error("detection ran under a configuration that does not set crop")
			return crop.Consensus{}
		}
	})
	row := rowForFile(t, st, "movie.mkv")
	if row.Status != store.Done || row.Outcome.Crop.Recorded() {
		t.Fatalf("the job is %q with crop %s, want done with nothing recorded", row.Status, row.Outcome.Crop)
	}
	if w, h := dimsOf(t, ffprobe, src); w != 320 || h != 240 {
		t.Errorf("the replacement is %dx%d: something cropped with the key unset", w, h)
	}
}

// TestCrop_ADolbyVisionSourceIsRefusedByTheDecisionItself (I7, P5 phase 1): a letterboxed
// source tagged Dolby Vision, with samples that agree on its picture, is refused a crop by the
// decision reading the source's own probe - not by the engine's DV guard, which this case
// does not go through - and the refusal is what the row would record.
func TestCrop_ADolbyVisionSourceIsRefusedByTheDecisionItself(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "dv.mp4")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x160:rate=10,format=yuv420p10le,pad=320:240:0:40:black",
		"-c:v", "libx265", "-pix_fmt", "yuv420p10le", "-x265-params", "log-level=error", "-tag:v", "dvh1", "--", src)
	prober := probe.New(ffmpeg, ffprobe)
	props := prober.VideoProps(context.Background(), src)
	agreed := crop.Consensus{Edges: crop.Edges{Top: 40, Bottom: 40}, Samples: 10, Valid: 10}

	got := cropApplied(&agreed, nil, props, "yuv420p10le")
	if got.Applied() || got.Reason != crop.ReasonDolbyVision {
		t.Fatalf("the DV source's crop decision is %+v, want refused %s", got, crop.ReasonDolbyVision)
	}
	// The same samples over the same picture without the tag crop: the refusal is the DV class.
	plain := filepath.Join(d, "plain.mp4")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-c", "copy", "-tag:v", "hvc1", "--", plain)
	if d := cropApplied(&agreed, nil, prober.VideoProps(context.Background(), plain), "yuv420p10le"); !d.Applied() || d.Rect.String() != lbRect {
		t.Fatalf("the untagged copy decided %+v, want %s: the DV case would not show the class deciding", d, lbRect)
	}

	cfg := config.Config{Encoder: "cpu", CRF: 22, Preset: "ultrafast", PixelFormat: "auto", ContainerExt: "mkv", Crop: crop.Auto}
	prof := cfg.TopLevelProfile()
	streams, _ := prober.Streams(context.Background(), src)
	job, err := deriveEncodePlan(planInputs{
		settings: cfg.TranscodeIn(prof, src), prof: prof, source: src, output: filepath.Join(d, "out.mkv"),
		streams:  DeriveStreamPlan(streams, prof, props.Codec()),
		snapshot: func() (*probe.VideoProps, error) { return props, nil }, crop: &agreed,
	})
	if err != nil {
		t.Fatalf("deriveEncodePlan: %v", err)
	}
	if job.Picture.Crop.Applied() {
		t.Fatalf("the plan crops a DV source: %v", job.Picture.Crop)
	}
	if rec := job.Picture.cropRecord(); !rec.Recorded() || rec.Record().Reason != crop.ReasonDolbyVision {
		t.Errorf("the row would record %s, want not cropped (dolby-vision)", rec)
	}
	_, body, err := job.args(encoder.X265Parallelism{})
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	if strings.Contains(strings.Join(body, " "), "crop=") {
		t.Errorf("the DV source's command line crops: %v", body)
	}
	// And the engine's own detection does not sample a DV source at all.
	eng := New(cfg, prober, nil, newTestStore(t, d), discardLogger())
	eng.cropDetect = func(context.Context, string, *probe.VideoProps) crop.Consensus {
		t.Error("a DV source was sampled")
		return crop.Consensus{}
	}
	if c := eng.cropConsensus(context.Background(), src, props, false, false); c == nil || c.Valid != 0 {
		t.Errorf("the DV source's consensus is %+v", c)
	}
}

// TestCrop_TheChainRunsDeinterlaceThenCropThenScale: on a job that does all three, the encode
// deinterlaces the fields where they are, crops the deinterlaced frame, and scales only the
// picture it kept; the perceptual gate's reference goes through the same deinterlace and the
// same crop and nothing else, and the output is scaled back up to the CROPPED size to be
// scored. The crop gate holds the output to the scaled size.
func TestCrop_TheChainRunsDeinterlaceThenCropThenScale(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "interlaced.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=640x360:rate=25,pad=640:480:0:60:black", "-vf", "interlace=scan=tff",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p", "-flags", "+ilme+ildct", "--", src)
	prober := probe.New(ffmpeg, ffprobe)
	props := prober.VideoProps(context.Background(), src)
	if !interlacedFieldOrder(props.FieldOrder()) {
		t.Fatalf("the fixture is not interlaced (field order %q)", props.FieldOrder())
	}
	ack := true
	cfg := config.Config{Encoder: "cpu", CRF: 22, Preset: "ultrafast", PixelFormat: "auto", ContainerExt: "mkv",
		Crop: crop.Auto, Deinterlace: "yadif", MaxHeight: 240, DownscaleAck: &ack}
	prof := cfg.TopLevelProfile()
	streams, _ := prober.Streams(context.Background(), src)
	agreed := crop.Consensus{Edges: crop.Edges{Top: 60, Bottom: 60}, Samples: 10, Valid: 10}
	job, err := deriveEncodePlan(planInputs{
		settings: cfg.TranscodeIn(prof, src), prof: prof, source: src, output: filepath.Join(d, "out.mkv"),
		streams:  DeriveStreamPlan(streams, prof, props.Codec()),
		snapshot: func() (*probe.VideoProps, error) { return props, nil }, crop: &agreed,
	})
	if err != nil {
		t.Fatalf("deriveEncodePlan: %v", err)
	}
	pic := job.Picture
	if pic.Crop.Rect.String() != "640:360:0:60" {
		t.Fatalf("crop %v, want 640:360:0:60", pic.Crop)
	}
	// The scale is resolved against the CROPPED picture: 640x360 to 240 tall is 426x240.
	if pic.Downscale.Width != 426 || pic.Downscale.Height != 240 || pic.Downscale.SourceWidth != 640 || pic.Downscale.SourceHeight != 360 {
		t.Fatalf("scale %+v, want 426x240 from the cropped 640x360", pic.Downscale)
	}
	if w, h, ok := pic.OutputSize(); !ok || w != 426 || h != 240 {
		t.Errorf("the declared output size is %dx%d (%v)", w, h, ok)
	}
	_, body, err := job.args(encoder.X265Parallelism{})
	if err != nil {
		t.Fatalf("args: %v", err)
	}
	chain, _ := vfChain(body)
	want := pic.Deinterlace.Spec + "," + pic.Crop.Rect.Spec() + "," + pic.Downscale.Spec()
	if !strings.HasPrefix(chain, want) {
		t.Errorf("the -vf chain is %q, want it to start %q (deinterlace, crop, scale)", chain, want)
	}
	ref := referenceChain(pic.Deinterlace.Spec, pic.Crop.Rect)
	if ref != pic.Deinterlace.Spec+","+pic.Crop.Rect.Spec() {
		t.Errorf("the reference chain is %q", ref)
	}
	graph := vmaf.BuildFilter(vmaf.Request{Distorted: "o", Reference: "s", Subsample: 1, Threads: 1, Model: "m",
		PixelFormat: "yuv420p", ReferenceFilter: ref, DistortedFilter: pic.Downscale.ScoreSpec()}, filepath.Join(d, "v.json"))
	dist, refChain := chains(t, graph)
	if strings.Contains(refChain, "scale=") || !strings.Contains(refChain, pic.Crop.Rect.Spec()) {
		t.Errorf("the reference chain %q must carry the crop and never a scale", refChain)
	}
	if !strings.Contains(dist, "scale=640:360:") {
		t.Errorf("the distorted chain %q must scale the output back up to the CROPPED 640x360", dist)
	}
	if referenceChain("", crop.Rect{}) != "" || referenceChain("yadif", crop.Rect{}) != "yadif" ||
		referenceChain("", crop.Rect{W: 2, H: 2}) != "crop=2:2:0:0:exact=1" {
		t.Error("referenceChain composes the empty cases wrongly")
	}
}

// TestCrop_TheGateHoldsTheOutputToTheDeclaredSize: an output of the whole frame for a plan
// that declares a crop is refused by the crop gate, deterministically, before its bars are
// even measured.
func TestCrop_TheGateHoldsTheOutputToTheDeclaredSize(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkCropLetterbox(t, ffmpeg, src, "1", "")
	out := filepath.Join(d, "out.mkv")
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src, "-c:v", "libx265", "-preset", "ultrafast",
		"-x265-params", "log-level=error", "--", out)
	prober := probe.New(ffmpeg, ffprobe)
	cfg := config.Config{Encoder: "cpu", CRF: 22, Preset: "ultrafast", PixelFormat: "auto", ContainerExt: "mkv", Crop: crop.Auto}
	prof := cfg.TopLevelProfile()
	props := prober.VideoProps(context.Background(), src)
	streams, _ := prober.Streams(context.Background(), src)
	agreed := crop.Consensus{Edges: crop.Edges{Top: 40, Bottom: 40}, Samples: 10, Valid: 10}
	job, err := deriveEncodePlan(planInputs{
		settings: cfg.TranscodeIn(prof, src), prof: prof, source: src, output: out,
		streams:  DeriveStreamPlan(streams, prof, props.Codec()),
		snapshot: func() (*probe.VideoProps, error) { return props, nil }, crop: &agreed,
	})
	if err != nil {
		t.Fatalf("deriveEncodePlan: %v", err)
	}
	eng := New(cfg, prober, nil, newTestStore(t, d), discardLogger())
	gate, class, err := eng.cropGate(context.Background(), job)
	if err == nil || gate != GateCrop || class != store.FailureDeterministic {
		t.Fatalf("the uncropped output passed or was misclassified: %q %q %v", gate, class, err)
	}
	if !strings.Contains(err.Error(), "320x240") || !strings.Contains(err.Error(), "declares 320x160") {
		t.Errorf("the refusal does not name both sizes: %v", err)
	}
	// And the gate is in the vocabulary a consumer enumerates.
	found := false
	for _, g := range GateVocabulary {
		found = found || g == GateCrop
	}
	if !found {
		t.Error("GateCrop is missing from GateVocabulary")
	}
	// The pre-check reads the source's own pixel format and passes clean bars.
	if err := eng.cropPrecheck(context.Background(), job, props); err != nil {
		t.Errorf("the pre-check refused clean bars: %v", err)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatal(err)
	}
}
