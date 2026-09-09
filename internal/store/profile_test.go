package store

import (
	"context"
	"testing"
)

// AC-A10, the ledger half: a job reaching a terminal state records the name of the
// profile that supplied its settings, and "" when the top-level settings did.
//
// "" is a REAL value for this field and not a missing measurement - it says the
// top-level settings ran - so the round trip through NULL has to come back as "" and
// not as anything else.
func TestOutcome_TheProfileThatSuppliedTheSettingsIsOnTheTerminalRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	cases := []struct {
		path    string
		profile string
		status  Status
	}{
		{"/lib/4k.mkv", "4k-av1", Done},
		{"/lib/plain.mkv", "", Done},
		{"/lib/skipped.mkv", "bulk-tv", Skipped},
		{"/lib/failed.mkv", "bulk-tv", Failed},
	}
	for _, tc := range cases {
		if ok, err := s.Claim(ctx, tc.path, "fp", "w0", 3, sameConfig); err != nil || !ok {
			t.Fatalf("Claim(%s): ok=%v err=%v", tc.path, ok, err)
		}
		if err := s.Finish(ctx, tc.path, "fp", tc.status, &Outcome{
			Encoder: "svtav1", Profile: tc.profile, Reason: "r",
		}); err != nil {
			t.Fatalf("Finish(%s): %v", tc.path, err)
		}
	}

	rows, err := s.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]string{}
	for _, r := range rows {
		got[r.Path] = r.Outcome.Profile
	}
	for _, tc := range cases {
		if have, ok := got[tc.path]; !ok || have != tc.profile {
			t.Errorf("%s recorded profile %q (found=%v), want %q", tc.path, have, ok, tc.profile)
		}
	}
}

// An outcome belongs to an ATTEMPT, not to a file, and the profile is part of that
// outcome: a job re-claimed for a retry must not sit in probing/encoding/verifying
// still advertising the profile of the attempt that failed - the operator may have
// edited transcode_profiles between the two, which is exactly why the row is being
// re-tried.
func TestClaim_ARetryClearsThePreviousAttemptsProfile(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	if ok, err := s.Claim(ctx, "/lib/a.mkv", "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/a.mkv", "fp", Failed, &Outcome{Profile: "old", Reason: "boom"}); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if ok, err := s.Claim(ctx, "/lib/a.mkv", "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("retry Claim: ok=%v err=%v", ok, err)
	}

	rows, err := s.List(ctx, []Status{Probing}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 in-flight row, got %d", len(rows))
	}
	if rows[0].Outcome.Profile != "" {
		t.Fatalf("the re-claimed row still advertises profile %q from the attempt that failed", rows[0].Outcome.Profile)
	}
}

// The migration's own anti-vacuity arm: a database written before this column
// existed gains it in place, keeps its rows, and reads every one of them back as
// "the top-level settings ran" - which for this field is the TRUE answer for an old
// row rather than a fabricated one.
func TestMigrate_APreProfileDatabaseGainsTheColumnAndReadsAsTopLevel(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/lib/old.mkv", "fp", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/old.mkv", "fp", Done, &Outcome{Encoder: "cpu"}); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	rows, err := s.List(ctx, []Status{Done}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want 1 done row, got %d", len(rows))
	}
	if rows[0].Outcome.Profile != "" {
		t.Fatalf("a row written with no profile reads back as %q, want the empty string", rows[0].Outcome.Profile)
	}
	if rows[0].Outcome.Encoder != "cpu" {
		t.Fatalf("the rest of the outcome did not survive: %+v", rows[0].Outcome)
	}
}
