package store

// The durable record of a swap that did not complete cleanly, and the operator's way
// out of the state it creates.
//
// The engine's own tests prove the ENGINE reaches these records over real files; these
// prove the record itself: that the two writes are atomic, that a parked job is
// recognised by its recorded PATHS, that a recorded replacement path stays out of
// enumeration for as long as the record lives, and that the two refusals a resolution
// must never get past are enforced here rather than trusted to a caller.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func parkedIncident(sourcePath string) SwapIncident {
	return SwapIncident{
		SourcePath:        sourcePath,
		SourceFingerprint: "218376:1700000000",
		ReplacementPath:   filepath.Dir(sourcePath) + "/movie.__holdfast-replacement__.mkv",
		SourceAttrs:       "218376:1700000000",
		ReplacementAttrs:  "94779:1700000001",
		ObservedAttrs:     "218376:1700000000",
		Outcome:           Indeterminate,
		SwapError:         "swap failed: simulated - the storage is non-local (nfs)",
		StorageClass:      "non-local",
		StorageType:       "nfs",
		JobOutcome:        &Outcome{Encoder: "cpu", GuardAttributes: "size,mtime", GuardTimeResolution: "1s", GuardResidualWindow: ResidualWindowNetwork},
	}
}

// TestRecordSwapIncident_MovesTheJobRowAndTheRecordTogether. Two separate writes could
// leave a job in "indeterminate" with no record of which two files it is about, or a
// record with no job state behind it. Either half alone is worse than neither.
func TestRecordSwapIncident_MovesTheJobRowAndTheRecordTogether(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	in := parkedIncident("/lib/tv/movie.mkv")
	if ok, err := s.Claim(ctx, in.SourcePath, in.SourceFingerprint, "w0", 3); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.RecordSwapIncident(ctx, in); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}

	st, _, exists, err := s.Get(ctx, in.SourcePath, in.SourceFingerprint)
	if err != nil || !exists {
		t.Fatalf("Get: exists=%v err=%v", exists, err)
	}
	if st != Indeterminate {
		t.Errorf("job status = %q, want %q", st, Indeterminate)
	}
	got, ok, err := s.IncidentByID(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("IncidentByID: ok=%v err=%v", ok, err)
	}
	if !got.Parked() {
		t.Error("a freshly recorded indeterminate outcome is not parked")
	}
	// Everything an operator needs to identify both files without logs.
	if got.SourcePath != in.SourcePath || got.ReplacementPath != in.ReplacementPath {
		t.Errorf("recorded paths = %q / %q", got.SourcePath, got.ReplacementPath)
	}
	if got.SourceAttrs != in.SourceAttrs || got.ReplacementAttrs != in.ReplacementAttrs {
		t.Errorf("recorded attributes = %q / %q", got.SourceAttrs, got.ReplacementAttrs)
	}
	if got.StorageType != "nfs" || got.StorageClass != "non-local" {
		t.Errorf("recorded storage = %q/%q", got.StorageClass, got.StorageType)
	}
	// The job row carries the proof an ordinary terminal row would, including the
	// guard's granularity record - an operator reading the ledger should not have to
	// join to another table to learn why the swap did not complete.
	rows, err := s.List(ctx, []Status{Indeterminate}, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List: %d rows, err %v", len(rows), err)
	}
	if rows[0].Outcome.Reason != in.SwapError {
		t.Errorf("job reason = %q, want the recorded swap error", rows[0].Outcome.Reason)
	}
	if rows[0].Outcome.GuardResidualWindow != ResidualWindowNetwork {
		t.Errorf("job window = %q, want %q", rows[0].Outcome.GuardResidualWindow, ResidualWindowNetwork)
	}
}

