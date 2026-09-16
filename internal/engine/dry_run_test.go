package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// What a DRY RUN records, and what it still refuses to do.
//
// An operator turns dry_run on to answer one question before they let this tool delete
// anything: which files would it transcode, and what would it do to each? The answer has to
// be a RECORD - one terminal row per file it looked at, carrying what a real run would have
// done and no measurement of work nobody did - and taking it has to leave the library and
// the pipeline exactly as they were, so a real run afterwards behaves as though the preview
// had never happened.
//
// Every case below names the acceptance criterion it grades.

// dryRunFixture builds a library holding one h264 file that passes every guard, plus an
// already-HEVC file that does not, and returns the root and the engine. The encoder is one
// that FAILS THE TEST if it is called: "a dry run encodes nothing" is asserted by making
// an encode impossible rather than by inspecting what was left behind afterwards.
func dryRunFixture(t *testing.T) (root string, eng *Engine, ts *testStore) {
	t.Helper()
	return dryRunFixtureExt(t, "source") // an in-place decision; no ext change to complicate it
}

// dryRunFixtureExt is dryRunFixture with the output container named, for the one case that
// needs the target path to be a DIFFERENT path from the source.
func dryRunFixtureExt(t *testing.T, containerExt string) (root string, eng *Engine, ts *testStore) {
	t.Helper()
	ffmpeg, _ := tools(t)
	root = t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(root, "candidate.mkv"), "8M")
	mkHevc(t, ffmpeg, filepath.Join(root, "already.mkv"), "8M")

	cfg := baseCfg(root)
	cfg.DryRun = true
	cfg.ContainerExt = containerExt
	ts = newTestStore(t, root)
	return root, newDryEngine(t, cfg, ts, discardLogger()), ts
}

// newDryEngine builds an engine over cfg whose encoder FAILS THE TEST if it is reached.
func newDryEngine(t *testing.T, cfg config.Config, ts *testStore, log *slog.Logger) *Engine {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
		t.Errorf("the ENCODER RAN during a dry run (%s -> %s). A dry run reports what it would "+
			"do and does none of it", in, out)
		return nil
	})
	return New(cfg, probe.New(ffmpeg, ffprobe), enc, ts, log)
}

// TestDryRun_LeavesNoJobActive grades [AC-1]: once the pass is over the store reports zero
// rows in any active state, and exactly one terminal row for each file the pass was eligible
// to look at.
//
// `probing` means CLAIMED AND NOT YET DECIDED, so a file the run has finished deciding
// sitting in that bucket makes the dashboard, the summary and /metrics all report work in
// hand over a library nothing is working on. The count is asserted from the SUMMARY, which
// is the figure those surfaces read, and the eligible set comes from the engine's own
// enumeration rather than from the fixture's filenames - a criterion about "each eligible
// file" that the test decided the meaning of for itself would grade nothing.
func TestDryRun_LeavesNoJobActive(t *testing.T) {
	_, eng, ts := dryRunFixture(t)
	ctx := context.Background()

	eligible, _ := eng.enumerate()
	if len(eligible) != 2 {
		t.Fatalf("the fixture offers %d eligible file(s), want 2: %v", len(eligible), eligible)
	}

	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	sum, err := ts.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	for st, n := range sum {
		if st.Active() && n != 0 {
			rows, _ := ts.List(ctx, []store.Status{st}, 0)
			t.Errorf("after a finished dry run %d row(s) are still %q on account of a decision that "+
				"run already took: %+v", n, st, rows)
		}
	}

	rows, err := ts.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	perPath := map[string]int{}
	for _, r := range rows {
		perPath[r.Path]++
		if !r.Status.Terminal() {
			t.Errorf("the row for %s is in a non-terminal state (%q) after the pass finished", r.Path, r.Status)
		}
	}
	for _, f := range eligible {
		if perPath[f] != 1 {
			t.Errorf("the pass left %d row(s) for the eligible file %s, want exactly 1. A decision this "+
				"run took has to be one recorded fact about that file: %+v", perPath[f], f, rows)
		}
	}
	if len(rows) != len(eligible) {
		t.Errorf("the pass left %d row(s) over %d eligible file(s): %+v", len(rows), len(eligible), rows)
	}
}

