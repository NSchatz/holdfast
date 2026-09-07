package engine

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// Retention, where it meets the engine (LEDGER-5).
//
// Criterion 6: WHEN retention has pruned rows for files that are still present in the
// library THE SYSTEM SHALL NOT thereby cause any of those files to be encoded again.
//
// Criterion 7: WHEN retention is configured and a scan completes THE SYSTEM SHALL bring
// the terminal rows within the retention with no operator action and no request to the API.
//
// Criterion 10: IF the store returns an error part way through a prune THEN THE SYSTEM
// SHALL log that failure, leave every row it did not remove in place, and keep serving and
// encoding.
//
// Criterion 6 is the sharp one, and the hazard is bigger than it first looks. A terminal
// row is a DECISION, not only a record: Claim refuses a done or skipped row outright, and
// every skip guard that would re-derive the same verdict runs AFTER Claim. So a pruned
// verdict re-derives itself only while the configuration it was taken under has not moved -
// and two supported settings move it, a change of target codec and a lowered
// min_bitrate_kbps. Under either, deleting the row of a file still in the library hands
// that file to the encoder. A parked FAILED row is the same failure through a third door:
// fail_count is what parks a file that has already failed max_failures times.
//
// The rule the engine therefore applies is that a row may only go when THIS RUN LISTED the
// directory the file should be in and the file was not there (engine.rowIsSpent). The
// fixtures below grade it across the moved-configuration cases, over rows seeded on the
// files' REAL fingerprints - the key Claim looks up - and each one is paired with a
// mutation that shows the same fixture DOES encode when the rule is defeated.

// countingEncoder records every file handed to the encoder. Under these criteria the
// count is usually zero, so it also has to be able to be non-zero: the mutation fixtures
// below defeat the parked-row rule and watch this counter move.
func countingEncoder(n *atomic.Int32) EncoderFunc {
	return func(_ context.Context, _, _ string, _ *probe.VideoProps) error {
		n.Add(1)
		return errFake // the outcome does not matter; reaching the encoder at all does
	}
}

// captureLogger returns a logger writing into buf, for the criteria that require a
// failure to be REPORTED rather than swallowed.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// seedRow writes one terminal row directly, for a path that need not exist on disk. It is
// how a ledger with history is built without paying for real encodes: retention is about
// ROWS, and the engine's prune does not care how they got there.
//
// The fingerprint is a literal, so a row seeded this way NEVER collides with a scanned
// file's real one. That makes it the right tool for "history for a file that is not there"
// and the wrong tool for criterion 6, which is about rows the engine would meet again:
// those are seeded with seedRowForRealFile.
func seedRow(t *testing.T, ts *testStore, path string, st store.Status, o *store.Outcome) {
	t.Helper()
	ctx := context.Background()
	if _, err := ts.Claim(ctx, path, "seed", "w0", 3); err != nil {
		t.Fatalf("seed claim %s: %v", path, err)
	}
	if err := ts.Finish(ctx, path, "seed", st, o); err != nil {
		t.Fatalf("seed finish %s: %v", path, err)
	}
}

// seedRowForRealFile writes a terminal row keyed on the file's REAL fingerprint - the key
// Claim looks up - so the row is exactly what a previous scan would have left behind, and
// deleting it is exactly what hands the file back to the encoder.
func seedRowForRealFile(t *testing.T, ts *testStore, path string, st store.Status, o *store.Outcome) {
	t.Helper()
	ctx := context.Background()
	key := probe.Fingerprint(path)
	ok, err := ts.Claim(ctx, path, key, "w0", 3)
	if err != nil || !ok {
		t.Fatalf("seed claim %s: ok=%v err=%v", path, ok, err)
	}
	if err := ts.Finish(ctx, path, key, st, o); err != nil {
		t.Fatalf("seed finish %s: %v", path, err)
	}
}

func terminalRows(t *testing.T, ts *testStore) []store.Job {
	t.Helper()
	rows, err := ts.List(context.Background(), []store.Status{store.Done, store.Skipped, store.Failed}, 0)
	if err != nil {
		t.Fatalf("List(terminal): %v", err)
	}
	return rows
}

// --- criterion 7: a completed scan brings the ledger within the retention --------------

