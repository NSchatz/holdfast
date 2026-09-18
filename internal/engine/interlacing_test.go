package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// mkUnknownFieldOrder writes a NON-HEVC source whose container says NOTHING about its scan
// type: ffprobe answers `field_order=unknown` for it.
//
// It is an ordinary library file rather than a contrivance. A DivX-era MS-MPEG4 AVI is
// exactly the pre-2010 material this guard is about, and the codec is what decides the
// answer: an h264 or hevc elementary stream carries its own picture structure, while this
// one does not, so nothing downstream of the probe can tell progressive from interlaced.
func mkUnknownFieldOrder(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "msmpeg4", "-b:v", bitrate, "-pix_fmt", "yuv420p", "--", path)
}

// mkInterlacedLong writes a PLAINLY interlaced source: real interlacing, no pulldown, and
// long enough for the cadence detector to have a sample worth judging (100 frames).
//
// mkH264Interlaced beside it produces exactly 10 - the detector's own floor - which is
// enough for the guards that only read field_order and too close to the edge for the ones
// that read the pixels.
func mkInterlacedLong(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=8:size=320x240:rate=25",
		"-vf", "interlace=scan=tff", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate,
		"-pix_fmt", "yuv420p", "-flags", "+ilme+ildct", "--", path)
}

// mkTelecined writes a TELECINED source: progressive film carried in an interlaced stream
// by ffmpeg's own 3:2 pulldown, which is the pattern the cadence guard exists to detect.
// Its field_order is interlaced, exactly as a real telecined broadcast master's is, so
// nothing before the detector can tell it from the fixture above.
func mkTelecined(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=5:size=320x240:rate=24",
		"-vf", "telecine=pattern=23", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate,
		"-pix_fmt", "yuv420p", "-flags", "+ilme+ildct", "--", path)
}

// mkInterlacedTooShortToJudge writes an interlaced source of four frames: the container
// says interlaced and the detector cannot establish a five-frame pulldown pattern from a
// sample that small, which is the UNDETERMINED case as a real file rather than as a stub.
func mkInterlacedTooShortToJudge(t *testing.T, ffmpeg, path, bitrate string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=1:size=320x240:rate=8",
		"-vf", "interlace=scan=tff", "-c:v", "libx264", "-preset", "ultrafast", "-b:v", bitrate,
		"-pix_fmt", "yuv420p", "-flags", "+ilme+ildct", "--", path)
}

// deinterlacing returns a config mutation that turns the named filter on for every root.
func deinterlacing(filter string) func(*config.Config) {
	return func(c *config.Config) { c.Deinterlace = filter }
}

