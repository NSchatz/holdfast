package main

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
	_ "modernc.org/sqlite"
)

// regress_0048_F1 (S0048-holdfast-ledger-5, impl-gate ordinal 1).
//
// README.md, in the section this spec added, states of `holdfast export`:
//
//	"It is a local, operator-run read of your own store: no listener, no port, no
//	 network surface, and it never writes to the store it reads."
//
// cmd/holdfast/export.go's own header repeats it: "it never writes to the store it
// reads."
//
// cmdExport opens the store with store.Open, which runs migrate() unconditionally. So an
// export against a ledger written by an EARLIER holdfast silently upgrades that ledger's
// schema in place. The consequence is not cosmetic: store.migrate REFUSES a database whose
// user_version is ahead of the build, so the older holdfast that wrote the file can no
// longer open it after a newer holdfast merely EXPORTED from it.
//
// The fixture is a v4 database - the schema this repository shipped immediately before
// this branch appended v5 - carrying real rows.
func TestRegress0048F1_ExportMigratesTheStoreItClaimsOnlyToRead(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "state", "jobs.db")

	// A ledger with history, at the current schema, then wound back to the v4 shape:
	// no ledger_totals, no idx_jobs_status_updated, user_version 4. That is byte-for-byte
	// what a database written by the previous holdfast looks like.
	st, err := store.Open(db)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	ctx := context.Background()
	for _, p := range []string{"/lib/a.mkv", "/lib/b.mkv"} {
		ok, err := st.Claim(ctx, p, "fp", "w0", 3)
		if err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", p, ok, err)
		}
		if err := st.Finish(ctx, p, "fp", store.Skipped, &store.Outcome{Reason: "already-at-target-codec"}); err != nil {
			t.Fatalf("seed finish %s: %v", p, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	windBackToV4(t, db)
	if got := userVersion(t, db); got != 4 {
		t.Fatalf("fixture is at user_version %d, want 4", got)
	}

	// The config `export` will read, pointing at that store.
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + dir + "\nstate_dir: " + filepath.Join(dir, "state") + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdExport([]string{"--config", cfgPath}, &stdout, &stderr); code != 0 {
		t.Fatalf("export exited %d: %s", code, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Fatalf("the export wrote nothing; the fixture is wrong, not the claim")
	}

	if got := userVersion(t, db); got != 4 {
		t.Errorf("`holdfast export` left the store at user_version %d, was 4 before the export.\n"+
			"README.md and cmd/holdfast/export.go both state it \"never writes to the store it reads\".\n"+
			"store.Open runs migrate() unconditionally, so exporting from a ledger written by an earlier\n"+
			"holdfast upgrades that ledger in place - and store.migrate then REFUSES the file to the very\n"+
			"binary that wrote it, because its user_version is now ahead of that build.", got)
	}
}

// windBackToV4 removes exactly what migration v5 added and restores the version stamp.
func windBackToV4(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS ledger_totals`,
		`DROP INDEX IF EXISTS idx_jobs_status_updated`,
		`PRAGMA user_version = 4`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}