// TestClaim_NeitherNewOutcomeIsEverReClaimed. A parked job must not be re-encoded or
// re-swapped in ANY run until an operator acts; an applied-despite-error job is over and
// is never re-attempted on the strength of that job.
func TestClaim_NeitherNewOutcomeIsEverReClaimed(t *testing.T) {
	for _, outcome := range []Status{Indeterminate, AppliedDespiteError} {
		t.Run(string(outcome), func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			in := parkedIncident("/lib/tv/movie.mkv")
			in.Outcome = outcome
			if ok, err := s.Claim(ctx, in.SourcePath, in.SourceFingerprint, "w0", 3); err != nil || !ok {
				t.Fatalf("Claim: ok=%v err=%v", ok, err)
			}
			if err := s.RecordSwapIncident(ctx, in); err != nil {
				t.Fatalf("RecordSwapIncident: %v", err)
			}
			// Every later attempt, in this run or any other.
			for i := 0; i < 3; i++ {
				ok, err := s.Claim(ctx, in.SourcePath, in.SourceFingerprint, "w1", 3)
				if err != nil {
					t.Fatalf("Claim: %v", err)
				}
				if ok {
					t.Fatalf("a %q job was re-claimed", outcome)
				}
			}
			// RecoverStale must not resurrect it either: it is terminal, not active.
			if n, err := s.RecoverStale(ctx); err != nil || n != 0 {
				t.Errorf("RecoverStale reset %d rows (err %v) - a %q job is not stale work", n, err, outcome)
			}
			if st, _, _, _ := s.Get(ctx, in.SourcePath, in.SourceFingerprint); st != outcome {
				t.Errorf("status after RecoverStale = %q, want %q", st, outcome)
			}
		})
	}
}

// TestExcludedReplacementPaths_IsKeyedToTheRecordAndNotToTheParkedState. A replacement
// retained THROUGH a resolution is still a file holdfast wrote; enumerating it would
// queue a gate-passed encode as a source and leave the library a permanent duplicate.
func TestExcludedReplacementPaths_IsKeyedToTheRecordAndNotToTheParkedState(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	in := parkedIncident("/lib/tv/movie.mkv")
	if err := s.RecordSwapIncident(ctx, in); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	excluded, err := s.ExcludedReplacementPaths(ctx)
	if err != nil {
		t.Fatalf("ExcludedReplacementPaths: %v", err)
	}
	if len(excluded) != 1 || excluded[0] != in.ReplacementPath {
		t.Fatalf("while parked, excluded = %v, want [%s]", excluded, in.ReplacementPath)
	}

	// Resolved, with the replacement RETAINED: still excluded.
	if err := s.ResolveIncident(ctx, 1, Resolution{
		Determination:          SourceIsIntact,
		By:                     "operator",
		DispositionSource:      KeptInPlace,
		DispositionReplacement: RetainedExcluded,
	}); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	excluded, err = s.ExcludedReplacementPaths(ctx)
	if err != nil {
		t.Fatalf("ExcludedReplacementPaths: %v", err)
	}
	if len(excluded) != 1 {
		t.Fatalf("after a retaining resolution, excluded = %v, want the replacement still held", excluded)
	}
	// And it is no longer PARKED, so a run stops reporting it and stops holding the
	// SOURCE path back.
	if parked, err := s.ParkedIncidents(ctx); err != nil || len(parked) != 0 {
		t.Errorf("a resolved job is still parked (%d, err %v)", len(parked), err)
	}
}

// TestExcludedReplacementPaths_DeletedAndAbsentReleaseThePath. A path with nothing at it
// must not hold back a scan forever.
func TestExcludedReplacementPaths_DeletedAndAbsentReleaseThePath(t *testing.T) {
	for _, d := range []Disposition{Deleted, Absent} {
		t.Run(string(d), func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			if err := s.RecordSwapIncident(ctx, parkedIncident("/lib/tv/movie.mkv")); err != nil {
				t.Fatalf("RecordSwapIncident: %v", err)
			}
			if err := s.ResolveIncident(ctx, 1, Resolution{
				Determination:          SwapWasApplied,
				By:                     "operator",
				DispositionSource:      KeptInPlace,
				DispositionReplacement: d,
			}); err != nil {
				t.Fatalf("ResolveIncident: %v", err)
			}
			excluded, err := s.ExcludedReplacementPaths(ctx)
			if err != nil {
				t.Fatalf("ExcludedReplacementPaths: %v", err)
			}
			if len(excluded) != 0 {
				t.Errorf("a %q replacement still excludes %v", d, excluded)
			}
		})
	}
}

// TestRecordSwapIncident_AnAppliedOutcomeIsBornWithAnAbsentReplacement. Reaching that
// outcome REQUIRES that nothing is at the recorded replacement path - a rename that took
// effect cannot account for two files - so "absent" is an established fact when the
// record is written, not an assumption.
func TestRecordSwapIncident_AnAppliedOutcomeIsBornWithAnAbsentReplacement(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	in := parkedIncident("/lib/tv/movie.mkv")
	in.Outcome = AppliedDespiteError
	if err := s.RecordSwapIncident(ctx, in); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	got, _, err := s.IncidentByID(ctx, 1)
	if err != nil {
		t.Fatalf("IncidentByID: %v", err)
	}
	if got.DispositionReplacement != Absent {
		t.Errorf("replacement disposition = %q, want %q", got.DispositionReplacement, Absent)
	}
	if got.Parked() {
		t.Error("an applied-despite-error job is parked - it needs no operator action")
	}
	if excluded, err := s.ExcludedReplacementPaths(ctx); err != nil || len(excluded) != 0 {
		t.Errorf("an absent replacement path is excluding %v", excluded)
	}
}