// TestDryRun_RecordsTheDecisionAndEncodesNothing grades [AC-2] and [AC-3]: the terminal row
// carries the four facts that say what a real run WOULD have done to this file - the path
// the replacement would have been written to, the target codec, the encoder that would have
// run, and the size of the source - and carries NO measurement of the encode that never
// happened.
//
// The container is forced to a DIFFERENT extension on purpose. With an in-place decision the
// target path is the source's own path and the assertion would hold whatever the row
// recorded; here the name the swap would publish under is a fact nothing else on the row
// carries, and an operator cannot re-derive it without re-resolving the layering themselves.
func TestDryRun_RecordsTheDecisionAndEncodesNothing(t *testing.T) {
	root, eng, ts := dryRunFixtureExt(t, "mp4")
	ctx := context.Background()

	candidate := filepath.Join(root, "candidate.mkv")
	before := md5f(t, candidate)
	beforeSize := fileSize(t, candidate)
	beforeTree := treeSnapshot(t, root)

	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// --- the decision is RECORDED, and it is terminal -----------------------------
	rows, err := ts.List(ctx, []store.Status{store.WouldTranscode}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("a dry run over one qualifying file recorded %d would-transcode rows, want 1. "+
			"A decision this run took has to be a recorded fact, not a row left in the state the "+
			"worker parked it in: %+v", len(rows), rows)
	}
	row := rows[0]
	if row.Path != candidate {
		t.Errorf("the recorded decision names %q, want %q", row.Path, candidate)
	}
	if !row.Status.Terminal() {
		t.Errorf("the recorded decision is in a non-terminal state (%q)", row.Status)
	}

	// --- [AC-2] carrying the action it would have taken ---------------------------
	//
	// The codec and the size are read FROM THE FILE THAT WAS DECIDED: the codec is what
	// ffprobe answered for it, and the size is what the filesystem reports for it right now.
	wantCodec := codecOf(t, envOr("HOLDFAST_FFPROBE", "ffprobe"), candidate)
	if wantCodec == "" {
		t.Fatal("the fixture has no probeable video codec - the case would prove nothing")
	}
	if row.Outcome.SourceCodec != wantCodec {
		t.Errorf("the recorded source codec is %q, want %q (the codec of the file that was decided)",
			row.Outcome.SourceCodec, wantCodec)
	}
	wantTarget := filepath.Join(root, "candidate.mp4")
	if wantTarget == candidate {
		t.Fatal("the fixture's target path is the source's own path, so recording it would be free " +
			"and this assertion would prove nothing")
	}
	if row.Outcome.TargetPath != wantTarget {
		t.Errorf("the recorded target path is %q, want %q. An operator reading the row has to be able "+
			"to see the name the swap would publish under, and container_ext is layered per root and "+
			"per encode profile, so it is not something they can derive from the source's name",
			row.Outcome.TargetPath, wantTarget)
	}
	// The target codec and the encoder that would have run are recorded as what the decision
	// READ from the configuration, which is where "would have" belongs: the `encoder` column
	// says which encoder ACTUALLY RAN, and a would-transcode row filling it would claim an
	// encode that never started.
	if got, ok := row.Outcome.DecisionInputs.Value(InputTargetCodec); !ok || got != "hevc" {
		t.Errorf("the recorded target codec is %q (recorded=%v), want %q", got, ok, "hevc")
	}
	if got, ok := row.Outcome.DecisionInputs.Value(InputEncoder); !ok || got != "cpu" {
		t.Errorf("the recorded encoder is %q (recorded=%v), want %q", got, ok, "cpu")
	}
	if row.Outcome.Encoder != "" {
		t.Errorf("the row names %q as the encoder that RAN. Nothing encoded anything, and a reader "+
			"of that column is owed the difference", row.Outcome.Encoder)
	}
	if row.Outcome.SourceBytes == nil {
		t.Fatal("the recorded decision carries NO source size. An operator turns dry_run on to size " +
			"the job, and a candidate with no size is a candidate they cannot add up")
	}
	if *row.Outcome.SourceBytes != beforeSize {
		t.Errorf("the recorded source size is %d, want %d (the size of the file that was decided)",
			*row.Outcome.SourceBytes, beforeSize)
	}

	// --- [AC-3] and NO measurement of an encode nobody ran ------------------------
	assertNoEncodeMeasurement(t, row.Outcome)

	// --- and the dry run did NOTHING to the library -------------------------------
	if got := md5f(t, candidate); got != before {
		t.Error("the source was MODIFIED by a dry run")
	}
	if diff := treeDiff(beforeTree, treeSnapshot(t, root)); diff != "" {
		t.Errorf("a dry run changed the library:\n%s", diff)
	}

	// The guards still decide everything else exactly as they did: the already-HEVC file
	// is skipped, and skipped is not what a candidate is recorded as.
	if !ledgerHas(t, ts, store.Skipped, "already.mkv") {
		t.Error("the already-at-target-codec file is not recorded as skipped; a dry run changes what " +
			"is RECORDED about a decision, never which decision the guards take")
	}
}

