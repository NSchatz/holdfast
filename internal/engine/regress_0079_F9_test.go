package engine

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
)

// S0079 F9 - AC-A9 is not satisfied on a ledger that already holds a row.
//
// AC-A9: "WHEN a job's effective encoder differs from the top-level encoder, THE
// SYSTEM SHALL decide the already-at-target-codec skip and the output-codec
// acceptance check against the codec THAT job's encoder produces: a source already
// in the top-level target codec whose matching profile targets a different codec
// SHALL be transcoded rather than skipped".
//
// The guard itself reads the job's effective encoder, and on a FRESH ledger the
// criterion holds. What this case covers is the state every existing install is in
// when an operator adds their first encode profile: the file already carries a
// skipped/already-at-target-codec row, and whether the guard is allowed to run again
// at all is decided by store.Claim, from the decision inputs that row recorded.
//
// Those inputs are read from the LIBRARY ROOT's profile only (Engine.inputsRead ->
// DecisionInputsForProfile -> targetCodecFor(prof.Encoder)), so the row records
// target_codec=hevc whatever the encode profiles say - and the current value Claim
// compares it against is read from the same place (Engine.inputsFor(prof)). Adding,
// editing or removing an encode profile moves NEITHER side, the row is never
// re-opened, and the source the criterion says SHALL be transcoded stays skipped.
//
// The two halves below are what make that a defect rather than a limitation of the
// ledger: the IDENTICAL operator intent expressed on the root's own `encoder` does
// move both values and does offer the file back. The re-opening machinery is there
// and it works; the layer this item adds is not wired into it.
func TestRegressS0079F9_AnAddedEncodeProfileOffersBackTheFileItsTargetCodecMoved(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	// scanTwice puts one hevc source under a fresh root, scans it under the top-level
	// settings (which target hevc, so the already-at-target-codec guard skips it), then
	// scans the SAME ledger under the second configuration and reports how many files
	// reached the encoder on that second pass. Two Engines rather than one mutated in
	// place, because a root's resolved profile is read from the configuration once, in
	// New - so an engine whose Cfg is edited afterwards is not the run an operator gets.
	scanTwice := func(t *testing.T, second func(*config.Config)) (int32, string) {
		t.Helper()
		root := t.TempDir()
		mkHevc(t, ffmpeg, filepath.Join(root, "film.mkv"), "800k")

		var encodes atomic.Int32
		prober := probe.New(ffmpeg, ffprobe)
		ts := newTestStore(t, root)

		first := New(baseCfg(root), prober, countingEncoder(&encodes), ts, discardLogger())
		if err := first.RunOneshot(context.Background()); err != nil {
			t.Fatalf("first RunOneshot: %v", err)
		}
		if got := skipReason(t, ts, "film.mkv"); got != SkipAlreadyTargetCodec {
			t.Fatalf("film.mkv is skipped %q, want %q - the fixture never reached the guard under test",
				got, SkipAlreadyTargetCodec)
		}
		if n := encodes.Load(); n != 0 {
			t.Fatalf("the first scan handed %d file(s) to the encoder; an already-at-target source must not reach it", n)
		}

		cfg := baseCfg(root)
		second(&cfg)
		next := New(cfg, prober, countingEncoder(&encodes), ts, discardLogger())
		if err := next.RunOneshot(context.Background()); err != nil {
			t.Fatalf("second RunOneshot: %v", err)
		}
		return encodes.Load(), skipReason(t, ts, "film.mkv")
	}

	// The control. Moving the ROOT's encoder is the same operator intent - "these files
	// target AV1 now" - and it re-opens the row, so the guard runs again against the new
	// target and the file is offered to the encoder. If this half ever fails, the half
	// below proves nothing.
	t.Run("moving the top-level encoder offers the file back", func(t *testing.T) {
		n, reason := scanTwice(t, func(c *config.Config) { c.Encoder = "svtav1" })
		if n == 0 {
			t.Fatalf("the top-level encoder moved from cpu (hevc) to svtav1 (av1) and the hevc source was not "+
				"offered to the encoder (skip reason now %q). Nothing below this line can be read as a defect "+
				"in encode_profiles if the ledger never re-opens a row at all", reason)
		}
	})

	// The defect. The same change expressed the way this item adds - an encode profile
	// whose match selects the file and whose encoder targets av1 - reaches the identical
	// job settings and leaves the file skipped for ever.
	t.Run("an encode profile that moves the target codec does not", func(t *testing.T) {
		n, reason := scanTwice(t, func(c *config.Config) {
			c.EncodeProfiles = []config.EncodeProfile{
				{Name: "av1", Match: "*.mkv", Encoder: strp("svtav1")},
			}
		})
		if n == 0 {
			t.Errorf("AC-A9: with an encode profile targeting av1 in force, the hevc source was NOT offered to "+
				"the encoder - it still carries the skipped/%s row written before the profile existed. Its row "+
				"recorded target_codec from the LIBRARY ROOT's profile and store.Claim compares it against a "+
				"current value read from the same place, so no encode profile can move either side. The criterion "+
				"says a source already in the top-level target codec whose matching profile targets a different "+
				"codec SHALL be transcoded rather than skipped; the control above shows the ledger re-opens that "+
				"very row for the very same change made on the root's own encoder", reason)
		}
		if reason == SkipAlreadyTargetCodec {
			t.Errorf("AC-A9: film.mkv still carries the skipped/%s row decided under the configuration that had "+
				"no encode profiles", reason)
		}
	})
}
