package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// What a DRY RUN records, and what it still refuses to do.
//
// An operator turns dry_run on to answer one question before they let this tool delete
// anything: which files would it transcode? The answer was computed - the file passed
// every guard - and then discarded, so the ledger left the row wherever the worker had
// parked it while deciding. `probing`. Indistinguishable from a file a worker was still
// examining, and every count that says what the run concluded reported nothing.
//
// These cases hold both halves at once: the decision is now RECORDED, and a dry run still
// encodes nothing, swaps nothing and deletes nothing.

// dryRunFixture builds a library holding one h264 file that passes every guard, plus an
// already-HEVC file that does not, and returns the root and the engine. The encoder is one
// that FAILS THE TEST if it is called: "a dry run encodes nothing" is asserted by making
// an encode impossible rather than by inspecting what was left behind afterwards.
func dryRunFixture(t *testing.T) (root string, eng *Engine, ts *testStore) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root = t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(root, "candidate.mkv"), "8M")
	mkHevc(t, ffmpeg, filepath.Join(root, "already.mkv"), "8M")

	cfg := baseCfg(root)
	cfg.DryRun = true
	cfg.ContainerExt = "source" // an in-place decision; no ext change to complicate it
	ts = newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	enc := EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
		t.Errorf("the ENCODER RAN during a dry run (%s -> %s). A dry run reports what it would "+
			"do and does none of it", in, out)
		return nil
	})
	return root, New(cfg, prober, enc, ts, discardLogger()), ts
}

// TestDryRun_RecordsTheDecisionAndEncodesNothing is the criterion in one case: a file that
// passes every guard gets a TERMINAL would-transcode row carrying the source's codec and
// its size, and nothing on disk moves.
func TestDryRun_RecordsTheDecisionAndEncodesNothing(t *testing.T) {
	root, eng, ts := dryRunFixture(t)
	ctx := context.Background()

	candidate := filepath.Join(root, "candidate.mkv")
	before := md5f(t, candidate)
	beforeSize := fileSize(t, candidate)
	beforeNames := dirNames(t, root)

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

	// --- carrying what an operator needs to size the job --------------------------
	//
	// Both are read FROM THE FILE THAT WAS DECIDED: the codec is what ffprobe answered
	// for it, and the size is what the filesystem reports for it right now.
	wantCodec := codecOf(t, envOr("HOLDFAST_FFPROBE", "ffprobe"), candidate)
	if wantCodec == "" {
		t.Fatal("the fixture has no probeable video codec - the case would prove nothing")
	}
	if row.Outcome.SourceCodec != wantCodec {
		t.Errorf("the recorded source codec is %q, want %q (the codec of the file that was decided)",
			row.Outcome.SourceCodec, wantCodec)
	}
	if row.Outcome.SourceBytes == nil {
		t.Fatal("the recorded decision carries NO source size. An operator turns dry_run on to size " +
			"the job, and a candidate with no size is a candidate they cannot add up")
	}
	if *row.Outcome.SourceBytes != beforeSize {
		t.Errorf("the recorded source size is %d, want %d (the size of the file that was decided)",
			*row.Outcome.SourceBytes, beforeSize)
	}

	// --- and the dry run did NOTHING to the library -------------------------------
	if got := md5f(t, candidate); got != before {
		t.Error("the source was MODIFIED by a dry run")
	}
	if got := fileSize(t, candidate); got != beforeSize {
		t.Errorf("the source's size moved during a dry run: %d -> %d", beforeSize, got)
	}
	after := dirNames(t, root)
	if len(after) != len(beforeNames) {
		t.Errorf("a dry run changed what is in the library directory:\n  before %v\n  after  %v", beforeNames, after)
	}
	for i := range after {
		if i < len(beforeNames) && after[i] != beforeNames[i] {
			t.Errorf("a dry run changed what is in the library directory:\n  before %v\n  after  %v", beforeNames, after)
			break
		}
	}

	// The guards still decide everything else exactly as they did: the already-HEVC file
	// is skipped, and skipped is not what a candidate is recorded as.
	if !ledgerHas(t, ts, store.Skipped, "already.mkv") {
		t.Error("the already-at-target-codec file is not recorded as skipped; a dry run changes what " +
			"is RECORDED about a decision, never which decision the guards take")
	}
}