// TestDryRun_ARecordedZeroIsNotWhatAnAbsentMeasurementReadsAs is the other half of [AC-3]:
// the absent figures have to be distinguishable from figures somebody measured as zero.
//
// Without this the case above could pass against a build that stored 0 for everything it did
// not measure and a reader that rendered nil and 0 the same way. A VMAF of 0.0 is a destroyed
// frame, and an encode of 0 ms is one that did not run; neither is "nobody looked".
func TestDryRun_ARecordedZeroIsNotWhatAnAbsentMeasurementReadsAs(t *testing.T) {
	root, eng, ts := dryRunFixture(t)
	ctx := context.Background()

	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	rows, err := ts.List(ctx, []store.Status{store.WouldTranscode}, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List(would-transcode): %d row(s), err=%v", len(rows), err)
	}
	assertNoEncodeMeasurement(t, rows[0].Outcome)

	// A row that DID record zeros, written through the same store and read back the same way.
	measured := filepath.Join(root, "measured-at-zero.mkv")
	if ok, err := ts.Claim(ctx, measured, "0:0", "w0", 3, store.DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", measured, ok, err)
	}
	zeroF, zeroI := 0.0, int64(0)
	if err := ts.Finish(ctx, measured, "0:0", store.Done, &store.Outcome{
		VmafMean: &zeroF, VmafMin: &zeroF, VmafChroma: &zeroF,
		OutputBytes: &zeroI, EncodeMs: &zeroI,
	}, 3); err != nil {
		t.Fatalf("Finish(done): %v", err)
	}
	done, err := ts.List(ctx, []store.Status{store.Done}, 0)
	if err != nil || len(done) != 1 {
		t.Fatalf("List(done): %d row(s), err=%v", len(done), err)
	}
	for _, c := range []struct {
		field string
		got   any
	}{
		{"vmaf_mean", done[0].Outcome.VmafMean},
		{"vmaf_min", done[0].Outcome.VmafMin},
		{"vmaf_chroma", done[0].Outcome.VmafChroma},
		{"output_bytes", done[0].Outcome.OutputBytes},
		{"encode_ms", done[0].Outcome.EncodeMs},
	} {
		switch v := c.got.(type) {
		case *float64:
			if v == nil || *v != 0 {
				t.Errorf("a recorded zero for %s read back as %v; if a measured 0 cannot survive the "+
					"round trip, the dry run's nil above is not distinguishable from one", c.field, v)
			}
		case *int64:
			if v == nil || *v != 0 {
				t.Errorf("a recorded zero for %s read back as %v; if a measured 0 cannot survive the "+
					"round trip, the dry run's nil above is not distinguishable from one", c.field, v)
			}
		}
	}
}

// assertNoEncodeMeasurement is [AC-3] in one place: nothing was encoded, so the
// perceptual-quality figures, the output size and the encode duration are all NOT RECORDED.
func assertNoEncodeMeasurement(t *testing.T, o store.Outcome) {
	t.Helper()
	for _, c := range []struct {
		field string
		got   *float64
	}{
		{"vmaf_mean", o.VmafMean},
		{"vmaf_min", o.VmafMin},
		{"vmaf_chroma", o.VmafChroma},
	} {
		if c.got != nil {
			t.Errorf("a dry-run row records %s = %v. Nothing was encoded and nothing was compared, so "+
				"any number there is a measurement of work nobody did", c.field, *c.got)
		}
	}
	if o.OutputBytes != nil {
		t.Errorf("a dry-run row records output_bytes = %d; there is no output to have a size", *o.OutputBytes)
	}
	if o.EncodeMs != nil {
		t.Errorf("a dry-run row records encode_ms = %d; no encode ran to take any time", *o.EncodeMs)
	}
	for _, c := range []struct{ field, got string }{
		{"vmaf_model", o.VmafModel},
		{"vmaf_pix_fmt", o.VmafPixFmt},
		{"vmaf_chroma_metric", o.VmafChromaMetric},
		{"vmaf_stream", o.VmafStream},
	} {
		if c.got != "" {
			t.Errorf("a dry-run row records %s = %q, describing a comparison nobody made", c.field, c.got)
		}
	}
}

