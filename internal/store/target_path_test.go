package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// The path a replacement WOULD have been written to, across the upgrade that adds it.
//
// It is the one fact [AC-2] asks for that this ledger did not already hold, so it is the one
// that had to be persisted - and a column added to a table whose rows decide whether a source
// file may be deleted is governed by `data-migration` M1 (expand before contract: the old
// shape still reads, nothing is dropped, nothing is backfilled) and M6 (a stored record names
// the schema version that wrote it, so a reader can tell what it is holding).

// seedPreviousBuildRow inserts, through a raw handle, the row a build one schema version back
// would have written for a dry run's decision: every fact that build could record, stamped
// with that build's version, and no column for the one it could not.
func seedPreviousBuildRow(t *testing.T, path, jobPath string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(
		`INSERT INTO jobs (path, fingerprint, status, fail_count, worker, updated_at,
			source_codec, source_bytes, decision_inputs, schema_version)
		 VALUES (?, ?, ?, 0, NULL, ?, ?, ?, ?, ?)`,
		jobPath, "9:9", string(WouldTranscode), 4000,
		"h264", 4_000_000, "encoder=cpu;target_codec=hevc", schemaVersion()-1); err != nil {
		t.Fatalf("seed the previous build's row: %v", err)
	}
}

// TestTargetPath_ALedgerWrittenBeforeTheColumnKeepsEveryRowAndReadsAsNotRecorded grades
// [AC-2]'s persistence half.
//
// The fixture is a real database at the version before this build's, carrying a row a dry run
// of that build wrote. Opening it with this build must EXPAND - add the column, keep every row,
// leave every recorded fact alone - and the row it did not write must read as NOT RECORDED for
// the new fact rather than being handed a path derived from whatever is configured now. Such a
// path would name a file nobody ever chose to write, on a row whose whole job is to be evidence.
func TestTargetPath_ALedgerWrittenBeforeTheColumnKeepsEveryRowAndReadsAsNotRecorded(t *testing.T) {
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

	// M1: the migration expanded. Every row survived, which is what the step's own row-count
	// declaration already refuses to commit without, asserted again from outside because a
	// terminal row is what says a source file was already handled.
	if after := rowCountOf(t, dbPath, "jobs"); after != before {
		t.Fatalf("the migration left %d job row(s), was %d", after, before)
	}

	rows, err := st.List(ctx, []Status{WouldTranscode}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Path != older {
		t.Fatalf("the previous build's decision is not readable after the migration: %+v", rows)
	}
	old := rows[0]
	if old.Outcome.TargetPath != "" {
		t.Errorf("a row written before the column existed reads its target path as %q. Nothing recorded "+
			"one, and a path on that row would name a file that build never chose to write",
			old.Outcome.TargetPath)
	}
	// Everything that build DID record is untouched: an expansion adds, it does not rewrite.
	if old.Outcome.SourceCodec != "h264" || old.Outcome.SourceBytes == nil || *old.Outcome.SourceBytes != 4_000_000 {
		t.Errorf("the migration disturbed what the previous build recorded: %+v", old.Outcome)
	}
	// M6: the record says which build wrote it, so "not recorded" here is a fact about that
	// build rather than a guess from an empty column.
	if v, ok := old.Stamp.Version(); !ok || v != prev {
		t.Errorf("the previous build's row is stamped %s, want version %d. Without the stamp a reader "+
			"infers the writer from which columns are empty, which is a coincidence and not a fact",
			old.Stamp, prev)
	}

	// A row THIS build writes carries the fact and says so.
	fresh := "/lib/decided-now.mkv"
	if ok, err := st.Claim(ctx, fresh, "8:8", "w0", 3, DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim(%s): ok=%v err=%v", fresh, ok, err)
	}
	if err := st.Finish(ctx, fresh, "8:8", WouldTranscode,
		&Outcome{SourceCodec: "h264", TargetPath: "/lib/decided-now.mp4"}, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, _, exists, err := st.Get(ctx, fresh, "8:8")
	if err != nil || !exists || got != WouldTranscode {
		t.Fatalf("Get: status=%q exists=%v err=%v", got, exists, err)
	}
	rows, err = st.List(ctx, []Status{WouldTranscode}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var written *Job
	for i := range rows {
		if rows[i].Path == fresh {
			written = &rows[i]
		}
	}
	if written == nil {
		t.Fatalf("the row this build just wrote is not in the ledger: %+v", rows)
	}
	if written.Outcome.TargetPath != "/lib/decided-now.mp4" {
		t.Errorf("the target path round-tripped as %q, want %q", written.Outcome.TargetPath, "/lib/decided-now.mp4")
	}
	if v, ok := written.Stamp.Version(); !ok || v != schemaVersion() {
		t.Errorf("the row this build wrote is stamped %s, want version %d", written.Stamp, schemaVersion())
	}

	// A claim BEGINS A NEW ATTEMPT and therefore clears the proof, this column included: a
	// target path left behind would describe a decision that is no longer the row's.
	if ok, err := st.Claim(ctx, fresh, "8:8", "w0", 3, DecisionInputs{}); err != nil || !ok {
		t.Fatalf("re-Claim(%s): ok=%v err=%v", fresh, ok, err)
	}
	inFlight, err := st.List(ctx, []Status{Probing}, 0)
	if err != nil {
		t.Fatalf("List(probing): %v", err)
	}
	if len(inFlight) != 1 || inFlight[0].Path != fresh {
		t.Fatalf("the re-claimed row is not in probing: %+v", inFlight)
	}
	if inFlight[0].Outcome.TargetPath != "" {
		t.Errorf("a re-claimed row still carries the previous decision's target path (%q)",
			inFlight[0].Outcome.TargetPath)
	}
}
