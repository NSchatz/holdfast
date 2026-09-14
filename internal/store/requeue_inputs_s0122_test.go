package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

// S0122 - what the read-set rule does to a ledger an EARLIER BUILD wrote.
//
// The rule changed what a recorded value MEANS (it is resolved for the row's own path now,
// not for its library root alone) and changed nothing about how it is stored, so there is
// no new shape to migrate to. What still has to hold is the reading of the two ledgers an
// upgrade actually meets: one whose rows record nothing, and one whose schema predates the
// column those records live in.

// stepsSinceDecisionInputs undoes, newest first, every migration applied after the one that
// added decision_inputs. Each entry is the exact reverse of its step's SQL, so what it
// leaves behind is a real database of the shape that build wrote rather than a current
// database wearing an older number - which would be migrated by re-running a step it has
// already run, a duplicate-column error and not an older ledger.
var stepsSinceDecisionInputs = []struct {
	version int
	undo    []string
}{
	{14, []string{`ALTER TABLE jobs DROP COLUMN profile`}},
	{13, []string{
		`ALTER TABLE jobs DROP COLUMN schema_version`,
		`ALTER TABLE retained_originals DROP COLUMN schema_version`,
		`ALTER TABLE swap_incidents DROP COLUMN schema_version`,
	}},
	{12, []string{
		`ALTER TABLE jobs DROP COLUMN library_root`,
		`ALTER TABLE jobs DROP COLUMN profile_digest`,
	}},
	{11, []string{`ALTER TABLE jobs DROP COLUMN vmaf_stream`}},
	{10, []string{
		`DROP INDEX IF EXISTS idx_jobs_status_inputs`,
		`ALTER TABLE jobs DROP COLUMN decision_inputs`,
	}},
}

// windBackBeforeDecisionInputs turns a current ledger into one written before the
// decision_inputs column existed, and returns the version it now carries.
func windBackBeforeDecisionInputs(t *testing.T, path string) int {
	t.Helper()
	if want := stepsSinceDecisionInputs[0].version; schemaVersion() != want {
		t.Fatalf("this build is at schema %d and the wind-back knows how to undo %d. A migration was "+
			"added without extending stepsSinceDecisionInputs, so the fixture below is not the ledger "+
			"the previous build wrote", schemaVersion(), want)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	for _, step := range stepsSinceDecisionInputs {
		for _, stmt := range step.undo {
			if _, err := db.Exec(stmt); err != nil {
				t.Fatalf("undo v%d (%s): %v", step.version, stmt, err)
			}
		}
	}
	prev := stepsSinceDecisionInputs[len(stepsSinceDecisionInputs)-1].version - 1
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, prev)); err != nil {
		t.Fatalf("stamp user_version = %d: %v", prev, err)
	}
	return prev
}