// TestDryRun_ASecondPassOverAnUnchangedFileStillReportsOneCandidate. Two dry runs are a
// thing an operator does - they change a guard's setting and look again - and a candidate
// count that doubles each time is a count they cannot use. It is [AC-1]'s "exactly one
// terminal row per eligible file" held across a second pass.
func TestDryRun_ASecondPassOverAnUnchangedFileStillReportsOneCandidate(t *testing.T) {
	_, eng, ts := dryRunFixture(t)
	ctx := context.Background()

	for pass := 1; pass <= 2; pass++ {
		if err := eng.RunOneshot(ctx); err != nil {
			t.Fatalf("RunOneshot pass %d: %v", pass, err)
		}
	}
	rows, err := ts.List(ctx, []store.Status{store.WouldTranscode}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("two dry runs over the same unchanged file report %d candidates, want 1: %+v", len(rows), rows)
	}
	if len(rows) == 1 && (rows[0].Outcome.SourceCodec == "" || rows[0].Outcome.SourceBytes == nil) {
		t.Errorf("the second pass left the candidate without its recorded facts: %+v", rows[0].Outcome)
	}
}

// TestDryRun_RowDoesNotHoldTheFileOutOfARealRun grades [AC-7]: the row recording a transcode
// a dry run would have performed re-opens UNCONDITIONALLY, so a real run over the same
// library transcodes the file whatever the configuration in force is and whether or not it
// has moved since.
//
// It is driven end to end through the engine rather than asserted of Claim alone: the
// operator looks at the candidates, turns dry_run off, and the SAME files are the ones that
// get transcoded. If the recorded decision were treated as work already completed, this run
// would find nothing to do and the library would never be reclaimed - with the ledger full
// of tidy decisions and no transcodes.
//
// The UNMOVED case is the one with teeth. A moved configuration re-opens a `done` row too, so
// a row that only re-opened then would be indistinguishable from every other terminal row;
// what this criterion asks is that a preview re-opens when nothing has moved at all.
func TestDryRun_RowDoesNotHoldTheFileOutOfARealRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(*config.Config)
	}{
		{name: "with the configuration unmoved", move: func(*config.Config) {}},
		{name: "with the configuration moved since", move: func(c *config.Config) { c.CRF = 26 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ffmpeg, ffprobe := tools(t)
			root := t.TempDir()
			candidate := filepath.Join(root, "candidate.mkv")
			mkH264(t, ffmpeg, candidate, "8M")

			cfg := baseCfg(root)
			cfg.ContainerExt = "source"
			cfg.DryRun = true
			ts := newTestStore(t, root)
			prober := probe.New(ffmpeg, ffprobe)

			if err := newDryEngine(t, cfg, ts, discardLogger()).RunOneshot(context.Background()); err != nil {
				t.Fatalf("dry RunOneshot: %v", err)
			}
			if !ledgerHas(t, ts, store.WouldTranscode, "candidate.mkv") {
				t.Fatal("the dry run recorded no candidate, so the re-claim half of this case would prove nothing")
			}

			// Same store, same file, dry_run off.
			realCfg := cfg
			realCfg.DryRun = false
			tc.move(&realCfg)
			real := New(realCfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: realCfg, Probe: prober}, ts, discardLogger())
			if err := real.RunOneshot(context.Background()); err != nil {
				t.Fatalf("real RunOneshot: %v", err)
			}

			if !ledgerHas(t, ts, store.Done, "candidate.mkv") {
				rows, _ := ts.List(context.Background(), nil, 0)
				t.Fatalf("a file a dry run had recorded as a candidate was NOT transcoded once dry_run was "+
					"turned off. A recorded decision is not work already completed - if it were, every file "+
					"examined during a dry run would be excluded for ever: %+v", rows)
			}
			if codec := codecOf(t, ffprobe, candidate); codec != "hevc" {
				t.Errorf("the transcoded file is %q, want hevc - the real run did not actually re-encode it", codec)
			}
		})
	}
}