func TestRetention_ACompletedScanBringsTheLedgerWithinTheRetentionWithNoOperatorAction(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.HistoryRetentionRows = 4
	})
	ts := eng.Store.(*testStore)
	// History for files that were in the library and are not any more - the rows a
	// churning library accumulates, and the ones a retention is FOR. The scan lists the
	// directory they name and does not find them, which is what makes them removable.
	for i := 0; i < 15; i++ {
		seedRow(t, ts, filepath.Join(root, "gone"+strconv.Itoa(i)+".mkv"), store.Skipped, because(SkipLowBitrate))
	}

	// The ONLY thing called is RunOneshot. No API, no operator, no separate command.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if got := len(terminalRows(t, ts)); got != 4 {
		t.Errorf("the ledger holds %d terminal rows after a scan with history_retention_rows: 4", got)
	}
}

func TestRetention_DisabledByDefaultTheSameScanKeepsEveryRow(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()

	// The identical fixture with the key left alone. This is the pair that makes the
	// previous test evidence rather than a coincidence, and it is the criterion the
	// shipped default has to meet: a scan on a stock configuration deletes NOTHING.
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	ts := eng.Store.(*testStore)
	if eng.Cfg.RetentionEnabled() {
		t.Fatal("baseCfg enabled retention; the shipped default must be disabled")
	}
	for i := 0; i < 15; i++ {
		seedRow(t, ts, filepath.Join(root, "gone"+strconv.Itoa(i)+".mkv"), store.Skipped, because(SkipLowBitrate))
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if got := len(terminalRows(t, ts)); got != 15 {
		t.Errorf("a scan with retention disabled left %d of 15 terminal rows", got)
	}
}

// --- criterion 6: a prune must not cause an encode ------------------------------------

// libraryStillPresent builds a library of files that are ALREADY at the target codec -
// the state a file is in after holdfast has transcoded it - plus one h264 file that has
// failed max_failures times and is therefore parked. It returns the parked file's path.
func libraryStillPresent(t *testing.T, ffmpeg, root string, n int) string {
	t.Helper()
	for i := 0; i < n; i++ {
		mkHevc(t, ffmpeg, filepath.Join(root, "transcoded"+strconv.Itoa(i)+".mkv"), "800k")
	}
	parked := filepath.Join(root, "parked.mkv")
	mkH264(t, ffmpeg, parked, "8M")
	return parked
}

// parkFile writes the failed row a file that has exhausted its retries carries: status
// failed with fail_count at max_failures, which is exactly what Claim refuses.
func parkFile(t *testing.T, ts *testStore, path string, maxFailures int) {
	t.Helper()
	ctx := context.Background()
	key := probe.Fingerprint(path)
	for i := 0; i < maxFailures; i++ {
		ok, err := ts.Claim(ctx, path, key, "w0", maxFailures)
		if err != nil {
			t.Fatalf("park claim %s: %v", path, err)
		}
		if !ok {
			t.Fatalf("park claim %s refused on attempt %d", path, i+1)
		}
		if err := ts.Finish(ctx, path, key, store.Failed, &store.Outcome{Reason: "simulated"}); err != nil {
			t.Fatalf("park finish %s: %v", path, err)
		}
	}
	st, fc, exists, err := ts.Get(ctx, path, key)
	if err != nil || !exists || st != store.Failed || fc < maxFailures {
		t.Fatalf("parking %s produced status=%q fail_count=%d exists=%v err=%v", path, st, fc, exists, err)
	}
}

func TestRetention_PruningRowsForFilesStillPresentEncodesNoneOfThemAgain(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	parked := libraryStillPresent(t, ffmpeg, root, 3)

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes), func(c *config.Config) {
		c.HistoryRetentionRows = 1 // aggressive on purpose: prune everything it may
	})
	ts := eng.Store.(*testStore)
	parkFile(t, ts, parked, eng.Cfg.MaxFailures)

	// Scan one: the transcoded files are recorded as skipped (already at target codec),
	// the parked one is refused by Claim, and the retention pass then runs.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	if n := encodes.Load(); n != 0 {
		t.Fatalf("the first scan encoded %d file(s); this fixture has nothing to encode", n)
	}
	rows := terminalRows(t, ts)
	// Every one of these four rows is holding a file that is still in the library out of
	// the encoder, so the retention of 1 cannot be met and none of them may go.
	if len(rows) != 4 {
		t.Fatalf("the retention pass left %d terminal rows, want all 4: every one of them is a decision "+
			"about a file that is still there", len(rows))
	}

	// Scan two, over the same library. Nothing may reach the encoder.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	if n := encodes.Load(); n != 0 {
		t.Fatalf("after a prune, the next scan encoded %d file(s)", n)
	}
	// The transcoded files are still on disk, byte for byte: nothing about a retention
	// pass touches a media file.
	for i := 0; i < 3; i++ {
		if !exists(filepath.Join(root, "transcoded"+strconv.Itoa(i)+".mkv")) {
			t.Errorf("transcoded%d.mkv is gone; retention must never touch a media file", i)
		}
	}
}

