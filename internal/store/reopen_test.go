package store

import (
	"context"
	"testing"
)

// Re-opening a terminal row whose decision inputs have moved (S0080).
//
// A terminal row is a permanent exclusion keyed on path+fingerprint, and until this phase
// it was permanent FULL STOP: Claim answered done and skipped with a bare `return false`.
// The verdicts behind those rows were computed from ordinary editable YAML keys, so four
// routine edits became silent no-ops - the daemon ran, the dashboard was green, and
// nothing happened.
//
// Every case here is driven through the real Claim, on a real store, because the whole
// property lives inside that one transaction: the read of the row, the decision, and the
// write that takes the claim have to be atomic together or the two-claimants bug the
// method's comment describes comes back.

// terminalRow seeds one terminal row carrying reason and the inputs its decision read.
func terminalRow(t *testing.T, s *SQLite, path, fp string, st Status, reason string, in DecisionInputs) {
	t.Helper()
	ctx := context.Background()
	if ok, err := s.Claim(ctx, path, fp, "seed", 3, sameConfig); err != nil || !ok {
		t.Fatalf("seed claim %s: ok=%v err=%v", path, ok, err)
	}
	if err := s.Finish(ctx, path, fp, st, &Outcome{Reason: reason, DecisionInputs: in}, 3); err != nil {
		t.Fatalf("seed finish %s: %v", path, err)
	}
}

// lowBitrateAt is the record a low-bitrate skip writes: the threshold it compared the
// source against, and nothing else.
func lowBitrateAt(kbps string) DecisionInputs {
	return InputsRead(map[string]string{"min_bitrate_kbps": kbps})
}

// TestClaim_ReopensATerminalRowWhoseDecisionInputsMoved is THE test the item required to
// red against the shipped build: seed a `skipped / low-bitrate` row recorded at
// min_bitrate_kbps 2500, open with 1200, and Claim must hand the file over.
//
// Against `case st == Done || st == Skipped: return false, nil` this fails, which is the
// whole defect: the operator lowers the threshold and the files it was raised against are
// never looked at again.
func TestClaim_ReopensATerminalRowWhoseDecisionInputsMoved(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	terminalRow(t, s, "/lib/quiet.mkv", "10:100", Skipped, "low-bitrate", lowBitrateAt("2500"))

	lowered := InputsRead(map[string]string{"min_bitrate_kbps": "1200"})
	ok, err := s.Claim(ctx, "/lib/quiet.mkv", "10:100", "w0", 3, lowered)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !ok {
		t.Fatal("a skipped row recorded at min_bitrate_kbps 2500 must be re-opened under 1200: " +
			"the threshold the guard compared against has moved, so the verdict no longer re-derives")
	}

	// Re-opened means offered exactly as an unseen file is: the row is back in the
	// pipeline's hands, under this worker, carrying none of the previous verdict.
	st, _, exists, err := s.Get(ctx, "/lib/quiet.mkv", "10:100")
	if err != nil || !exists {
		t.Fatalf("Get after re-open: st=%v exists=%v err=%v", st, exists, err)
	}
	if st != Probing {
		t.Errorf("a re-opened row is at %q, want %q", st, Probing)
	}
	rows, err := s.List(ctx, []Status{Probing}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Outcome.Reason != "" || rows[0].Outcome.DecisionInputs.Recorded() {
		t.Errorf("a re-opened row still carries the previous verdict: %+v", rows)
	}
}

// TestClaim_StillRefusesATerminalRowWhoseInputsMatch is the other half, and it is the one
// that keeps this from being "re-encode everything on every scan". A row whose recorded
// values are still what the configuration holds is exactly as terminal as it ever was.
func TestClaim_StillRefusesATerminalRowWhoseInputsMatch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	terminalRow(t, s, "/lib/quiet.mkv", "10:100", Skipped, "low-bitrate", lowBitrateAt("2500"))
	terminalRow(t, s, "/lib/done.mkv", "20:200", Done, "", sameConfig)

	for _, c := range []struct{ path, fp string }{
		{"/lib/quiet.mkv", "10:100"},
		{"/lib/done.mkv", "20:200"},
	} {
		current := lowBitrateAt("2500")
		if c.path == "/lib/done.mkv" {
			current = sameConfig
		}
		ok, err := s.Claim(ctx, c.path, c.fp, "w0", 3, current)
		if err != nil {
			t.Fatalf("Claim(%s): %v", c.path, err)
		}
		if ok {
			t.Errorf("%s was re-opened under the very configuration it was decided under; "+
				"nothing has moved, so nothing may be re-derived", c.path)
		}
	}
}

