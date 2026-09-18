package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/deinterlace"
	"github.com/NSchatz/holdfast/internal/downscale"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
	"github.com/NSchatz/holdfast/internal/vmaf"
)

// The perceptual gate on a deinterlacing job.
//
// This is where the blast radius is. VMAF is the last no-loss layer before a source is
// deleted, and a deinterlaced encode compared against the interlaced source it came from is
// not a measurement of that encode at all: the filter removed something the reference still
// carries, so the number that comes back describes the FILTER. Every case below is about
// that one substitution and what must happen when it cannot be made.

// deinterlacedEncode writes a real deinterlaced encode of src through the PRODUCTION
// encoder at the profile given, and returns the output path. It is the production argv
// rather than a hand-built ffmpeg call, so what is scored below is what this build produces.
func deinterlacedEncode(t *testing.T, ffmpeg, ffprobe string, cfg config.Config, src, out string) {
	t.Helper()
	prober := probe.New(ffmpeg, ffprobe)
	enc := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}.ForProfile(cfg.TopLevelProfile())
	if err := enc.Encode(context.Background(), src, out, prober.VideoProps(context.Background(), src)); err != nil {
		t.Fatalf("encoding %s: %v", src, err)
	}
}

// scoreAgainst measures distorted against reference through REAL libvmaf, with the
// reference put through film first.
func scoreAgainst(t *testing.T, ffmpeg, ffprobe, distorted, reference string, film deinterlace.Filter) (vmaf.Result, error) {
	t.Helper()
	prober := probe.New(ffmpeg, ffprobe)
	ctx := context.Background()
	pixFmt, ok := vmaf.ComparisonFormat(prober.PixFmt(ctx, reference), prober.PixFmt(ctx, distorted))
	if !ok {
		t.Fatalf("no comparison pixel format for %s and %s", reference, distorted)
	}
	return vmaf.Score(ctx, ffmpeg, vmaf.Request{
		Distorted:       distorted,
		Reference:       reference,
		Subsample:       1,
		Model:           vmaf.ResolveModel("auto", prober.Height(ctx, distorted)),
		PixelFormat:     pixFmt,
		ReferenceFilter: film.Spec,
	})
}

