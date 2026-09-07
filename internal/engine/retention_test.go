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
// Criterion 6 is the sharp one, and its hazard is not the obvious half. A pruned DONE row
// for a file still on disk is re-claimed on the next scan and re-skipped by the
// already-at-target-codec guard: a weaker record of the same swap, but no encode. A pruned
// FAILED row is different - fail_count is what parks a file that has already failed
// max_failures times, and deleting it hands that file straight back to the encoder. The
// prune therefore keeps parked rows, and the fixture below proves both halves: that
// nothing is encoded with the rule in place, and that something IS encoded without it.

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
	for i := 0; i < 15; i++ {
		seedRow(t, ts, "/lib/history"+strconv.Itoa(i)+".mkv", store.Skipped, because(SkipLowBitrate))
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
		seedRow(t, ts, "/lib/history"+strconv.Itoa(i)+".mkv", store.Skipped, because(SkipLowBitrate))
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
	// The prune took the skips for files still on disk and left the parked row.
	var parkedRows int
	for _, r := range rows {
		if r.Status == store.Failed {
			parkedRows++
		}
	}
	if parkedRows != 1 {
		t.Fatalf("the retention pass removed the parked row: %d failed rows remain of 1", parkedRows)
	}
	if len(rows) > 2 {
		t.Fatalf("the retention pass left %d terminal rows with history_retention_rows: 1", len(rows))
	}

	// Scan two, over the same library. Every file whose row was pruned is still on disk,
	// and not one of them may reach the encoder.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}
	if n := encodes.Load(); n != 0 {
		t.Fatalf("after a prune, the next scan encoded %d file(s) whose rows the prune removed", n)
	}
	// The transcoded files are still on disk, byte for byte: nothing about a retention
	// pass touches a media file.
	for i := 0; i < 3; i++ {
		if !exists(filepath.Join(root, "transcoded"+strconv.Itoa(i)+".mkv")) {
			t.Errorf("transcoded%d.mkv is gone; retention must never touch a media file", i)
		}
	}
}

// The anti-vacuity half: without the parked-row rule the same fixture DOES encode. If this
// ever stops reporting an encode, the criterion above is asserting a property nothing can
// break and is not evidence.
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

// --- criterion 10: a prune failure is logged, survivable, and takes nothing with it ------

// pruneFailingStore fails the retention pass after reporting that some rows were already
// removed - the shape a real batched prune fails in.
type pruneFailingStore struct {
	*testStore
	err   error
	calls atomic.Int32
}

func (s *pruneFailingStore) PruneTerminal(_ context.Context, _, _ int) (store.Prune, error) {
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
