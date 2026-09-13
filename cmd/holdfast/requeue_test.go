package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// `holdfast requeue`, and the two counts a run and `validate` owe (S0080).
//
// Everything here is graded through the REAL command - dispatch, the real config load,
// the real store on disk - because requeue widens the set of files the encoder is allowed
// to touch, which in this repository is the most destructive thing that can be done. Its
// refusals are therefore worth as much as its successes, and each of them is asserted to
// have exited non-zero AND to have changed nothing.

// ledgerConfig writes a config whose state directory is empty, and returns both paths.
func ledgerConfig(t *testing.T, extra string) (cfgPath, state string) {
	t.Helper()
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(dir, "state")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\n" + extra
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, state
}

// seedLedger writes rows into the state directory's ledger through the real store, then
// closes it - so the command under test opens the same file an operator's run left behind.
func seedLedger(t *testing.T, state string, rows func(*store.SQLite)) {
	t.Helper()
	st, err := store.Open(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rows(st)
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
}

// seedTerminalRow writes one terminal row carrying reason and the inputs it was decided
// under.
func seedTerminalRow(t *testing.T, st *store.SQLite, path string, status store.Status, reason string, in store.DecisionInputs) {
	t.Helper()
	ctx := context.Background()
	if ok, err := st.Claim(ctx, path, "fp", "seed", 3, store.DecisionInputs{}); err != nil || !ok {
		t.Fatalf("seed claim %s: ok=%v err=%v", path, ok, err)
	}
	if err := st.Finish(ctx, path, "fp", status, &store.Outcome{Reason: reason, DecisionInputs: in}, 3); err != nil {
		t.Fatalf("seed finish %s: %v", path, err)
	}
}

// inForce is the decision-input record of the configuration a config file describes: the
// one a seeded row must carry to be a LIVE decision rather than one the scan would
// re-open on its own.
func inForce(t *testing.T, cfgPath string) store.DecisionInputs {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return engine.DecisionInputsFor(*cfg)
}

// claimable reports whether the next scan would hand this row to a worker.
func claimable(t *testing.T, state, path string, in store.DecisionInputs) bool {
	t.Helper()
	st, err := store.Open(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	ok, err := st.Claim(context.Background(), path, "fp", "probe", 3, in)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	return ok
}

// TestUsage_ListsRequeueAsACommand. A command nobody can find is a command that does not
// exist, so it is listed where `resolve` and `restore` are - on the bare invocation and
// on -h alike, which are two different writers and two different exit codes.
func TestUsage_ListsRequeueAsACommand(t *testing.T) {
	code, out, _ := cli(t, "-h")
	if code != 0 {
		t.Errorf("holdfast -h exited %d, want 0", code)
	}
	if !strings.Contains(out, "requeue") {
		t.Errorf("`holdfast -h` does not list requeue among its commands:\n%s", out)
	}
	for _, want := range []string{"resolve", "restore", "requeue"} {
		if !strings.Contains(out, want) {
			t.Errorf("the command list is missing %q", want)
		}
	}

	code, _, errOut := cli(t)
	if code != 2 {
		t.Errorf("holdfast with no arguments exited %d, want 2", code)
	}
	if !strings.Contains(errOut, "requeue") {
		t.Errorf("the no-argument usage does not list requeue:\n%s", errOut)
	}
}

// TestRequeue_APathReopensThatOneRowAndReportsIt.
func TestRequeue_APathReopensThatOneRowAndReportsIt(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")
	in := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, "/lib/one.mkv", store.Skipped, engine.SkipLowBitrate, in)
		seedTerminalRow(t, st, "/lib/two.mkv", store.Skipped, engine.SkipLowBitrate, in)
	})

	code, out, errOut := cli(t, "requeue", "--config", cfgPath, "/lib/one.mkv")
	if code != 0 {
		t.Fatalf("requeue exited %d, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "/lib/one.mkv") {
		t.Errorf("the output does not say what it re-opened:\n%s", out)
	}
	if !strings.Contains(out, "re-opened 1 row(s)") {
		t.Errorf("the output does not print the count:\n%s", out)
	}
	if !claimable(t, state, "/lib/one.mkv", in) {
		t.Error("the named row is still not claimable, so the next scan will not look at it")
	}
	if claimable(t, state, "/lib/two.mkv", in) {
		t.Error("a row the operator did not name was re-opened too")
	}
}