// TestResolveIncident_RefusesAResolutionThatWouldUndoTheRecord. Both refusals are the
// two ways a resolution could quietly undo the thing the record exists for.
func TestResolveIncident_RefusesAResolutionThatWouldUndoTheRecord(t *testing.T) {
	cases := []struct {
		name string
		res  Resolution
		want string
	}{
		{"no determination", Resolution{DispositionSource: KeptInPlace, DispositionReplacement: Deleted}, "determination"},
		{"a determination that is not one of the two", Resolution{Determination: "maybe", DispositionSource: KeptInPlace, DispositionReplacement: Deleted}, "determination"},
		{"no disposition for the source", Resolution{Determination: SourceIsIntact, DispositionReplacement: Deleted}, "source path has no disposition"},
		{"no disposition for the replacement", Resolution{Determination: SourceIsIntact, DispositionSource: KeptInPlace}, "replacement path has no disposition"},
		{"the replacement kept in place", Resolution{Determination: SourceIsIntact, DispositionSource: KeptInPlace, DispositionReplacement: KeptInPlace}, "never kept-in-place"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t)
			ctx := context.Background()
			if err := s.RecordSwapIncident(ctx, parkedIncident("/lib/tv/movie.mkv")); err != nil {
				t.Fatalf("RecordSwapIncident: %v", err)
			}
			err := s.ResolveIncident(ctx, 1, tc.res)
			if err == nil {
				t.Fatal("the resolution was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
			// Refused means NOTHING moved: the job is still parked and re-invocable.
			got, _, _ := s.IncidentByID(ctx, 1)
			if !got.Parked() {
				t.Error("a refused resolution left the job un-parked")
			}
		})
	}
}

// TestResolveIncident_ReleasesTheJobSoALaterRunTreatsThePathAsNewWork. The jobs row goes
// with the resolution, in the same transaction: a row still saying "indeterminate" would
// be a hold-back nothing states, and a source resolved "the source is intact" could
// never be reclaimed.
func TestResolveIncident_ReleasesTheJobSoALaterRunTreatsThePathAsNewWork(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	in := parkedIncident("/lib/tv/movie.mkv")
	if ok, err := s.Claim(ctx, in.SourcePath, in.SourceFingerprint, "w0", 3); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.RecordSwapIncident(ctx, in); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	if err := s.ResolveIncident(ctx, 1, Resolution{
		Determination:          SourceIsIntact,
		By:                     "operator",
		ObservedSource:         "218376:1700000000",
		ObservedReplacement:    "94779:1700000001",
		DispositionSource:      KeptInPlace,
		DispositionReplacement: RetainedExcluded,
	}); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}

	if _, _, exists, err := s.Get(ctx, in.SourcePath, in.SourceFingerprint); err != nil || exists {
		t.Errorf("the jobs row survived the resolution (exists=%v err=%v) - the source could never be reclaimed", exists, err)
	}
	// The source path is new work: it claims cleanly.
	if ok, err := s.Claim(ctx, in.SourcePath, in.SourceFingerprint, "w0", 3); err != nil || !ok {
		t.Errorf("a released source path did not claim: ok=%v err=%v", ok, err)
	}
	// The durable record of what the operator decided survives all of that.
	got, _, err := s.IncidentByID(ctx, 1)
	if err != nil {
		t.Fatalf("IncidentByID: %v", err)
	}
	if got.Resolution != SourceIsIntact || got.ResolvedBy != "operator" || got.ResolvedAt == 0 {
		t.Errorf("the resolution record is incomplete: %+v", got)
	}
	if got.DispositionSource != KeptInPlace || got.DispositionReplacement != RetainedExcluded {
		t.Errorf("dispositions = %q / %q", got.DispositionSource, got.DispositionReplacement)
	}
	if got.ObservedAtResolutionSource == "" || got.ObservedAtResolutionReplacement == "" {
		t.Error("what was observed at each path at that moment was not recorded")
	}
}

