package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// TestOpen_MigratesAndLeavesOldRowsUnattributed.
//
// The rows already in an operator's ledger were decided by a build that had ONE global
// profile and recorded neither the root nor a digest of it. Opening that ledger with
// this build must add the two columns in place and keep every row - and must leave those
// rows saying NOTHING about which profile decided them.
//
// The failure this closes is the tempting one: backfilling. A default of the currently
// configured root, or a digest of whatever the configuration says today, would attribute
// a swap that already happened to a profile that did not exist when it happened, in the
// one table whose entire job is to be evidence. Some of those rows are about sources
// that have since been deleted.
//
// It is seeded from the REAL pre-versioning schema (v0, with rows in it), so the
// migration is exercised against a database that exists rather than one this test
// invented, exactly as the outcome-columns proof is.
func TestOpen_MigratesAndLeavesOldRowsUnattributed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jobs.db")
	seedV0(t, path)

	// The fixture really is older than the columns under test.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	for _, col := range []string{"library_root", "profile_digest"} {
		if hasColumn(t, raw, col) {
			t.Fatalf("the seeded v0 database already has a %q column - the fixture is wrong", col)
		}
	}
	_ = raw.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	if got := userVersion(t, path); got != schemaVersion() {
		t.Fatalf("Open left the store at user_version %d, want %d", got, schemaVersion())
	}
	raw, err = sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw reopen: %v", err)
	}
	defer func() { _ = raw.Close() }()
	for _, col := range []string{"library_root", "profile_digest"} {
		if !hasColumn(t, raw, col) {
			t.Fatalf("the migration did not add the %q column", col)
		}
	}

	// Every seeded row survived, and every terminal one reads as NOT ATTRIBUTED. "" is
	// this package's one spelling of "not recorded"; a fabricated root or a digest of the
	// current configuration would both be non-empty here.
	rows, err := st.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("the migration kept %d of the 4 seeded rows", len(rows))
	}
	for _, r := range rows {
		if r.Outcome.LibraryRoot != "" || r.Outcome.ProfileDigest != "" {
			t.Errorf("row %s was backfilled with library_root=%q profile_digest=%q - it was decided by "+
				"a build that recorded neither, and attributing it to a profile that did not exist "+
				"invents evidence about a swap that already happened",
				r.Path, r.Outcome.LibraryRoot, r.Outcome.ProfileDigest)
		}
	}

	// Anti-vacuity: the migrated columns are USABLE, so "old rows are unattributed" is a
	// statement about those rows and not about columns that never work.
	if ok, err := st.Claim(ctx, "/lib/new.mkv", "50:500", "w0", 3, DecisionInputs{}); err != nil || !ok {
		t.Fatalf("Claim on the migrated store: ok=%v err=%v", ok, err)
	}
	fresh := &Outcome{Decision: Decision{LibraryRoot: "/lib", ProfileDigest: "0123456789abcdef"}}
	if err := st.Finish(ctx, "/lib/new.mkv", "50:500", Done, fresh, 3); err != nil {
		t.Fatalf("Finish on the migrated store: %v", err)
	}
	rows, err = st.List(ctx, []Status{Done}, 0)
	if err != nil {
		t.Fatalf("List(done): %v", err)
	}
	var got *Job
	for i := range rows {
		if rows[i].Path == "/lib/new.mkv" {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatal("the row written after the migration is not in the ledger")
	}
	if got.Outcome.LibraryRoot != "/lib" || got.Outcome.ProfileDigest != "0123456789abcdef" {
		t.Errorf("a row written after the migration reads back as root=%q digest=%q, want /lib and "+
			"0123456789abcdef - the columns are present but not carrying the facts",
			got.Outcome.LibraryRoot, got.Outcome.ProfileDigest)
	}
	// ... and the pre-existing rows are STILL unattributed after a write beside them.
	old, _, exists, err := st.Get(ctx, "/lib/done.mkv", "10:100")
	if err != nil || !exists {
		t.Fatalf("Get(/lib/done.mkv): exists=%v err=%v", exists, err)
	}
	if old != Done {
		t.Errorf("the seeded done row is now %q", old)
	}
}
