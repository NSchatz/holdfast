package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// The resolution ceiling: what it encodes, what it refuses, and - the case the whole item
// turns on - which of the two files the perceptual gate resamples before it scores them.
//
// This is where the blast radius is. A downscaling swap deletes a source that had pixels the
// replacement does not, and the only thing standing in front of it is a VMAF measurement. If
// that measurement is taken against a source resampled DOWN to meet the output, the detail
// the downscale threw away is gone from both sides, every such encode scores near the top of
// the scale, and the floors admit exactly what they exist to refuse. Nothing re-runs that
// measurement, because its subject is gone.

// mkH264Tall writes a 640x480 H.264 clip: tall enough that a ceiling of 240 halves it, and
// detailed enough (testsrc2 at 24fps over 10s) that throwing half the lines away is visible
// to a perceptual metric rather than lost in flat colour.
func mkH264Tall(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=10:size=640x480:rate=24",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// capping returns the config mutation one root's ceiling is written by, with the
// acknowledgement the final-swap guard requires so that a case about SCALING is not answered
// by the guard about acknowledging it.
func capping(height int) func(*config.Config) {
	return func(c *config.Config) {
		c.MaxHeight = height
		ack := true
		c.DownscaleAck = &ack
	}
}

// dimsOf reads one file's coded dimensions, failing the test when the probe establishes
// neither - an assertion about a resolution nobody measured is not an assertion.
func dimsOf(t *testing.T, ffprobe, path string) (int, int) {
	t.Helper()
	w, h, ok := probe.New("", ffprobe).Dimensions(context.Background(), path)
	if !ok {
		t.Fatalf("ffprobe established no dimensions for %s", path)
	}
	return w, h
}

// pixels renders a recorded resolution for a failure message, with `not recorded` where the
// row holds nothing - printing the pointers would put an address where a reader needs a size.
func pixels(w, h *int) string {
	one := func(p *int) string {
		if p == nil {
			return "not recorded"
		}
		return strconv.Itoa(*p)
	}
	return one(w) + "x" + one(h)
}

// vfChain returns the value of the argv's single -vf option, and whether it carries one.
func vfChain(args []string) (string, bool) {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-vf" {
			return args[i+1], true
		}
	}
	return "", false
}

// TestDownscale_ScalesASourceAboveTheCeiling grades [AC-1]: a profile that sets `max_height`
// on a source taller than it encodes with a scaling step to that height, in the source's own
// aspect ratio and with even dimensions in both axes.
//
// It runs the REAL production encoder and measures the file that came out, because "there was
// a scale filter in the argv" and "the replacement is 320x240" are two different claims and
// only the second is what an operator gets. The argv is asserted beside it so that a build
// which scaled nothing could not pass by producing a source-sized output that happened to
// match.
//
// MUTATION: drop withDownscale from the argv and both the argv arm and the dimensions arm
// red. The ROUNDING is not gradeable here and deliberately is not graded here: 640x480 at a
// ceiling of 240 has an ideal width of exactly 320, so a build that truncated or emitted an
// odd width would pass this case. That is the next test.
func TestDownscale_ScalesASourceAboveTheCeiling(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Tall(t, ffmpeg, src, "8M")
	srcW, srcH := dimsOf(t, ffprobe, src)

	cfg := baseCfg(d)
	capping(240)(&cfg)
	var argv []string
	enc := FFmpegEncoder{
		FFmpeg: ffmpeg, Cfg: cfg, Probe: probe.New(ffmpeg, ffprobe),
		argvObserver: func(args []string) { argv = append([]string(nil), args...) },
	}
	ts := run(t, ffmpeg, ffprobe, d, enc, capping(240))

	if !ledgerHas(t, ts, store.Done, "movie.mkv") {
		row := rowForFile(t, ts, "movie.mkv")
		t.Fatalf("the downscaling encode is %q/%q, not done - every gate ran at its existing "+
			"strictness and one of them refused it", row.Status, row.Outcome.Reason)
	}

	chain, ok := vfChain(argv)
	if !ok || !strings.Contains(chain, "scale=320:240:flags="+downscale.Scaler) {
		t.Errorf("the encode's argv carries no scaling step to the ceiling (-vf %q): without one this "+
			"case would pass on a build that scales nothing and happens to meet the size assertion by "+
			"another route.\nargv: %v", chain, argv)
	}

	// The file that replaced the source. The swap has already happened, so this reads the
	// library path an operator would open.
	gotW, gotH := dimsOf(t, ffprobe, src)
	if gotH != 240 {
		t.Errorf("the replacement is %d pixels tall, want the configured ceiling of 240", gotH)
	}
	if gotW != 320 {
		t.Errorf("the replacement is %dx%d and the source was %dx%d: the width that holds the "+
			"source's aspect ratio at 240 is 320", gotW, gotH, srcW, srcH)
	}
	if gotW%2 != 0 || gotH%2 != 0 {
		t.Errorf("the replacement is %dx%d, which is odd in at least one axis - every pixel format "+
			"this build encodes to is 4:2:0 and has no representation for one", gotW, gotH)
	}
	// The aspect ratio, asserted as a ratio rather than as two numbers, so this arm still
	// bites on a source whose dimensions are not the fixture's.
	if srcW*gotH != gotW*srcH {
		t.Errorf("the replacement is %dx%d and the source was %dx%d: the aspect ratio moved",
			gotW, gotH, srcW, srcH)
	}
}

