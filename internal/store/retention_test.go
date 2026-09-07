package store

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The retention prune (LEDGER-5), and the invariant it exists to not break.
//
// Criterion 1: WHEN terminal rows exceed the configured retention THE SYSTEM SHALL prune
// the oldest beyond it and SHALL NOT prune a row that still contributes to a durable
// total.
//
// Criterion 5: WHEN a prune has removed rows that recorded both sizes THE SYSTEM SHALL
// report a lifetime reclaimed total no lower than the one it reported immediately before
// that prune, both on the running server and after a restart.
//
// The restart half is the one that bites, and it is why this file re-OPENS a database on
// disk rather than only asserting against a live handle. internal/server's Hub reads the
// reclaimed baseline ONCE, at construction, so a prune that quietly removed contributing
// rows would report a correct total for as long as the daemon lived and a wrong one the
// next time it started - a defect whose only symptom appears weeks later, in a figure that
// shrank. The fixtures here therefore always ask the question twice: before and after the
// prune on the same handle, and again through a fresh Open of the same file.

// everyRowSpent is the caller's answer when nothing in the fixture is in the library any
// more: every terminal row here is pure history, so removing it can cause no encode. Most
// fixtures below are about the ARITHMETIC of a prune and want the pass to take what it may,
// so this is what they pass. The rows that are NOT spent - the ones holding a file out of
// the encoder - are graded where that can be seen: internal/engine/retention_test.go.
func everyRowSpent(string, string, Status) bool { return true }

// noRowSpent is the opposite answer, and the one a caller that cannot see the library must
// give. The store may delete nothing on it.
func noRowSpent(string, string, Status) bool { return false }

// seedTerminal writes one terminal row with an explicit transition time, so "the oldest"
// is a fact of the fixture and not of how fast the test ran. reclaimed > 0 records both
// sizes on a done row, which is what makes the row contribute to the lifetime total.
func seedRetentionRow(t *testing.T, s *SQLite, path string, st Status, at int64, reclaimed int64, failCount int) {
	t.Helper()
	ctx := context.Background()
	var srcP, outP *int64
	if reclaimed > 0 {
		src := int64(10_000_000)
		out := src - reclaimed
		srcP, outP = &src, &out
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at, source_bytes, output_bytes)
		 VALUES (?, ?, ?, ?, NULL, ?, ?, ?)`,
		path, "fp", string(st), failCount, at, nullInt(srcP), nullInt(outP)); err != nil {
		t.Fatalf("seed %s: %v", path, err)
	}
}

func terminalCount(t *testing.T, s *SQLite) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status IN ('done','skipped','failed')`).Scan(&n); err != nil {
		t.Fatalf("count terminal: %v", err)
	}
	return n
}

func mustReclaimed(t *testing.T, s *SQLite) int64 {
	t.Helper()
	total, err := s.ReclaimedTotal(context.Background())
	if err != nil {
		t.Fatalf("ReclaimedTotal: %v", err)
	}
	return total
}