// TestResolveIncident_RefusesAJobThatIsNotParked is the store half of "reported
// distinctly from the absent-file case": there is nothing to resolve, and nothing moves.
func TestResolveIncident_RefusesAJobThatIsNotParked(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	res := Resolution{Determination: SourceIsIntact, DispositionSource: KeptInPlace, DispositionReplacement: RetainedExcluded}

	if err := s.ResolveIncident(ctx, 99, res); !errors.Is(err, ErrNotParked) {
		t.Errorf("resolving an id that does not exist = %v, want ErrNotParked", err)
	}
	in := parkedIncident("/lib/tv/movie.mkv")
	in.Outcome = AppliedDespiteError
	if err := s.RecordSwapIncident(ctx, in); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	if err := s.ResolveIncident(ctx, 1, res); !errors.Is(err, ErrNotParked) {
		t.Errorf("resolving an applied-despite-error job = %v, want ErrNotParked", err)
	}
	// And an already-resolved one.
	in2 := parkedIncident("/lib/tv/other.mkv")
	if err := s.RecordSwapIncident(ctx, in2); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	if err := s.ResolveIncident(ctx, 2, res); err != nil {
		t.Fatalf("first resolution: %v", err)
	}
	if err := s.ResolveIncident(ctx, 2, res); !errors.Is(err, ErrNotParked) {
		t.Errorf("resolving twice = %v, want ErrNotParked", err)
	}
}

// TestAmendReplacementDisposition_CorrectsARecordThatWouldOtherwiseLie is the store half
// of the licensed-removal-that-failed case: the record must not go on claiming a
// deletion that did not happen, and the surviving file must stay out of enumeration.
func TestAmendReplacementDisposition_CorrectsARecordThatWouldOtherwiseLie(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.RecordSwapIncident(ctx, parkedIncident("/lib/tv/movie.mkv")); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	if err := s.ResolveIncident(ctx, 1, Resolution{
		Determination:          SwapWasApplied,
		By:                     "operator",
		DispositionSource:      KeptInPlace,
		DispositionReplacement: Deleted,
	}); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}
	if excluded, _ := s.ExcludedReplacementPaths(ctx); len(excluded) != 0 {
		t.Fatalf("a deleted replacement is still excluding %v", excluded)
	}

	// The removal failed. Correct the record; the file is still there, so the path is
	// held out of enumeration again.
	if err := s.AmendReplacementDisposition(ctx, 1, RetainedExcluded, "unlink: read-only file system"); err != nil {
		t.Fatalf("AmendReplacementDisposition: %v", err)
	}
	excluded, err := s.ExcludedReplacementPaths(ctx)
	if err != nil || len(excluded) != 1 {
		t.Fatalf("after the correction, excluded = %v (err %v), want the surviving file held", excluded, err)
	}
	got, _, _ := s.IncidentByID(ctx, 1)
	if got.DispositionReplacement != RetainedExcluded {
		t.Errorf("disposition = %q, want %q", got.DispositionReplacement, RetainedExcluded)
	}
	if !strings.Contains(got.RemovalError, "read-only") {
		t.Errorf("the removal failure was not recorded: %q", got.RemovalError)
	}
	// The determination the operator gave is untouched by the correction.
	if got.Resolution != SwapWasApplied {
		t.Errorf("the determination changed to %q", got.Resolution)
	}
	// A replacement path may never be amended INTO kept-in-place either.
	if err := s.AmendReplacementDisposition(ctx, 1, KeptInPlace, ""); err == nil {
		t.Error("a replacement path was amended to kept-in-place")
	}
}

// TestParkedIncidents_ReturnsOnlyWhatIsAwaitingADetermination.
func TestParkedIncidents_ReturnsOnlyWhatIsAwaitingADetermination(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, p := range []string{"/lib/a.mkv", "/lib/b.mkv"} {
		if err := s.RecordSwapIncident(ctx, parkedIncident(p)); err != nil {
			t.Fatalf("RecordSwapIncident: %v", err)
		}
	}
	applied := parkedIncident("/lib/c.mkv")
	applied.Outcome = AppliedDespiteError
	if err := s.RecordSwapIncident(ctx, applied); err != nil {
		t.Fatalf("RecordSwapIncident: %v", err)
	}
	parked, err := s.ParkedIncidents(ctx)
	if err != nil {
		t.Fatalf("ParkedIncidents: %v", err)
	}
	if len(parked) != 2 {
		t.Fatalf("parked = %d, want 2 (an applied-despite-error job is not parked)", len(parked))
	}
	if parked[0].ID >= parked[1].ID {
		t.Error("parked jobs are not oldest-first")
	}
}

