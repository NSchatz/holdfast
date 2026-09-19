package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// What the daemon records about a migration it just ran.
//
// A migration is the one moment the shape underneath an operator's evidence moves, and
// until it was recorded the only report of it was that nothing had crashed. These grade
// the record itself: one structured object per step, at a level that means what it says.

// jsonLog is a logger whose every line is one JSON object, and the buffer it writes to.
// The production logger renders text for a terminal; what these assert is that the record
// is built from ATTRIBUTES rather than from a sentence, which is exactly what a JSON
// handler renders as one object per line and a formatted string could not.
func jsonLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})), &buf
}

// decodeLines parses every emitted line as one JSON object, failing on the first that is
// not one.
func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("an emitted line is not one JSON object: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// [AC-11] Each applied step is emitted as a structured record carrying the step, the
// version it stamps and each table's count before and after, at a level below error.
func TestMigrationRecord_EachAppliedStepIsOneStructuredRecordBelowError(t *testing.T) {
	log, buf := jsonLog()
	reportMigrations(log, []store.MigrationStep{
		{Version: 12, Name: "deciding library profile", Tables: []store.TableRows{
			{Table: "jobs", Before: 7, After: 7},
		}},
		{Version: 13, Name: "record version stamp", Tables: []store.TableRows{
			{Table: "jobs", Before: 7, After: 7},
			{Table: "ledger_totals", Before: 1, After: 1},
		}},
	})

	lines := decodeLines(t, buf)
	if len(lines) != 2 {
		t.Fatalf("two steps applied and %d record(s) were emitted: %s", len(lines), buf)
	}
	for _, rec := range lines {
		if got := rec["level"]; got != "INFO" {
			t.Errorf("an applied step was recorded at level %v, want a level below error", got)
		}
		if _, ok := rec["msg"].(string); !ok {
			t.Errorf("the record carries no message field: %v", rec)
		}
		if got := rec["component"]; got != "store.migrate" {
			t.Errorf("the record names component %v, want store.migrate", got)
		}
	}

	second := lines[1]
	if got := second["step"]; got != float64(13) {
		t.Errorf("the record names step %v, want 13", got)
	}
	if got := second["step_name"]; got != "record version stamp" {
		t.Errorf("the record names %v, want the step's name", got)
	}
	if got := second["stamped_schema_version"]; got != float64(13) {
		t.Errorf("the record says it stamped %v, want 13", got)
	}
	rows, ok := second["rows"].(map[string]any)
	if !ok {
		t.Fatalf("the record carries no per-table counts: %v", second)
	}
	jobs, ok := rows["jobs"].(map[string]any)
	if !ok {
		t.Fatalf("the record carries no count for jobs: %v", rows)
	}
	if jobs["before"] != float64(7) || jobs["after"] != float64(7) {
		t.Errorf("the record says jobs went from %v to %v, want 7 either side",
			jobs["before"], jobs["after"])
	}
	if _, ok := rows["ledger_totals"].(map[string]any); !ok {
		t.Errorf("the record counts only some of the tables the step reported: %v", rows)
	}
}

// [AC-11] A step REFUSED for moving rows it did not declare is the one migration record at
// `error`, and it carries the numbers that are the finding.
//
// The other half matters as much: any other reason an open failed is NOT recorded here.
// A process that logs at error for a condition it handled trains its reader to ignore the
// level, and this is the one that means a human must act.
func TestMigrationRecord_OnlyARefusedStepIsRecordedAtError(t *testing.T) {
	log, buf := jsonLog()
	refusal := fmt.Errorf("store: migration 13 (record version stamp): %w", &store.UndeclaredRowChangeError{
		Version: 13, Step: "record version stamp", Table: "jobs",
		Declared: 0, Observed: -1, Before: 7, After: 6,
	})
	if !reportMigrationRefusal(log, refusal) {
		t.Fatal("an undeclared-row-change refusal was not recognised as one")
	}

	lines := decodeLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("one refusal and %d record(s): %s", len(lines), buf)
	}
	rec := lines[0]
	if got := rec["level"]; got != "ERROR" {
		t.Errorf("a refused step was recorded at level %v, want ERROR", got)
	}
	for field, want := range map[string]any{
		"component":     "store.migrate",
		"step":          float64(13),
		"step_name":     "record version stamp",
		"table":         "jobs",
		"rows_before":   float64(7),
		"rows_after":    float64(6),
		"rows_changed":  float64(-1),
		"rows_declared": float64(0),
	} {
		if got := rec[field]; got != want {
			t.Errorf("the refusal record carries %s=%v, want %v", field, got, want)
		}
	}

	// An open that failed for any other reason is not this record.
	other, otherBuf := jsonLog()
	if reportMigrationRefusal(other, errors.New("store: open jobs.db: permission denied")) {
		t.Error("an unrelated failure was reported as an undeclared row-count change")
	}
	if otherBuf.Len() != 0 {
		t.Errorf("an unrelated failure emitted a migration record: %s", otherBuf)
	}

	// And an open that applied nothing says nothing: a daemon migrates once in its life
	// and restarts many times.
	quiet, quietBuf := jsonLog()
	reportMigrations(quiet, nil)
	if quietBuf.Len() != 0 {
		t.Errorf("an open that applied no step emitted %s", quietBuf)
	}
}

// [AC-11] The DAEMON emits it. `run` and `serve` build their engine through one function,
// and this drives that function against a real ledger an earlier build wrote: the record
// that reaches the log is the one the store handed back from the open it just did.
//
// This is the test that would catch the record being built correctly and never emitted.
func TestBuildEngine_TheDaemonEmitsTheRecordOfTheMigrationItJustRan(t *testing.T) {
	cfgPath, _, stateDir, _ := preflightLibrary(t, "")
	seedOlderLedger(t, stateDir)

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	log, buf := jsonLog()
	eng, st, code := buildEngine(cfg, log, io.Discard, classifyScope{})
	if code != 0 || eng == nil || st == nil {
		t.Fatalf("buildEngine exit code = %d", code)
	}
	defer func() { _ = st.Close() }()

	if got := readSchemaStamp(t, filepath.Join(stateDir, "jobs.db")); got == olderSchemaVersion {
		t.Fatal("the fixture was not migrated, so what the daemon recorded about it proves nothing")
	}

	var applied map[string]any
	for _, rec := range decodeLines(t, buf) {
		if rec["component"] == "store.migrate" {
			applied = rec
		}
	}
	if applied == nil {
		t.Fatalf("the daemon migrated the ledger and recorded nothing about it: %s", buf)
	}
	if got := applied["level"]; got != "INFO" {
		t.Errorf("the daemon recorded an applied step at level %v, want a level below error", got)
	}
	rows, ok := applied["rows"].(map[string]any)
	if !ok {
		t.Fatalf("the daemon's record carries no per-table counts: %v", applied)
	}
	jobs, ok := rows["jobs"].(map[string]any)
	if !ok {
		t.Fatalf("the daemon's record carries no count for jobs: %v", rows)
	}
	// seedOlderLedger writes two terminal rows, and the step that migrates them moves
	// neither: the counts in the log are the counts in the file.
	if jobs["before"] != float64(2) || jobs["after"] != float64(2) {
		t.Errorf("the daemon recorded jobs going from %v to %v, want 2 either side",
			jobs["before"], jobs["after"])
	}
}