// TestClaim_AnUnrelatedConfigKeyReopensNothing. The record is the values the guard READ
// and no others, which is why editing a notification URL or adding a library root leaves
// every row alone. A digest or a copy of the whole configuration would fail this, and it
// would fail it across an entire library at once.
func TestClaim_AnUnrelatedConfigKeyReopensNothing(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	terminalRow(t, s, "/lib/quiet.mkv", "10:100", Skipped, "low-bitrate", lowBitrateAt("2500"))

	// The same three keys a guard could have read, plus keys no guard reads at all -
	// which is what an operator changing a notification URL or a library root produces.
	widened := InputsRead(map[string]string{
		"min_bitrate_kbps": "2500",
		"encoder":          "cpu",
		"crf":              "22",
		"notify_url":       "shoutrrr://somewhere/else",
		"library_roots":    "/mnt/media:/mnt/more",
	})
	ok, err := s.Claim(ctx, "/lib/quiet.mkv", "10:100", "w0", 3, widened)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("a key no guard read moved and the row was re-opened anyway - the record is " +
			"tied to the whole configuration, so every unrelated edit re-opens the library")
	}
}

// TestClaim_ARowWithNoRecordedInputsIsNotAMatch. "Nothing recorded" is its own state and
// must never read as a zero, an empty string, or a match. Every row written before the
// column existed is in it.
func TestClaim_ARowWithNoRecordedInputsIsNotAMatch(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	// Finish with an Outcome carrying the zero DecisionInputs: the column goes to NULL.
	terminalRow(t, s, "/lib/old.mkv", "10:100", Skipped, "low-bitrate", DecisionInputs{})

	rows, err := s.List(ctx, []Status{Skipped}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("want one skipped row, got %d", len(rows))
	}
	in := rows[0].Outcome.DecisionInputs
	if in.Recorded() {
		t.Errorf("a row written with no inputs reads as RECORDING some (%q)", in.Encode())
	}
	if in.StillMatches(sameConfig) {
		t.Error("a row recording nothing MATCHED a configuration - it cannot match anything, " +
			"because there is nothing to compare")
	}
	// And the empty set is a different thing entirely: a decision that read no
	// configuration always matches, which is what stops a verdict nothing can move from
	// being re-opened on every scan for ever.
	if !InputsRead(nil).StillMatches(sameConfig) {
		t.Error("a decision that recorded reading NO configuration must always match")
	}
}

// TestClaim_ARowRecordingNoInputsIsReopenedOnceThenCarriesThem is the migration path in
// one test: an existing row is offered to the pipeline once, the decision it reaches
// records what it read, and the scan after that leaves it alone. Without the second half
// this would be a library re-opened on every scan for ever.
func TestClaim_ARowRecordingNoInputsIsReopenedOnceThenCarriesThem(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	terminalRow(t, s, "/lib/old.mkv", "10:100", Skipped, "low-bitrate", DecisionInputs{})

	current := lowBitrateAt("2500")
	ok, err := s.Claim(ctx, "/lib/old.mkv", "10:100", "w0", 3, current)
	if err != nil || !ok {
		t.Fatalf("a row recording no inputs must be re-opened: ok=%v err=%v", ok, err)
	}
	// The guards run again and reach the same verdict - in microseconds, with nothing
	// encoded - and THIS time the decision records what it read.
	if err := s.Finish(ctx, "/lib/old.mkv", "10:100", Skipped,
		&Outcome{Reason: "low-bitrate", DecisionInputs: current}, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	ok, err = s.Claim(ctx, "/lib/old.mkv", "10:100", "w0", 3, current)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatal("the row was re-opened a SECOND time under the same configuration: the decision " +
			"it reached did not record its inputs, so every scan re-opens it for ever")
	}
}