// TestDryRun_APreviewRowIsTellableApartFromACompletedSwap grades [AC-6]: the row a dry run
// writes for a file it would transcode, and the row a real run writes for the same file, are
// tellable apart by a terminal status or a reason token alone.
//
// The real row is produced by an ACTUAL transcode rather than seeded, which is what makes the
// comparison one against a completed swap rather than against whatever a fixture asserted a
// completed swap looks like.
func TestDryRun_APreviewRowIsTellableApartFromACompletedSwap(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()
	prober := probe.New(ffmpeg, ffprobe)

	dryRoot := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(dryRoot, "candidate.mkv"), "8M")
	dryCfg := baseCfg(dryRoot)
	dryCfg.ContainerExt = "source"
	dryCfg.DryRun = true
	dryTS := newTestStore(t, dryRoot)
	if err := newDryEngine(t, dryCfg, dryTS, discardLogger()).RunOneshot(ctx); err != nil {
		t.Fatalf("dry RunOneshot: %v", err)
	}

	realRoot := t.TempDir()
	realCandidate := filepath.Join(realRoot, "candidate.mkv")
	mkH264(t, ffmpeg, realCandidate, "8M")
	realCfg := baseCfg(realRoot)
	realCfg.ContainerExt = "source"
	realTS := newTestStore(t, realRoot)
	real := New(realCfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: realCfg, Probe: prober}, realTS, discardLogger())
	if err := real.RunOneshot(ctx); err != nil {
		t.Fatalf("real RunOneshot: %v", err)
	}
	if codec := codecOf(t, ffprobe, realCandidate); codec != "hevc" {
		t.Fatalf("the real run did not transcode its candidate (%q), so there is no completed swap to "+
			"tell the preview apart from", codec)
	}

	dry := onlyRow(t, dryTS)
	done := onlyRow(t, realTS)
	if !dry.Status.Terminal() || !done.Status.Terminal() {
		t.Fatalf("one of the two rows is not terminal: preview %q, completed swap %q", dry.Status, done.Status)
	}
	if dry.Status == done.Status && dry.Outcome.Reason == done.Outcome.Reason {
		t.Errorf("the preview row and the completed swap's row carry the same status (%q) and the same "+
			"reason (%q). A later reader holding one of them cannot tell a plan from a transcode that "+
			"happened, and this ledger is what says a source file was already handled",
			dry.Status, dry.Outcome.Reason)
	}
}

// TestDryRun_ASkipIsTheSameFactARealRunRecords grades [AC-5]: a file a guard skips during a
// dry run records the same terminal status, the same guard token and the same decision inputs
// that a real run records for it under the same configuration - and therefore holds the file
// out, and re-opens, on exactly the same terms.
//
// The re-opening half is asked of Claim directly and not through a second scan, because that
// is the boundary the rule lives at: the same question is put to both rows, first with the
// configuration the verdict was taken under and then with the one key that guard READ moved.
// A skip that re-opened unconditionally would change what a real run's `already-at-target-codec`
// row means for every operator who never enabled dry_run.
func TestDryRun_ASkipIsTheSameFactARealRunRecords(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()
	root := t.TempDir()
	already := filepath.Join(root, "already.mkv")
	mkHevc(t, ffmpeg, already, "8M")
	key := probe.Fingerprint(already)

	cfg := baseCfg(root)
	cfg.ContainerExt = "source"

	dryCfg := cfg
	dryCfg.DryRun = true
	dryTS := newTestStore(t, root)
	if err := newDryEngine(t, dryCfg, dryTS, discardLogger()).RunOneshot(ctx); err != nil {
		t.Fatalf("dry RunOneshot: %v", err)
	}

	realTS := newTestStore(t, root)
	realEng := New(cfg, probe.New(ffmpeg, ffprobe),
		FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: probe.New(ffmpeg, ffprobe)}, realTS, discardLogger())
	if err := realEng.RunOneshot(ctx); err != nil {
		t.Fatalf("real RunOneshot: %v", err)
	}

	dry := onlyRow(t, dryTS)
	real := onlyRow(t, realTS)
	if real.Status != store.Skipped {
		t.Fatalf("the real run recorded %q for the guarded file, not a skip - there is no real run's "+
			"skip here to compare against", real.Status)
	}
	if dry.Status != real.Status {
		t.Errorf("a dry run recorded the guarded file as %q where a real run records %q. A dry run's "+
			"skip is not a different fact from a real run's skip", dry.Status, real.Status)
	}
	if dry.Outcome.Reason != real.Outcome.Reason {
		t.Errorf("the dry run names guard %q and the real run names %q", dry.Outcome.Reason, real.Outcome.Reason)
	}
	if got, want := dry.Outcome.DecisionInputs.Encode(), real.Outcome.DecisionInputs.Encode(); got != want {
		t.Errorf("the dry run's skip records inputs %q and the real run's records %q. The row's recorded "+
			"inputs are what decides when it re-opens, so two different records are two different verdicts",
			got, want)
	}
	if dry.Outcome.Profile != real.Outcome.Profile || dry.Outcome.Decision != real.Outcome.Decision {
		t.Errorf("the dry run's skip is attributed to %+v / profile %q and the real run's to %+v / profile %q",
			dry.Outcome.Decision, dry.Outcome.Profile, real.Outcome.Decision, real.Outcome.Profile)
	}

	// The same question put to both rows: held out while the key the guard read still
	// resolves the same way, re-opened when it does not. `encoder: svtav1` resolves
	// target_codec to av1, which is the one key `already-at-target-codec` read.
	inForce := DecisionInputsFor(cfg)
	movedCfg := cfg
	movedCfg.Encoder = "svtav1"
	moved := DecisionInputsFor(movedCfg)
	if inForce.Encode() == moved.Encode() {
		t.Fatal("the moved configuration resolves to the same inputs as the one in force, so the " +
			"re-opening half of this case would prove nothing")
	}
	for _, c := range []struct {
		what string
		ts   *testStore
	}{{"a dry run", dryTS}, {"a real run", realTS}} {
		held, err := c.ts.Claim(ctx, already, key, "w0", 3, inForce)
		if err != nil {
			t.Fatalf("Claim(%s, unmoved): %v", c.what, err)
		}
		if held {
			t.Errorf("the skip %s recorded was re-opened under the configuration it was taken under. A "+
				"skip is a verdict that configuration can still justify, and re-opening it every scan "+
				"would offer the file back for ever", c.what)
		}
		reopened, err := c.ts.Claim(ctx, already, key, "w0", 3, moved)
		if err != nil {
			t.Fatalf("Claim(%s, moved): %v", c.what, err)
		}
		if !reopened {
			t.Errorf("the skip %s recorded was NOT re-opened after the key its guard read moved", c.what)
		}
	}
}

