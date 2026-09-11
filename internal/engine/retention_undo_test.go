package engine

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// Where LEDGER-5's retention meets UNDO-6's window.
//
// The two features MEET over the same rows, and neither phase's own suite grades the
// meeting: retention prunes terminal ledger rows, and the undo window retains an
// original hard-linked against exactly such a row. LEDGER-5's criterion 6 - "WHEN
// retention has pruned rows for files that are still present in the library THE SYSTEM
// SHALL NOT thereby cause any of those files to be encoded again" - is the clause that
// binds here, because a retained original IS a file that is still present, and a
// RESTORED original is a file that has come back.
//
// The rule the engine applies (rowIsSpent) is stated entirely in terms of what is at a
// LIBRARY PATH right now: a row may only go when this run listed its directory and the
// file was not there. The undo window's own state lives in `retained_originals`, a table
// the prune neither reads nor writes. That is why the two agree - but "they agree"
// asserted in a comment is worth nothing on the one path this repository's card says is
// unrecoverable, so each half is graded below, and the mutation that would break it is
// named in each case.
//
// Every case here drives REAL encodes and REAL swaps, because the interaction only
// exists after a swap has happened: a retention is taken before the rename and recorded
// after it.

// retentionUndoEngine builds an engine with BOTH features on: the undo window open for
// hours, and the ledger bounded at rows. It returns the engine and the store behind it,
// so a test can drive several passes over one ledger - the interaction is a
// several-passes property (retain and swap in pass one, prune in pass one's tail, meet
// the same files again in pass two).
func retentionUndoEngine(t *testing.T, ffmpeg, ffprobe, root string, hours, rows int, enc Encoder) (*Engine, *testStore) {
	t.Helper()
	cfg := undoCfg(root, hours)
	cfg.HistoryRetentionRows = rows
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	return New(cfg, prober, enc, ts, discardLogger()), ts
}

// seedGoneAfter writes n terminal rows for files that are not in the library, named so
// they sort AFTER the fixtures' own `movie*.mkv`.
//
// The name is the whole of the point. The prune walks oldest first on
// (updated_at, path, fingerprint) and updated_at is a unix SECOND, so a fixture built
// inside one second leaves the PATH as the only ordering that matters - and a `gone*`
// name would put every seeded row ahead of `movie.mkv`, so the row under test would be
// the last one offered and would survive a prune with no rule in it whatsoever. Sorting
// them after it makes the row under test the FIRST thing the prune reaches, which is the
// only arrangement in which keeping it proves anything.
func seedGoneAfter(t *testing.T, ts *testStore, root string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		seedRow(t, ts, filepath.Join(root, "zz-gone"+strconv.Itoa(i)+".mkv"), store.Skipped, &store.Outcome{Reason: SkipLowBitrate})
	}
}

// --- the row whose original the window is holding -------------------------------------