// TestAggregates_TheTwoSwapOutcomesAreCountedAsThemselves is AC15j on the whole-ledger
// figure. The outcome breakdown is a surface that reports job outcomes, so a parked job
// counted under `failed` would tell an operator the source is fine - the exact false
// comfort this item removes - and one left out of the set entirely would make the single
// job that is waiting for a human the only one this figure never sees. It also pins the
// COVERAGE string, because DASH-7's whole contract is that a figure states the set it
// covers, and a set that has grown while its description has not is a figure that lies.
func TestAggregates_TheTwoSwapOutcomesAreCountedAsThemselves(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Claim each first, exactly as the engine does: a swap incident is recorded
	// against the job row the pipeline already owns.
	parked := parkedIncident("/lib/parked.mkv")
	applied := parkedIncident("/lib/applied.mkv")
	applied.Outcome = AppliedDespiteError
	for _, in := range []SwapIncident{parked, applied} {
		if ok, err := s.Claim(ctx, in.SourcePath, in.SourceFingerprint, "w0", 3); err != nil || !ok {
			t.Fatalf("Claim(%s): ok=%v err=%v", in.SourcePath, ok, err)
		}
		if err := s.RecordSwapIncident(ctx, in); err != nil {
			t.Fatalf("RecordSwapIncident(%s): %v", in.SourcePath, err)
		}
	}
	if ok, err := s.Claim(ctx, "/lib/ordinary.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/ordinary.mkv", "fp1", Failed, &Outcome{Reason: "encode error"}); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	b := s.Aggregates(ctx).Outcomes
	if b.Err != nil {
		t.Fatalf("outcome counts: %v", b.Err)
	}
	got := map[string]int64{}
	for _, bucket := range b.Buckets {
		got[bucket.Key] = bucket.Count
	}
	for _, want := range []struct {
		key   string
		count int64
	}{
		{string(Indeterminate), 1},
		{string(AppliedDespiteError), 1},
		{string(Failed), 1},
	} {
		if got[want.key] != want.count {
			t.Errorf("the outcome breakdown counts %s = %d, want %d (buckets: %v)",
				want.key, got[want.key], want.count, got)
		}
	}
	if b.Counted != 3 {
		t.Errorf("counted = %d, want 3 - a terminal row was not counted at all", b.Counted)
	}
	for _, name := range []string{string(Indeterminate), string(AppliedDespiteError)} {
		if !strings.Contains(b.Coverage.Set, name) {
			t.Errorf("the stated coverage %q does not name %q, so the figure covers a set it does not describe",
				b.Coverage.Set, name)
		}
	}
}

// TestRecordSwapIncident_RefusesAnOutcomeThisRecordCannotCarry - the record is for the
// two outcomes this phase adds and nothing else.
func TestRecordSwapIncident_RefusesAnOutcomeThisRecordCannotCarry(t *testing.T) {
	s := openTest(t)
	in := parkedIncident("/lib/tv/movie.mkv")
	in.Outcome = Done
	if err := s.RecordSwapIncident(context.Background(), in); err == nil {
		t.Fatal("a done outcome was recorded as a swap incident")
	}
}

// TestMigrate_TheSwapColumnsLandOnADatabaseThatAlreadyExists. The repo's migration rule
// is append-only and version-stamped; this proves the swap step is a real ALTER on a
// database that already has rows, not a create that only a fresh file would get.
func TestMigrate_TheSwapColumnsLandOnADatabaseThatAlreadyExists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if ok, err := s.Claim(ctx, "/lib/movie.mkv", "fp1", "w0", 3); err != nil || !ok {
		t.Fatalf("Claim: ok=%v err=%v", ok, err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Re-open: the migration is idempotent, and the row survives.
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	defer func() { _ = s2.Close() }()
	if err := s2.RecordSwapIncident(ctx, parkedIncident("/lib/movie.mkv")); err != nil {
		t.Fatalf("RecordSwapIncident on a re-opened database: %v", err)
	}
	if _, _, exists, err := s2.Get(ctx, "/lib/movie.mkv", "fp1"); err != nil || !exists {
		t.Errorf("the pre-existing row did not survive (exists=%v err=%v)", exists, err)
	}
}