// widthError is how far a produced width is from the one that exactly holds the source's
// aspect ratio at this height, in whole pixels and in integer arithmetic: |w - srcW*h/srcH|
// multiplied through by srcH, so nothing is compared through a float the way the code under
// test deliberately does not.
func widthError(gotW, srcW, srcH, h int) int {
	e := gotW*srcH - srcW*h
	if e < 0 {
		return -e
	}
	return e
}

// TestDownscale_HoldsTheAspectRatioAndEvenDimensionsWhereTheyDoNotDivide grades the second
// half of [AC-1] - "preserving the source's aspect ratio and producing even dimensions in
// both axes" - at the values where that clause can actually be broken.
//
// The case above cannot break it. 640x480 at a ceiling of 240 has an ideal width of exactly
// 320, so the rounding term contributes nothing there and a build that truncated, or that
// emitted an odd width, would pass it. A clause graded only where it cannot fail is a clause
// with no grader behind it.
//
// Two halves, because the clause is owed to two readers. A REAL encode at a ceiling where the
// ideal width is NOT a whole number proves ffmpeg accepted the dimensions this build asked it
// for - an odd or zero dimension is a filter expression it refuses outright. The resolution
// itself, over the shapes a library actually holds, is where the clause can be graded at more
// than one point for the cost of arithmetic.
//
// Every assertion is the CLAUSE and not the implementation: the height is the ceiling, both
// dimensions are even, and the width is within ONE pixel of the one that exactly holds the
// source's ratio. Nothing here asserts a number this build happens to compute.
//
// MUTATION: truncate in evenWidth (drop the `+ half/2`) and the width arm reds on 640x480 at
// 220 and on 1440x1080 at 700; drop the `2 *` and the even arm reds; drop the MinHeight clamp
// and the narrow-source arm reds with a width of zero.
func TestDownscale_HoldsTheAspectRatioAndEvenDimensionsWhereTheyDoNotDivide(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	// A ceiling of 220 on the 640x480 fixture: the width that exactly holds 4:3 there is
	// 293.33, so what gets written is whatever the rounding decides, and truncation and
	// nearest-even decide it differently (292 against 294).
	const ceiling = 220
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Tall(t, ffmpeg, src, "8M")
	srcW, srcH := dimsOf(t, ffprobe, src)

	ts := run(t, ffmpeg, ffprobe, d, FFmpegEncoder{
		FFmpeg: ffmpeg, Cfg: func() config.Config { c := baseCfg(d); capping(ceiling)(&c); return c }(),
		Probe: probe.New(ffmpeg, ffprobe),
	}, capping(ceiling))

	if !ledgerHas(t, ts, store.Done, "movie.mkv") {
		row := rowForFile(t, ts, "movie.mkv")
		t.Fatalf("the encode at a ceiling of %d is %q/%q, not done - a width ffmpeg refused looks "+
			"exactly like this", ceiling, row.Status, row.Outcome.Reason)
	}
	gotW, gotH := dimsOf(t, ffprobe, src)
	if gotH != ceiling {
		t.Errorf("the replacement is %d pixels tall, want the configured ceiling of %d", gotH, ceiling)
	}
	if gotW%2 != 0 || gotH%2 != 0 {
		t.Errorf("the replacement is %dx%d, which is odd in at least one axis - every pixel format "+
			"this build encodes to is 4:2:0 and has no representation for one", gotW, gotH)
	}
	if e := widthError(gotW, srcW, srcH, ceiling); e > srcH {
		t.Errorf("the replacement is %dx%d from a %dx%d source: the width that holds that ratio at "+
			"%d is %.2f, and %d is more than a pixel away from it, so the aspect ratio moved",
			gotW, gotH, srcW, srcH, ceiling, float64(srcW*ceiling)/float64(srcH), gotW)
	}

	// The shapes a library holds, resolved rather than encoded: a 4:3 source whose width does
	// not divide, an ultrawide one, a UHD remux at 1080p, PAL SD, and a 4:3 HD master at a
	// ceiling that is not a standard height.
	for _, tc := range []struct {
		name                  string
		srcW, srcH, maxHeight int
	}{
		{"a 4:3 source whose width does not divide at the ceiling", 640, 480, 220},
		{"an ultrawide source", 2560, 1080, 720},
		{"a UHD remux at 1080p", 3840, 2160, 1080},
		{"PAL standard definition", 720, 576, 480},
		{"a 4:3 HD master at a non-standard ceiling", 1440, 1080, 700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := downscale.Resolve(tc.maxHeight, tc.srcW, tc.srcH)
			if !s.Enabled() {
				t.Fatalf("a %dx%d source under a ceiling of %d scales nothing, so the clause is not "+
					"being graded at all", tc.srcW, tc.srcH, tc.maxHeight)
			}
			if s.Height != tc.maxHeight {
				t.Errorf("the target is %d pixels tall, want the ceiling of %d", s.Height, tc.maxHeight)
			}
			if s.Width%2 != 0 || s.Height%2 != 0 {
				t.Errorf("the target is %dx%d, which is odd in at least one axis - no 4:2:0 pixel "+
					"format has a representation for one, and ffmpeg refuses the filter", s.Width, s.Height)
			}
			if e := widthError(s.Width, tc.srcW, tc.srcH, tc.maxHeight); e > tc.srcH {
				t.Errorf("a %dx%d source at a ceiling of %d resolves to %dx%d: the width that holds "+
					"that ratio is %.2f, and %d is more than a pixel away from it",
					tc.srcW, tc.srcH, tc.maxHeight, s.Width, s.Height,
					float64(tc.srcW*tc.maxHeight)/float64(tc.srcH), s.Width)
			}
		})
	}

	// The floor, which is the other end of the same clause: a source so narrow that the width
	// holding its ratio rounds to zero still resolves to a picture. `scale=0:2` is not a
	// dimension ffmpeg can target, and a filter expression it refuses fails the job rather
	// than producing one.
	if s := downscale.Resolve(downscale.MinHeight, 2, 1000); s.Width < downscale.MinHeight || s.Width%2 != 0 {
		t.Errorf("a 2x1000 source at a ceiling of %d resolves to a width of %d: below %d, or odd, is "+
			"a scale filter ffmpeg refuses", downscale.MinHeight, s.Width, downscale.MinHeight)
	}
}

