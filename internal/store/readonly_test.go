package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// OpenReadOnly - the door a READER goes through (LEDGER-5).
//
// Open migrates unconditionally, which is right for the daemon and wrong for `holdfast
// export`: a read that upgrades the ledger's schema in place leaves that file unopenable by
// the holdfast which wrote it, because store.migrate refuses a user_version ahead of the
// build. These tests pin the two properties that stop it - the handle physically cannot
// write, and a version that is not this build's is refused in BOTH directions rather than
// repaired.

func seedTwoTerminalRows(t *testing.T, dbPath string) {
	t.Helper()
	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	for _, p := range []string{"/lib/a.mkv", "/lib/b.mkv"} {
		ok, err := st.Claim(ctx, p, "fp", "w0", 3, sameConfig)
		if err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", p, ok, err)
		}
		if err := st.Finish(ctx, p, "fp", Skipped, &Outcome{Reason: "already-at-target-codec"}, 3); err != nil {
			t.Fatalf("seed finish %s: %v", p, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func fileDigest(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

func rawUserVersion(t *testing.T, path string) int {
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

// windBackOneSchemaVersion removes exactly what the newest migration added and restores the
// previous version stamp, producing a real database of the shape the PREVIOUS holdfast
// wrote - not merely a current database wearing an older number.
func windBackOneSchemaVersion(t *testing.T, path string) int {
	t.Helper()
	prev := schemaVersion() - 1
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("raw open %s: %v", path, err)
	}
	defer func() { _ = db.Close() }()
	// Exactly what the NEWEST migration added, undone. That is v10 (what a terminal
	// decision read from the configuration) and not the step before it: this helper has
	// to track the END of the migrations slice, because the whole point of it is to
	// produce the database the PREVIOUS build wrote, and a wind-back that undid a step
	// which is no longer the last one would leave a database Open migrates by re-running
	// a step it has already run - which is a duplicate-column error, not an older ledger.
	//
	// The index goes first: SQLite refuses to drop a column an index refers to.
	for _, stmt := range []string{
		`DROP INDEX IF EXISTS idx_jobs_status_inputs`,
		`ALTER TABLE jobs DROP COLUMN decision_inputs`,
		fmt.Sprintf(`PRAGMA user_version = %d`, prev),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return prev
}

func TestOpenReadOnly_ReadsEveryRowAndRefusesEveryWrite(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)

	st, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	var paths []string
	if err := st.EachTerminal(ctx, func(j Job) error {
		paths = append(paths, j.Path)
		return nil
	}); err != nil {
		t.Fatalf("EachTerminal through a read-only handle: %v", err)
	}
	if len(paths) != 2 {
		t.Fatalf("the read-only handle saw %d terminal rows, want 2: %v", len(paths), paths)
	}

	// The refusal is the DRIVER's, not a rule in Go above it: mode=ro means no future
	// caller can quietly reintroduce a write on this path.
	if err := st.Finish(ctx, "/lib/a.mkv", "fp", Done, &Outcome{Encoder: "cpu"}, 3); err == nil {
		t.Error("a read-only handle accepted a write; `never writes to the store it reads` must be enforced, not promised")
	}
	if _, err := st.PruneTerminal(ctx, 1, 3, everyRowSpent); err == nil {
		t.Error("a read-only handle accepted a prune - the one irreversible act in this package")
	}
}

func TestOpenReadOnly_LeavesTheDatabaseByteForByteAsItFoundIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)

	before, beforeVersion := fileDigest(t, dbPath), rawUserVersion(t, dbPath)

	st, err := OpenReadOnly(dbPath)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if err := st.EachTerminal(context.Background(), func(Job) error { return nil }); err != nil {
		t.Fatalf("EachTerminal: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if after := fileDigest(t, dbPath); after != before {
		t.Errorf("a read-only open changed the database file:\n  before %s\n  after  %s", before, after)
	}
	if after := rawUserVersion(t, dbPath); after != beforeVersion {
		t.Errorf("a read-only open moved the schema version from %d to %d", beforeVersion, after)
	}
}

func TestOpenReadOnly_RefusesALedgerAnEarlierBuildWroteRatherThanMigratingIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)
	prev := windBackOneSchemaVersion(t, dbPath)

	st, err := OpenReadOnly(dbPath)
	if err == nil {
		_ = st.Close()
		t.Fatal("OpenReadOnly accepted a ledger from an earlier build")
	}
	msg := err.Error()
	for _, want := range []string{fmt.Sprint(prev), fmt.Sprint(schemaVersion()), "holdfast run"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the refusal does not mention %q: %v", want, msg)
		}
	}
	if got := rawUserVersion(t, dbPath); got != prev {
		t.Errorf("the refused read left the store at user_version %d, was %d", got, prev)
	}
}

