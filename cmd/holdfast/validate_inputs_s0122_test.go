package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0122 - the counts `run`, `serve` and `validate` print, against a configuration that has
// encode profiles in it.
//
// The claim path and the report path can drift, and when they do the scan is correct while
// the figures an operator reads are not - the surface half of this defect, re-created one
// layer up. These cases grade them as one thing: the report classifies a row by the rule the
// scan applies to it, resolved for that row's own path.

// profiledLedgerConfig writes a config whose library root is selected by an encode profile,
// and returns the config path, the state directory and the library root.
func profiledLedgerConfig(t *testing.T) (cfgPath, state, lib string) {
	t.Helper()
	dir := t.TempDir()
	lib = filepath.Join(dir, "media")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatal(err)
	}
	state = filepath.Join(dir, "state")
	cfgPath = filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - " + lib + "\nstate_dir: " + state + "\ncrf: 22\n" +
		"encode_profiles:\n  - name: bulk\n    match: \"*.mkv\"\n    crf: 26\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath, state, lib
}

func loadConfigFile(t *testing.T, cfgPath string) *config.Config {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	return cfg
}

// TestValidateInputs_ClassifiesEachRowByTheRuleTheScanApplies grades [AC-5]: a row still
// matching the PROFILE-RESOLVED configuration is counted as matching and not as moved, and
// `validate` still reads the ledger read-only and passes.
//
// The fixture is what an upgrade actually holds: rows this build wrote, which record what
// the encode profile resolved, beside a row the previous build wrote, which records the
// library root's own value for the same key. Exactly one of them has moved. A report that
// measured the whole ledger against one value would answer the opposite way round - two
// moved, one matching - so the numbers below are the rule and not a restatement of it.
func TestValidateInputs_ClassifiesEachRowByTheRuleTheScanApplies(t *testing.T) {
	cfgPath, state, lib := profiledLedgerConfig(t)
	cfg := loadConfigFile(t, cfgPath)
	perPath := engine.DecisionInputsPerPath(*cfg)

	live := func(path string) store.DecisionInputs {
		in, rooted := perPath(path)
		if !rooted {
			t.Fatalf("%s resolved to no library root, so the fixture is not the one this case describes", path)
		}
		return in
	}
	inProfile, inRoot := live(filepath.Join(lib, "a.mkv")), engine.DecisionInputsFor(*cfg)
	if inProfile.Encode() == inRoot.Encode() {
		t.Fatalf("the encode profile resolves to the root's own values (%q), so nothing below can "+
			"tell the two readings apart", inRoot.Encode())
	}
	seedLedger(t, state, func(st *store.SQLite) {
		seedTerminalRow(t, st, filepath.Join(lib, "a.mkv"), store.Skipped, engine.SkipLowBitrate, inProfile)
		seedTerminalRow(t, st, filepath.Join(lib, "b.mkv"), store.Done, "", live(filepath.Join(lib, "b.mkv")))
		// The row a previous build left: the same keys, resolved from the library root alone.
		seedTerminalRow(t, st, filepath.Join(lib, "old.mkv"), store.Skipped, engine.SkipLowBitrate, inRoot)
	})
	before := sha256File(t, filepath.Join(state, "jobs.db"))

	code, out, errOut := cli(t, "validate", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("validate exited %d, want 0 (stderr: %s)", code, errOut)
	}
	for _, want := range []string{
		"1 terminal row(s) were taken under a configuration that has since moved",
		"0 terminal row(s) record no decision inputs",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("validate does not report %q - the two rows decided under the encode profile "+
				"are still in force and must be counted as matching:\n%s", want, out)
		}
	}
	// And it READ the ledger: a validate that migrated or rewrote an operator's store as a
	// side effect of describing it would make that file unopenable by the daemon still
	// running against it.
	if after := sha256File(t, filepath.Join(state, "jobs.db")); after != before {
		t.Error("validate changed the ledger it was describing")
	}
}

// TestValidateInputs_WithNoLedgerCreatesNothingAndPasses grades the rest of [AC-5]: the
// widening of what `validate` touches must never redden a first run or leave a database
// behind on an install that has never transcoded anything.
func TestValidateInputs_WithNoLedgerCreatesNothingAndPasses(t *testing.T) {
	cfgPath, state, _ := profiledLedgerConfig(t)

	code, out, errOut := cli(t, "validate", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("validate exited %d on a fresh install, want 0 (stderr: %s)", code, errOut)
	}
	if !strings.Contains(out, "config OK") || !strings.Contains(out, "none at") {
		t.Errorf("validate did not report a valid config with no ledger to read:\n%s", out)
	}
	if _, err := os.Stat(state); err == nil {
		t.Error("validate created the state directory")
	}
}