// TestDeinterlace_ScoresAgainstADeinterlacedReference grades [AC-10] of
// S0107-holdfast-interlacing-decision: the perceptual gate compares a deinterlaced encode
// against a reference produced from the SOURCE by the same filter at the same parameters,
// and records that filter and its parameters in the same proof that carries the comparison
// pixel format.
//
// It runs through REAL ffmpeg and real libvmaf on both arms. The measurement arm is what
// makes the criterion more than a plumbing assertion: the same encode scored against the
// interlaced source it came from scores FAR lower - low enough to be rejected by the
// shipped floors - so a build that skipped the reference filter would not merely record a
// slightly different number, it would reject good encodes and, on the day a floor moved,
// accept bad ones for reasons nobody could read off the row.
func TestDeinterlace_ScoresAgainstADeinterlacedReference(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkInterlacedLong(t, ffmpeg, src, "8M")

	cfg := baseCfg(d)
	cfg.Deinterlace = "yadif"
	film, err := deinterlaceFor(cfg.TopLevelProfile())
	if err != nil {
		t.Fatalf("resolving the configured filter: %v", err)
	}
	out := filepath.Join(d, "out.mkv")
	deinterlacedEncode(t, ffmpeg, ffprobe, cfg, src, out)

	// 1. THE MEASUREMENT, against the reference the gate is supposed to build.
	good, err := scoreAgainst(t, ffmpeg, ffprobe, out, src, film)
	if err != nil {
		t.Fatalf("scoring against a deinterlaced reference: %v", err)
	}
	if good.ReferenceFilter != film.Spec {
		t.Errorf("the result carries reference filter %q, want %q - the filter travels with the "+
			"number or the number cannot be interpreted", good.ReferenceFilter, film.Spec)
	}
	if good.PixelFormat == "" {
		t.Error("the result carries no comparison pixel format, so the filter is recorded in a proof " +
			"that no longer carries the fact it was supposed to sit beside")
	}

	// 2. THE SUBSTITUTION THAT IS NOT ALLOWED: the same encode against the interlaced
	// source. If this scored the same, the criterion would be about nothing.
	bad, err := scoreAgainst(t, ffmpeg, ffprobe, out, src, deinterlace.Filter{})
	if err != nil {
		t.Fatalf("scoring against the interlaced source: %v", err)
	}
	if !(good.HarmonicMean > bad.HarmonicMean+5) {
		t.Errorf("scoring against a deinterlaced reference gave %.2f and against the interlaced source "+
			"gave %.2f - the two are not meaningfully apart, so this case does not establish that the "+
			"reference filter is what makes the gate a measurement of THIS encode",
			good.HarmonicMean, bad.HarmonicMean)
	}
	// And the difference is DECISION-CHANGING, not merely visible: the shipped min_vmaf is
	// 95, so the wrong reference rejects an encode the right one accepts. That is what makes
	// "a score against an interlaced reference is not a measurement of this encode" a
	// statement about files rather than about numbers.
	const shippedMinVmaf = 95.0
	if good.HarmonicMean < shippedMinVmaf || bad.HarmonicMean >= shippedMinVmaf {
		t.Errorf("against the deinterlaced reference this encode scores %.2f and against the interlaced "+
			"source %.2f, with the shipped min_vmaf at %.2f - the pair does not straddle the floor, so "+
			"this fixture does not show the substitution changing a verdict",
			good.HarmonicMean, bad.HarmonicMean, shippedMinVmaf)
	}

	// 3. THE PROOF THE GATE CARRIES, taken from the gate itself with the real scorer: the
	// filter is recorded beside the comparison format, on the same proof, from one run.
	eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.Deinterlace = "yadif"
		c.VmafEnable = boolPtr(true)
	})
	prof := eng.Cfg.TopLevelProfile()
	proof, gate, class, verr := eng.verifyOutput(context.Background(), src, out, prof,
		targetCodecFor(eng.Cfg.TranscodeIn(prof, src).Encoder), planFor(t, eng, src, prof), film,
		downscale.Scale{})
	if verr != nil {
		t.Fatalf("the gate rejected a deinterlaced encode scored against a deinterlaced reference "+
			"(gate=%q class=%q): %v", gate, class, verr)
	}
	if proof.Deinterlace != film.Spec {
		t.Errorf("the recorded proof carries deinterlace %q, want %q - the filter and its parameters "+
			"belong in the same proof as the comparison format, everywhere that proof is recorded",
			proof.Deinterlace, film.Spec)
	}
	if proof.PixFmt == "" || proof.Mean == nil {
		t.Errorf("the proof is incomplete (pix_fmt=%q mean=%v), so what the filter was recorded beside "+
			"is not the proof the gate produced", proof.PixFmt, proof.Mean)
	}

	// 4. EVERYWHERE THAT PROOF IS RECORDED. A proof that carries the filter and a ROW that
	// does not is a filter recorded nowhere a reader will look: the row outlives the source,
	// and the log line scrolls away.
	e2e := t.TempDir()
	esrc := filepath.Join(e2e, "movie.mkv")
	mkInterlacedLong(t, ffmpeg, esrc, "8M")
	led := run(t, ffmpeg, ffprobe, e2e, nil, func(c *config.Config) {
		c.Deinterlace = "yadif"
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 90
	})
	row := rowForFile(t, led, "movie.mkv")
	if row.Status != store.Done {
		t.Fatalf("the end-to-end job is %q/%q, want done", row.Status, row.Outcome.Reason)
	}
	if row.Outcome.VmafPixFmt == "" {
		t.Fatal("the row carries no comparison pixel format, so there is no proof here for the filter " +
			"to be recorded beside")
	}
	if row.Outcome.DeinterlaceFilter != film.Spec {
		t.Errorf("the terminal row records deinterlace filter %q, want %q - beside vmaf_pix_fmt, on the "+
			"same row, because that is where the proof is kept", row.Outcome.DeinterlaceFilter, film.Spec)
	}
	if row.Outcome.Deinterlaced == nil || !*row.Outcome.Deinterlaced {
		t.Errorf("the terminal row records deinterlaced = %v for a job that deinterlaced its source",
			row.Outcome.Deinterlaced)
	}
}

