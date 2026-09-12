package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/server"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast export` (LEDGER-5) - taking the record somewhere else.
//
// Criterion 2: WHEN the history is exported THE SYSTEM SHALL write every terminal row with
// its recorded outcome, rendering an unmeasured field as absent rather than as zero.
//
// Criterion 11: WHEN the export runs against a ledger holding no terminal row THE SYSTEM
// SHALL produce an empty export and exit zero, distinguishably from any failure.
//
// Criterion 12: IF the export destination already exists, or cannot be created or written
// THEN THE SYSTEM SHALL exit non-zero naming that path and SHALL leave no partial export
// behind.
//
// Criterion 13: IF the job store cannot be opened, is unreadable, or reports a schema
// version this build does not know THEN THE SYSTEM SHALL exit non-zero naming the store
// path and write no export.
//
// The absence rule is the reason this format exists at all. Every numeric field is a
// pointer in the store because 0 is legal for all of them, and a VMAF of 0.0 is a
// destroyed frame rather than a missing measurement. The assertions below therefore run
// against the RAW JSON of each line: decoding into a struct would erase exactly the
// distinction they are checking.

// exportFixture builds a state directory with a jobs.db and returns the directory and the
// config path pointing at it.
func exportFixture(t *testing.T) (stateDir, cfgPath string) {
	t.Helper()
	dir := t.TempDir()
	stateDir = filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("mkdir state: %v", err)
	}
	root := filepath.Join(dir, "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + root + "\nstate_dir: " + stateDir + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return stateDir, cfgPath
}