func rowExists(t *testing.T, s *SQLite, path string) bool {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE path = ?`, path).Scan(&n); err != nil {
		t.Fatalf("row exists %s: %v", path, err)
	}
	return n > 0
}

// --- criterion 1: the oldest beyond the retention, and nothing that still counts -------

func TestPrune_RemovesTheOldestTerminalRowsBeyondTheRetention(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Twelve terminal rows, oldest first by updated_at. Retention keeps five.
	for i := 0; i < 12; i++ {
		seedRetentionRow(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", Skipped, int64(1000+i), 0, 0)
	}
	p, err := s.PruneTerminal(ctx, 5, 3, everyRowSpent)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if p.Removed != 7 {
		t.Fatalf("prune removed %d rows, want 7 (12 terminal rows, retention 5)", p.Removed)
	}
	if got := terminalCount(t, s); got != 5 {
		t.Fatalf("the ledger holds %d terminal rows after a retention of 5", got)
	}
	// The OLDEST went and the newest stayed. A prune that removed an arbitrary seven
	// would pass the count assertion above and fail this one.
	for i := 0; i < 7; i++ {
		if rowExists(t, s, "/lib/f"+strconv.Itoa(i)+".mkv") {
			t.Errorf("/lib/f%d.mkv is one of the seven oldest and is still in the ledger", i)
		}
	}
	for i := 7; i < 12; i++ {
		if !rowExists(t, s, "/lib/f"+strconv.Itoa(i)+".mkv") {
			t.Errorf("/lib/f%d.mkv is one of the five newest and was pruned", i)
		}
	}
}

func TestPrune_LeavesANonTerminalRowAlone(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Retention is over TERMINAL rows. A pending or in-flight job is not history, it is
	// work, and deleting it would drop a claim out from under a running worker.
	for i := 0; i < 6; i++ {
		seedRetentionRow(t, s, "/lib/done"+strconv.Itoa(i)+".mkv", Done, int64(1000+i), 0, 0)
	}
	for i, st := range []Status{Pending, Probing, Encoding, Verifying} {
		seedRetentionRow(t, s, "/lib/live"+strconv.Itoa(i)+".mkv", st, int64(1), 0, 0)
	}
	if _, err := s.PruneTerminal(ctx, 2, 3, everyRowSpent); err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	for i := 0; i < 4; i++ {
		if !rowExists(t, s, "/lib/live"+strconv.Itoa(i)+".mkv") {
			t.Errorf("/lib/live%d.mkv is a non-terminal row and the prune removed it", i)
		}
	}
	if got := terminalCount(t, s); got != 2 {
		t.Errorf("the ledger holds %d terminal rows after a retention of 2", got)
	}
}

func TestPrune_RetentionDisabledKeepsEveryRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 40; i++ {
		seedRetentionRow(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", Done, int64(1000+i), 1_000, 0)
	}
	before := mustReclaimed(t, s)

	// 0 is the shipped default and what an absent key resolves to. A negative value never
	// reaches here (config refuses it), and is treated as disabled rather than as an
	// invitation to interpret.
	for _, maxRows := range []int{0, -1, -1000} {
		p, err := s.PruneTerminal(ctx, maxRows, 3, everyRowSpent)
		if err != nil {
			t.Fatalf("PruneTerminal(%d): %v", maxRows, err)
		}
		if p.Removed != 0 || p.ReclaimedCarried != 0 || p.Kept != 0 {
			t.Errorf("PruneTerminal(%d) reported %+v; disabled retention must do nothing at all", maxRows, p)
		}
		if got := terminalCount(t, s); got != 40 {
			t.Errorf("PruneTerminal(%d) left %d of 40 rows", maxRows, got)
		}
	}
	if got := mustReclaimed(t, s); got != before {
		t.Errorf("disabled retention moved the lifetime total from %d to %d", before, got)
	}
}

// --- criterion 5: the lifetime total never runs backwards -----------------------------

func TestPrune_LifetimeReclaimedTotalIsUnchangedByAPruneOnTheRunningStoreAndAfterRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Thirty done rows, every one of them recording both sizes, so every one contributes
	// to the lifetime total. This is the exact shape the criterion is about.
	const rows, each = 30, 1_500_000
	for i := 0; i < rows; i++ {
		seedRetentionRow(t, s, "/lib/done"+strconv.Itoa(i)+".mkv", Done, int64(1000+i), each, 0)
	}
	before := mustReclaimed(t, s)
	if want := int64(rows * each); before != want {
		t.Fatalf("the lifetime total before the prune is %d, want %d", before, want)
	}

	p, err := s.PruneTerminal(ctx, 4, 3, everyRowSpent)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if p.Removed != rows-4 {
		t.Fatalf("prune removed %d rows, want %d", p.Removed, rows-4)
	}
	if p.ReclaimedCarried != int64(p.Removed)*each {
		t.Fatalf("prune carried %d bytes forward for %d removed rows, want %d",
			p.ReclaimedCarried, p.Removed, int64(p.Removed)*each)
	}

	// Half one: the RUNNING store. The rows that contributed are gone; the figure is not.
	if after := mustReclaimed(t, s); after != before {
		t.Fatalf("the lifetime total moved from %d to %d across a prune on the running store", before, after)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Half two: the RESTART. This is where a prune that simply deleted the rows would show
	// its damage - Hub re-reads this exact figure as its baseline on every start, so a
	// total that only survived in memory would come back short.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if after := mustReclaimed(t, s2); after != before {
		t.Fatalf("the lifetime total is %d after a restart, was %d before the prune: a prune moved the baseline", after, before)
	}
	if got := terminalCount(t, s2); got != 4 {
		t.Fatalf("the reopened ledger holds %d terminal rows, want the retained 4", got)
	}
}

func TestPrune_TotalDoesNotDropWhenOnlySomeRowsRecordedTheirSizes(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// A ledger of the shape a real upgrade produces: rows written before the outcome
	// columns existed (no sizes), beside rows that recorded them, beside skips and
	// failures that never had any. Only the first kind may move the total.
	for i := 0; i < 10; i++ {
		seedRetentionRow(t, s, "/lib/measured"+strconv.Itoa(i)+".mkv", Done, int64(100+i), 2_000, 0)
		seedRetentionRow(t, s, "/lib/unmeasured"+strconv.Itoa(i)+".mkv", Done, int64(200+i), 0, 0)
		seedRetentionRow(t, s, "/lib/skipped"+strconv.Itoa(i)+".mkv", Skipped, int64(300+i), 0, 0)
	}
	before := mustReclaimed(t, s)
	if before != 20_000 {
		t.Fatalf("the lifetime total before the prune is %d, want 20000", before)
	}
	if _, err := s.PruneTerminal(ctx, 1, 3, everyRowSpent); err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if got := mustReclaimed(t, s); got != before {
		t.Fatalf("the lifetime total moved from %d to %d; an unmeasured row must contribute nothing "+
			"and a measured one must be carried", before, got)
	}
	if got := terminalCount(t, s); got != 1 {
		t.Fatalf("the ledger holds %d terminal rows after a retention of 1", got)
	}
}

func TestPrune_RepeatedPrunesNeverAccumulateOrLoseTheCarriedTotal(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Prune, add more, prune again. Double counting and dropping are both failures the
	// single-pass fixtures above cannot see: a carry-forward that re-added a row it had
	// already carried would INFLATE the total, which is just as dishonest as losing it.
	var expected int64
	for round := 0; round < 4; round++ {
		for i := 0; i < 10; i++ {
			seedRetentionRow(t, s, "/lib/r"+strconv.Itoa(round)+"-"+strconv.Itoa(i)+".mkv",
				Done, int64(round*100+i), 1_000, 0)
			expected += 1_000
		}
		if _, err := s.PruneTerminal(ctx, 3, 3, everyRowSpent); err != nil {
			t.Fatalf("PruneTerminal round %d: %v", round, err)
		}
		if got := mustReclaimed(t, s); got != expected {
			t.Fatalf("after round %d the lifetime total is %d, want %d", round, got, expected)
		}
		if got := terminalCount(t, s); got != 3 {
			t.Fatalf("after round %d the ledger holds %d terminal rows, want 3", round, got)
		}
	}
}

// TestPrune_TheReclaimedInvariantRedsAgainstAPruneThatSimplyDeletes is the anti-vacuity
// proof for the two fixtures above. A test that a total did not move is worthless unless
// something can move it, so here is the implementation anyone would write first - delete
// the oldest rows and nothing else - measured with the same reading. It MUST come back
// lower, on the running handle and after a restart alike; if it does not, the fixtures
// above are asserting a property this code could not break, and they should be deleted
// rather than trusted.
func TestPrune_TheReclaimedInvariantRedsAgainstAPruneThatSimplyDeletes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const rows, each = 30, 1_500_000
	for i := 0; i < rows; i++ {
		seedRetentionRow(t, s, "/lib/done"+strconv.Itoa(i)+".mkv", Done, int64(1000+i), each, 0)
	}
	before := mustReclaimed(t, s)

	// The mutation: the delete, with no carry-forward.
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM jobs WHERE rowid IN (
			SELECT rowid FROM jobs WHERE status = 'done' ORDER BY updated_at ASC LIMIT ?)`,
		rows-4); err != nil {
		t.Fatalf("naive delete: %v", err)
	}
	if after := mustReclaimed(t, s); after >= before {
		t.Fatalf("a prune that merely deleted the contributing rows left the lifetime total at %d (was %d): "+
			"the invariant fixtures above cannot fail and are not evidence", after, before)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if after := mustReclaimed(t, s2); after >= before {
		t.Fatalf("after a restart, a prune that merely deleted left the lifetime total at %d (was %d)", after, before)
	}
}