// movedConfig is one library, the terminal rows it already carries, and a configuration the
// operator has since changed. Both runs get the identical fixture and the identical
// configuration; history_retention_rows is the only thing that varies, which is what makes
// the PRUNE the cause of any encode rather than the configuration change.
type movedConfig struct {
	name  string
	build func(t *testing.T, ffmpeg, root string) []string
	row   func(path string) (store.Status, *store.Outcome)
	cfg   func(c *config.Config)
}

// movedConfigCases are the configurations under which a pruned verdict does NOT re-derive
// itself. In each, the recorded row is the only thing holding the file out of the encoder:
// the guard that produced it does not fire under the configuration now in force.
//
// This is why the already-at-target-codec fixture above is not enough on its own. That one
// is the single configuration in which the guard re-derives the pruned verdict, so it
// cannot see a prune that deletes a live decision.
func movedConfigCases() []movedConfig {
	return []movedConfig{
		{
			name: "the operator moved the target codec from hevc to av1",
			build: func(t *testing.T, ffmpeg, root string) []string {
				var out []string
				for i := 0; i < 3; i++ {
					p := filepath.Join(root, "already-hevc"+strconv.Itoa(i)+".mkv")
					mkHevc(t, ffmpeg, p, "800k")
					out = append(out, p)
				}
				return out
			},
			row: func(string) (store.Status, *store.Outcome) {
				src, dst := int64(4096), int64(1024)
				return store.Done, &store.Outcome{Encoder: "cpu", SourceBytes: &src, OutputBytes: &dst}
			},
			cfg: func(c *config.Config) { c.Encoder = "svtav1" },
		},
		{
			name: "the operator lowered min_bitrate_kbps",
			build: func(t *testing.T, ffmpeg, root string) []string {
				var out []string
				for i := 0; i < 3; i++ {
					p := filepath.Join(root, "was-low-bitrate"+strconv.Itoa(i)+".mkv")
					mkH264(t, ffmpeg, p, "8M")
					out = append(out, p)
				}
				return out
			},
			row: func(string) (store.Status, *store.Outcome) {
				return store.Skipped, because(SkipLowBitrate)
			},
			// baseCfg already sets MinBitrateKbps: 0 - the threshold the operator has
			// lowered TO. The rows were recorded under a higher one.
			cfg: func(c *config.Config) { c.MinBitrateKbps = 0 },
		},
	}
}

// movedConfigEncodes builds one case, runs two scans over it at the given retention, and
// returns how many files reached the encoder.
func movedConfigEncodes(t *testing.T, c movedConfig, retention int) int32 {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	files := c.build(t, ffmpeg, root)

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes), func(cfg *config.Config) {
		c.cfg(cfg)
		cfg.HistoryRetentionRows = retention
	})
	ts := eng.Store.(*testStore)
	for _, f := range files {
		st, o := c.row(f)
		seedRowForRealFile(t, ts, f, st, o)
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	rowsAfterOne := len(terminalRows(t, ts))
	// Every file is still on disk. Criterion 6 says none of them may be encoded.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	t.Logf("%s / history_retention_rows=%d: %d terminal rows survived scan one, %d encode(s) by the end of scan two",
		c.name, retention, rowsAfterOne, encodes.Load())
	return encodes.Load()
}

func TestRetention_PruningUnderAMovedConfigurationStillEncodesNothing(t *testing.T) {
	for _, c := range movedConfigCases() {
		t.Run(c.name, func(t *testing.T) {
			// The control: retention disabled over the identical fixture. It must encode
			// nothing, or the case is measuring the configuration change and not the prune.
			if off := movedConfigEncodes(t, c, 0); off != 0 {
				t.Fatalf("the control encoded %d file(s) with retention DISABLED; the fixture is wrong, "+
					"not the claim", off)
			}
			if on := movedConfigEncodes(t, c, 1); on > 0 {
				t.Errorf("history_retention_rows=1 pruned the terminal rows of files still present in the "+
					"library and the next scan handed %d of them to the encoder.\n"+
					"Criterion 6: WHEN retention has pruned rows for files that are still present in the "+
					"library THE SYSTEM SHALL NOT thereby cause any of those files to be encoded again.", on)
			}
		})
	}
}