// TestValidateInputs_ReportsWhatItCouldNotResolveAndStillPasses grades the `validate` half
// of [AC-7]: a row whose path resolves to no configured library root, and a ledger that
// cannot be read at all, are both REPORTED - naming what could not be resolved - and neither
// fails the command.
func TestValidateInputs_ReportsWhatItCouldNotResolveAndStillPasses(t *testing.T) {
	t.Run("a row under no configured library root", func(t *testing.T) {
		cfgPath, state, lib := profiledLedgerConfig(t)
		cfg := loadConfigFile(t, cfgPath)
		perPath := engine.DecisionInputsPerPath(*cfg)
		here, _ := perPath(filepath.Join(lib, "here.mkv"))
		gone, rooted := perPath("/gone/film.mkv")
		if rooted {
			t.Fatal("/gone/film.mkv resolved to a configured root, so this case is not about an unrooted row")
		}
		seedLedger(t, state, func(st *store.SQLite) {
			seedTerminalRow(t, st, filepath.Join(lib, "here.mkv"), store.Skipped, engine.SkipLowBitrate, here)
			seedTerminalRow(t, st, "/gone/film.mkv", store.Skipped, engine.SkipLowBitrate, gone)
		})

		code, out, errOut := cli(t, "validate", "--config", cfgPath)
		if code != 0 {
			t.Fatalf("validate exited %d over a ledger holding an unrooted row, want 0 (stderr: %s)", code, errOut)
		}
		for _, want := range []string{"lie under no configured library root", "/gone/film.mkv"} {
			if !strings.Contains(out, want) {
				t.Errorf("validate does not report %q - a count about files an operator cannot find "+
					"is not a report:\n%s", want, out)
			}
		}
	})

	t.Run("a ledger that cannot be read at all", func(t *testing.T) {
		cfgPath, state, _ := profiledLedgerConfig(t)
		if err := os.MkdirAll(state, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(state, "jobs.db"), []byte("this is not SQLite"), 0o600); err != nil {
			t.Fatal(err)
		}

		code, out, errOut := cli(t, "validate", "--config", cfgPath)
		if code != 0 {
			t.Fatalf("validate exited %d over an unreadable ledger, want 0 - it validates a "+
				"CONFIGURATION, and the ledger is reported beside one that is still valid (stderr: %s)",
				code, errOut)
		}
		if !strings.Contains(out, "could not be read") {
			t.Errorf("validate does not report that the ledger could not be read:\n%s", out)
		}
	})
}

// logRecords parses what a logger wrote into one map per line. Every line is one JSON
// object, so a level and a field are read as a machine reads them rather than by matching
// substrings of a sentence.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("a log line is not one JSON object (%q): %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func levelled(recs []map[string]any, level string) []map[string]any {
	var out []map[string]any
	for _, r := range recs {
		if r["level"] == level {
			out = append(out, r)
		}
	}
	return out
}

// TestLogLedgerAgainstConfig_ReportsAtWarnAndNeverAtError grades the daemon half of [AC-7]:
// a ledger that cannot be read, and rows that resolve to no configured library root, are
// recorded at `warn` - the process continued in a degraded state - and never at `error`,
// which would mean a human must act. Each names what it could not resolve, what it tried and
// what happens next, so the record is a stated condition rather than a trace.
func TestLogLedgerAgainstConfig_ReportsAtWarnAndNeverAtError(t *testing.T) {
	const dbPath = "/var/lib/holdfast/jobs.db"

	t.Run("the ledger could not be read", func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		logLedgerAgainstConfig(log, dbPath, store.DecisionInputsSurvey{}, errors.New("file is not a database"))

		recs := logRecords(t, &buf)
		if n := len(levelled(recs, "ERROR")); n != 0 {
			t.Errorf("%d record(s) at ERROR: a count the scan does not depend on is not something a "+
				"human must act on, and a process that logs at error for a handled condition trains "+
				"its reader to ignore the level", n)
		}
		warns := levelled(recs, "WARN")
		if len(warns) != 1 {
			t.Fatalf("%d record(s) at WARN, want exactly 1: %v", len(warns), recs)
		}
		for _, key := range []string{"ledger", "tried", "next", "err"} {
			if _, ok := warns[0][key]; !ok {
				t.Errorf("the warn carries no %q - a failed dependency is recorded as which "+
					"dependency, what was tried and what happens next: %v", key, warns[0])
			}
		}
		if warns[0]["ledger"] != dbPath {
			t.Errorf("the warn names ledger=%v, want the file it could not read (%s)", warns[0]["ledger"], dbPath)
		}
	})

	t.Run("rows under no configured library root", func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		logLedgerAgainstConfig(log, dbPath, store.DecisionInputsSurvey{
			Matching: 3, Unrooted: 2, UnrootedExample: "/gone/film.mkv",
		}, nil)

		recs := logRecords(t, &buf)
		if n := len(levelled(recs, "ERROR")); n != 0 {
			t.Errorf("%d record(s) at ERROR for rows the scan simply never reaches", n)
		}
		warns := levelled(recs, "WARN")
		if len(warns) != 1 {
			t.Fatalf("%d record(s) at WARN, want exactly 1: %v", len(warns), recs)
		}
		if warns[0]["example"] != "/gone/film.mkv" {
			t.Errorf("the warn does not name what it could not resolve: %v", warns[0])
		}
		if warns[0]["rows"] != float64(2) {
			t.Errorf("the warn reports rows=%v, want 2", warns[0]["rows"])
		}
	})

	t.Run("a ledger with nothing to report", func(t *testing.T) {
		var buf bytes.Buffer
		log := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
		logLedgerAgainstConfig(log, dbPath, store.DecisionInputsSurvey{Matching: 4}, nil)

		recs := logRecords(t, &buf)
		if n := len(levelled(recs, "WARN")) + len(levelled(recs, "ERROR")); n != 0 {
			t.Errorf("%d record(s) above info on a ledger entirely in force - a run that warns when "+
				"nothing is degraded is a run whose warnings mean nothing: %v", n, recs)
		}
		if len(levelled(recs, "INFO")) == 0 {
			t.Error("the report said nothing at all; the counts are owed before the scan whether or " +
				"not anything has moved")
		}
	})
}