// --- criterion 1, the second clause: a parked row is not the prune's to take -----------

func TestPrune_KeepsAFailedRowThatIsParkedAtMaxFailures(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// A failed row carries fail_count, which is what parks a file the engine has already
	// given max_failures attempts. Deleting it resets that accounting and hands the file
	// straight back to the encoder, which is the one prune that provably CAUSES work.
	// The parked row is also the OLDEST, so a prune ordering by updated_at alone would
	// take it first. It must be the one thing that survives.
	seedRetentionRow(t, s, "/lib/parked.mkv", Failed, 1, 0, 3)
	seedRetentionRow(t, s, "/lib/retryable.mkv", Failed, 2, 0, 1)
	for i := 0; i < 8; i++ {
		seedRetentionRow(t, s, "/lib/done"+strconv.Itoa(i)+".mkv", Done, int64(10+i), 0, 0)
	}

	p, err := s.PruneTerminal(ctx, 1, 3, everyRowSpent)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if !rowExists(t, s, "/lib/parked.mkv") {
		t.Error("the prune removed the failed row parked at max_failures; the next scan would encode that file again")
	}
	if rowExists(t, s, "/lib/retryable.mkv") {
		t.Error("a failed row below max_failures is already claimable, so pruning it causes no encode: it should have gone")
	}
	if p.Removed != 9 {
		t.Errorf("the prune removed %d rows, want the 9 that were not parked", p.Removed)
	}
	if got := terminalCount(t, s); got != 1 {
		t.Errorf("the ledger holds %d terminal rows after a retention of 1, want the parked one", got)
	}
}