// The anti-vacuity half, and it is the important one: this is the prune the criterion
// forbids, run on purpose. Take the same fixtures, prune with every row declared spent -
// which is what a retention that treated a terminal row as pure history would do - and the
// next scan MUST encode. If it ever stops, the graders above are asserting a property
// nothing can break and are not evidence.
func TestRetention_APruneThatTreatsALiveDecisionAsHistoryIsWhatCausesTheReEncode(t *testing.T) {
	everyRowSpent := func(string, string, store.Status) bool { return true }
	for _, c := range movedConfigCases() {
		t.Run(c.name, func(t *testing.T) {
			ffmpeg, ffprobe := tools(t)
			root := t.TempDir()
			files := c.build(t, ffmpeg, root)

			var encodes atomic.Int32
			eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes), func(cfg *config.Config) {
				c.cfg(cfg)
				cfg.HistoryRetentionRows = 1
			})
			ts := eng.Store.(*testStore)
			for _, f := range files {
				st, o := c.row(f)
				seedRowForRealFile(t, ts, f, st, o)
			}
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("first RunOneshot: %v", err)
			}
			if n := encodes.Load(); n != 0 {
				t.Fatalf("the first scan encoded %d file(s); this fixture starts with nothing to encode", n)
			}

			// The mutation: the store's own prune, told that every row is spent.
			p, err := ts.PruneTerminal(context.Background(), 1, eng.Cfg.MaxFailures, everyRowSpent)
			if err != nil {
				t.Fatalf("mutation prune: %v", err)
			}
			if p.Removed == 0 {
				t.Fatalf("the mutation prune removed no row (%+v); it cannot demonstrate anything", p)
			}
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("second RunOneshot: %v", err)
			}
			if n := encodes.Load(); n == 0 {
				t.Fatal("pruning every terminal row did NOT cause an encode, so the graders above cannot " +
					"fail and prove nothing about the rule that keeps them")
			}
		})
	}
}

// The anti-vacuity half for the parked row, which reaches the encoder through a third door:
// fail_count, not a skip guard.
func TestRetention_TheParkedRowRuleIsWhatPreventsTheReEncode(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	parked := libraryStillPresent(t, ffmpeg, root, 3)

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes), func(c *config.Config) {
		c.HistoryRetentionRows = 1
	})
	ts := eng.Store.(*testStore)
	parkFile(t, ts, parked, eng.Cfg.MaxFailures)

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	if n := encodes.Load(); n != 0 {
		t.Fatalf("the first scan encoded %d file(s)", n)
	}

	// The mutation: remove the parked row, which is precisely what a prune that treated
	// every terminal row alike would have done.
	if err := ts.Delete(context.Background(), parked, probe.Fingerprint(parked)); err != nil {
		t.Fatalf("mutation delete: %v", err)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	if n := encodes.Load(); n == 0 {
		t.Fatal("removing the parked row did NOT cause an encode, so the fixture above cannot fail " +
			"and proves nothing about the parked-row rule")
	}
}

// --- the rule's other half: what it still prunes, and where it refuses to look ----------

// TestRetention_TheBoundIsStillMetOverHistoryTheLibraryHasFinishedWith. An exclusion that
// never prunes anything would defeat the criterion it serves, so this is the pair to the
// graders above: with the SAME aggressive retention and the SAME live library, the rows for
// files the library no longer holds are removed, and only the live decisions stay.
func TestRetention_TheBoundIsStillMetOverHistoryTheLibraryHasFinishedWith(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		mkHevc(t, ffmpeg, filepath.Join(root, "present"+strconv.Itoa(i)+".mkv"), "800k")
	}

	var encodes atomic.Int32
	eng := buildEngine(t, ffmpeg, ffprobe, root, countingEncoder(&encodes), func(c *config.Config) {
		c.HistoryRetentionRows = 3
		c.Encoder = "svtav1" // the moved configuration, so the live rows are load-bearing
	})
	ts := eng.Store.(*testStore)
	for i := 0; i < 2; i++ {
		src, dst := int64(4096), int64(1024)
		seedRowForRealFile(t, ts, filepath.Join(root, "present"+strconv.Itoa(i)+".mkv"),
			store.Done, &store.Outcome{Encoder: "cpu", SourceBytes: &src, OutputBytes: &dst})
	}
	// Twenty rows for files that were in this directory and are not any more.
	for i := 0; i < 20; i++ {
		seedRow(t, ts, filepath.Join(root, "gone"+strconv.Itoa(i)+".mkv"), store.Skipped, because(SkipLowBitrate))
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	rows := terminalRows(t, ts)
	if len(rows) != 3 {
		t.Errorf("the ledger holds %d terminal rows with history_retention_rows: 3; the bound is met over "+
			"the rows the library has finished with", len(rows))
	}
	for i := 0; i < 2; i++ {
		p := filepath.Join(root, "present"+strconv.Itoa(i)+".mkv")
		var found bool
		for _, r := range rows {
			if r.Path == p {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is still in the library and its row was pruned", p)
		}
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	if n := encodes.Load(); n != 0 {
		t.Errorf("the next scan encoded %d file(s) after the prune", n)
	}
}

// TestRetention_RowsUnderADirectoryThisRunDidNotListAreNeverPruned is the unmounted-subtree
// case, and it is why absence alone is not evidence. A nested mount that is down looks
// EXACTLY like a library the operator emptied: the roots list fine and a whole subtree is
// simply not there. Pruning on that and meeting the files again when the mount returns
// would re-encode an entire library at once, so a row is only ever spent if this run
// LISTED the directory the file should be in.
func TestRetention_RowsUnderADirectoryThisRunDidNotListAreNeverPruned(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	mount := filepath.Join(root, "tv")

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.HistoryRetentionRows = 1
	})
	ts := eng.Store.(*testStore)
	for i := 0; i < 10; i++ {
		seedRow(t, ts, filepath.Join(mount, "ep"+strconv.Itoa(i)+".mkv"), store.Done, nil)
	}

	// The mount is down: root lists, root/tv does not exist.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot with the subtree absent: %v", err)
	}
	if got := len(terminalRows(t, ts)); got != 10 {
		t.Fatalf("the retention pass removed %d of 10 rows under a directory it never listed", 10-got)
	}

	// The mount comes back, empty - now the run HAS looked, and the same absence is
	// evidence. Without this half the test above would pass against a prune that had
	// simply stopped working.
	if err := os.MkdirAll(mount, 0o755); err != nil {
		t.Fatalf("restore the subtree: %v", err)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot with the subtree present: %v", err)
	}
	if got := len(terminalRows(t, ts)); got != 1 {
		t.Errorf("the ledger holds %d terminal rows once the directory could be listed, want the retained 1", got)
	}
}