// TestDeinterlace_SkipsTelecinedContent grades [AC-6] of
// S0107-holdfast-interlacing-decision: a source established as telecined skips under a
// guard token DISTINCT from the interlace one and is not deinterlaced, whatever value the
// `deinterlace` key carries.
//
// It is the case where a wrong answer is invisible to every gate this tool has.
// Deinterlacing telecined content interpolates fields that were never a moving picture:
// each frame it produces is individually plausible, so VMAF scores it highly and every
// structural check passes, while the motion judders. The source is then deleted. Nothing
// downstream can catch this, which is why it is caught here.
//
// The second arm is what stops the first from being vacuous: a PLAINLY interlaced source,
// same configuration, is not held back by this guard at all.
func TestDeinterlace_SkipsTelecinedContent(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	prober := probe.New(ffmpeg, ffprobe)

	// Every accepted filter, because the criterion is "whatever `deinterlace` is set to":
	// no value of this key may unlock the transformation for telecined content.
	for _, filter := range []string{"yadif", "bwdif"} {
		t.Run("telecined, deinterlace "+filter, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkTelecined(t, ffmpeg, src, "8M")

			// The fixture has to BE telecined, or this grades the wrong branch.
			if cad := prober.Cadence(context.Background(), src); cad.Class != probe.CadenceTelecined {
				t.Fatalf("fixture cadence is %q (frames=%d repeated=%d), want %q - this case is about "+
					"content the detector establishes as telecined",
					cad.Class, cad.Frames, cad.Repeated, probe.CadenceTelecined)
			}

			before := md5f(t, src)
			led := run(t, ffmpeg, ffprobe, d, nil, deinterlacing(filter))

			if got := skipReason(t, led, "movie.mkv"); got != SkipTelecineCadence {
				t.Errorf("recorded reason = %q, want %q - telecined content needs inverse telecine, "+
					"and deinterlacing it produces judder no perceptual metric flags well",
					got, SkipTelecineCadence)
			}
			if SkipTelecineCadence == SkipInterlaced {
				t.Error("the cadence guard must carry a token distinct from the interlace one")
			}
			if md5f(t, src) != before {
				t.Error("the telecined source was modified")
			}
			if codecOf(t, ffprobe, src) != "h264" {
				t.Error("the telecined source was transcoded")
			}
			if nTemp(t, d) != 0 {
				t.Error("temp left behind")
			}
		})
	}

	t.Run("plainly interlaced content is not held back by this guard", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkInterlacedLong(t, ffmpeg, src, "8M")

		if cad := prober.Cadence(context.Background(), src); cad.Class != probe.CadenceInterlaced {
			t.Fatalf("control fixture cadence is %q, want %q", cad.Class, probe.CadenceInterlaced)
		}
		led := run(t, ffmpeg, ffprobe, d, nil, deinterlacing("yadif"))
		if got := skipReason(t, led, "movie.mkv"); got == SkipTelecineCadence {
			t.Error("a plainly interlaced source was held back by the cadence guard, which would " +
				"make the telecine skip above a skip of everything")
		}
	})
}

// TestDeinterlace_SkipsUndeterminedCadence grades [AC-7] of
// S0107-holdfast-interlacing-decision: a source whose cadence cannot be established either
// way skips under the SAME telecine guard rather than being transformed on a guess.
//
// Cadence detection is a heuristic, and this is what keeps a heuristic's uncertainty from
// becoming a transformation. The fixture is a real file that is genuinely too short for a
// five-frame pattern to be established, not a stubbed detector: the reason has to be one an
// operator could meet.
func TestDeinterlace_SkipsUndeterminedCadence(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	prober := probe.New(ffmpeg, ffprobe)

	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkInterlacedTooShortToJudge(t, ffmpeg, src, "8M")

	cad := prober.Cadence(context.Background(), src)
	if cad.Class != probe.CadenceUndetermined {
		t.Fatalf("fixture cadence is %q, want %q - this case is about content nobody could classify",
			cad.Class, probe.CadenceUndetermined)
	}
	if cad.Why == "" {
		t.Error("an undetermined cadence carries no reason, so a row recording it sends an operator " +
			"to a file with nothing to look at")
	}

	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, nil, deinterlacing("yadif"))

	if got := skipReason(t, led, "movie.mkv"); got != SkipTelecineCadence {
		t.Errorf("recorded reason = %q, want %q - an unestablished cadence is not a licence to "+
			"transform, and it shares a token with the telecine case because it shares a remedy",
			got, SkipTelecineCadence)
	}
	if md5f(t, src) != before {
		t.Error("a source nobody could classify was modified")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp left behind")
	}
}