// TestRetentionUndo_APruneKeepsTheRowWhoseOriginalTheWindowIsHolding is the interaction
// stated directly. After a swap under an open window there are three things about one
// file: the swapped file at the library path, the `done` row keyed on ITS fingerprint,
// and a retained original holding the pre-swap bytes under a second name.
//
// An aggressive retention (1 row, against a ledger that will hold more) is then run over
// that. It must remove nothing: the file the row decided is still there, so the row is
// still what holds it out of the encoder. And the retention must come through untouched -
// the record, the retained bytes, and the ability to actually put them back.
//
// The restore at the end is not decoration. A prune that had quietly taken the ledger row
// out from under a live retention would still leave `restore` working (it reads a
// different table), so the case is only evidence if it asserts BOTH: that the row stayed
// AND that the window still delivers.
func TestRetentionUndo_APruneKeepsTheRowWhoseOriginalTheWindowIsHolding(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	originalSum := sha256f(t, src)

	// Retention of 1, so the pass is pushed to prune everything the rule permits. The
	// ledger will hold more than one terminal row: the done row for the swap, plus the
	// rows seeded below for files that really are gone.
	eng, ts := retentionUndoEngine(t, ffmpeg, ffprobe, root, 24, 1, nil)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	// History the retention SHOULD be able to take: rows for files this run will look
	// for and not find. Without them the pass would have nothing to remove and the case
	// could not tell "kept the right row" from "pruned nothing at all".
	//
	// They are seeded AFTER the swap and named to sort after it, both deliberately. The
	// pass walks oldest first on (updated_at, path, fingerprint), and updated_at is a
	// UNIX SECOND - a whole fixture can land inside one of them, whereupon the path is
	// the tiebreak. Seeded first, or named `gone*`, the done row would be the last row
	// offered and would survive a prune that had no rule at all, which is a fixture that
	// cannot tell the rule from its absence.
	seedGoneAfter(t, ts, root, 6)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}

	// The swap happened and the window is holding the original.
	r := onlyRetained(t, ts)
	if !exists(r.RetainedPath) {
		t.Fatalf("the retained original %s is not on disk after the pass that pruned the ledger", r.RetainedPath)
	}
	if got := sha256f(t, r.RetainedPath); got != originalSum {
		t.Fatalf("the retained original is not the source's bytes any more (%s != %s)", got, originalSum)
	}

	// The prune ran and did its job on the rows it MAY take...
	rows := terminalRows(t, ts)
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d terminal rows, want 1: the six seeded rows name files this run "+
			"listed for and did not find, so they are exactly what retention bounds: %+v", len(rows), rows)
	}
	// ...and the one row it kept is the done row for the swapped file, NOT one of the
	// seeded ones. A pass that had pruned the done row and kept a seeded row would also
	// leave exactly one row behind, so the identity is the assertion.
	kept := rows[0]
	if kept.Path != r.SwappedPath || kept.Status != store.Done {
		t.Fatalf("the surviving row is %s/%s; want the done row for the swapped file %s - the row whose "+
			"original the undo window is holding is the one row that may never go", kept.Path, kept.Status, r.SwappedPath)
	}
	if kept.Fingerprint != probe.Fingerprint(r.SwappedPath) {
		t.Errorf("the surviving row's fingerprint %q is not the swapped file's %q, so Claim would not find it",
			kept.Fingerprint, probe.Fingerprint(r.SwappedPath))
	}

	// The window still delivers: the original goes back, byte for byte.
	res, err := eng.Restore(context.Background(), r.SourcePath)
	if err != nil {
		t.Fatalf("restore after the prune: %v", err)
	}
	if res.RestoredAt == 0 {
		t.Error("the restore reported no timestamp")
	}
	if got := sha256f(t, r.SourcePath); got != originalSum {
		t.Errorf("the restored file is not the original's bytes (%s != %s)", got, originalSum)
	}
}

// TestRetentionUndo_APruneRemovesNoRetentionRecordAndNoRetainedByte grades the other
// direction: the prune is an irreversible DELETE, and the table it deletes from is not
// the only one holding a promise to an operator. It must leave `retained_originals`
// alone, leave the retained bytes on disk, and leave the held-space figure the API
// publishes (`bytes_held_by_undo_window`) exactly where it was.
//
// The figure matters on its own. A prune carries a removed row's reclaimed contribution
// forward so the lifetime reclaimed total cannot move; if it also moved the HELD figure,
// the pair would start claiming space had come back that is still allocated under a
// second link - the same class of lie criterion 5 exists to prevent, through the other
// half of the report.
func TestRetentionUndo_APruneRemovesNoRetentionRecordAndNoRetainedByte(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		mkH264(t, ffmpeg, filepath.Join(root, "movie"+strconv.Itoa(i)+".mkv"), "8M")
	}

	eng, ts := retentionUndoEngine(t, ffmpeg, ffprobe, root, 24, 1, nil)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	seedGoneAfter(t, ts, root, 8)

	before := retainedRows(t, ts)
	if len(before) != 2 {
		t.Fatalf("the window is holding %d originals, want 2", len(before))
	}
	heldBefore, err := ts.HeldByUndoWindow(context.Background())
	if err != nil {
		t.Fatalf("HeldByUndoWindow: %v", err)
	}
	if heldBefore <= 0 {
		t.Fatalf("the window reports %d bytes held; the fixture retained two originals", heldBefore)
	}
	sums := map[string]string{}
	for _, r := range before {
		sums[r.RetainedPath] = sha256f(t, r.RetainedPath)
	}

	// Two more passes with the same aggressive retention. Each one runs a full prune,
	// so "the record survived" is not an artefact of the prune having run only once.
	for i := 0; i < 2; i++ {
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot %d: %v", i+2, err)
		}
	}

	// The prune really did fire, so what follows is evidence about a prune and not about
	// a build whose retention never ran: ten terminal rows went in and only the two the
	// rule may not take came out.
	if got := len(terminalRows(t, ts)); got != 2 {
		t.Fatalf("the ledger holds %d terminal rows after two prunes at history_retention_rows: 1; want the "+
			"2 done rows for the files still in the library, with the 8 seeded rows gone", got)
	}

	after := retainedRows(t, ts)
	if len(after) != len(before) {
		t.Fatalf("the ledger holds %d retained originals after two more prunes, want %d - a prune must not "+
			"remove a promise the undo window made", len(after), len(before))
	}
	for _, r := range after {
		want, ok := sums[r.RetainedPath]
		if !ok {
			t.Errorf("a retention record appeared for %s that was not there before the prunes", r.RetainedPath)
			continue
		}
		if !exists(r.RetainedPath) {
			t.Errorf("the retained original %s is gone after a prune", r.RetainedPath)
			continue
		}
		if got := sha256f(t, r.RetainedPath); got != want {
			t.Errorf("the retained original %s changed across the prunes", r.RetainedPath)
		}
	}
	heldAfter, err := ts.HeldByUndoWindow(context.Background())
	if err != nil {
		t.Fatalf("HeldByUndoWindow after: %v", err)
	}
	if heldAfter != heldBefore {
		t.Errorf("the space the window reports holding moved across a prune: %d -> %d. A prune returns no "+
			"space to the filesystem and must not report that it did", heldBefore, heldAfter)
	}
}