func TestPrune_ReportsTheParkedRowsThatHoldTheLedgerAboveTheRetention(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// More parked rows than the retention allows. The bound cannot be met without causing
	// encodes, so it is not met - and the pass says how many rows it refused to take
	// rather than leaving an operator to wonder why the table stayed large.
	for i := 0; i < 5; i++ {
		seedRetentionRow(t, s, "/lib/parked"+strconv.Itoa(i)+".mkv", Failed, int64(i), 0, 3)
		seedRetentionRow(t, s, "/lib/done"+strconv.Itoa(i)+".mkv", Done, int64(100+i), 0, 0)
	}
	p, err := s.PruneTerminal(ctx, 2, 3, everyRowSpent)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if p.Removed != 5 {
		t.Errorf("the prune removed %d rows, want the 5 prunable ones", p.Removed)
	}
	if p.Kept != 5 {
		t.Errorf("the prune reported %d kept parked rows, want 5 - an operator whose retention cannot be met is owed the reason", p.Kept)
	}
	if got := terminalCount(t, s); got != 5 {
		t.Errorf("the ledger holds %d terminal rows, want the 5 parked ones the prune must not take", got)
	}
	// And it TERMINATES: a pass that kept re-reading a table it cannot shrink would spin.
	if p2, err := s.PruneTerminal(ctx, 2, 3, everyRowSpent); err != nil || p2.Removed != 0 || p2.Kept != 5 {
		t.Errorf("a second pass over the same ledger reported %+v (err %v), want nothing removed and the same 5 kept", p2, err)
	}
}

// --- criterion 1 and criterion 6 together: the caller's refusal is absolute -------------

func TestPrune_RemovesOnlyTheRowsTheCallerSaysAreSpentAndReportsTheRest(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Ten done rows; the caller (in production, the engine looking at the library) says
	// only the even-numbered files are gone. The retention cannot be met without deleting
	// a row that is holding a file out of the encoder, so it is NOT met - and the pass
	// says how many rows it refused rather than leaving the table quietly large.
	for i := 0; i < 10; i++ {
		seedRetentionRow(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", Done, int64(1000+i), 1_000, 0)
	}
	before := mustReclaimed(t, s)

	spent := func(path, _ string, _ Status) bool {
		i, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(path, "/lib/f"), ".mkv"))
		if err != nil {
			t.Errorf("prunable asked about an unexpected path %q", path)
			return false
		}
		return i%2 == 0
	}
	p, err := s.PruneTerminal(ctx, 1, 3, spent)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if p.Removed != 5 {
		t.Errorf("the prune removed %d rows, want the 5 the caller said were spent", p.Removed)
	}
	if p.Kept != 5 {
		t.Errorf("the prune reported %d kept rows, want the 5 it was refused", p.Kept)
	}
	for i := 0; i < 10; i++ {
		path := "/lib/f" + strconv.Itoa(i) + ".mkv"
		if got, want := rowExists(t, s, path), i%2 == 1; got != want {
			t.Errorf("%s exists=%v, want %v: the prune must take exactly the rows the caller approved", path, got, want)
		}
	}
	if got := mustReclaimed(t, s); got != before {
		t.Errorf("the lifetime total moved from %d to %d across a partial prune", before, got)
	}
}