// TestDryRun_RecordsNoSizeItCouldNotRead grades [AC-4]. The absence path, because a fabricated
// size is exactly what the rest of this ledger exists to refuse. It drives the one condition
// that produces it - the file gone between the decision and the stat - and asserts the row is
// written with the size NOT RECORDED rather than as a zero, and that the log SAYS why the
// figure is absent.
//
// The log line is read as a structured record and its level is asserted: an unreadable size
// here is a condition this pass HANDLES (it records the decision anyway), so reporting it at
// `error` would ask for a human who is not needed and train its reader to ignore the ones who
// are (`observability` O1, O3).
func TestDryRun_RecordsNoSizeItCouldNotRead(t *testing.T) {
	ffmpeg, _ := tools(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "vanishing.mkv")
	mkH264(t, ffmpeg, candidate, "8M")

	cfg := baseCfg(root)
	cfg.ContainerExt = "source"
	cfg.DryRun = true
	ts := newTestStore(t, root)
	var logged bytes.Buffer
	eng := newDryEngine(t, cfg, ts, jsonLogger(&logged))

	// The scan enumerated it and the guards probed it; it is removed in the window before
	// the decision is written, which is the one way the size read can fail on a file that
	// got this far.
	key := probe.Fingerprint(candidate)
	eng.hookBeforeDryRunRecord = func(path string) {
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove the fixture: %v", err)
		}
	}
	if err := eng.ProcessFile(context.Background(), "w0", candidate); err != nil {
		t.Fatalf("ProcessFile: %v", err)
	}

	got, _, exists, err := ts.Get(context.Background(), candidate, key)
	if err != nil || !exists {
		t.Fatalf("Get: status=%q exists=%v err=%v", got, exists, err)
	}
	if got != store.WouldTranscode {
		t.Fatalf("the decision is recorded as %q, want %q - a size that could not be read must not "+
			"cost the decision itself", got, store.WouldTranscode)
	}
	rows, err := ts.List(context.Background(), []store.Status{store.WouldTranscode}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("List returned %d rows, want 1", len(rows))
	}
	if rows[0].Outcome.SourceBytes != nil {
		t.Errorf("a size nobody could read was recorded as %d. nil is NOT RECORDED, and a reader "+
			"shows it as such; a number here is a figure an operator would add up",
			*rows[0].Outcome.SourceBytes)
	}
	if rows[0].Outcome.SourceCodec == "" {
		t.Error("the codec, which WAS read, was dropped along with the size it was recorded beside")
	}
	if rows[0].Outcome.TargetPath == "" {
		t.Error("the target path, which was already chosen, was dropped along with the size")
	}

	// The log says WHY the figure is absent, once, as a structured record.
	records := logRecords(t, &logged)
	var said map[string]any
	for _, rec := range records {
		if msg, _ := rec["msg"].(string); strings.Contains(msg, "source size") {
			said = rec
			break
		}
	}
	if said == nil {
		t.Fatalf("no log record says why the source size is absent. A row whose figure is missing and "+
			"whose logs are silent about it is one nobody can explain:\n%s", logged.String())
	}
	if lvl, _ := said["level"].(string); lvl != "WARN" {
		t.Errorf("the unreadable size is logged at %q. It is a condition this pass HANDLES - the "+
			"decision is recorded anyway - so `error`, which means a human must act, is the wrong "+
			"level for it", lvl)
	}
	if f, _ := said["file"].(string); f != candidate {
		t.Errorf("the record names file %q, want %q", f, candidate)
	}
	if e, _ := said["err"].(string); e == "" {
		t.Error("the record carries no error, so it says the figure is absent without saying why")
	}
	for _, rec := range records {
		if lvl, _ := rec["level"].(string); lvl == "ERROR" {
			t.Errorf("a dry run that handled everything it met logged at ERROR: %v", rec)
		}
	}
}