// TestRequeue_AGuardTokenReopensEveryRowCarryingIt.
func TestRequeue_AGuardTokenReopensEveryRowCarryingIt(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")
	in := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, "/lib/quiet-a.mkv", store.Skipped, engine.SkipLowBitrate, in)
		seedTerminalRow(t, st, "/lib/quiet-b.mkv", store.Skipped, engine.SkipLowBitrate, in)
		seedTerminalRow(t, st, "/lib/at-codec.mkv", store.Skipped, engine.SkipAlreadyTargetCodec, in)
	})

	code, out, errOut := cli(t, "requeue", "--config", cfgPath, "--guard", engine.SkipLowBitrate)
	if code != 0 {
		t.Fatalf("requeue exited %d, want 0 (stderr: %s)", code, errOut)
	}
	for _, want := range []string{"/lib/quiet-a.mkv", "/lib/quiet-b.mkv", "re-opened 2 row(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output does not carry %q:\n%s", want, out)
		}
	}
	for _, p := range []string{"/lib/quiet-a.mkv", "/lib/quiet-b.mkv"} {
		if !claimable(t, state, p, in) {
			t.Errorf("%s is still not claimable", p)
		}
	}
	if claimable(t, state, "/lib/at-codec.mkv", in) {
		t.Error("a row carrying a different guard was re-opened")
	}
}

// TestRequeue_FailedReopensAParkedRowSoTheNextScanClaimsIt. A parked row is held by its
// attempt count and by nothing else, so this is the one selector that has to move that
// count - clearing the decision inputs alone would leave the file exactly where it was.
func TestRequeue_FailedReopensAParkedRowSoTheNextScanClaimsIt(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "max_failures: 3\n")
	in := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		ctx := context.Background()
		if ok, err := st.Claim(ctx, "/lib/parked.mkv", "fp", "seed", 3, store.DecisionInputs{}); err != nil || !ok {
			t.Fatalf("seed claim: ok=%v err=%v", ok, err)
		}
		for i := 0; i < 3; i++ {
			if err := st.Finish(ctx, "/lib/parked.mkv", "fp", store.Failed,
				&store.Outcome{Reason: "ffmpeg died"}, 3); err != nil {
				t.Fatalf("seed finish: %v", err)
			}
		}
	})
	if claimable(t, state, "/lib/parked.mkv", in) {
		t.Fatal("the fixture is wrong: the row is not parked, so re-opening it proves nothing")
	}

	code, out, errOut := cli(t, "requeue", "--config", cfgPath, "--failed")
	if code != 0 {
		t.Fatalf("requeue --failed exited %d, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "/lib/parked.mkv") || !strings.Contains(out, "re-opened 1 row(s)") {
		t.Errorf("the output does not say what it re-opened, or the count:\n%s", out)
	}
	if !claimable(t, state, "/lib/parked.mkv", in) {
		t.Error("--failed did not make the parked row claimable by the next scan")
	}
}

// TestRequeue_RefusesAnUnknownGuardToken. A typo and an empty match set are different
// problems: reporting a typo as zero matches sends an operator to look at their library
// for a row that was never going to match anything.
func TestRequeue_RefusesAnUnknownGuardToken(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")
	in := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, "/lib/quiet.mkv", store.Skipped, engine.SkipLowBitrate, in)
	})

	code, _, errOut := cli(t, "requeue", "--config", cfgPath, "--guard", "low_bitrate")
	if code == 0 {
		t.Fatal("an unrecognised guard token must exit non-zero")
	}
	for _, want := range []string{"low_bitrate", engine.SkipLowBitrate, engine.SkipAlreadyTargetCodec} {
		if !strings.Contains(errOut, want) {
			t.Errorf("the refusal does not name %q; it must say what it does recognise:\n%s", want, errOut)
		}
	}
	if claimable(t, state, "/lib/quiet.mkv", in) {
		t.Error("a refused requeue re-opened a row anyway")
	}
}

// TestRequeue_RefusesWithNoSelector. Re-opening the whole ledger is not something anybody
// types by accident, so an empty selector is a refusal that prints the usage rather than a
// request to offer the library to the encoder.
func TestRequeue_RefusesWithNoSelector(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")
	in := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, "/lib/quiet.mkv", store.Skipped, engine.SkipLowBitrate, in)
		seedTerminalRow(t, st, "/lib/done.mkv", store.Done, "", in)
	})

	code, _, errOut := cli(t, "requeue", "--config", cfgPath)
	if code == 0 {
		t.Fatal("requeue with no path, no --guard and no --failed must exit non-zero")
	}
	if !strings.Contains(errOut, "holdfast requeue") {
		t.Errorf("the refusal does not print the usage:\n%s", errOut)
	}
	for _, p := range []string{"/lib/quiet.mkv", "/lib/done.mkv"} {
		if claimable(t, state, p, in) {
			t.Errorf("%s was re-opened by a requeue that refused", p)
		}
	}
}

