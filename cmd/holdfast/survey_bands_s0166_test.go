package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0166's report half: the no-source-height figure is rendered as an upper bound and never
// folded into the count of files the next scan offers, the daemon's startup record carries it
// as a field of its own, and `validate` answers over a ledger older than the column.

// offersFiles matches a line stating a NON-ZERO count of files the next scan offers to the guards.
var offersFiles = regexp.MustCompile(`offers (those )?[1-9][0-9]* file`)

// TestS0166AC7_ANothingMovedSurveyOffersNothing grades AC-7: rendered for AC-1's survey (every
// row still matching), the lines state that 0 rows were taken under a moved configuration and
// that the next scan re-opens none of them, and no line names a file the scan offers.
func TestS0166AC7_ANothingMovedSurveyOffersNothing(t *testing.T) {
	lines := decisionInputsLines(store.DecisionInputsSurvey{Matching: 38})
	all := strings.Join(lines, "\n")
	if !strings.Contains(all, "0 terminal row(s) were taken under a configuration that has since moved") {
		t.Errorf("no line states that 0 rows were taken under a moved configuration:\n%s", all)
	}
	if !strings.Contains(all, "re-opens none of them") {
		t.Errorf("no line states that the next scan re-opens none of them:\n%s", all)
	}
	for _, l := range lines {
		if offersFiles.MatchString(l) {
			t.Errorf("a line names files the next scan offers to the guards: %s", l)
		}
	}
}

// TestS0166AC8_TheHeightlessRowsAreAnUpperBoundOfTheirOwn grades AC-8: a non-zero
// no-source-height figure gets a line of its own, stated as an upper bound and naming the
// source height as what decides, and it is not in the count the "next scan offers" line
// states; a zero figure gets no such line.
func TestS0166AC8_TheHeightlessRowsAreAnUpperBoundOfTheirOwn(t *testing.T) {
	s := store.DecisionInputsSurvey{Moved: 2, NotRecorded: 1, Matching: 4, NoSourceHeight: 3}
	lines := decisionInputsLines(s)
	var bound, offers string
	for _, l := range lines {
		if strings.Contains(l, "up to ") {
			bound = l
		}
		if strings.Contains(l, "offers those") {
			offers = l
		}
	}
	if !strings.Contains(bound, "up to 3") || !strings.Contains(bound, "source height") {
		t.Errorf("no line gives the 3 heightless rows as an upper bound decided by the source height:\n%s",
			strings.Join(lines, "\n"))
	}
	if !strings.Contains(offers, "offers those 3 file(s)") {
		t.Errorf("the offers line must state moved plus not recorded (3), not the upper bound too: %q", offers)
	}

	s.NoSourceHeight = 0
	for _, l := range decisionInputsLines(s) {
		if strings.Contains(l, "up to ") || strings.Contains(l, "store no source height") {
			t.Errorf("a zero no-source-height figure still rendered a line: %s", l)
		}
	}
}

// TestS0166AC9_TheStartupRecordCarriesTheFigure grades AC-9: the daemon's startup record for
// the survey carries the no-source-height figure as its own field, and its re-open field stays
// moved plus not recorded, without it.
func TestS0166AC9_TheStartupRecordCarriesTheFigure(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	logLedgerAgainstConfig(log, "/state/jobs.db",
		store.DecisionInputsSurvey{Moved: 2, NotRecorded: 1, Matching: 4, NoSourceHeight: 3}, nil)

	var record string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `msg="ledger against this configuration"`) {
			record = l
		}
	}
	if record == "" {
		t.Fatalf("no \"ledger against this configuration\" record:\n%s", buf.String())
	}
	for _, want := range []string{" rows_on_banded_roots_with_no_source_height=3", " rows_this_scan_reopens=3 "} {
		if !strings.Contains(record+" ", want) {
			t.Errorf("the record lacks %q:\n%s", strings.TrimSpace(want), record)
		}
	}
}

// TestS0166AC10_ValidateAnswersOverALedgerOlderThanTheColumn grades AC-10: over a ledger whose
// jobs table has no source_height column, `validate` with a banded root exits 0, counts every
// row on that root in the no-source-height figure, and leaves the ledger file byte-identical.
// The read turns on the columns it needs, not on the version stamp (store.SurveyLedgerDecisionInputs),
// so the ledger is written by this build and the column is then taken away.
func TestS0166AC10_ValidateAnswersOverALedgerOlderThanTheColumn(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "media")
	state := filepath.Join(dir, "state")
	for _, d := range []string{lib, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := "library_roots:\n  - path: " + lib + "\n    min_bitrate_kbps: 2500\n    rules:\n" +
		"      - when:\n          min_source_height: 720\n        min_bitrate_kbps: 4000\n" +
		"state_dir: " + state + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	dbPath := filepath.Join(state, "jobs.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	ctx := context.Background()
	const rows = 4
	for i := 0; i < rows; i++ {
		path := filepath.Join(lib, fmt.Sprintf("film-%d.mkv", i))
		if ok, err := st.Claim(ctx, path, "fp", "seed", 3, store.DecisionInputs{}); err != nil || !ok {
			t.Fatalf("seed claim %s: ok=%v err=%v", path, ok, err)
		}
		h := 1080
		o := &store.Outcome{Reason: engine.SkipLowBitrate, SourceHeight: &h,
			DecisionInputs: store.InputsRead(map[string]string{engine.InputMinBitrateKbps: strconv.Itoa(4000)})}
		if err := st.Finish(ctx, path, "fp", store.Skipped, o, 3); err != nil {
			t.Fatalf("seed finish %s: %v", path, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE jobs DROP COLUMN source_height`); err != nil {
		t.Fatalf("take the source_height column away: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := sha256File(t, dbPath)

	code, stdout, stderr := cli(t, "validate", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("validate exited %d:\n%s\n%s", code, stdout, stderr)
	}
	if want := fmt.Sprintf("up to %d further terminal row(s)", rows); !strings.Contains(stdout, want) {
		t.Errorf("validate did not count the %d heightless rows as the upper bound (%q):\n%s", rows, want, stdout)
	}
	if !strings.Contains(stdout, "0 terminal row(s) were taken under a configuration that has since moved") {
		t.Errorf("validate counted heightless rows as moved:\n%s", stdout)
	}
	if after := sha256File(t, dbPath); after != before {
		t.Errorf("validate changed the ledger it only read (sha256 %s -> %s)", before, after)
	}
}