// --- criterion 10: a prune failure is logged, survivable, and takes nothing with it ------

// pruneFailingStore fails the retention pass after reporting that some rows were already
// removed - the shape a real batched prune fails in.
type pruneFailingStore struct {
	*testStore
	err   error
	calls atomic.Int32
}

func (s *pruneFailingStore) PruneTerminal(_ context.Context, _, _ int, _ store.Prunable) (store.Prune, error) {
	s.calls.Add(1)
	return store.Prune{Removed: 3, ReclaimedCarried: 99}, s.err
}

func TestRetention_APruneFailureIsLoggedAndLeavesEveryRowItDidNotRemoveInPlace(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	mkH264(t, ffmpeg, filepath.Join(root, "fresh.mkv"), "8M")

	var buf bytes.Buffer
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.HistoryRetentionRows = 2
	})
	ts := eng.Store.(*testStore)
	failing := &pruneFailingStore{testStore: ts, err: os.ErrDeadlineExceeded}
	eng.Store = failing
	eng.Log = captureLogger(&buf)

	var encodes atomic.Int32
	eng.Enc = countingEncoder(&encodes)

	seeded := make([]string, 0, 9)
	for i := 0; i < 9; i++ {
		p := "/lib/history" + strconv.Itoa(i) + ".mkv"
		seedRow(t, ts, p, store.Skipped, because(SkipLowBitrate))
		seeded = append(seeded, p)
	}

	// The scan must SUCCEED. A prune is bookkeeping about history; it is not the reason
	// this process exists, and a store that cannot be pruned must not stop the daemon.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot returned %v; a prune failure must not fail the scan", err)
	}
	if failing.calls.Load() != 1 {
		t.Fatalf("the retention pass ran %d times, want once per completed scan", failing.calls.Load())
	}

	// Logged, naming the failure - not swallowed.
	log := buf.String()
	if !strings.Contains(log, "retention") || !strings.Contains(log, os.ErrDeadlineExceeded.Error()) {
		t.Errorf("the prune failure was not reported in the log:\n%s", log)
	}

	// Every row the failing pass did not remove is still there, by name. (It removed none:
	// the engine deletes nothing itself, so a row the store still holds must survive a
	// pass that only claimed to have removed some.)
	held := map[string]bool{}
	for _, r := range terminalRows(t, ts) {
		held[r.Path] = true
	}
	for _, p := range seeded {
		if !held[p] {
			t.Errorf("%s was not removed by the failing prune and is gone anyway", p)
		}
	}

	// And it KEEPS ENCODING: the next scan still hands the library's one encodable file
	// to the encoder.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	if n := encodes.Load(); n == 0 {
		t.Error("after a prune failure the engine stopped encoding; a bookkeeping failure must not " +
			"stop the thing this process exists to do")
	}
}