// --- a restored original is a file that has come back ---------------------------------

// TestRetentionUndo_TheParkRowARestoreWritesSurvivesEveryPruneAndIsNotReEncoded is the
// sharp case, and it is criterion 6 read through UNDO-6's own reconciliation.
//
// A restore does not merely move bytes. `recordRestoreInJobs` deletes the done row of the
// encode it removed and writes a `skipped` / `restored-original` row for the original -
// and that row is the ONLY thing standing between the restored file and the encoder. The
// gates that passed the encode the operator has just rejected will pass it again, and
// the swap would then destroy the bytes they were given a window to recover.
//
// So a retention pass that treated that row as history would undo the undo, on the next
// scan, silently. The rule keeps it because the file is there. Two further scans with an
// aggressive retention are what proves it, since the row must survive being OFFERED to
// the prune rather than merely never reaching it.
//
// MUTATION: force rowIsSpent to `return true` (the pre-F5 behaviour, which is also what
// "prune the oldest, full stop" would do) and this test reds with the restored original
// re-encoded.
func TestRetentionUndo_TheParkRowARestoreWritesSurvivesEveryPruneAndIsNotReEncoded(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	originalSum := sha256f(t, src)

	var encodes atomic.Int32
	// A real encoder for pass one (there has to be a real swap to restore), swapped for a
	// counting encoder afterwards. Counting from the start would make "0 encodes" untrue
	// by construction, since the first pass MUST encode.
	eng, ts := retentionUndoEngine(t, ffmpeg, ffprobe, root, 24, 1, nil)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	r := onlyRetained(t, ts)
	if _, err := eng.Restore(context.Background(), r.SourcePath); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := sha256f(t, r.SourcePath); got != originalSum {
		t.Fatalf("the restore did not put the original back")
	}

	// The park row the restore wrote, before any prune has seen it.
	restoredKey := probe.Fingerprint(r.SourcePath)
	st, _, found, err := ts.Get(context.Background(), r.SourcePath, restoredKey)
	if err != nil {
		t.Fatalf("Get the park row: %v", err)
	}
	if !found || st != store.Skipped {
		t.Fatalf("after a restore the ledger holds status=%q found=%v for %s; the restore's park row is "+
			"the fixture and without it this test asserts nothing", st, found, r.SourcePath)
	}

	// Now the prune meets it, twice, with a retention of 1 and history it may take, so
	// the pass has both a reason to prune and rows it is allowed to prune.
	eng.Enc = countingEncoder(&encodes)
	seedGoneAfter(t, ts, root, 8)
	for i := 0; i < 2; i++ {
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot %d after the restore: %v", i+2, err)
		}
	}

	if n := encodes.Load(); n != 0 {
		t.Errorf("after a restore and %d prunes the scan handed %d file(s) back to the encoder - a prune "+
			"undid the undo", 2, n)
	}
	st, _, found, err = ts.Get(context.Background(), r.SourcePath, restoredKey)
	if err != nil {
		t.Fatalf("Get the park row after the prunes: %v", err)
	}
	if !found || st != store.Skipped {
		t.Errorf("the restore's park row for %s is status=%q found=%v after the prunes; it is what holds a "+
			"restored original out of the encoder", r.SourcePath, st, found)
	}
	if got := sha256f(t, r.SourcePath); got != originalSum {
		t.Errorf("the restored original's bytes changed after the prunes (%s != %s)", got, originalSum)
	}
}

// --- the retention area is never evidence ---------------------------------------------