// terminalRowCount counts the done and skipped rows in a database this build may not share
// a schema with, through a raw handle - so it can be asked on either side of a migration.
func terminalRowCount(t *testing.T, path string) int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	var n int64
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE status IN (?, ?)`,
		string(Done), string(Skipped)).Scan(&n); err != nil {
		t.Fatalf("count terminal rows: %v", err)
	}
	return n
}

// TestRequeueInputs_ALedgerAnEarlierBuildWroteIsReadReportedAndPreserved grades [AC-6]: a
// ledger with no recorded inputs, on a schema predating the column that holds them, is read
// and reported WITHOUT being migrated under `validate`, keeps every terminal row through the
// migration `run` and `serve` do apply, and re-opens a row that recorded nothing exactly
// once.
func TestRequeueInputs_ALedgerAnEarlierBuildWroteIsReadReportedAndPreserved(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)
	prev := windBackBeforeDecisionInputs(t, dbPath)
	rowsBefore := terminalRowCount(t, dbPath)
	if rowsBefore != 2 {
		t.Fatalf("the fixture holds %d terminal row(s), want 2", rowsBefore)
	}
	digestBefore := fileDigest(t, dbPath)
	ctx := context.Background()

	// 1. READ AND REPORTED, NOT MIGRATED. This is the read `validate` goes through, and the
	// population it matters most for: a schema with no column for the inputs is one in which
	// no row can have recorded any, so the count needs no migration to be true - and taking
	// one would stamp a version the daemon still running against this file then refuses.
	got, err := SurveyLedgerDecisionInputs(ctx, dbPath, everyPath(sameConfig))
	if err != nil {
		t.Fatalf("SurveyLedgerDecisionInputs over a ledger written before the column existed: %v", err)
	}
	if want := (DecisionInputsSurvey{NotRecorded: 2}); got != want {
		t.Errorf("surveyed %+v, want %+v - every row in a ledger with no decision_inputs column "+
			"records nothing, and the first scan after the upgrade re-opens all of them", got, want)
	}
	if v := rawUserVersion(t, dbPath); v != prev {
		t.Errorf("the survey migrated the ledger it was describing: user_version is now %d, was %d", v, prev)
	}
	if fileDigest(t, dbPath) != digestBefore {
		t.Error("the survey changed the ledger it was describing")
	}

	// 2. EVERY TERMINAL ROW SURVIVES THE MIGRATION `run` AND `serve` DO APPLY. The row-count
	// assertion is inside the migration itself (a step that moves a table's count by anything
	// it did not declare is rolled back and the store refuses to open), so the open succeeding
	// IS that assertion passing - and it is checked again from outside, because a terminal row
	// is what says a source file was already handled.
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open a ledger the previous build wrote: %v", err)
	}
	defer func() { _ = st.Close() }()
	if rowsAfter := terminalRowCount(t, dbPath); rowsAfter != rowsBefore {
		t.Errorf("the migration left %d terminal row(s), was %d", rowsAfter, rowsBefore)
	}
	report := st.MigrationReport()
	if len(report) != len(stepsSinceDecisionInputs) {
		t.Errorf("the open applied %d step(s), want %d - the fixture was not the shape this case "+
			"says it is", len(report), len(stepsSinceDecisionInputs))
	}
	for _, step := range report {
		for _, tbl := range step.Tables {
			if tbl.Changed() != 0 {
				t.Errorf("step v%d (%s) moved %q from %d row(s) to %d - no step in this range "+
					"declares a row change", step.Version, step.Name, tbl.Table, tbl.Before, tbl.After)
			}
		}
	}

	// 3. RE-OPENED EXACTLY ONCE. A row that recorded nothing cannot be re-derived, so it is
	// offered to the guards once; the decision it then reaches records what it read, and the
	// scan after that leaves it alone. Anything else is a row re-opened on every scan for
	// ever, which on a library is a re-encode of everything.
	const path = "/lib/a.mkv"
	ok, err := st.Claim(ctx, path, "fp", "w0", 3, sameConfig)
	if err != nil || !ok {
		t.Fatalf("the first claim of a row recording nothing: ok=%v err=%v - it must be re-opened once", ok, err)
	}
	if err := st.Finish(ctx, path, "fp", Skipped,
		&Outcome{Reason: "already-at-target-codec", DecisionInputs: sameConfig}, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if ok, err := st.Claim(ctx, path, "fp", "w0", 3, sameConfig); err != nil || ok {
		t.Errorf("the row was re-opened a SECOND time (ok=%v err=%v) after recording what its "+
			"decision read", ok, err)
	}
	// And the report now reads it as what it is: one row recorded and matching, one still
	// recording nothing and still owed its single re-open.
	after, err := st.SurveyDecisionInputs(ctx, everyPath(sameConfig))
	if err != nil {
		t.Fatalf("SurveyDecisionInputs: %v", err)
	}
	if want := (DecisionInputsSurvey{NotRecorded: 1, Matching: 1}); after != want {
		t.Errorf("surveyed %+v after the single re-open, want %+v", after, want)
	}
}

// TestRequeueInputs_TheSurveyAsksAboutEachRowsOwnPath grades the mechanism [AC-5] rests on
// at the layer that owns it: two rows carrying the IDENTICAL recorded text are classified
// separately, because the configuration in force resolves per path.
//
// A survey that grouped by the stored value could not tell them apart at all - it would
// report one answer for both - which is exactly what made an encode profile invisible to the
// counts an operator reads.
func TestRequeueInputs_TheSurveyAsksAboutEachRowsOwnPath(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	terminalRow(t, s, "/lib/selected.mkv", "10:100", Skipped, "already-at-target-codec", sameConfig)
	terminalRow(t, s, "/lib/inherited.mkv", "20:200", Skipped, "already-at-target-codec", sameConfig)

	// The same record, resolved differently for the two paths: one is selected by something
	// that moved, the other is not.
	perPath := func(path string) (DecisionInputs, bool) {
		if path == "/lib/selected.mkv" {
			return movedConfig, true
		}
		return sameConfig, true
	}
	got, err := s.SurveyDecisionInputs(ctx, perPath)
	if err != nil {
		t.Fatalf("SurveyDecisionInputs: %v", err)
	}
	if want := (DecisionInputsSurvey{Moved: 1, Matching: 1}); got != want {
		t.Errorf("surveyed %+v, want %+v - the two rows record the same text and resolve to "+
			"different values, so a survey that reads the text alone answers for neither", got, want)
	}
}

// TestRequeueInputs_ARowUnderNoConfiguredRootIsCountedAndNamed grades the store half of
// [AC-7]: a terminal row whose path resolves to no configured library root is classified
// against the top-level resolution, counted, and NAMED - never dropped, never an error.
//
// The count alone would be a number about files an operator cannot find. The scan walks the
// configured roots, so it never enumerates those paths and never re-opens them whatever they
// record, and the name is what lets someone see which tree it was - usually a root that was
// renamed or removed.
func TestRequeueInputs_ARowUnderNoConfiguredRootIsCountedAndNamed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	terminalRow(t, s, "/gone/a.mkv", "10:100", Skipped, "already-at-target-codec", sameConfig)
	terminalRow(t, s, "/gone/b.mkv", "20:200", Skipped, "already-at-target-codec", sameConfig)
	terminalRow(t, s, "/lib/here.mkv", "30:300", Skipped, "already-at-target-codec", sameConfig)

	rooted := func(path string) (DecisionInputs, bool) {
		return sameConfig, path == "/lib/here.mkv"
	}
	got, err := s.SurveyDecisionInputs(ctx, rooted)
	if err != nil {
		t.Fatalf("SurveyDecisionInputs: %v", err)
	}
	if want := (DecisionInputsSurvey{Matching: 3, Unrooted: 2, UnrootedExample: "/gone/a.mkv"}); got != want {
		t.Errorf("surveyed %+v, want %+v - an unrooted row is still classified (it is an annotation "+
			"and not a fourth bucket), counted, and named by the lexically first of its paths", got, want)
	}
}

// TestRequeueInputs_AnUnrootedRowIsNamedWhateverTheRowRecorded grades [AC-7] over the ledger
// shape that cannot hold a recorded input at all: a schema predating the column. Every row in
// such a file records nothing, so an annotation taken only for rows that recorded something
// would be silent across a whole ledger - and that ledger is the one an upgrade meets, which
// is when the figures are read.
//
// The row's classification is unchanged by being named: both rows are still NotRecorded and
// still owed their single re-open. What the annotation adds is that the scan walks roots, so
// one of those two re-opens will never happen.
func TestRequeueInputs_AnUnrootedRowIsNamedWhateverTheRowRecorded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	seedTwoTerminalRows(t, dbPath)
	windBackBeforeDecisionInputs(t, dbPath)

	rooted := func(path string) (DecisionInputs, bool) {
		return sameConfig, path == "/lib/b.mkv"
	}
	got, err := SurveyLedgerDecisionInputs(context.Background(), dbPath, rooted)
	if err != nil {
		t.Fatalf("SurveyLedgerDecisionInputs over a ledger written before the column existed: %v", err)
	}
	if want := (DecisionInputsSurvey{NotRecorded: 2, Unrooted: 1, UnrootedExample: "/lib/a.mkv"}); got != want {
		t.Errorf("surveyed %+v, want %+v - a row recording nothing is a row whose path still has "+
			"to be resolved, or the condition is reported for no row of this ledger at all", got, want)
	}
	// Anti-vacuity: the same file, resolved by a configuration that roots every path, reports
	// the condition for none of them. A count that could not go to zero would grade nothing.
	if got, err := SurveyLedgerDecisionInputs(context.Background(), dbPath, everyPath(sameConfig)); err != nil {
		t.Fatalf("SurveyLedgerDecisionInputs: %v", err)
	} else if want := (DecisionInputsSurvey{NotRecorded: 2}); got != want {
		t.Errorf("surveyed %+v, want %+v - every path is under a configured root here", got, want)
	}
}