// TestDownscale_LeavesASourceAtOrBelowTheCeilingAlone grades [AC-2]: a profile that sets
// `max_height` on a source at or below it encodes with NO scaling step, producing the same
// filtergraph it produces with the key unset.
//
// The two argv are compared to each other rather than to a written-down string, which is what
// makes "the same filtergraph" the claim rather than "a filtergraph I expected". The source
// is exactly AT the ceiling, because at-or-below is a boundary and the off-by-one that scales
// a file to its own size is the one a test written against a clearly-shorter source misses.
func TestDownscale_LeavesASourceAtOrBelowTheCeilingAlone(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	argvFor := func(t *testing.T, mutate func(*config.Config)) ([]string, string, *testStore) {
		t.Helper()
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264Tall(t, ffmpeg, src, "8M")
		cfg := baseCfg(d)
		if mutate != nil {
			mutate(&cfg)
		}
		var argv []string
		enc := FFmpegEncoder{
			FFmpeg: ffmpeg, Cfg: cfg, Probe: probe.New(ffmpeg, ffprobe),
			argvObserver: func(args []string) { argv = append([]string(nil), args...) },
		}
		return argv, src, run(t, ffmpeg, ffprobe, d, enc, mutate)
	}

	// 480 is the fixture's own height, so the ceiling is in force and admits every frame.
	capped, cappedSrc, cappedLedger := argvFor(t, capping(480))
	plain, _, _ := argvFor(t, nil)

	if !ledgerHas(t, cappedLedger, store.Done, "movie.mkv") {
		row := rowForFile(t, cappedLedger, "movie.mkv")
		t.Fatalf("a source at the ceiling is %q/%q, not done", row.Status, row.Outcome.Reason)
	}
	if chain, ok := vfChain(capped); ok {
		t.Errorf("a source AT the ceiling was given the video filter chain %q: nothing above the "+
			"ceiling means nothing to scale", chain)
	}
	if len(capped) != len(plain) {
		t.Fatalf("the argv for a source at the ceiling has %d arguments and the argv with the key "+
			"unset has %d\n  capped: %v\n  unset:  %v", len(capped), len(plain), capped, plain)
	}
	for i := range capped {
		// The two runs use different temp directories, so the two paths differ by
		// construction; everything that is not a path has to match exactly.
		if capped[i] == plain[i] || strings.Contains(capped[i], string(filepath.Separator)) {
			continue
		}
		t.Errorf("argument %d differs: capped %q, key unset %q - a ceiling no file is above must "+
			"produce the filtergraph this build produces without one", i, capped[i], plain[i])
	}

	gotW, gotH := dimsOf(t, ffprobe, cappedSrc)
	if gotW != 640 || gotH != 480 {
		t.Errorf("a source at the ceiling came out %dx%d, want its own 640x480", gotW, gotH)
	}
}