// TestSkipGuards_IncludeTelecineAndUnknownFieldOrder grades [AC-8] of
// S0107-holdfast-interlacing-decision: both new guards draw their token from the closed,
// stable skip vocabulary the existing guards use, and `holdfast requeue --guard` accepts
// each of them alongside the rest.
//
// A skip token is a WIRE FORMAT: it lands on a stored row, in the API payload and on a
// /metrics label. A guard whose token is absent from SkipGuards is a permanent, silent
// exclusion - requeue answers ErrUnknownGuard for it and no configuration change re-derives
// a verdict that read no key - so acceptance is asserted against the RUNNING verb over real
// rows rather than against the list.
func TestSkipGuards_IncludeTelecineAndUnknownFieldOrder(t *testing.T) {
	ctx := context.Background()
	for _, tok := range []string{SkipTelecineCadence, SkipUnknownFieldOrder} {
		t.Run(tok, func(t *testing.T) {
			// In the whole vocabulary, which is what every consumer that must account for
			// every skip this engine records reads (the /metrics label set is the standing one).
			if !slices.Contains(SkipVocabulary, tok) {
				t.Errorf("%q is not in SkipVocabulary, so a consumer enumerating every skip this "+
					"engine can record has no bucket for it", tok)
			}
			if !KnownGuard(tok) {
				t.Errorf("KnownGuard(%q) = false, so `requeue --guard %s` answers ErrUnknownGuard and "+
					"the rows it skipped have no lever at all", tok, tok)
			}

			// And accepted by the verb itself, over a row that carries it.
			ts := requeueStore(t)
			path := "/lib/" + tok + ".mkv"
			seedTerminal(t, ts, path, store.Skipped, &store.Outcome{Reason: tok})
			res, err := Requeue(ctx, ts, RequeueSelector{Guard: tok}, 3)
			if err != nil {
				t.Fatalf("requeue --guard %s: %v", tok, err)
			}
			if len(res.Reopened) != 1 || res.Reopened[0] != path {
				t.Errorf("requeue --guard %s re-opened %v, want [%s]", tok, res.Reopened, path)
			}
		})
	}
}

// TestDeinterlace_DefaultOffPreservesSkipInterlaced grades [AC-1] of
// S0107-holdfast-interlacing-decision: with no profile setting `deinterlace`, an
// interlaced source behaves exactly as it did before this feature existed - it skips under
// the `interlaced` guard, nothing on disk moves, and the row it records is the row the
// build before this one wrote.
//
// The second half is the one that is easy to lose. A new profile knob is seeded from the
// top level for every root, so it can silently reach two things a terminal row carries: the
// decision inputs the guard records, and the profile digest that attaches the row to the
// profile which decided it. An install that never asked for a deinterlace must see neither
// move, or every row in its ledger changes meaning on upgrade.
func TestDeinterlace_DefaultOffPreservesSkipInterlaced(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264Interlaced(t, ffmpeg, src, "8M")

	before := md5f(t, src)
	led := run(t, ffmpeg, ffprobe, d, nil, nil)

	if got := skipReason(t, led, "movie.mkv"); got != SkipInterlaced {
		t.Errorf("recorded reason = %q, want %q - with the key unset this tool skips interlaced "+
			"sources exactly as it always has", got, SkipInterlaced)
	}
	if md5f(t, src) != before {
		t.Error("the interlaced source was modified")
	}
	if nTemp(t, d) != 0 {
		t.Error("temp left behind")
	}

	row := rowForFile(t, led, "movie.mkv")
	if _, ok := row.Outcome.DecisionInputs.Value(InputDeinterlace); ok {
		t.Errorf("the row records %s=%v with the key unset - a root that asks for no deinterlace must "+
			"record exactly the read-set it recorded before the knob existed, or every skipped row in "+
			"an existing ledger changes on upgrade", InputDeinterlace, row.Outcome.DecisionInputs.Keys())
	}

	// The digest half: the knob is in profileKnobs, so it reaches the digest input unless
	// the default is excluded from it. A root that deinterlaces nothing must digest as it
	// did before, and a root that DOES must not share that digest - the second arm is what
	// keeps the first from being an unconditional exclusion.
	cfg := baseCfg(d)
	prof := cfg.TopLevelProfile()
	unset := prof
	unset.Deinterlace = ""
	off := prof
	off.Deinterlace = "off"
	if unset.Digest() != off.Digest() {
		t.Errorf("an unset deinterlace digests %s and an explicit off digests %s - two profiles that "+
			"deinterlace nothing decide every file identically and must carry one identifier",
			unset.Digest(), off.Digest())
	}
	if row.Outcome.ProfileDigest != unset.Digest() {
		t.Errorf("the row records profile digest %q, want %q - the digest an install that asks for no "+
			"deinterlace carried before this knob existed", row.Outcome.ProfileDigest, unset.Digest())
	}
	on := prof
	on.Deinterlace = "yadif"
	if on.Digest() == unset.Digest() {
		t.Error("a root that deinterlaces digests the same as one that does not: a row decided under a " +
			"converting profile would be indistinguishable from one decided under a preserving one")
	}
}