// TestClaim_NeverReopensAnIndeterminateOrAppliedDespiteErrorRow. Both are parked on a
// question no configuration answers.
//
// Indeterminate is a swap whose outcome could not be established: re-opening would mean
// re-encoding and re-swapping a path whose current contents are exactly what nobody knows.
// AppliedDespiteError is a swap that took effect: the file at the path IS the replacement,
// and this row describes an attempt that is over.
func TestClaim_NeverReopensAnIndeterminateOrAppliedDespiteErrorRow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	for _, st := range []Status{Indeterminate, AppliedDespiteError} {
		path := "/lib/" + string(st) + ".mkv"
		// Recorded under one configuration...
		terminalRow(t, s, path, "10:100", st, "", lowBitrateAt("2500"))
		// ... and asked for under a configuration that has moved as far as it can.
		ok, err := s.Claim(ctx, path, "10:100", "w0", 3, lowBitrateAt("1"))
		if err != nil {
			t.Fatalf("Claim(%s): %v", path, err)
		}
		if ok {
			t.Errorf("a %q row was re-opened by a configuration change; what is unknown (or already "+
				"applied) about it is not a configuration question", st)
		}
		// And a row recording nothing at all is refused just as flatly: the
		// not-recorded rule must not reach these two either.
		other := "/lib/unrecorded-" + string(st) + ".mkv"
		terminalRow(t, s, other, "20:200", st, "", DecisionInputs{})
		if ok, err := s.Claim(ctx, other, "20:200", "w0", 3, sameConfig); err != nil || ok {
			t.Errorf("a %q row recording no inputs was re-opened: ok=%v err=%v", st, ok, err)
		}
	}
}