// TestDryRun_LeavesNoRowInProbingForADecisionItTook is the defect stated as a criterion.
//
// `probing` means CLAIMED AND NOT YET DECIDED. The old dry-run branch returned with the
// claim still on the row, so a file the run had finished deciding sat in that bucket for
// ever - and the dashboard, the summary and /metrics all reported it as work in hand. The
// count is asserted from the SUMMARY, which is the figure those surfaces read.
func TestDryRun_LeavesNoRowInProbingForADecisionItTook(t *testing.T) {
	_, eng, ts := dryRunFixture(t)
	ctx := context.Background()

	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	sum, err := ts.Summary(ctx)
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	for _, active := range []store.Status{store.Probing, store.Encoding, store.Verifying} {
		if n := sum[active]; n != 0 {
			rows, _ := ts.List(ctx, []store.Status{active}, 0)
			t.Errorf("after a finished dry run %d row(s) are still %q on account of a decision that "+
				"run already took: %+v", n, active, rows)
		}
	}
	if n := sum[store.WouldTranscode]; n != 1 {
		t.Errorf("the finished dry run reports %d would-transcode rows, want 1", n)
	}
}

// TestDryRun_ASecondPassOverAnUnchangedFileStillReportsOneCandidate. Two dry runs are a
// thing an operator does - they change a guard's setting and look again - and a candidate
// count that doubles each time is a count they cannot use.
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

// TestDryRun_ARecordedCandidateIsTranscodedOnceDryRunIsOff is the other half of "terminal
// but re-claimable", driven end to end through the engine rather than asserted of Claim
// alone: the operator looks at the candidates, turns dry_run off, and the SAME files are
// the ones that get transcoded.
//
// If the recorded decision were treated as work already completed, this run would find
// nothing to do and the library would never be reclaimed - with the dashboard reporting a
// tidy ledger full of decisions and no transcodes.
func TestDryRun_ARecordedCandidateIsTranscodedOnceDryRunIsOff(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate.mkv")
	mkH264(t, ffmpeg, candidate, "8M")

	cfg := baseCfg(root)
	cfg.ContainerExt = "source"
	cfg.DryRun = true
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)

	dry := New(cfg, prober, EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
		t.Errorf("the encoder ran during the dry run (%s -> %s)", in, out)
		return nil
	}), ts, discardLogger())
	if err := dry.RunOneshot(context.Background()); err != nil {
		t.Fatalf("dry RunOneshot: %v", err)
	}
	if !ledgerHas(t, ts, store.WouldTranscode, "candidate.mkv") {
		t.Fatal("the dry run recorded no candidate, so the re-claim half of this case would prove nothing")
	}

	// Same store, same file, dry_run off.
	realCfg := cfg
	realCfg.DryRun = false
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
}

// TestDryRun_RecordsNoSizeItCouldNotRead. The absence path, because a fabricated size is
// exactly what the rest of this ledger exists to refuse. It drives the one condition that
// produces it - the file gone between the decision and the stat - and asserts the row is
// written with the size NOT RECORDED rather than as a zero.
func TestDryRun_RecordsNoSizeItCouldNotRead(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "vanishing.mkv")
	mkH264(t, ffmpeg, candidate, "8M")

	cfg := baseCfg(root)
	cfg.ContainerExt = "source"
	cfg.DryRun = true
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	eng := New(cfg, prober, EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
		t.Errorf("the encoder ran during a dry run (%s -> %s)", in, out)
		return nil
	}), ts, discardLogger())

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
}

// fileSize is the size the filesystem reports for path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// dirNames is every entry in dir, in directory order, so a case can assert that a dry run
// created nothing and removed nothing.
func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}