// TestDeinterlace_ReferenceFailureLeavesSourceIntact grades [AC-11] of
// S0107-holdfast-interlacing-decision: when the deinterlaced reference cannot be produced
// or cannot be scored, the temp output is discarded, the source is left intact, the failure
// is recorded, and there is NO second attempt against the interlaced source.
//
// The fallback is the thing this criterion exists to forbid, and it is the tempting
// implementation: a measurement failed, a reference is right there, and scoring against it
// produces a number. That number would be a measurement of the filter, and accepting an
// encode on it - or rejecting a good one - is a verdict about a file this tool then deletes.
func TestDeinterlace_ReferenceFailureLeavesSourceIntact(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("a reference ffmpeg cannot produce is a real failure, not a hypothetical", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkInterlacedLong(t, ffmpeg, src, "8M")
		cfg := baseCfg(d)
		cfg.Deinterlace = "yadif"
		out := filepath.Join(d, "out.mkv")
		deinterlacedEncode(t, ffmpeg, ffprobe, cfg, src, out)

		// A filter expression ffmpeg parses and refuses: the graph cannot be built, so the
		// reference cannot be produced, so nothing is measured.
		_, err := scoreAgainst(t, ffmpeg, ffprobe, out, src,
			deinterlace.Filter{Name: "yadif", Spec: "yadif=mode=nonsense"})
		if err == nil {
			t.Fatal("libvmaf scored a pair whose reference filter ffmpeg cannot build - then 'the " +
				"reference could not be produced' is not a state this gate can ever be in, and the " +
				"case below would prove nothing")
		}
	})

	t.Run("the engine discards the temp, keeps the source and never falls back", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkInterlacedLong(t, ffmpeg, src, "8M")
		before := md5f(t, src)

		eng := buildEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
			c.Deinterlace = "yadif"
			c.VmafEnable = boolPtr(true)
		})
		ts := eng.Store.(*testStore)
		var requests []vmaf.Request
		eng.vmafScore = func(_ context.Context, req vmaf.Request) (vmaf.Result, error) {
			requests = append(requests, req)
			return vmaf.Result{}, errors.New("the deinterlaced reference could not be produced")
		}
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}

		if md5f(t, src) != before {
			t.Error("the source was modified after the reference could not be produced - the swap is " +
				"licensed by a measurement, and there was none")
		}
		if nTemp(t, d) != 0 {
			t.Error("the temp output was left behind by a job whose gate could not measure it")
		}
		row := rowFor(t, ts, src)
		if row.Status != store.Failed {
			t.Fatalf("the row is %q/%q, want failed - an unmeasured encode is refused like any other",
				row.Status, row.Outcome.Reason)
		}
		if !strings.Contains(row.Outcome.Reason, "could not be produced") {
			t.Errorf("the recorded reason does not carry the measurement's own failure: %q", row.Outcome.Reason)
		}

		// THE FALLBACK THAT MUST NOT HAPPEN. Every attempt made carried the filter; not one
		// of them asked for a score against the source as it is.
		if len(requests) == 0 {
			t.Fatal("the scorer was never called, so nothing here says what it was asked for")
		}
		for i, req := range requests {
			if req.ReferenceFilter == "" {
				t.Errorf("attempt %d asked for a score against an UNFILTERED reference: that is a "+
					"measurement of the difference the filter made, offered as a measurement of the "+
					"encode", i)
			}
		}
	})
}