// TestDownscale_IsRefusedWhenTheUndoWindowIsOffAndUnacknowledged grades [AC-4]: a file that
// would be downscaled, with the undo window disabled and no acknowledgement on the governing
// profile, is SKIPPED with a reason naming both conditions; no encode starts and the source
// is left byte for byte intact.
//
// The two keys are not one statement. `max_height` says what the replacement should look
// like; `downscale_acknowledged` says the operator accepts that the original is not coming
// back. With `undo_window_hours: 0` - the shipped default - those differ, because the rename
// that publishes the replacement destroys the source and nothing retains it.
//
// The three release arms are what stop this being a test about a build that simply never
// downscales: removing EITHER condition has to let the same file through, and the file is
// identical in all four runs.
//
// MUTATION: drop the acknowledgement from the guard's condition and the first arm reds; drop
// the undo-window half and the third arm's row would still be a skip.
func TestDownscale_IsRefusedWhenTheUndoWindowIsOffAndUnacknowledged(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	withCeiling := func(t *testing.T, mutate func(*config.Config)) (*testStore, string, string) {
		t.Helper()
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264Tall(t, ffmpeg, src, "8M")
		before := md5f(t, src)
		return run(t, ffmpeg, ffprobe, d, nil, mutate), src, before
	}

	t.Run("refused, and the source is untouched", func(t *testing.T) {
		ts, src, before := withCeiling(t, func(c *config.Config) {
			c.MaxHeight = 240 // and no acknowledgement, and undo_window_hours stays 0
		})
		out, status, found := outcomeFor(t, ts, src)
		if !found {
			t.Fatalf("no terminal row was written for a file the ceiling would have scaled")
		}
		if status != store.Skipped || out.Reason != SkipDownscaleUnacknowledged {
			t.Fatalf("the job is %q/%q, want skipped/%s", status, out.Reason, SkipDownscaleUnacknowledged)
		}
		if after := md5f(t, src); after != before {
			t.Errorf("the source's bytes changed (%s -> %s): a refusal that encodes is not a refusal",
				before, after)
		}
		if got := codecOf(t, ffprobe, src); got != "h264" {
			t.Errorf("the source is now %q: something encoded and swapped a file this build refused", got)
		}
		// Nothing was written beside it either: a temp left behind is an encode that started.
		entries, err := os.ReadDir(filepath.Dir(src))
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		if len(entries) != 1 {
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Errorf("the library holds %v after a refusal - the encode was started", names)
		}
		// The verdict records BOTH keys it weighed, or the operator's second remedy - opening
		// the undo window - would leave the file held on a skip nothing re-derives.
		for _, key := range []string{InputMaxHeight, InputUndoWindow} {
			if _, ok := out.DecisionInputs.Value(key); !ok {
				t.Errorf("the refusal records no %s, so editing it would not offer the file back: %v",
					key, out.DecisionInputs)
			}
		}
	})

	t.Run("acknowledged, so it runs", func(t *testing.T) {
		ts, src, _ := withCeiling(t, capping(240))
		if !ledgerHas(t, ts, store.Done, "movie.mkv") {
			row := rowForFile(t, ts, "movie.mkv")
			t.Fatalf("an ACKNOWLEDGED ceiling is %q/%q, not done - the guard is refusing more than "+
				"the criterion names", row.Status, row.Outcome.Reason)
		}
		if _, h := dimsOf(t, ffprobe, src); h != 240 {
			t.Errorf("the replacement is %d tall, want 240 - the acknowledged path did not scale", h)
		}
	})

	t.Run("the undo window is open, so it runs", func(t *testing.T) {
		ts, src, _ := withCeiling(t, func(c *config.Config) {
			c.MaxHeight = 240
			c.UndoWindowHours = 24 // the swap can be walked back; no acknowledgement needed
		})
		if !ledgerHas(t, ts, store.Done, "movie.mkv") {
			row := rowForFile(t, ts, "movie.mkv")
			t.Fatalf("a ceiling under an OPEN undo window is %q/%q, not done - the guard fires on the "+
				"acknowledgement alone, which is not what it is for", row.Status, row.Outcome.Reason)
		}
		if _, h := dimsOf(t, ffprobe, src); h != 240 {
			t.Errorf("the replacement is %d tall, want 240", h)
		}
	})

	t.Run("nothing to scale, so the guard never fires", func(t *testing.T) {
		ts, _, _ := withCeiling(t, func(c *config.Config) {
			c.MaxHeight = 480 // the fixture's own height: in force, and above nothing
		})
		if !ledgerHas(t, ts, store.Done, "movie.mkv") {
			row := rowForFile(t, ts, "movie.mkv")
			t.Fatalf("a source AT the ceiling is %q/%q, not done - the guard is refusing files the "+
				"key would not have scaled", row.Status, row.Outcome.Reason)
		}
	})
}