// TestRequeue_RefusesWhenThereIsNoLedgerYet. A fresh install has nothing to re-open, and
// a command that answered by CREATING the database it was asked about would be writing
// state into a directory an operator had not yet let holdfast touch.
func TestRequeue_RefusesWhenThereIsNoLedgerYet(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")

	code, _, errOut := cli(t, "requeue", "--config", cfgPath, "/lib/anything.mkv")
	if code == 0 {
		t.Fatal("requeue against a state directory with no ledger must exit non-zero")
	}
	if !strings.Contains(errOut, "no ledger") {
		t.Errorf("the refusal does not say there is no ledger yet:\n%s", errOut)
	}
	if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
		t.Error("the refusal CREATED the ledger it was asked about")
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("the refusal created the state directory")
	}
}

// TestRequeue_LeavesTheProtectedRowsAloneAndSaysSo is the command-level half of the two
// engine cases: the operator asked, and what they get back names the rows holdfast
// refused and why, with a non-zero exit because nothing was re-opened.
func TestRequeue_LeavesTheProtectedRowsAloneAndSaysSo(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")
	in := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		if _, err := st.RecordSkip(context.Background(), "/lib/rescued.mkv", "fp",
			engine.SkipRestoredOriginal, store.Decision{}, ""); err != nil {
			t.Fatalf("RecordSkip: %v", err)
		}
		seedTerminalRow(t, st, "/lib/parked.mkv", store.Indeterminate, "the swap did not complete cleanly", in)
	})

	for _, tc := range []struct{ path, want string }{
		{"/lib/rescued.mkv", "undo window"},
		{"/lib/parked.mkv", "resolve"},
	} {
		code, out, errOut := cli(t, "requeue", "--config", cfgPath, tc.path)
		if code == 0 {
			t.Errorf("requeue %s exited 0 having re-opened nothing", tc.path)
		}
		if !strings.Contains(out, "left alone: "+tc.path) {
			t.Errorf("requeue %s did not say it left the row alone:\n%s", tc.path, out)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("requeue %s did not say WHY (want %q):\n%s", tc.path, tc.want, out)
		}
		if !strings.Contains(errOut, "never re-opens") {
			t.Errorf("requeue %s did not refuse plainly:\n%s", tc.path, errOut)
		}
	}
}

// ---- the two counts a run and `validate` owe ---------------------------------

// TestValidate_ReportsTheRowsTakenUnderAMovedConfiguration. `validate` is where an
// operator asks what a configuration will DO, and since a terminal row is only terminal
// for the configuration it was taken under, the answer is incomplete without the count of
// the rows the next scan will re-open.
func TestValidate_ReportsTheRowsTakenUnderAMovedConfiguration(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "crf: 23\n")
	moved := inForce(t, cfgPath) // recorded under crf 23 ...
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, "/lib/a.mkv", store.Done, "", moved)
		seedTerminalRow(t, st, "/lib/b.mkv", store.Skipped, engine.SkipLowBitrate, moved)
		seedTerminalRow(t, st, "/lib/old.mkv", store.Skipped, engine.SkipLowBitrate, store.DecisionInputs{})
	})

	// ... and validated under crf 24, which every one of those rows read.
	moveConfig(t, cfgPath, "crf: 24\n")
	before := sha256File(t, filepath.Join(state, "jobs.db"))

	code, out, errOut := cli(t, "validate", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("validate exited %d, want 0 (stderr: %s)", code, errOut)
	}
	for _, want := range []string{"2 terminal row(s) were taken under a configuration that has since moved",
		"1 terminal row(s) record no decision inputs", "3 file(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("validate does not report %q:\n%s", want, out)
		}
	}
	// And it READ the ledger: a validate that migrated or rewrote an operator's store as
	// a side effect of describing it would make that file unopenable by the daemon still
	// running against it.
	if after := sha256File(t, filepath.Join(state, "jobs.db")); after != before {
		t.Error("validate changed the ledger it was describing")
	}
}

