package store

import (
	"context"
	"testing"
)

// S0122 refuter finding F1, against [AC-7]: "IF a terminal row's path resolves to no
// configured library root ... THE SYSTEM SHALL report that condition ... naming what it
// could not resolve".
//
// classifyRecordedInputs returns before it ever resolves the path of a row that recorded
// NOTHING, so the unrooted condition is never detected for that class of row. That class is
// not an edge: it is every row of a ledger an earlier build wrote, which is the population
// docs/requeue.md itself calls "the upgrade you most want the figures for".
//
// The shipped documentation this branch writes asserts the behaviour this case asks for:
// "A row whose path lies under no configured library root is reported separately and named:
// the scan walks the configured roots, so it never reaches that file and never re-opens it,
// WHATEVER THE ROW RECORDS" (docs/requeue.md).
func TestRegressS0122F1_AnUnrootedRowThatRecordedNothingIsNeitherCountedNorNamed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// The ledger an upgrade meets: a row written before the column existed (records
	// nothing) whose library root has since been renamed away, beside a live row that is
	// still under a configured root.
	terminalRow(t, s, "/gone/a.mkv", "10:100", Skipped, "already-at-target-codec", DecisionInputs{})
	terminalRow(t, s, "/lib/here.mkv", "20:200", Skipped, "already-at-target-codec", sameConfig)

	rooted := func(path string) (DecisionInputs, bool) {
		return sameConfig, path == "/lib/here.mkv"
	}
	got, err := s.SurveyDecisionInputs(ctx, rooted)
	if err != nil {
		t.Fatalf("SurveyDecisionInputs: %v", err)
	}

	want := DecisionInputsSurvey{NotRecorded: 1, Matching: 1, Unrooted: 1, UnrootedExample: "/gone/a.mkv"}
	if got != want {
		t.Errorf("surveyed %+v, want %+v - the row is counted in Reopening(), which tells the "+
			"operator the next scan will offer that file back, and the annotation that says the "+
			"scan never reaches it is silently absent for exactly the rows an upgrade holds",
			got, want)
	}
}