// TestDownscale_ScoresAgainstTheSourceResolution grades [AC-5], and it is the criterion this
// whole item turns on: the VMAF comparison scales the DISTORTED input UP to the source's
// resolution and scores at the source's resolution, and does NOT scale the reference down.
//
// Both halves are asserted, because either alone would pass a wrong build.
//
//  1. WHICH INPUT the graph scales, read off the filtergraph this build hands ffmpeg. libvmaf
//     takes the distorted stream first and the reference second, so the scale has to sit in
//     the [0:...] chain and must not appear in the [1:...] chain. "A scaling step is present"
//     is true of the backwards build too.
//  2. WHAT IT MEASURES, through real libvmaf on both arms. The same encode scored the
//     backwards way - the source resampled down to meet it - scores FAR higher, because the
//     detail the downscale threw away is then missing from both sides and nothing measures
//     its loss. The two straddle the shipped min_vmaf, so the wrong direction does not merely
//     record a different number: it ACCEPTS an encode this one refuses, and the swap that
//     follows deletes a source that had those pixels.
//
// MUTATION: move the scale onto ReferenceFilter and the first half reds by name; leave the
// distorted unscaled and ffmpeg refuses the graph outright.
func TestDownscale_ScoresAgainstTheSourceResolution(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Tall(t, ffmpeg, src, "8M")

	cfg := baseCfg(d)
	capping(240)(&cfg)
	prober := probe.New(ffmpeg, ffprobe)
	out := filepath.Join(d, "out.mkv")
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}.ForProfile(cfg.TopLevelProfile())
	ctx := context.Background()
	if err := enc.Encode(ctx, src, out, prober.VideoProps(ctx, src)); err != nil {
		t.Fatalf("encoding the downscaled output: %v", err)
	}
	if _, h := dimsOf(t, ffprobe, out); h != 240 {
		t.Fatalf("the fixture output is %d tall, want 240 - there is no resolution gap to score across", h)
	}
	shrink := downscaleApplied(cfg.TopLevelProfile(), prober.VideoProps(ctx, src))
	if !shrink.Enabled() {
		t.Fatal("the profile resolved to no scale at all, so this case would grade nothing")
	}

	// 1. THE GRAPH. The distorted chain runs [0:v:0] -> [dist]; the reference chain runs
	// [1:v:0] -> [ref]. The up-scale belongs in the first and nowhere near the second.
	pixFmt, ok := vmaf.ComparisonFormat(prober.PixFmt(ctx, src), prober.PixFmt(ctx, out))
	if !ok {
		t.Fatalf("no comparison pixel format for the pair")
	}
	req := vmaf.Request{
		Distorted: out, Reference: src, Subsample: 1, Threads: 1,
		Model:           vmaf.ResolveModel("auto", shrink.ScoredHeight()),
		PixelFormat:     pixFmt,
		DistortedFilter: shrink.ScoreSpec(),
	}
	graph := vmaf.BuildFilter(req, filepath.Join(d, "vmaf.json"))
	distChain, refChain := chains(t, graph)
	wantScale := "scale=640:480:flags=" + downscale.Scaler
	if !strings.Contains(distChain, wantScale) {
		t.Errorf("the DISTORTED chain does not scale the output up to the source's resolution.\n"+
			"  want %q in %q\n  graph: %s", wantScale, distChain, graph)
	}
	if strings.Contains(refChain, "scale=") {
		t.Fatalf("the REFERENCE chain carries a scale (%q): the gate would then be grading this "+
			"encode against a source degraded to meet it, every downscaling swap would score near "+
			"the top of the scale, and the source is deleted immediately afterwards.\n  graph: %s",
			refChain, graph)
	}

	// 2. THE MEASUREMENT, through real libvmaf, both ways.
	right, err := vmaf.Score(ctx, ffmpeg, req)
	if err != nil {
		t.Fatalf("scoring with the output scaled up to the source: %v", err)
	}
	if right.DistortedFilter != shrink.ScoreSpec() {
		t.Errorf("the result carries distorted filter %q, want %q - the filter travels with the "+
			"number or the number cannot be interpreted", right.DistortedFilter, shrink.ScoreSpec())
	}
	backwards, err := vmaf.Score(ctx, ffmpeg, vmaf.Request{
		Distorted: out, Reference: src, Subsample: 1, Threads: 1,
		Model:           vmaf.ResolveModel("auto", 240),
		PixelFormat:     pixFmt,
		ReferenceFilter: "scale=320:240:flags=" + downscale.Scaler,
	})
	if err != nil {
		t.Fatalf("scoring the backwards way: %v", err)
	}
	if !(backwards.HarmonicMean > right.HarmonicMean+5) {
		t.Errorf("scoring at the source's resolution gave %.2f and scoring against a source resampled "+
			"down to meet the output gave %.2f - the two are not meaningfully apart, so this case does "+
			"not establish that the direction is what makes the gate a measurement of what the "+
			"downscale cost", right.HarmonicMean, backwards.HarmonicMean)
	}
	// And the difference is DECISION-CHANGING rather than merely visible: the shipped
	// min_vmaf is 95, so the backwards direction ACCEPTS an encode this one refuses.
	const shippedMinVmaf = 95.0
	if right.HarmonicMean >= shippedMinVmaf || backwards.HarmonicMean < shippedMinVmaf {
		t.Errorf("at the source's resolution this encode scores %.2f and the backwards way %.2f, with "+
			"the shipped min_vmaf at %.2f - the pair does not straddle the floor, so this fixture does "+
			"not show the direction changing a verdict",
			right.HarmonicMean, backwards.HarmonicMean, shippedMinVmaf)
	}
}