// TestFieldOrderUnknown_IsClassifiedExplicitly grades [AC-9] of
// S0107-holdfast-interlacing-decision: a source whose field order ffprobe could not
// establish is classified under its OWN guard token and its own recorded outcome, rather
// than reaching the progressive encode path by omission.
//
// The defect it closes is a silent one. The interlace guard named the four interlaced
// spellings and let everything else through, so a file whose container never stated its
// scan type was re-encoded by a progressive-assuming pipeline - which bakes combing in
// permanently - and then had its source deleted by the swap. Nothing in the ledger said a
// guess had been made, because nothing knew one had been.
//
// The second arm is what stops the first from being vacuous: the identical configuration
// over a source that DOES declare itself progressive still transcodes, so the skip is the
// field order and not the fixture.
func TestFieldOrderUnknown_IsClassifiedExplicitly(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	prober := probe.New(ffmpeg, ffprobe)

	t.Run("an unestablished field order skips under its own token", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.avi")
		mkUnknownFieldOrder(t, ffmpeg, src, "3M")

		// The fixture has to BE the case under test, or the assertions below grade nothing.
		if fo := prober.FieldOrder(context.Background(), src); fo != "" {
			t.Fatalf("fixture's field order normalised to %q, want \"\" - this fixture is meant to "+
				"be the source ffprobe cannot establish a field order for", fo)
		}

		before := md5f(t, src)
		led := run(t, ffmpeg, ffprobe, d, nil, nil)

		if got := skipReason(t, led, "movie.avi"); got != SkipUnknownFieldOrder {
			t.Errorf("recorded reason = %q, want %q - an unestablished field order must be "+
				"classified explicitly, so an operator can see that the file was held back and "+
				"`requeue --guard %s` can offer it again", got, SkipUnknownFieldOrder, SkipUnknownFieldOrder)
		}
		if !ledgerHas(t, led, store.Skipped, "movie.avi") {
			t.Error("expected a skipped row for a source whose field order could not be established")
		}
		if md5f(t, src) != before {
			t.Error("the source was modified - a file nothing could classify must be left byte-for-byte alone")
		}
		if codecOf(t, ffprobe, src) != "msmpeg4v3" {
			t.Error("the source was transcoded - the unknown field order reached the encode path by omission")
		}
		if nTemp(t, d) != 0 {
			t.Error("temp left behind")
		}
	})

	t.Run("a configured deinterlace does not license the guess either", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.avi")
		mkUnknownFieldOrder(t, ffmpeg, src, "3M")

		before := md5f(t, src)
		led := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) { c.Deinterlace = "yadif" })

		if got := skipReason(t, led, "movie.avi"); got != SkipUnknownFieldOrder {
			t.Errorf("recorded reason = %q, want %q - a deinterlace answers what to do about an "+
				"interlaced source, not what to do about one nobody can classify: applying a filter "+
				"to a file that may well be progressive is a transformation on a guess",
				got, SkipUnknownFieldOrder)
		}
		if md5f(t, src) != before {
			t.Error("the source was modified with the deinterlace configured")
		}
	})

	t.Run("a declared-progressive source still transcodes", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")

		if fo := prober.FieldOrder(context.Background(), src); fo != "progressive" {
			t.Fatalf("control fixture's field order = %q, want \"progressive\"", fo)
		}
		run(t, ffmpeg, ffprobe, d, nil, nil)
		if got := codecOf(t, ffprobe, src); got != "hevc" {
			t.Errorf("control source codec = %q, want hevc - the new guard must refuse only the "+
				"sources nobody can classify, not every source", got)
		}
	})
}