// TestValidate_ReadsALedgerAnEarlierBuildWroteWithoutMigratingIt. Upgrade day is when the
// two counts carry the most: every row the previous build wrote records nothing, so the
// first scan is about to offer the whole terminal set to the guards again. `validate` has
// to say so over a file whose schema it does not share - and it has to do that WITHOUT
// migrating it, because stamping a newer version onto an operator's ledger would make that
// file unopenable by the holdfast still running against it.
//
// The counts need no migration to be true: a schema with no column for them is one in which
// no row can have recorded any.
func TestValidate_ReadsALedgerAnEarlierBuildWroteWithoutMigratingIt(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")
	seedOlderLedger(t, state)
	dbPath := filepath.Join(state, "jobs.db")
	if got := readSchemaStamp(t, dbPath); got != olderSchemaVersion {
		t.Fatalf("the fixture is at schema %d, want %d - it is not a previous build's ledger",
			got, olderSchemaVersion)
	}
	before := sha256File(t, dbPath)

	code, out, errOut := cli(t, "validate", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("validate exited %d, want 0 (stderr: %s)", code, errOut)
	}
	// Two terminal rows in the fixture, both recording nothing, and both re-opened by the
	// first scan this build runs.
	for _, want := range []string{
		"0 terminal row(s) were taken under a configuration that has since moved",
		"2 terminal row(s) record no decision inputs",
		"2 file(s)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("validate does not report %q for a ledger the previous build wrote:\n%s", want, out)
		}
	}
	if got := readSchemaStamp(t, dbPath); got != olderSchemaVersion {
		t.Errorf("validate MIGRATED the ledger it was describing: schema is now %d, was %d",
			got, olderSchemaVersion)
	}
	if after := sha256File(t, dbPath); after != before {
		t.Error("validate changed the ledger it was describing")
	}
}

// TestValidate_PrintsBothFiguresWhenNothingHasMoved. "Nothing has moved" is the answer an
// operator most needs to be able to trust, so both figures are printed as NUMBERS even when
// both are zero: a line that only appears when there is something to report cannot be told
// apart from a report that was never attempted.
func TestValidate_PrintsBothFiguresWhenNothingHasMoved(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "crf: 23\n")
	live := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, "/lib/a.mkv", store.Done, "", live)
		seedTerminalRow(t, st, "/lib/b.mkv", store.Skipped, engine.SkipLowBitrate, live)
	})

	code, out, errOut := cli(t, "validate", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("validate exited %d, want 0 (stderr: %s)", code, errOut)
	}
	for _, want := range []string{
		"0 terminal row(s) were taken under a configuration that has since moved",
		"0 terminal row(s) record no decision inputs",
		"re-opens none of them",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("validate does not report %q for a ledger entirely in force:\n%s", want, out)
		}
	}
}

// TestRequeue_RefusesTwoSelectorsRatherThanActingOnOne. A path, --guard and --failed name
// three different sets. Given two, acting on one would hand an operator a set they did not
// ask for and report it as success, which is the silence this command exists to end.
func TestRequeue_RefusesTwoSelectorsRatherThanActingOnOne(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")
	live := inForce(t, cfgPath)
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, "/lib/quiet.mkv", store.Skipped, engine.SkipLowBitrate, live)
	})

	code, out, errOut := cli(t, "requeue", "--config", cfgPath, "--guard", engine.SkipLowBitrate, "--failed")
	if code == 0 {
		t.Fatalf("requeue accepted two selectors and exited 0:\n%s", out)
	}
	if !strings.Contains(errOut, "ONE selector") && !strings.Contains(errOut, "takes ONE selector") {
		t.Errorf("the refusal does not say one selector per run:\n%s", errOut)
	}
	if claimable(t, state, "/lib/quiet.mkv", live) {
		t.Error("the refused requeue re-opened the row the guard selector would have matched")
	}

	// Anti-vacuity: the same guard ALONE re-opens that row, so the refusal above is about
	// the two selectors and not about a command that can do nothing.
	if code, out, errOut := cli(t, "requeue", "--config", cfgPath, "--guard", engine.SkipLowBitrate); code != 0 {
		t.Fatalf("requeue of the guard alone exited %d:\n%s\n%s", code, out, errOut)
	}
	if !claimable(t, state, "/lib/quiet.mkv", live) {
		t.Error("the guard selector alone did not re-open the row")
	}
}