// chains splits a scoring filtergraph into the DISTORTED chain and the REFERENCE chain, which
// are the two halves AC-5 is about. It fails the test rather than guessing when the graph is
// not the two-chain shape this build writes: an assertion about a chain that was not found is
// an assertion about "".
func chains(t *testing.T, graph string) (dist, ref string) {
	t.Helper()
	parts := strings.Split(graph, ";")
	for _, p := range parts {
		switch {
		case strings.HasSuffix(p, "[dist]"):
			dist = p
		case strings.HasSuffix(p, "[ref]"):
			ref = p
		}
	}
	if dist == "" || ref == "" {
		t.Fatalf("the scoring graph is not the two-chain shape this build writes: %s", graph)
	}
	return dist, ref
}

// TestDownscale_RecordsTheScalingStepInTheProof grades [AC-6]: a downscaled output's job
// records, beside the pixel format it already records, that a scaling step was applied, which
// scaler performed it, and the resolution the comparison was scored at.
//
// The scored resolution is the one of the three that cannot be re-derived from anything else
// on the row: the row already says the output is 320x240, and a reader holding only that
// cannot tell whether the gate measured there or at the source's 640x480. Those are two
// different measurements and one of them is not a measurement of this encode at all.
func TestDownscale_RecordsTheScalingStepInTheProof(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Tall(t, ffmpeg, src, "8M")

	// The gate has to RUN for a scored resolution to exist, and the shipped floors reject a
	// half-height encode of this fixture (that is what TestDownscale_ScoresAgainstTheSource-
	// Resolution measures), so the floors are lowered here. Lowered, not removed: what this
	// case is about is what the row records, and a row from a gate that never ran records
	// nothing at all.
	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		capping(240)(c)
		on := true
		c.VmafEnable = &on
		c.MinVmaf, c.VmafMinPool, c.VmafMinChroma = 10, 5, 5
	})

	out, status, found := outcomeFor(t, ts, src)
	if !found || status != store.Done {
		t.Fatalf("the downscaled job is %q/%q, so there is no proof to read", status, out.Reason)
	}
	if out.Downscaled == nil || !*out.Downscaled {
		t.Errorf("the row does not record that a scaling step was applied (downscaled=%v)", out.Downscaled)
	}
	if out.DownscaleScaler != downscale.Scaler {
		t.Errorf("the row records scaler %q, want %q - two resamplers produce two different pictures "+
			"from one source, so a row that cannot tell them apart does not state what was done",
			out.DownscaleScaler, downscale.Scaler)
	}
	if out.VmafScoredWidth == nil || out.VmafScoredHeight == nil ||
		*out.VmafScoredWidth != 640 || *out.VmafScoredHeight != 480 {
		t.Errorf("the row records the comparison as made at %s, want the SOURCE's 640x480 - the "+
			"output was scaled back up to meet it", pixels(out.VmafScoredWidth, out.VmafScoredHeight))
	}
	if out.OutputWidth == nil || out.OutputHeight == nil ||
		*out.OutputWidth != 320 || *out.OutputHeight != 240 {
		t.Errorf("the row records the output as %s, want 320x240",
			pixels(out.OutputWidth, out.OutputHeight))
	}
	// Beside the pixel format it already records, in the same record.
	if out.VmafPixFmt == "" {
		t.Error("the row carries no comparison pixel format, so the scaling step was recorded in a " +
			"proof that no longer carries the fact it was supposed to sit beside")
	}
}