// TestRetentionUndo_TheRetentionAreaIsNeverEvidenceThatAFileIsGone grades the one place
// the two phases' code physically collided: enumerate() marks every directory it LISTED
// so the prune knows where this run actually looked, and it SKIPS the retention area so a
// retained original can never be enumerated as a source. Those two lines are in the same
// branch, and their order is the whole of the behaviour.
//
// A retention area recorded as "listed" would make the prune's evidence say holdfast had
// looked inside `.holdfast-undo` and found nothing - about a directory it deliberately
// never opened. The Coverage branch has always skipped it before marking; the direct-walk
// branch must do the same, or the rule differs between the production path (Coverage,
// which the startup walk sets and which INCLUDES the retention area) and every other.
//
// MUTATION: mark the directory observed before the skip - `observed[path] = true` above
// the UndoDirName check - and the second half of this test reds.
func TestRetentionUndo_TheRetentionAreaIsNeverEvidenceThatAFileIsGone(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, withCoverage := range []bool{false, true} {
		name := "walking the roots directly"
		if withCoverage {
			name = "bounded by the startup walk's coverage (the production path)"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			undoDir := undoDirFor(root)
			if err := os.MkdirAll(undoDir, 0o755); err != nil {
				t.Fatalf("mkdir the retention area: %v", err)
			}
			// A real file in it, so the directory is listable and non-empty: the walk
			// has every reason to descend and record it.
			mkHevc(t, ffmpeg, filepath.Join(undoDir, "held."+UndoMarker+".mkv"), "800k")

			eng, ts := retentionUndoEngine(t, ffmpeg, ffprobe, root, 24, 1, nil)
			if withCoverage {
				// Exactly what the startup walk produces - it descends into the
				// retention area, so the area is in Coverage.
				eng.Coverage = []string{root, undoDir}
			}

			_, observed := eng.enumerate()
			if observed[undoDir] {
				t.Errorf("enumerate() recorded the retention area %s as a directory this run listed. It did "+
					"not list it - it skipped it - and the prune reads that set as proof of where holdfast "+
					"actually looked", undoDir)
			}
			if !observed[root] {
				t.Fatalf("enumerate() did not record the library root %s as listed; the assertion above would "+
					"then pass against an enumerate that observed nothing at all", root)
			}

			// And the consequence, end to end: a terminal row naming a file that would
			// live in the retention area is never spent, so an aggressive retention
			// cannot take it.
			ghost := filepath.Join(undoDir, "vanished."+UndoMarker+".mkv")
			seedRow(t, ts, ghost, store.Done, nil)
			for i := 0; i < 5; i++ {
				seedRow(t, ts, filepath.Join(root, "gone"+strconv.Itoa(i)+".mkv"), store.Skipped, &store.Outcome{Reason: SkipLowBitrate})
			}
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}
			var ghostKept bool
			for _, row := range terminalRows(t, ts) {
				if row.Path == ghost {
					ghostKept = true
				}
			}
			if !ghostKept {
				t.Errorf("the retention pass removed the row for %s on the strength of an absence inside a "+
					"directory this run never listed", ghost)
			}
		})
	}
}

// TestRetentionUndo_TurningTheWindowOffDoesNotLetThePruneStrandARetention is the
// setting-change case both phases warn about from their own side. UNDO-6: setting
// `undo_window_hours` back to 0 must not strand what is already held - the release sweep
// is driven by what is RETAINED, never by the setting. LEDGER-5: retention is enforced
// after every scan whatever else is configured. Together that means a run with the window
// off and retention on prunes the ledger AND releases the expired retention in the same
// pass, and neither act may take the other's state with it.
func TestRetentionUndo_TurningTheWindowOffDoesNotLetThePruneStrandARetention(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	// Pass one with the window OPEN: a real swap, so there is a retention to strand.
	eng, ts := retentionUndoEngine(t, ffmpeg, ffprobe, root, 24, 1, nil)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("first RunOneshot: %v", err)
	}
	r := onlyRetained(t, ts)

	// The operator turns the window off and leaves retention on. The retention already
	// taken carries the expiry it was GIVEN, so it is not yet due; the prune runs anyway.
	off := eng.Cfg
	off.UndoWindowHours = 0
	eng.Cfg = off
	if eng.Cfg.UndoEnabled() {
		t.Fatal("the fixture did not turn the window off")
	}
	seedGoneAfter(t, ts, root, 6)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot with the window off: %v", err)
	}
	if got := len(retainedRows(t, ts)); got != 1 {
		t.Fatalf("the ledger holds %d retentions after a prune with the window off, want the 1 already "+
			"taken: turning the setting off must not make a prune forget what is held", got)
	}
	if !exists(r.RetainedPath) {
		t.Errorf("the retained original %s is gone after a prune with the window off", r.RetainedPath)
	}

	// And once the window HAS closed, the release still runs and the space comes back -
	// the prune in between changed nothing about that. Without this half the test above
	// would pass against a build that had simply stopped releasing anything.
	eng.undoNow = func() time.Time { return time.Now().Add(48 * time.Hour) }
	rep := eng.ReleaseExpired(context.Background())
	if rep.Released != 1 {
		t.Fatalf("the sweep released %d retentions once the window had closed, want 1", rep.Released)
	}
	if exists(r.RetainedPath) {
		t.Errorf("the retained original %s survived its own release", r.RetainedPath)
	}
}