// The anti-vacuity half of the fixture above: the same ledger with the same retention,
// answered "everything is spent", DOES come down to the bound. Without this, the test above
// could be passing because the prune is broken rather than because the refusal is honoured.
func TestPrune_TheCallersAnswerIsWhatDecidesAndTheSameLedgerPrunesFullyWhenNothingIsHeld(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for i := 0; i < 10; i++ {
		seedRetentionRow(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", Done, int64(1000+i), 1_000, 0)
	}
	p, err := s.PruneTerminal(ctx, 1, 3, everyRowSpent)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if p.Removed != 9 || p.Kept != 0 {
		t.Fatalf("prune reported %+v, want 9 removed and none kept", p)
	}
	if got := terminalCount(t, s); got != 1 {
		t.Fatalf("the ledger holds %d terminal rows, want the retained 1", got)
	}
}

func TestPrune_ACallerThatCannotAnswerLosesNoRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// nil is not "prune freely", and neither is a caller that refuses everything. This is
	// the one irreversible act in the package: the default has to be the safe one, or a
	// future caller wired up without a Prunable deletes an operator's audit history by
	// omission.
	for i := 0; i < 20; i++ {
		seedRetentionRow(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", Done, int64(1000+i), 1_000, 0)
	}
	for _, prunable := range []Prunable{nil, noRowSpent} {
		p, err := s.PruneTerminal(ctx, 1, 3, prunable)
		if err != nil {
			t.Fatalf("PruneTerminal: %v", err)
		}
		if p.Removed != 0 || p.ReclaimedCarried != 0 {
			t.Errorf("a prune with no usable answer reported %+v; it must remove nothing", p)
		}
		if got := terminalCount(t, s); got != 20 {
			t.Errorf("the ledger holds %d of 20 rows after a prune with no usable answer", got)
		}
		if p.Kept != 20 {
			t.Errorf("the pass reported %d kept rows, want the 20 it examined and refused", p.Kept)
		}
	}
}

// --- the carry-forward's own row must be there to carry into --------------------------

func TestPrune_ACarryWithNowhereToGoAbortsTheDeleteRatherThanLoseTheTotal(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	for i := 0; i < 6; i++ {
		seedRetentionRow(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", Done, int64(1000+i), 2_000, 0)
	}
	// The singleton ledger_totals row is seeded by migration v5 and nothing in this package
	// removes it, so this is a shape only a future schema change could produce. That is
	// exactly why the transaction has to enforce it rather than the migration remember it:
	// without the row, the UPDATE matches nothing, and a DELETE that committed beside it
	// would drop the removed rows' contribution out of the lifetime total for good - a
	// figure that shrinks at some restart weeks later.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ledger_totals`); err != nil {
		t.Fatalf("remove the carry row: %v", err)
	}
	p, err := s.PruneTerminal(ctx, 1, 3, everyRowSpent)
	if err == nil {
		t.Fatalf("the prune reported success (%+v) with nowhere to carry the removed rows' bytes", p)
	}
	if p.Removed != 0 {
		t.Errorf("the failed prune reported %d rows removed", p.Removed)
	}
	if got := terminalCount(t, s); got != 6 {
		t.Errorf("the ledger holds %d of 6 rows after a prune that could not carry; nothing may be deleted "+
			"until what it contributed is already somewhere else", got)
	}
}

// --- the batching, and what a failure part way through leaves behind -------------------

func TestPrune_WalksAGeneralLedgerInBatchesAndStillLandsExactlyOnTheRetention(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// More rows than one batch (pruneBatch), so the loop runs several times and the
	// carry-forward is exercised across transaction boundaries.
	const rows = pruneBatch*2 + 37
	var expected int64
	for i := 0; i < rows; i++ {
		seedRetentionRow(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", Done, int64(i), 11, 0)
		expected += 11
	}
	p, err := s.PruneTerminal(ctx, 10, 3, everyRowSpent)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if p.Removed != rows-10 {
		t.Fatalf("prune removed %d rows, want %d", p.Removed, rows-10)
	}
	if got := terminalCount(t, s); got != 10 {
		t.Fatalf("the ledger holds %d terminal rows after a retention of 10", got)
	}
	if got := mustReclaimed(t, s); got != expected {
		t.Fatalf("the lifetime total is %d after a multi-batch prune, want %d", got, expected)
	}
}

// seedRetentionRowKeyed seeds a terminal row under an explicit fingerprint, which
// seedRetentionRow cannot do: the table's key is (path, fingerprint), so a path can carry
// more than one row and only this can build that shape.
func seedRetentionRowKeyed(t *testing.T, s *SQLite, path, fingerprint string, st Status, at int64) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at)
		 VALUES (?, ?, ?, 0, NULL, ?)`,
		path, fingerprint, string(st), at); err != nil {
		t.Fatalf("seed %s/%s: %v", path, fingerprint, err)
	}
}

// TestPrune_StepsOverNeitherOfTwoRowsSharingAPathAndATransitionSecond. The walk pages
// through the terminal rows on a keyset cursor, and a cursor is only correct if its columns
// are unique. (path, fingerprint) is the primary key, so a file whose content changed
// leaves a second row under the same path - and the fixture puts a BATCH BOUNDARY exactly
// between two such rows, which is the only place a cursor of (updated_at, path) alone can
// lose one: it would resume at "path > dup" and step straight over the second.
func TestPrune_StepsOverNeitherOfTwoRowsSharingAPathAndATransitionSecond(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	const dup = "/lib/dup.mkv"
	for i := 0; i < pruneBatch-1; i++ {
		seedRetentionRowKeyed(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", "fp", Skipped, int64(1000+i))
	}
	at := int64(1000 + pruneBatch)
	seedRetentionRowKeyed(t, s, dup, "fpA", Skipped, at)
	seedRetentionRowKeyed(t, s, dup, "fpB", Skipped, at)
	seedRetentionRowKeyed(t, s, "/lib/last.mkv", "fp", Skipped, at+1)

	total := pruneBatch + 2 // the first batch ends on dup/fpA and the next must resume on fpB
	p, err := s.PruneTerminal(ctx, 1, 3, everyRowSpent)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if p.Removed != int64(total-1) {
		t.Errorf("the prune removed %d rows, want %d", p.Removed, total-1)
	}
	if got := terminalCount(t, s); got != 1 {
		t.Errorf("the ledger holds %d terminal rows after a retention of 1", got)
	}
	// The row that survives is the NEWEST. A walk that stepped over dup/fpB would keep
	// that one instead and delete /lib/last.mkv, which is not the oldest of anything.
	if !rowExists(t, s, "/lib/last.mkv") {
		t.Error("the newest row was pruned and an older one kept: the candidate walk lost a row " +
			"at the batch boundary")
	}
}

func TestPrune_AFailurePartWayThroughLeavesEveryRowItDidNotRemoveInPlace(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const rows = pruneBatch + 40
	for i := 0; i < rows; i++ {
		seedRetentionRow(t, s, "/lib/f"+strconv.Itoa(i)+".mkv", Done, int64(i), 7, 0)
	}
	before := mustReclaimed(t, s)

	// One batch commits for real, then the store is broken under the pass. That is the
	// shape of a mid-prune failure: some rows removed and durably accounted for, the rest
	// untouched, and no state anywhere that only existed in memory. (A retention of
	// rows-pruneBatch is exactly one batch's worth of excess, so the pass commits one
	// batch and stops.)
	first, err := s.PruneTerminal(ctx, rows-pruneBatch, 3, everyRowSpent)
	if err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if first.Removed != pruneBatch {
		t.Fatalf("the first batch removed %d rows, want %d", first.Removed, pruneBatch)
	}
	_ = s.db.Close()
	if _, err := s.PruneTerminal(ctx, 10, 3, everyRowSpent); err == nil {
		t.Fatal("a prune against a broken store reported success")
	}

	// Read the file back with a fresh handle: what the failed pass did not remove is
	// still there, and the total already accounts for what the committed batch took.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if got := terminalCount(t, s2); got != rows-pruneBatch {
		t.Errorf("the ledger holds %d terminal rows after a prune that failed part way, want the %d it did not remove",
			got, rows-pruneBatch)
	}
	for i := pruneBatch; i < rows; i++ {
		if !rowExists(t, s2, "/lib/f"+strconv.Itoa(i)+".mkv") {
			t.Fatalf("/lib/f%d.mkv was not removed by the committed batch and is gone anyway", i)
		}
	}
	if got := mustReclaimed(t, s2); got != before {
		t.Errorf("the lifetime total is %d after a prune that failed part way, want %d: the committed batch's "+
			"contribution must already be carried", got, before)
	}
}