// TestDownscale_AppliesTheSameFloorsAndKeepsTheSourceWhenTheyAreMissed grades [AC-7]: a
// downscaled output is held to the same VMAF floors as any other, and one that cannot clear
// them fails the gate, has its temp discarded, and leaves the source intact.
//
// The floors are the SHIPPED ones and nothing here moves them in either direction. That is the
// criterion: a downscaling job is not given a discount, and a 640x480 source halved is exactly
// the encode that cannot clear min_vmaf: 95 when it is measured at the resolution it is about
// to replace.
func TestDownscale_AppliesTheSameFloorsAndKeepsTheSourceWhenTheyAreMissed(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Tall(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	// Loaded through the real config layer rather than assembled here, so every floor is
	// whatever this build SHIPS: a case about "the same floors" graded against floors a test
	// wrote down would go on passing the day one of them moved. Only the two keys this case
	// is about, plus the preset that keeps a real libx265 encode quick, are named.
	cfg := profileCfg(t, `
library_roots:
  - path: `+d+`
    preset: ultrafast
    min_bitrate_kbps: 0
    max_height: 240
    downscale_acknowledged: true
`)
	if cfg.TopLevelProfile().MinVmaf == 0 {
		t.Fatal("the shipped min_vmaf resolved to 0, so this case would grade nothing")
	}
	ts, _ := runProfiles(t, ffmpeg, ffprobe, cfg)

	out, status, found := outcomeFor(t, ts, src)
	if !found {
		t.Fatalf("no terminal row was written")
	}
	if status != store.Failed {
		t.Fatalf("a half-height encode of this fixture is %q/%q under the SHIPPED floors, want failed "+
			"- if this build accepts it, a downscaling job is being given a discount the criterion "+
			"forbids", status, out.Reason)
	}
	if !strings.Contains(out.Reason, "VMAF") && !strings.Contains(out.Reason, "vmaf") {
		t.Errorf("the rejection is %q, which is not a perceptual one: this case is about the floors", out.Reason)
	}
	if after := md5f(t, src); after != before {
		t.Errorf("the source's bytes changed (%s -> %s) after a rejected encode", before, after)
	}
	if got := codecOf(t, ffprobe, src); got != "h264" {
		t.Errorf("the source is now %q: a rejected encode was swapped in", got)
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the library holds %v after a rejected encode - the temp was not discarded", names)
	}
	// The numbers that rejected it are on the row, scored at the source's resolution.
	if out.VmafMean == nil {
		t.Error("the failed row carries no VMAF figure, so an operator cannot see what rejected it")
	}
	if out.VmafScoredHeight == nil || *out.VmafScoredHeight != 480 {
		t.Errorf("the failed row records the comparison as made at height %v, want the source's 480",
			out.VmafScoredHeight)
	}
}

// TestDownscale_SkipsWhenTheSourceHeightCannotBeDetermined grades [AC-11]: with `max_height`
// set and the source's height undeterminable, the file is skipped with a logged reason and
// nothing is encoded against a guessed or default height.
//
// The fixture is a file with a video EXTENSION and no video in it, which is the shape a
// truncated download and a mis-named file both take. The second arm is the re-derivation: the
// skip records the ceiling it read, so removing the key offers the file back rather than
// holding it on a verdict nothing can move.
//
// MUTATION: resolve the ceiling against a zero height and the file is encoded against a scale
// nobody measured; record no inputs and the second arm's row never moves.
func TestDownscale_SkipsWhenTheSourceHeightCannotBeDetermined(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "truncated.mkv")
	if err := os.WriteFile(src, []byte("this is not a matroska file"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	before := md5f(t, src)

	ts := run(t, ffmpeg, ffprobe, d, nil, capping(240))
	out, status, found := outcomeFor(t, ts, src)
	if !found {
		t.Fatalf("no terminal row was written for a source whose height could not be determined")
	}
	if status != store.Skipped || out.Reason != SkipUndeterminedSourceHeight {
		t.Fatalf("the job is %q/%q, want skipped/%s", status, out.Reason, SkipUndeterminedSourceHeight)
	}
	if after := md5f(t, src); after != before {
		t.Errorf("the source's bytes changed (%s -> %s)", before, after)
	}
	if _, ok := out.DecisionInputs.Value(InputMaxHeight); !ok {
		t.Errorf("the skip records no %s, so removing the ceiling would not offer the file back: %v",
			InputMaxHeight, out.DecisionInputs)
	}
	if out.Downscaled != nil {
		t.Errorf("a file that never reached an encode records downscaled=%v: a guard that fired in "+
			"front of the encoder measured nothing", *out.Downscaled)
	}
}

// TestDownscale_TheDefaultPathIsUnchanged grades [AC-12] at the level this package can: with
// no profile and no rule setting `max_height`, the encode carries no scaling step, the proof
// carries an explicit not-scaled and no scored resolution, and the configuration earns no
// downscaling notice.
//
// The criterion's own grader is `make check`, which runs every case in this repository - this
// one is the part of it that is ABOUT the default rather than merely written under it, and it
// is what says the new columns are silent rather than merely defaulted.
func TestDownscale_TheDefaultPathIsUnchanged(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Tall(t, ffmpeg, src, "8M")

	cfg := baseCfg(d)
	var argv []string
	enc := FFmpegEncoder{
		FFmpeg: ffmpeg, Cfg: cfg, Probe: probe.New(ffmpeg, ffprobe),
		argvObserver: func(args []string) { argv = append([]string(nil), args...) },
	}
	ts := run(t, ffmpeg, ffprobe, d, enc, nil)

	out, status, found := outcomeFor(t, ts, src)
	if !found || status != store.Done {
		t.Fatalf("the default path is %q/%q, not done", status, out.Reason)
	}
	if chain, ok := vfChain(argv); ok {
		t.Errorf("a configuration that sets no ceiling built the video filter chain %q", chain)
	}
	if out.Downscaled == nil || *out.Downscaled {
		t.Errorf("a job under no ceiling records downscaled=%v, want an explicit false - this build "+
			"ran it and scaled nothing, which is a different fact from the null every row written "+
			"before the column carries", out.Downscaled)
	}
	if out.DownscaleScaler != "" {
		t.Errorf("a job that scaled nothing names the scaler %q", out.DownscaleScaler)
	}
	if out.VmafScoredWidth != nil || out.VmafScoredHeight != nil {
		t.Errorf("a job that scaled nothing records a scored resolution (%s): the comparison "+
			"was made at the output's own size, which the row already states",
			pixels(out.VmafScoredWidth, out.VmafScoredHeight))
	}
	if w, h := dimsOf(t, ffprobe, src); w != 640 || h != 480 {
		t.Errorf("the replacement is %dx%d, want the source's own 640x480", w, h)
	}
}