// The anti-vacuity half of the test above: the fixture really is a database an earlier
// holdfast wrote, and the DAEMON's door really does move it. Without this, a wind-back that
// silently did nothing would let the refusal test pass for the wrong reason - and it is the
// mutation the whole read-only door exists to prevent, run here on purpose.
func TestOpenReadOnly_TheDaemonsDoorIsWhatMigratesAndThatIsTheDifference(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)
	prev := windBackOneSchemaVersion(t, dbPath)
	if got := rawUserVersion(t, dbPath); got != prev {
		t.Fatalf("the fixture is at user_version %d, want %d", got, prev)
	}

	st, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_ = st.Close()
	if got := rawUserVersion(t, dbPath); got != schemaVersion() {
		t.Fatalf("Open left the store at user_version %d, want %d - the fixture is not an older ledger, "+
			"so the read-only refusal it feeds proves nothing", got, schemaVersion())
	}
}

func TestOpenReadOnly_RefusesALedgerFromTheFuture(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 9999`); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	_ = db.Close()

	st, err := OpenReadOnly(dbPath)
	if err == nil {
		_ = st.Close()
		t.Fatal("OpenReadOnly accepted a database from the future")
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Errorf("the refusal does not name the version it read: %v", err)
	}
	if got := rawUserVersion(t, dbPath); got != 9999 {
		t.Errorf("the refused read moved the version to %d", got)
	}
}

func TestOpenReadOnly_CreatesNothingWhenThereIsNothingToRead(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "absent", "jobs.db")

	st, err := OpenReadOnly(dbPath)
	if err == nil {
		_ = st.Close()
		t.Fatal("OpenReadOnly opened a store that does not exist")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "absent")); statErr == nil {
		t.Error("a read created the state directory; a read that finds nothing to read must leave nothing behind")
	}
	if _, statErr := os.Stat(dbPath); statErr == nil {
		t.Error("a read created an empty database - which would tell an operator who mistyped state_dir " +
			"that they had transcoded nothing")
	}
}

// SurveyLedgerDecisionInputs is the read `validate` goes through, and it is deliberately
// NOT OpenReadOnly's rule: a ledger behind this build is the one population the two counts
// matter most for, so it is answered rather than refused. What it must still never do is
// migrate the file, and a ledger from the future is still a refusal.
func TestSurveyLedgerDecisionInputs_AnswersForAnOlderLedgerWithoutMigratingIt(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)
	prev := windBackOneSchemaVersion(t, dbPath)
	before := fileDigest(t, dbPath)

	got, err := SurveyLedgerDecisionInputs(context.Background(), dbPath, sameConfig)
	if err != nil {
		t.Fatalf("SurveyLedgerDecisionInputs over a ledger the previous build wrote: %v", err)
	}
	// The fixture's rows were seeded recording the configuration in force, and the
	// wind-back took the column with them: under that schema NO row can record anything,
	// which is the whole reason the count needs no migration to be true.
	want := DecisionInputsSurvey{NotRecorded: 2}
	if got != want {
		t.Errorf("surveyed %+v, want %+v", got, want)
	}
	if v := rawUserVersion(t, dbPath); v != prev {
		t.Errorf("the survey migrated the ledger it was reading: user_version is now %d, was %d", v, prev)
	}
	if after := fileDigest(t, dbPath); after != before {
		t.Error("the survey changed the ledger it was reading")
	}
}

func TestSurveyLedgerDecisionInputs_RefusesALedgerFromTheFuture(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	seedTwoTerminalRows(t, dbPath)

	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 9999`); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	_ = db.Close()

	if _, err := SurveyLedgerDecisionInputs(context.Background(), dbPath, sameConfig); err == nil {
		t.Fatal("the survey described a ledger whose shape this build cannot see all of")
	} else if !strings.Contains(err.Error(), "9999") {
		t.Errorf("the refusal does not name the version it read: %v", err)
	}
}

func TestSurveyLedgerDecisionInputs_CreatesNothingWhenThereIsNothingToRead(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "absent", "jobs.db")

	if _, err := SurveyLedgerDecisionInputs(context.Background(), dbPath, sameConfig); err == nil {
		t.Fatal("the survey read a ledger that does not exist")
	}
	if _, statErr := os.Stat(filepath.Join(dir, "absent")); statErr == nil {
		t.Error("the survey created the state directory it was asked to read from")
	}
}

func TestOpenReadOnly_RefusesAFileThatIsNotADatabase(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "jobs.db")
	if err := os.WriteFile(dbPath, []byte("this is not SQLite"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := OpenReadOnly(dbPath)
	if err == nil {
		_ = st.Close()
		t.Fatal("OpenReadOnly accepted a file that is not a database")
	}
}