// TestDryRun_MutatesNothingEvenWithEveryLibraryRootReadOnly grades [AC-8]: a dry pass leaves
// every library root holding the same entries at the same sizes and modification times, and
// reaches the same decisions when every root is read-only to the process running it.
//
// The two halves catch different things and neither is enough alone. A before-and-after
// snapshot misses a temp path that is probed and then cleaned up; the read-only pass is what
// makes such a probe fail. And the read-only pass ESTABLISHES that the constraint binds before
// it concludes anything: mode bits do not constrain a process running as root, and a suite run
// in a container often is, so without that check this would report a green it did not earn -
// on the one criterion standing between this repository's whole safety property and a preview
// that writes into somebody's library.
func TestDryRun_MutatesNothingEvenWithEveryLibraryRootReadOnly(t *testing.T) {
	ffmpeg, _ := tools(t)
	ctx := context.Background()
	root := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(root, "candidate.mkv"), "8M")
	mkHevc(t, ffmpeg, filepath.Join(root, "already.mkv"), "8M")

	newPass := func() (*Engine, *testStore) {
		cfg := baseCfg(root)
		cfg.DryRun = true
		cfg.ContainerExt = "source"
		ts := newTestStore(t, root)
		return newDryEngine(t, cfg, ts, discardLogger()), ts
	}

	before := treeSnapshot(t, root)

	eng, ts := newPass()
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot over a writable library: %v", err)
	}
	writable := decisions(t, ts)
	if diff := treeDiff(before, treeSnapshot(t, root)); diff != "" {
		t.Errorf("a dry pass changed the library it was previewing:\n%s", diff)
	}

	// --- the same pass with every root READ-ONLY ----------------------------------
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatalf("chmod the library root read-only: %v", err)
	}
	// Restored before t.TempDir's own cleanup, which cannot remove a directory it may not
	// write to. Registered after that cleanup, so it runs before it.
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })

	// ESTABLISH THE CONSTRAINT FIRST. If this write succeeds the roots are not read-only to
	// this process whatever their mode says, the pass below would write whatever it was going
	// to write, and a pass reported as green here would have decided nothing at all.
	probePath := filepath.Join(root, "holdfast-readonly-check")
	if err := os.WriteFile(probePath, []byte("x"), 0o600); err == nil {
		_ = os.Remove(probePath)
		t.Fatalf("a direct write to %s SUCCEEDED while the root is mode 0500, so the read-only "+
			"constraint does not bind this process (mode bits do not bind root). This case cannot "+
			"conclude anything about a pass that writes nothing; run the suite as a non-root user", root)
	}

	roEng, roTS := newPass()
	if err := roEng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot over a read-only library: %v", err)
	}
	readOnly := decisions(t, roTS)
	if len(readOnly) == 0 {
		t.Fatal("the read-only pass reached no decision at all, so it proves nothing about a pass that " +
			"writes nothing")
	}
	if !sameDecisions(writable, readOnly) {
		t.Errorf("the pass reached different decisions with the library read-only:\n  writable  %v\n  read-only %v\n"+
			"A dry run takes no shortcut and writes nothing, so the roots' mode cannot change its verdict",
			writable, readOnly)
	}
	if diff := treeDiff(before, treeSnapshot(t, root)); diff != "" {
		t.Errorf("the library moved across the two passes:\n%s", diff)
	}
}