// TestClaim_NeverReopensARestoredOriginalRowOnAConfigChange. The sharpest one in this
// file: that row is what stands between an operator's rescued bytes and the gates that
// passed the encode they rejected. Only a change to the FILE - a new fingerprint, hence a
// new row - may put it back in front of the encoder.
//
// It is written by RecordSkip, which records no inputs, so the not-recorded rule alone
// would re-open it on the very next scan. This is the check that stops that.
func TestClaim_NeverReopensARestoredOriginalRowOnAConfigChange(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	// Through the real writer: `holdfast restore` records the skip this way.
	if _, err := s.RecordSkip(ctx, "/lib/rescued.mkv", "10:100", GuardRestoredOriginal, Decision{}); err != nil {
		t.Fatalf("RecordSkip: %v", err)
	}
	rows, err := s.List(ctx, []Status{Skipped}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Outcome.DecisionInputs.Recorded() {
		t.Fatalf("the fixture is wrong: a restored-original row must record no inputs, got %+v", rows)
	}

	for _, current := range []DecisionInputs{sameConfig, movedConfig, DecisionInputs{}, lowBitrateAt("1")} {
		ok, err := s.Claim(ctx, "/lib/rescued.mkv", "10:100", "w0", 3, current)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if ok {
			t.Fatalf("a restored-original row was re-opened under %q - an operator deliberately "+
				"put that file back, and re-opening it feeds their rescued bytes to the very gates "+
				"that passed the encode they rejected", current.Encode())
		}
	}

	// A new fingerprint is a new row and IS claimable: changing the file is the one way
	// back into the pipeline, and it must still work.
	if ok, err := s.Claim(ctx, "/lib/rescued.mkv", "99:999", "w0", 3, sameConfig); err != nil || !ok {
		t.Fatalf("a NEW fingerprint for the same path must be claimable: ok=%v err=%v", ok, err)
	}
}

// TestClaim_ReopeningADoneRowDoesNotLowerTheLifetimeReclaimedTotal. Claiming clears the
// outcome columns, and a done row's sizes are what the published lifetime total is summed
// from - so re-opening one would take its bytes out of the figure an operator uses to
// judge whether the tool was worth running, at the next restart rather than at the claim.
// The contribution is carried into the same durable total a prune carries into.
func TestClaim_ReopeningADoneRowDoesNotLowerTheLifetimeReclaimedTotal(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	src, out := int64(5_000_000), int64(2_000_000)
	if ok, err := s.Claim(ctx, "/lib/done.mkv", "10:100", "seed", 3, sameConfig); err != nil || !ok {
		t.Fatalf("seed claim: ok=%v err=%v", ok, err)
	}
	if err := s.Finish(ctx, "/lib/done.mkv", "10:100", Done, &Outcome{
		SourceBytes: &src, OutputBytes: &out, DecisionInputs: sameConfig,
	}, 3); err != nil {
		t.Fatalf("seed finish: %v", err)
	}

	before, err := s.ReclaimedTotal(ctx)
	if err != nil {
		t.Fatalf("ReclaimedTotal: %v", err)
	}
	if before != src-out {
		t.Fatalf("the fixture reclaimed %d bytes, want %d", before, src-out)
	}

	if ok, err := s.Claim(ctx, "/lib/done.mkv", "10:100", "w0", 3, movedConfig); err != nil || !ok {
		t.Fatalf("the moved configuration must re-open the done row: ok=%v err=%v", ok, err)
	}
	after, err := s.ReclaimedTotal(ctx)
	if err != nil {
		t.Fatalf("ReclaimedTotal: %v", err)
	}
	if after != before {
		t.Errorf("re-opening a done row moved the lifetime reclaimed total from %d to %d", before, after)
	}

	// And it is not double-counted when the re-opened job finishes as done again: the
	// new row's own contribution is measured from where the previous one left off.
	src2, out2 := out, int64(1_000_000)
	if err := s.Finish(ctx, "/lib/done.mkv", "10:100", Done, &Outcome{
		SourceBytes: &src2, OutputBytes: &out2, DecisionInputs: movedConfig,
	}, 3); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	total, err := s.ReclaimedTotal(ctx)
	if err != nil {
		t.Fatalf("ReclaimedTotal: %v", err)
	}
	if want := src - out2; total != want {
		t.Errorf("after a re-encode the lifetime total is %d, want %d (the original minus what is "+
			"on disk now, counted once)", total, want)
	}
}

// TestSurveyDecisionInputs_CountsWhatTheNextScanWillReopen backs the startup and
// `validate` reports. The counts are what an operator is owed BEFORE the scan: a burst of
// activity they did not ask for should be announced, not discovered.
func TestSurveyDecisionInputs_CountsWhatTheNextScanWillReopen(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	terminalRow(t, s, "/lib/matching.mkv", "10:100", Skipped, "low-bitrate", sameConfig)
	terminalRow(t, s, "/lib/moved-a.mkv", "20:200", Skipped, "low-bitrate", movedConfig)
	terminalRow(t, s, "/lib/moved-b.mkv", "30:300", Done, "", movedConfig)
	terminalRow(t, s, "/lib/unrecorded.mkv", "40:400", Skipped, "low-bitrate", DecisionInputs{})
	// Neither of these is ever re-opened, so neither is counted: a failed row is not a
	// configuration question, and a restored-original row is refused outright.
	terminalRow(t, s, "/lib/failed.mkv", "50:500", Failed, "ffmpeg died", DecisionInputs{})
	if _, err := s.RecordSkip(ctx, "/lib/rescued.mkv", "60:600", GuardRestoredOriginal, Decision{}); err != nil {
		t.Fatalf("RecordSkip: %v", err)
	}

	got, err := s.SurveyDecisionInputs(ctx, sameConfig)
	if err != nil {
		t.Fatalf("SurveyDecisionInputs: %v", err)
	}
	want := DecisionInputsSurvey{Moved: 2, NotRecorded: 1, Matching: 1}
	if got != want {
		t.Errorf("survey = %+v, want %+v", got, want)
	}
	if got.Reopening() != 3 {
		t.Errorf("the next scan re-opens %d rows, want 3", got.Reopening())
	}
}

// TestDecisionInputs_RoundTripAndFailSafeParsing. The column is one TEXT value, so what
// goes in has to come back out - including a value carrying the separators the encoding
// uses, which an operator's preset or container extension could.
func TestDecisionInputs_RoundTripAndFailSafeParsing(t *testing.T) {
	awkward := InputsRead(map[string]string{
		"preset":        "slow;crf=99",
		"container_ext": "mkv",
	})
	back := ParseDecisionInputs(awkward.Encode())
	if !back.StillMatches(awkward) || !awkward.StillMatches(back) {
		t.Errorf("a value carrying the encoding's own separators did not round-trip: %q -> %+v",
			awkward.Encode(), back)
	}
	if v, ok := back.Value("preset"); !ok || v != "slow;crf=99" {
		t.Errorf("preset came back as %q (ok=%v)", v, ok)
	}

	// Anything unreadable is NOT RECORDED, which re-opens the row once and lets the
	// decision it reaches write something this build can read. Reading it as a match
	// would hold a file out of the pipeline on the strength of a record nothing can
	// interpret.
	for _, bad := range []string{"no-equals-sign", "a=1;dangling", "%zz=1"} {
		if in := ParseDecisionInputs(bad); in.Recorded() {
			t.Errorf("%q parsed as a RECORD (%+v); an unreadable value must read as not recorded", bad, in)
		}
	}
	if in := ParseDecisionInputs(""); in.Recorded() {
		t.Error("the empty column must read as not recorded")
	}
	if in := ParseDecisionInputs(InputsRead(nil).Encode()); !in.Recorded() || len(in.Keys()) != 0 {
		t.Errorf("a record of the empty set must round-trip as a record of the empty set, got %+v", in)
	}
}
