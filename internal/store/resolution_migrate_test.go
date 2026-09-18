package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// S0089 - the four resolution columns, against a ledger an earlier build wrote.
//
// `data-migration` M5 asks that a migration be proved on a COPY OF REAL DATA taken by a
// route that is safe on a live database. This repository has no such route - there is no
// production ledger a test can VACUUM INTO - so the substitution the spec states is used
// instead: a real database built at the SHIPPED schema version before this one, through the
// production applyMigration, carrying a row the build that shipped it wrote. That is the
// shape an upgrade actually meets, and it is the closest thing to real data that exists
// here; the substitution is recorded rather than left as a silent gap.

// [AC-18] WHEN a database written at the schema version before this change is opened, the
// four columns are added NULLABLE WITH NO DEFAULT, every pre-existing row reads "not
// recorded", and the step asserts that the jobs row count is unchanged - failing the
// migration if it is not (data-migration M4).
//
// The NULL is the criterion rather than a detail. 0 is a legal pixel dimension for nothing,
// so a DEFAULT of 0 would put a fabricated resolution on every row already in the field at
// once - in the one table whose entire job is to be evidence about files that have since
// been deleted. And "not recorded" has to stay distinguishable from a recorded value, or a
// reader cannot tell a row this build wrote from one it never saw.
//
// MUTATION: give any of the four columns a DEFAULT 0 and the not-recorded assertions red;
// drop the `rows: noRowChange` declaration and the count assertion below reds, because a
// step carrying no declaration is one somebody forgot rather than one that meant zero.
func TestResolution_ALedgerAnEarlierBuildWroteMigratesAndReadsAsNotRecorded(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	prev := schemaVersion() - 1
	atShippedVersion(t, dbPath, prev)
	older := "/lib/decided-by-the-previous-build.mkv"
	seedPreviousBuildRow(t, dbPath, older)
	before := rowCountOf(t, dbPath, "jobs")

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open a ledger the previous build wrote: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// M1: the migration EXPANDED. Every row survived, which is also what the step's own
	// row-count assertion checked inside the transaction that made the change.
	if after := rowCountOf(t, dbPath, "jobs"); after != before {
		t.Fatalf("the migration left %d job row(s), was %d", after, before)
	}
	rows, err := st.List(ctx, []Status{WouldTranscode}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != older {
		t.Fatalf("the previous build's row is not readable after the migration: %+v", rows)
	}
	old := rows[0].Outcome
	if old.SourceWidth != nil || old.SourceHeight != nil ||
		old.OutputWidth != nil || old.OutputHeight != nil {
		t.Errorf("a row written before the columns existed reads a resolution (%v x %v -> %v x %v): "+
			"nothing measured one, and a number here is a claim about a job this build never saw",
			old.SourceWidth, old.SourceHeight, old.OutputWidth, old.OutputHeight)
	}
	// An expansion adds; it does not rewrite what that build recorded.
	if old.SourceCodec != "h264" || old.SourceBytes == nil || *old.SourceBytes != 4_000_000 {
		t.Errorf("the migration disturbed what the previous build recorded: %+v", old)
	}

	// The column shape itself, asked of SQLite rather than of the projection: NULLABLE with
	// NO DEFAULT, which is what makes the absence above a property of the schema and not of
	// this one fixture.
	for _, col := range []string{"source_width", "source_height", "output_width", "output_height"} {
		notNull, dflt, ok := columnShape(t, dbPath, col)
		if !ok {
			t.Errorf("the migration did not add the %s column at all", col)
			continue
		}
		if notNull {
			t.Errorf("%s is NOT NULL, so 'not recorded' has nowhere to live", col)
		}
		if dflt.Valid {
			t.Errorf("%s carries DEFAULT %q, which backfills every pre-existing row with a "+
				"resolution nobody measured", col, dflt.String)
		}
	}

	// And a row THIS build writes carries the fact, so the absence above is a fact about
	// that build rather than a column nothing ever fills.
	fresh := "/lib/decided-now.mkv"
	if ok, err := st.Claim(ctx, fresh, "8:8", "w0", 3, DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", fresh, ok, err)
	}
	w, h := 1920, 1080
	if err := st.Finish(ctx, fresh, "8:8", Done,
		&Outcome{Encoder: "cpu", SourceWidth: &w, SourceHeight: &h, OutputWidth: &w, OutputHeight: &h}, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	done, err := st.List(ctx, []Status{Done}, 0)
	if err != nil {
		t.Fatalf("List(done): %v", err)
	}
	var written *Job
	for i := range done {
		if done[i].Path == fresh {
			written = &done[i]
		}
	}
	if written == nil {
		t.Fatalf("the row this build just wrote is not in the ledger: %+v", done)
	}
	o := written.Outcome
	if o.SourceWidth == nil || *o.SourceWidth != 1920 || o.SourceHeight == nil || *o.SourceHeight != 1080 ||
		o.OutputWidth == nil || *o.OutputWidth != 1920 || o.OutputHeight == nil || *o.OutputHeight != 1080 {
		t.Errorf("a row this build wrote does not round-trip its resolution: %+v", o)
	}
}

// [AC-19] IF the migration cannot complete, the store refuses to open and the caller exits
// non-zero rather than running against a half-migrated database.
//
// The failure is FORCED rather than waited for: the column the step adds is created ahead
// of it on a database still stamped at the previous version, so the ALTER dies on a
// duplicate column exactly as a half-applied step would. What has to hold is that Open
// returns an error, that the error names the step, and that the version stamp does NOT
// move - because a database claiming a version whose columns it does not have is the one
// way a versioned schema can still lie, and the engine would then be writing the proof of
// its swaps into columns that may or may not exist.
//
// MUTATION: commit the step's DDL outside the transaction that stamps the version, or stamp
// the version before the DDL runs, and the user_version assertion reds.
func TestResolution_AMigrationThatCannotCompleteRefusesToOpenAndMovesNothing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "jobs.db")
	prev := schemaVersion() - 1
	atShippedVersion(t, dbPath, prev)
	seedPreviousBuildRow(t, dbPath, "/lib/decided-by-the-previous-build.mkv")
	before := rowCountOf(t, dbPath, "jobs")

	// Break the step in the only way that is faithful to a half-applied one: the shape it
	// means to add is already partly there, while the stamp still says it never ran.
	// It names the column the NEWEST step adds, for the reason every wind-back fixture in
	// this package tracks the end of the migrations slice: a step appended after this line
	// moves it, and pre-adding a column an EARLIER step already created would break the
	// fixture rather than the migration under test.
	execRaw(t, dbPath, `ALTER TABLE jobs ADD COLUMN downscaled INTEGER`)

	st, err := Open(dbPath)
	if err == nil {
		_ = st.Close()
		t.Fatal("Open accepted a database whose migration could not complete")
	}
	if !strings.Contains(err.Error(), "migration") {
		t.Errorf("the refusal does not say a migration failed: %v", err)
	}
	if got := rawVersion(t, dbPath); got != prev {
		t.Errorf("the failed migration left the database stamped %d, was %d - it now claims a "+
			"version whose columns it does not have", got, prev)
	}
	if after := rowCountOf(t, dbPath, "jobs"); after != before {
		t.Errorf("the failed migration left %d row(s), was %d", after, before)
	}
	// The other half of "rather than run against a half-migrated database": the columns the
	// step would have added are not there, so nothing wrote into a shape that half exists.
	if columnExists(t, dbPath, "downscale_scaler") {
		t.Error("the failed step left downscale_scaler behind: the transaction did not roll back")
	}
}

// columnShape reports one jobs column's NOT NULL flag and its DEFAULT, and whether the
// column is there at all. It reads SQLite's own table_info rather than the projection, so
// what is graded is the schema and not this build's reader of it.
func columnShape(t *testing.T, path, name string) (notNull bool, dflt sql.NullString, found bool) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT name, "notnull", dflt_value FROM pragma_table_info('jobs')`)
	if err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var col string
		var nn int
		var d sql.NullString
		if err := rows.Scan(&col, &nn, &d); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if col == name {
			return nn != 0, d, true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info rows: %v", err)
	}
	return false, sql.NullString{}, false
}

// columnExists reports whether the jobs table carries a column, through a raw handle.
func columnExists(t *testing.T, path, name string) bool {
	t.Helper()
	_, _, found := columnShape(t, path, name)
	return found
}

// execRaw runs one statement against a database this build may not share a schema with.
func execRaw(t *testing.T, path, stmt string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(stmt); err != nil {
		t.Fatalf("%s: %v", stmt, err)
	}
}

// rawVersion reads the schema stamp straight off the file.
func rawVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}