// TestDeinterlace_FloorsAreNotRelaxed grades [AC-12] of
// S0107-holdfast-interlacing-decision: a deinterlacing encode meets the same VMAF mean and
// worst-frame floors as any other, with no relaxation, exemption or alternate threshold
// keyed off `deinterlace`.
//
// It is asserted as an EQUIVALENCE over the same measured numbers: for each floor, the
// verdict a deinterlacing job gets is the verdict a plain one gets. An exemption keyed off
// the key - a wider floor, a skipped floor, a softened class - shows up here as a
// difference, whichever of the three it is written into.
func TestDeinterlace_FloorsAreNotRelaxed(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkInterlacedLong(t, ffmpeg, src, "8M")
	out := filepath.Join(d, "out.mkv")
	cfg := baseCfg(d)
	cfg.Deinterlace = "yadif"
	deinterlacedEncode(t, ffmpeg, ffprobe, cfg, src, out)

	film, err := deinterlaceFor(cfg.TopLevelProfile())
	if err != nil {
		t.Fatalf("resolving the configured filter: %v", err)
	}

	measured := []struct {
		name string
		res  vmaf.Result
	}{
		{"clears every floor", vmaf.Result{HarmonicMean: 98.4, Min: 96.1, ChromaMin: 41.2}},
		{"below min_vmaf", vmaf.Result{HarmonicMean: 90.0, Min: 96.1, ChromaMin: 41.2}},
		{"below vmaf_min_pool", vmaf.Result{HarmonicMean: 98.4, Min: 40.0, ChromaMin: 41.2}},
		{"below vmaf_min_chroma", vmaf.Result{HarmonicMean: 98.4, Min: 96.1, ChromaMin: 12.0}},
	}
	for _, m := range measured {
		t.Run(m.name, func(t *testing.T) {
			// The same engine, the same profile, the same measured numbers - once with a
			// deinterlace in force and once without.
			with := gateVerdict(t, ffmpeg, ffprobe, d, src, out, "yadif", film, m.res)
			without := gateVerdict(t, ffmpeg, ffprobe, d, src, out, "off", deinterlace.Filter{}, m.res)
			if with != without {
				t.Errorf("a deinterlacing job got %q and a plain one got %q for the same measurement "+
					"(mean=%.1f min=%.1f chroma=%.1f) - the floors are the floors, and an exemption "+
					"keyed off the deinterlace key is exactly what this forbids",
					with, without, m.res.HarmonicMean, m.res.Min, m.res.ChromaMin)
			}
		})
	}
}

// gateVerdict runs the perceptual gate over one pair with the measurement supplied, and
// renders the verdict as text so two of them can be compared whole: which gate refused,
// which class it carried, and whether it refused at all.
func gateVerdict(t *testing.T, ffmpeg, ffprobe, dir, src, out, key string,
	film deinterlace.Filter, res vmaf.Result) string {
	t.Helper()
	eng := buildEngine(t, ffmpeg, ffprobe, dir, nil, func(c *config.Config) {
		c.Deinterlace = key
		c.VmafEnable = boolPtr(true)
		c.MinVmaf = 95
		c.VmafMinPool = 60
		c.VmafMinChroma = 30
	})
	eng.vmafScore = func(_ context.Context, req vmaf.Request) (vmaf.Result, error) {
		r := res
		r.ChromaMetric, r.PixelFormat, r.Stream = vmaf.ChromaMetricName, req.PixelFormat, vmaf.ScoredStream
		r.ReferenceFilter = req.ReferenceFilter
		return r, nil
	}
	prof := eng.Cfg.TopLevelProfile()
	_, gate, class, err := eng.vmafGate(context.Background(), out, src, prof, film, downscale.Scale{})
	refused := "accepted"
	if err != nil {
		refused = "refused"
	}
	return refused + " gate=" + gate + " class=" + string(class)
}
