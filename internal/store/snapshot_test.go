package store

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// OpenSnapshot - the door a reader goes through when the DIRECTORY may not change
// either (S0092).
//
// OpenReadOnly already cannot write the database. What it cannot promise is that the
// directory around it is untouched: SQLite opens a shared-memory index and a
// write-ahead log beside any WAL database it reads, and those two files are created by
// the act of reading. For `holdfast analyze`, whose whole contract is that the state
// directory is byte-for-byte what it was, that is the difference between a read and a
// mutation. These tests pin both halves: nothing is created, and the cost of getting
// there (a live writer's uncheckpointed content is not seen) is the documented one.

func stateDirListing(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestOpenSnapshot_ReadsTheRowsAndCreatesNothingAtAll(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)

	before := stateDirListing(t, dir)
	digest := fileDigest(t, dbPath)
	if len(before) != 1 || before[0] != "jobs.db" {
		t.Fatalf("the fixture is not the state this test is about: %v", before)
	}

	st, err := OpenSnapshot(dbPath)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	var rows int
	if err := st.EachTerminal(context.Background(), func(Job) error { rows++; return nil }); err != nil {
		t.Fatalf("EachTerminal: %v", err)
	}
	if rows != 2 {
		t.Fatalf("read %d row(s), want the 2 that are in the ledger", rows)
	}
	// The two reads `analyze` actually makes, over a handle that must survive them.
	if _, err := st.ParkedIncidents(context.Background()); err != nil {
		t.Fatalf("ParkedIncidents: %v", err)
	}
	if _, err := st.ExcludedReplacementPaths(context.Background()); err != nil {
		t.Fatalf("ExcludedReplacementPaths: %v", err)
	}
	// WHILE THE HANDLE IS STILL OPEN: a sidecar created and then cleaned up on Close
	// would still have been a file in the operator's state directory.
	if during := stateDirListing(t, dir); len(during) != 1 {
		t.Fatalf("reading the ledger created %v beside it", during)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if after := stateDirListing(t, dir); len(after) != 1 || after[0] != "jobs.db" {
		t.Fatalf("the state directory holds %v after the read, want only jobs.db", after)
	}
	if got := fileDigest(t, dbPath); got != digest {
		t.Fatal("the ledger's own bytes moved under a read that promised not to touch them")
	}
}

func TestOpenSnapshot_RefusesEveryWrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)

	st, err := OpenSnapshot(dbPath)
	if err != nil {
		t.Fatalf("OpenSnapshot: %v", err)
	}
	defer func() { _ = st.Close() }()
	if _, err := st.Claim(context.Background(), "/lib/c.mkv", "fp", "w0", 3, sameConfig); err == nil {
		t.Fatal("a snapshot handle claimed a job: the read-only property is not physical")
	}
	if _, err := st.RecoverStale(context.Background()); err == nil {
		t.Fatal("a snapshot handle rewrote active rows")
	}
}

func TestOpenSnapshot_CreatesNothingWhenThereIsNothingToRead(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	if _, err := OpenSnapshot(dbPath); err == nil {
		t.Fatal("OpenSnapshot invented an empty ledger where there was none")
	}
	if names := stateDirListing(t, dir); len(names) != 0 {
		t.Fatalf("a read that found nothing to read left %v behind", names)
	}
}

func TestOpenSnapshot_RefusesALedgerAnEarlierBuildWroteRatherThanMigratingIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)
	prev := windBackOneSchemaVersion(t, dbPath)

	_, err := OpenSnapshot(dbPath)
	if err == nil {
		t.Fatal("OpenSnapshot accepted a schema that is not this build's")
	}
	if !strings.Contains(err.Error(), "schema") && !strings.Contains(err.Error(), "version") {
		t.Fatalf("the refusal does not say what is wrong with the file: %v", err)
	}
	if got := rawUserVersion(t, dbPath); got != prev {
		t.Fatalf("the refused open migrated the file anyway: user_version = %d, want %d", got, prev)
	}
}

// TestOpenSnapshot_SeesTheCheckpointedStateWhileAWriterHoldsTheLedger pins the COST of
// creating nothing, which is the half a future reader is most likely to be surprised
// by: content still in a live daemon's write-ahead log is not in this answer. It is
// stated in OpenSnapshot's own documentation, and it is why `export` and `validate`
// keep the reader that does open the WAL.
func TestOpenSnapshot_SeesTheCheckpointedStateWhileAWriterHoldsTheLedger(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)

	live, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = live.Close() }()
	if _, err := live.Claim(context.Background(), "/lib/c.mkv", "fp", "w0", 3, sameConfig); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	st, err := OpenSnapshot(dbPath)
	if err != nil {
		t.Fatalf("OpenSnapshot beside a live writer: %v", err)
	}
	defer func() { _ = st.Close() }()
	summary, err := st.Summary(context.Background())
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	total := 0
	for _, n := range summary {
		total += n
	}
	if total < 2 {
		t.Fatalf("the snapshot lost the checkpointed rows as well: %d", total)
	}
}