// openFixtureStore creates the jobs.db inside stateDir. Closing it matters: `export` opens
// the same file, and this suite is asserting on a store an operator is not running against.
func openFixtureStore(t *testing.T, stateDir string) *store.SQLite {
	t.Helper()
	st, err := store.Open(filepath.Join(stateDir, "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	return st
}

func finishRow(t *testing.T, st *store.SQLite, path string, status store.Status, o *store.Outcome) {
	t.Helper()
	ctx := context.Background()
	ok, err := st.Claim(ctx, path, "fp", "w0", 3, store.DecisionInputs{})
	if err != nil || !ok {
		t.Fatalf("claim %s: ok=%v err=%v", path, ok, err)
	}
	if err := st.Finish(ctx, path, "fp", status, o, 3); err != nil {
		t.Fatalf("finish %s: %v", path, err)
	}
}

// runExport drives the real dispatch path, exactly as a shell would.
func runExport(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = dispatch(append([]string{"export"}, args...), &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func ptrI(v int64) *int64     { return &v }
func ptrF(v float64) *float64 { return &v }

// --- criterion 2: every terminal row, with absence preserved -----------------------------

func TestExport_WritesEveryTerminalRowWithItsOutcomeAndKeepsAbsenceAbsent(t *testing.T) {
	stateDir, cfgPath := exportFixture(t)
	st := openFixtureStore(t, stateDir)

	// A fully measured done row, a done row from before the outcome columns existed, a
	// skip naming its guard, and a failure carrying its error. Between them they cover
	// every field the format publishes and every way one can be absent.
	finishRow(t, st, "/lib/measured.mkv", store.Done, &store.Outcome{
		Encoder: "cpu", VmafMean: ptrF(98.25), VmafMin: ptrF(91.5), VmafModel: "version=vmaf_v0.6.1",
		SourceBytes: ptrI(4_294_967_296), OutputBytes: ptrI(1_073_741_824), EncodeMs: ptrI(5_430_000),
	})
	finishRow(t, st, "/lib/unmeasured.mkv", store.Done, &store.Outcome{Encoder: "cpu"})
	finishRow(t, st, "/lib/skipped.mkv", store.Skipped, &store.Outcome{Reason: "hardlinked"})
	finishRow(t, st, "/lib/failed.mkv", store.Failed, &store.Outcome{
		Encoder: "cpu", Reason: "vmaf worst frame 43.2 below the floor 60", EncodeMs: ptrI(900_000),
	})
	// A genuine measured ZERO, which must survive as 0 and never be confused with absence.
	finishRow(t, st, "/lib/zero.mkv", store.Done, &store.Outcome{
		Encoder: "cpu", VmafMean: ptrF(0), VmafMin: ptrF(0), VmafModel: "version=vmaf_v0.6.1",
		SourceBytes: ptrI(1024), OutputBytes: ptrI(1024), EncodeMs: ptrI(0),
	})
	// A non-terminal row, which is not history and must not appear.
	if ok, err := st.Claim(context.Background(), "/lib/inflight.mkv", "fp", "w0", 3, store.DecisionInputs{}); err != nil || !ok {
		t.Fatalf("claim in-flight: ok=%v err=%v", ok, err)
	}
	_ = st.Close()

	code, stdout, stderr := runExport(t, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("export exited %d: %s", code, stderr)
	}

	lines := exportLines(t, stdout)
	if len(lines) != 5 {
		t.Fatalf("the export carries %d lines, want one per terminal row (5):\n%s", len(lines), stdout)
	}
	byPath := map[string]map[string]json.RawMessage{}
	for _, raw := range lines {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &obj); err != nil {
			t.Fatalf("an export line is not JSON (%v): %s", err, raw)
		}
		var p string
		if err := json.Unmarshal(obj["path"], &p); err != nil {
			t.Fatalf("an export line carries no path: %s", raw)
		}
		byPath[p] = obj
	}
	if _, ok := byPath["/lib/inflight.mkv"]; ok {
		t.Error("the export carries a non-terminal row; it is a record of what happened, not of what is happening")
	}

	// The measured row keeps every figure it recorded.
	measured := byPath["/lib/measured.mkv"]
	for field, want := range map[string]string{
		"status": `"done"`, "encoder": `"cpu"`, "vmaf_mean": "98.25", "vmaf_min": "91.5",
		"vmaf_model": `"version=vmaf_v0.6.1"`, "source_bytes": "4294967296",
		"output_bytes": "1073741824", "encode_ms": "5430000",
	} {
		if got := string(measured[field]); got != want {
			t.Errorf("/lib/measured.mkv exports %s as %s, want %s", field, got, want)
		}
	}

	// The unmeasured row states absence as an explicit null. NEVER 0 - the assertion is on
	// the raw bytes because that is the only place the difference still exists.
	unmeasured := byPath["/lib/unmeasured.mkv"]
	for _, field := range []string{"vmaf_mean", "vmaf_min", "source_bytes", "output_bytes", "encode_ms"} {
		got := string(unmeasured[field])
		if got != "null" {
			t.Errorf("/lib/unmeasured.mkv exports %s as %s; an unmeasured field must be an explicit null, "+
				"because a VMAF of 0.0 is a destroyed frame and a size of 0 would invent a 100%% reclaim", field, got)
		}
	}

	// And a REAL zero is exported as a real zero. Absence and a measured zero are two
	// different facts and this is the line that keeps them apart.
	zero := byPath["/lib/zero.mkv"]
	for _, field := range []string{"vmaf_mean", "vmaf_min", "encode_ms"} {
		if got := string(zero[field]); got != "0" {
			t.Errorf("/lib/zero.mkv exports a MEASURED %s as %s, want 0", field, got)
		}
	}

	// The recorded outcome of a skip and a failure: which guard, and which error.
	if got := string(byPath["/lib/skipped.mkv"]["reason"]); got != `"hardlinked"` {
		t.Errorf("the skipped row exports reason %s, want the guard token", got)
	}
	if got := string(byPath["/lib/failed.mkv"]["reason"]); !strings.Contains(got, "worst frame") {
		t.Errorf("the failed row exports reason %s, want the error that rejected it", got)
	}
}

func TestExport_UsesTheSameFieldNamesTheAPIPublishesForARow(t *testing.T) {
	// The format is defined as "what /api/history publishes for a row", and it is kept
	// that way by CALLING that projection. This asserts the promise directly: the exported
	// object's key set is the API's own.
	stateDir, cfgPath := exportFixture(t)
	st := openFixtureStore(t, stateDir)
	finishRow(t, st, "/lib/one.mkv", store.Done, &store.Outcome{
		Encoder: "cpu", SourceBytes: ptrI(10), OutputBytes: ptrI(5), EncodeMs: ptrI(7),
	})
	job, err := st.List(context.Background(), []store.Status{store.Done}, 0)
	if err != nil || len(job) != 1 {
		t.Fatalf("List: %v (%d rows)", err, len(job))
	}
	wantLine, err := server.HistoryRowJSON(job[0])
	if err != nil {
		t.Fatalf("HistoryRowJSON: %v", err)
	}
	_ = st.Close()

	code, stdout, stderr := runExport(t, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("export exited %d: %s", code, stderr)
	}
	lines := exportLines(t, stdout)
	if len(lines) != 1 {
		t.Fatalf("the export carries %d lines, want 1", len(lines))
	}
	if lines[0] != string(wantLine) {
		t.Errorf("the exported row is\n  %s\nand the API publishes\n  %s\nfor the same row", lines[0], wantLine)
	}
}

func TestExport_IsNewlineDelimitedJSONOneObjectPerRow(t *testing.T) {
	stateDir, cfgPath := exportFixture(t)
	st := openFixtureStore(t, stateDir)
	for i := 0; i < 250; i++ {
		finishRow(t, st, "/lib/f"+strconv.Itoa(i)+".mkv", store.Skipped, &store.Outcome{Reason: "low-bitrate"})
	}
	_ = st.Close()

	code, stdout, stderr := runExport(t, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("export exited %d: %s", code, stderr)
	}
	// Every row, not the 200 /api/history would have shipped: the export is the answer to
	// the cap, so it must not inherit it.
	lines := exportLines(t, stdout)
	if len(lines) != 250 {
		t.Fatalf("the export carries %d lines for 250 terminal rows", len(lines))
	}
	if !strings.HasSuffix(stdout, "\n") {
		t.Error("the export does not end with a newline; NDJSON terminates every record, including the last")
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Fatalf("line %d is blank", i+1)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not one JSON object: %v", i+1, err)
		}
	}
}

// --- criterion 11: an empty ledger is an empty export, exit zero -------------------------

func TestExport_AnEmptyLedgerProducesAnEmptyExportAndExitsZero(t *testing.T) {
	stateDir, cfgPath := exportFixture(t)
	// A real store with rows that are NOT terminal, so "empty export" is a statement
	// about history rather than about an empty file.
	st := openFixtureStore(t, stateDir)
	if ok, err := st.Claim(context.Background(), "/lib/inflight.mkv", "fp", "w0", 3, store.DecisionInputs{}); err != nil || !ok {
		t.Fatalf("claim: ok=%v err=%v", ok, err)
	}
	_ = st.Close()

	code, stdout, stderr := runExport(t, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("an empty export exited %d, want 0 - it must be distinguishable from a failure: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("an empty export wrote %q to stdout, want nothing", stdout)
	}
	if stderr != "" {
		t.Errorf("an empty export wrote %q to stderr; success says nothing", stderr)
	}

	// And to a file: an empty export is a real, empty file, not an absent one.
	out := filepath.Join(t.TempDir(), "ledger.ndjson")
	code, _, stderr = runExport(t, "--config", cfgPath, "--out", out)
	if code != 0 {
		t.Fatalf("an empty export to --out exited %d: %s", code, stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("an empty export left no file at %s: %v", out, err)
	}
	if len(b) != 0 {
		t.Errorf("an empty export wrote %d bytes, want an empty file", len(b))
	}
}

// --- criterion 12: the destination -------------------------------------------------------

func TestExport_RefusesAnExistingDestinationAndLeavesItUntouched(t *testing.T) {
	stateDir, cfgPath := exportFixture(t)
	st := openFixtureStore(t, stateDir)
	finishRow(t, st, "/lib/one.mkv", store.Done, &store.Outcome{Encoder: "cpu"})
	_ = st.Close()

	dir := t.TempDir()
	out := filepath.Join(dir, "ledger.ndjson")
	const previous = "THE PREVIOUS EXPORT, WHICH MAY BE THE ONLY COPY OF PRUNED ROWS\n"
	if err := os.WriteFile(out, []byte(previous), 0o644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}

	code, _, stderr := runExport(t, "--config", cfgPath, "--out", out)
	if code == 0 {
		t.Fatal("export overwrote an existing destination; an export is a record, and the one it replaced " +
			"may be the only copy of rows a prune has since removed")
	}
	if !strings.Contains(stderr, out) {
		t.Errorf("the refusal does not name the destination: %q", stderr)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(got) != previous {
		t.Errorf("the destination was modified: %q", got)
	}
	requireNoLeftovers(t, dir, "ledger.ndjson")
}

func TestExport_ADestinationThatCannotBeCreatedIsNonZeroAndLeavesNothingBehind(t *testing.T) {
	stateDir, cfgPath := exportFixture(t)
	st := openFixtureStore(t, stateDir)
	finishRow(t, st, "/lib/one.mkv", store.Done, &store.Outcome{Encoder: "cpu"})
	_ = st.Close()

	dir := t.TempDir()
	for _, c := range []struct{ name, out string }{
		{"a directory that does not exist", filepath.Join(dir, "nope", "ledger.ndjson")},
		{"a path that is itself a directory", dir},
	} {
		code, _, stderr := runExport(t, "--config", cfgPath, "--out", c.out)
		if code == 0 {
			t.Errorf("%s: export exited 0", c.name)
		}
		if !strings.Contains(stderr, c.out) {
			t.Errorf("%s: the failure does not name the path: %q", c.name, stderr)
		}
	}
	// The unwritable-directory case needs a non-root process to be meaningful; skip the
	// assertion rather than assert something that is true for the wrong reason.
	if os.Geteuid() != 0 {
		ro := filepath.Join(dir, "readonly")
		if err := os.MkdirAll(ro, 0o500); err != nil {
			t.Fatalf("mkdir readonly: %v", err)
		}
		out := filepath.Join(ro, "ledger.ndjson")
		code, _, stderr := runExport(t, "--config", cfgPath, "--out", out)
		if code == 0 {
			t.Error("export into an unwritable directory exited 0")
		}
		if !strings.Contains(stderr, out) {
			t.Errorf("the failure does not name the path: %q", stderr)
		}
		if _, err := os.Stat(out); err == nil {
			t.Errorf("a failed export left a file at %s", out)
		}
	}
	requireNoLeftovers(t, dir)
}

// requireNoLeftovers asserts dir holds nothing but the names given - no temp file from a
// refused or failed export. "leave no partial export behind" means no half-written file
// under the destination's name AND no debris beside it.
func requireNoLeftovers(t *testing.T, dir string, allowed ...string) {
	t.Helper()
	ok := map[string]bool{}
	for _, n := range allowed {
		ok[n] = true
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range ents {
		if e.IsDir() || ok[e.Name()] {
			continue
		}
		t.Errorf("a failed or refused export left %q behind in %s", e.Name(), dir)
	}
}

// --- criterion 13: the store ------------------------------------------------------------

func TestExport_AStoreThatCannotBeReadIsNonZeroNamesThePathAndWritesNoExport(t *testing.T) {
	for _, c := range []struct {
		name       string
		breakStore func(t *testing.T, stateDir string)
	}{
		{
			// No store at all. `export` must NOT create one: an export that silently made
			// an empty database and then reported an empty ledger would tell an operator
			// who mistyped state_dir that they had transcoded nothing.
			name:       "the store does not exist",
			breakStore: func(t *testing.T, stateDir string) {},
		},
		{
			name: "the store is not a database",
			breakStore: func(t *testing.T, stateDir string) {
				if err := os.WriteFile(filepath.Join(stateDir, "jobs.db"), []byte("this is not SQLite"), 0o644); err != nil {
					t.Fatalf("write junk store: %v", err)
				}
			},
		},
		{
			name: "the store reports a schema version this build does not know",
			breakStore: func(t *testing.T, stateDir string) {
				st := openFixtureStore(t, stateDir)
				finishRow(t, st, "/lib/one.mkv", store.Done, &store.Outcome{Encoder: "cpu"})
				_ = st.Close()
				bumpSchemaVersion(t, filepath.Join(stateDir, "jobs.db"), 9999)
			},
		},
		{
			// The other direction, and the reason the read-only door exists: a ledger an
			// EARLIER holdfast wrote. Reading it would mean querying columns the file may
			// not have, and the only way to get them is to migrate - which would stamp a
			// version the holdfast still running against that file would then refuse. So
			// this too is a refusal naming the store path, never a repair.
			name: "the store was written by an earlier holdfast",
			breakStore: func(t *testing.T, stateDir string) {
				seedOlderLedger(t, stateDir)
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			stateDir, cfgPath := exportFixture(t)
			c.breakStore(t, stateDir)
			dbPath := filepath.Join(stateDir, "jobs.db")

			out := filepath.Join(t.TempDir(), "ledger.ndjson")
			code, stdout, stderr := runExport(t, "--config", cfgPath, "--out", out)
			if code == 0 {
				t.Fatalf("export exited 0 over a store that cannot be read")
			}
			if !strings.Contains(stderr, dbPath) {
				t.Errorf("the failure does not name the store path %q: %q", dbPath, stderr)
			}
			if stdout != "" {
				t.Errorf("a failed export wrote %q to stdout", stdout)
			}
			if _, err := os.Stat(out); err == nil {
				t.Errorf("a failed export wrote a file at %s", out)
			}
		})
	}
}

// --- criterion 13 + the Interfaces section: a READ, and only a read -----------------------
//
// The spec fixes `holdfast export` as "a local, operator-run read of the operator's own
// store", and README.md and export.go both say it never writes to the store it reads. The
// store's daemon door (store.Open) MIGRATES unconditionally, so an export that went through
// it would upgrade the operator's ledger as a side effect of reading it - and store.migrate
// then refuses that file to the older holdfast that wrote it, which may be the daemon still
// running against it. These two tests are what hold the documented behaviour to the code.

// theStoreIsExactlyAsItWasFound is the whole claim, measured on the file itself rather than
// on an assertion about which function was called: the bytes and the schema stamp.
func theStoreIsExactlyAsItWasFound(t *testing.T, dbPath, wantDigest string, wantVersion int) {
	t.Helper()
	if got := dbDigest(t, dbPath); got != wantDigest {
		t.Errorf("`holdfast export` changed the database file it read:\n  before %s\n  after  %s", wantDigest, got)
	}
	if got := readSchemaStamp(t, dbPath); got != wantVersion {
		t.Errorf("`holdfast export` left the store at user_version %d, was %d before the export.\n"+
			"README.md and cmd/holdfast/export.go both state it never writes to the store it reads - "+
			"and a store whose version has moved ahead is a store the holdfast that wrote it will refuse to open.",
			got, wantVersion)
	}
}

func TestExport_NeverWritesToTheStoreItReads(t *testing.T) {
	stateDir, cfgPath := exportFixture(t)
	st := openFixtureStore(t, stateDir)
	finishRow(t, st, "/lib/one.mkv", store.Done, &store.Outcome{
		Encoder: "cpu", SourceBytes: ptrI(10), OutputBytes: ptrI(5),
	})
	finishRow(t, st, "/lib/two.mkv", store.Skipped, &store.Outcome{Reason: "low-bitrate"})
	_ = st.Close()

	dbPath := filepath.Join(stateDir, "jobs.db")
	digest, version := dbDigest(t, dbPath), readSchemaStamp(t, dbPath)

	code, stdout, stderr := runExport(t, "--config", cfgPath)
	if code != 0 {
		t.Fatalf("export exited %d: %s", code, stderr)
	}
	if len(exportLines(t, stdout)) != 2 {
		t.Fatalf("the export wrote %d lines; the fixture is wrong, not the claim", len(exportLines(t, stdout)))
	}
	theStoreIsExactlyAsItWasFound(t, dbPath, digest, version)
}

func TestExport_RefusesALedgerAnEarlierHoldfastWroteRatherThanUpgradingIt(t *testing.T) {
	stateDir, cfgPath := exportFixture(t)
	seedOlderLedger(t, stateDir)

	dbPath := filepath.Join(stateDir, "jobs.db")
	digest, version := dbDigest(t, dbPath), readSchemaStamp(t, dbPath)
	if version != olderSchemaVersion {
		t.Fatalf("the fixture is at user_version %d, want %d", version, olderSchemaVersion)
	}

	out := filepath.Join(t.TempDir(), "ledger.ndjson")
	code, stdout, stderr := runExport(t, "--config", cfgPath, "--out", out)
	if code == 0 {
		t.Fatalf("export exited 0 against a ledger written by an earlier holdfast")
	}
	if !strings.Contains(stderr, dbPath) {
		t.Errorf("the refusal does not name the store path %q: %q", dbPath, stderr)
	}
	if stdout != "" {
		t.Errorf("a refused export wrote %q to stdout", stdout)
	}
	if _, err := os.Stat(out); err == nil {
		t.Errorf("a refused export wrote a file at %s", out)
	}
	theStoreIsExactlyAsItWasFound(t, dbPath, digest, version)
}

// The anti-vacuity half. `store.Open` - the door the export used to go through - really does
// migrate this fixture in place, so the two tests above are measuring a difference that
// exists rather than a wind-back that quietly did nothing. This is the defect, run on
// purpose, one call away from the code that no longer makes it.
func TestExport_TheDaemonsDoorIsWhatMigratesAndThatIsWhyTheExportDoesNotUseIt(t *testing.T) {
	stateDir, _ := exportFixture(t)
	seedOlderLedger(t, stateDir)
	dbPath := filepath.Join(stateDir, "jobs.db")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	_ = st.Close()
	if got := readSchemaStamp(t, dbPath); got <= olderSchemaVersion {
		t.Fatalf("store.Open left the fixture at user_version %d; it is not an older ledger, so the "+
			"refusal tests above prove nothing", got)
	}
}

func TestExport_RequiresTheSameConfigFlagTheOtherSubcommandsTake(t *testing.T) {
	if code := mustDispatch(t, "export"); code == 0 {
		t.Error("export with no --config exited 0")
	}
	if code := mustDispatch(t, "export", "--config", filepath.Join(t.TempDir(), "absent.yaml")); code == 0 {
		t.Error("export with a --config that does not exist exited 0")
	}
}

func mustDispatch(t *testing.T, args ...string) int {
	t.Helper()
	var out, errBuf bytes.Buffer
	return dispatch(args, &out, &errBuf)
}

// bumpSchemaVersion stamps a user_version this build does not know, which is exactly the
// database-from-the-future store.migrate refuses to open rather than write through a
// schema that cannot see all of its columns.
func bumpSchemaVersion(t *testing.T, dbPath string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("stamp user_version: %v", err)
	}
}

// olderSchemaVersion is the schema this repository shipped immediately before the NEWEST
// migration appended its own step: the shape a database written by the previous holdfast
// has. That newest step is now the encode profile that supplied a job's settings,
// appended after which library profile decided the row, so the version below and the
// object seedOlderLedger removes both moved with it. It is a literal because cmd/holdfast
// cannot see the store's unexported version counter - and
// TestExport_TheDaemonsDoorIsWhatMigratesAndThatIsWhyTheExportDoesNotUseIt keeps the literal
// honest by asserting store.Open really does move a fixture built from it.
const olderSchemaVersion = 12

// seedOlderLedger builds a real ledger with rows and then removes exactly what the NEWEST
// migration added, restoring the previous version stamp. Not a current database wearing an
// older number: the objects are gone too, which is what an earlier holdfast's file looks
// like and what makes "read it without migrating it" a question with teeth.
//
// It has to track the END of the migrations slice. A fixture that undid a step which is no
// longer the last one would leave a database whose later columns are already present, and
// store.Open would then die re-running an ALTER it has already run - a duplicate-column
// error rather than the older ledger this test needs.
func seedOlderLedger(t *testing.T, stateDir string) {
	t.Helper()
	st := openFixtureStore(t, stateDir)
	finishRow(t, st, "/lib/a.mkv", store.Skipped, &store.Outcome{Reason: "already-at-target-codec"})
	finishRow(t, st, "/lib/b.mkv", store.Done, &store.Outcome{
		Encoder: "cpu", SourceBytes: ptrI(4096), OutputBytes: ptrI(1024),
	})
	_ = st.Close()

	dbPath := filepath.Join(stateDir, "jobs.db")
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("raw open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	// Any index goes first: SQLite refuses to drop a column an index refers to. The
	// newest step adds none, so there is nothing to drop ahead of the column today.
	for _, stmt := range []string{
		`ALTER TABLE jobs DROP COLUMN profile`,
		fmt.Sprintf(`PRAGMA user_version = %d`, olderSchemaVersion),
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

// readSchemaStamp reads PRAGMA user_version straight off the file, with no store code in
// between - the assertion has to be able to see a migration the store would have hidden.
func readSchemaStamp(t *testing.T, dbPath string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatalf("raw open %s: %v", dbPath, err)
	}
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func dbDigest(t *testing.T, dbPath string) string {
	t.Helper()
	b, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read %s: %v", dbPath, err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(b))
}

// exportLines splits an NDJSON export into its records, dropping the trailing empty
// element the final newline produces.
func exportLines(t *testing.T, body string) []string {
	t.Helper()
	if body == "" {
		return nil
	}
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	return lines
}