// TestDryRun_AnInterruptedPassLeavesTheRowToStaleRecovery grades [AC-10]: a pass interrupted
// after it claimed a file and before that file is decided leaves the row in an active state for
// the ordinary stale-row recovery every real run performs at startup, and writes no decision.
//
// The interruption is a cancelled context, which is what a SIGTERM becomes inside the engine,
// and it is taken at BOTH points where a claimed file is not yet decided. The second is the one
// with teeth: the guards have concluded, so a build that recorded its decision through anything
// but the interrupted pass's own context would write a would-transcode row for a pass that was
// already over, and the first case cannot see that because it never reaches the record at all.
func TestDryRun_AnInterruptedPassLeavesTheRowToStaleRecovery(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   func(eng *Engine, cancel func())
	}{
		{
			name: "interrupted before the guards concluded",
			at:   func(eng *Engine, cancel func()) { eng.onClaim = func(string, string) { cancel() } },
		},
		{
			name: "interrupted after the decision was computed and before it was recorded",
			at: func(eng *Engine, cancel func()) {
				eng.hookBeforeDryRunRecord = func(string) { cancel() }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ffmpeg, _ := tools(t)
			root := t.TempDir()
			candidate := filepath.Join(root, "candidate.mkv")
			mkH264(t, ffmpeg, candidate, "8M")
			key := probe.Fingerprint(candidate)

			cfg := baseCfg(root)
			cfg.ContainerExt = "source"
			cfg.DryRun = true
			ts := newTestStore(t, root)
			eng := newDryEngine(t, cfg, ts, discardLogger())

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			tc.at(eng, cancel)
			_ = eng.ProcessFile(ctx, "w0", candidate)

			live := context.Background()
			status, _, exists, err := ts.Get(live, candidate, key)
			if err != nil || !exists {
				t.Fatalf("Get after the interruption: status=%q exists=%v err=%v", status, exists, err)
			}
			if !status.Active() {
				t.Fatalf("the interrupted pass left the row %q. A pass cut off before the file was "+
					"decided must leave the row where the claim put it, so the recovery every run "+
					"already performs picks it up", status)
			}
			rows, err := ts.List(live, nil, 0)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			for _, r := range rows {
				if r.Status.Terminal() {
					t.Errorf("the interrupted pass recorded a terminal row (%q) for %s. The pass was over "+
						"before that file was decided, so the row describes a decision no pass published: %+v",
						r.Status, r.Path, r.Outcome)
				}
			}

			// The ordinary recovery a real run performs at startup, and nothing else, frees it.
			n, err := ts.RecoverStale(live)
			if err != nil {
				t.Fatalf("RecoverStale: %v", err)
			}
			if n != 1 {
				t.Errorf("stale recovery reset %d row(s), want 1", n)
			}
			status, _, _, err = ts.Get(live, candidate, key)
			if err != nil {
				t.Fatalf("Get after recovery: %v", err)
			}
			if status != store.Pending {
				t.Errorf("after the ordinary stale recovery the row is %q, want %q", status, store.Pending)
			}
		})
	}
}

// ---- helpers -----------------------------------------------------------------

// fileSize is the size the filesystem reports for path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// treeSnapshot is every entry under dir with its size and modification time, sorted, so a
// case can assert that a pass created nothing, removed nothing and touched nothing. Names
// alone would miss a file rewritten in place at the same length.
func treeSnapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out = append(out, fmt.Sprintf("%s size=%d mtime=%d dir=%v",
			rel, info.Size(), info.ModTime().UnixNano(), d.IsDir()))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// treeDiff renders what moved between two snapshots, "" when nothing did.
func treeDiff(before, after []string) string {
	if strings.Join(before, "\n") == strings.Join(after, "\n") {
		return ""
	}
	return "  before:\n    " + strings.Join(before, "\n    ") +
		"\n  after:\n    " + strings.Join(after, "\n    ")
}

// onlyRow returns the single row in ts, failing when there is not exactly one.
func onlyRow(t *testing.T, ts *testStore) store.Job {
	t.Helper()
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the store holds %d row(s), want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// decisions maps each row's file name to the verdict recorded for it: the status and, where
// a guard fired, the token naming it.
func decisions(t *testing.T, ts *testStore) map[string]string {
	t.Helper()
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[filepath.Base(r.Path)] = string(r.Status) + "/" + r.Outcome.Reason
	}
	return out
}

func sameDecisions(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// jsonLogger writes one JSON object per record into buf, which is the shape the fleet's
// logging convention asks for and the shape a case can read a level and a field back out of.
func jsonLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logRecords parses what jsonLogger captured. A line that is not one JSON object fails the
// case rather than being skipped: the convention is that every line IS one.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not one JSON object (%v): %s", err, line)
		}
		out = append(out, rec)
	}
	return out
}