// TestValidate_WithNoLedgerStillPasses. A fresh install validates cleanly, says there is
// nothing to read, and has created neither the state directory nor the database - which
// is what keeps this widening of what `validate` touches from reddening a first run.
func TestValidate_WithNoLedgerStillPasses(t *testing.T) {
	cfgPath, state := ledgerConfig(t, "")

	code, out, errOut := cli(t, "validate", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("validate exited %d on a fresh install, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "config OK") {
		t.Errorf("validate did not report the config as valid:\n%s", out)
	}
	if !strings.Contains(out, "none at") {
		t.Errorf("validate did not say there is no ledger to read:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(state, "jobs.db")); err == nil {
		t.Error("validate CREATED a ledger")
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("validate created the state directory")
	}
}

// TestStartup_ReportsTheRowsTakenUnderAMovedConfiguration. A scan offers every row whose
// recorded inputs have moved back to the guards, so a daemon that started and quietly did
// that would leave an operator watching unexplained activity. The count goes out BEFORE
// the scan, and it is graded off the REAL run's own output - a child process, the real
// logger, the real store on disk.
//
// The fixture moves min_bitrate_kbps between two values that both exceed the clip's real
// bitrate: the row moves, the file is re-opened, and the guard reaches the same verdict,
// so what is measured is the announcement and never an encode.
func TestStartup_ReportsTheRowsTakenUnderAMovedConfiguration(t *testing.T) {
	cfgPath, lib, state := realLibrary(t, "min_bitrate_kbps: 100000\n")

	if code, out := cliProcess(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("first run exited %d:\n%s", code, out)
	}
	// The fixture really did leave a row recorded at the OLD threshold; without that
	// there is nothing for the next start to report and the case is vacuous.
	assertRecordedAt(t, state, filepath.Join(lib, "movie.mkv"), engine.SkipLowBitrate, "100000")

	// The operator edits the YAML. Every low-bitrate row in the ledger was compared
	// against the old value.
	moveConfig(t, cfgPath, "min_bitrate_kbps: 90000\n")

	code, out := cliProcess(t, "run", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("second run exited %d:\n%s", code, out)
	}
	for _, want := range []string{
		"rows_taken_under_a_moved_configuration=1",
		"rows_recording_no_decision_inputs=0",
		"1 terminal row(s) were taken under a configuration that has since moved",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the startup report does not carry %q:\n%s", want, out)
		}
	}
	// Before the scan re-opens anything: the announcement has to precede the work it is
	// about, or it is a description of what already happened.
	report := strings.Index(out, "rows_taken_under_a_moved_configuration")
	scan := strings.Index(out, "scan complete")
	if report < 0 || scan < 0 || report > scan {
		t.Errorf("the report is not before the scan (report at %d, scan at %d):\n%s", report, scan, out)
	}
}

// assertRecordedAt reads the ledger a run left behind and asserts the row for path is the
// skip it should be, recorded against the threshold that decided it.
func assertRecordedAt(t *testing.T, state, path, reason, threshold string) {
	t.Helper()
	st, err := store.Open(filepath.Join(state, "jobs.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = st.Close() }()
	rows, err := st.List(context.Background(), []store.Status{store.Skipped}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range rows {
		if r.Path != path {
			continue
		}
		if r.Outcome.Reason != reason {
			t.Fatalf("%s is skipped %q, want %q", path, r.Outcome.Reason, reason)
		}
		got, ok := r.Outcome.DecisionInputs.Value("min_bitrate_kbps")
		if !ok || got != threshold {
			t.Fatalf("%s recorded min_bitrate_kbps=%q (present=%v), want %q", path, got, ok, threshold)
		}
		return
	}
	t.Fatalf("the run left no skipped row for %s, so there is nothing to re-open: %+v", path, rows)
}

// realLibrary builds a library holding one real clip and a config pointing at it. It is
// preflightLibrary without the min_bitrate_kbps that helper fixes at 0, because these
// cases move exactly that key.
func realLibrary(t *testing.T, extraCfg string) (cfgPath, lib, state string) {
	t.Helper()
	ffmpeg := envOr("HOLDFAST_FFMPEG", "ffmpeg")
	if _, err := exec.LookPath(ffmpeg); err != nil {
		t.Fatalf("::error:: ffmpeg required to build this fixture: %v", err)
	}
	dir := t.TempDir()
	lib = filepath.Join(dir, "media")
	state = filepath.Join(dir, "state")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-f", "lavfi",
		"-i", "testsrc2=duration=2:size=320x240:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M", "-pix_fmt", "yuv420p",
		"--", filepath.Join(lib, "movie.mkv")).CombinedOutput()
	if err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\nvmaf_enable: false\n" + extraCfg
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, lib, state
}

// moveConfig rewrites the config file with a changed key, which is the act this whole
// phase is about: an operator edits the YAML and expects the tool to obey.
func moveConfig(t *testing.T, cfgPath, replacement string) {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	key := strings.SplitN(replacement, ":", 2)[0]
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, key+":") {
			continue
		}
		kept = append(kept, line)
	}
	body := strings.Join(kept, "\n") + replacement
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
