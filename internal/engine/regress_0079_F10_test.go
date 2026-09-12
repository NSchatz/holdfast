package engine

import (
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0079 F10 - what a terminal row records once an encode profile can supply a job's
// settings, and why it is the LIBRARY ROOT's values and not the job's.
//
// The advisory this pins asked for the opposite: that a done row record the crf the
// encode was actually taken under, because docs/requeue.md says a terminal row records
// "the configuration values the decision that wrote it actually read, and no others".
// Taken literally that is right, and taking it literally is what this test refuses -
// with evidence, because the naive reading breaks the machinery the sentence exists to
// describe.
//
// A decision input has exactly one job: to be COMPARED against the configuration in
// force, so a verdict can be re-derived instead of standing for ever. Both comparisons
// read the LIBRARY ROOT's profile and neither can see a pattern match:
//
//   - store.Claim asks DecisionInputs.StillMatches against Engine.inputsFor(prof);
//   - store.SurveyLedgerDecisionInputs - the count `run`, `serve` and `validate` print
//     as "rows taken under a configuration that has since moved" - compares EVERY
//     terminal row against ONE value, DecisionInputsFor(cfg), with no path in hand at
//     all. It is a single GROUP BY over the encoded column.
//
// So a row that recorded the profile's crf would never match either side again. Not
// once: on every scan and in every report, for the life of that row. The second half
// below drives exactly that and shows it failing, which is what makes this a decision
// rather than a preference.
//
// Nothing is lost by the narrowing, because the attribution is on the row separately:
// Outcome.Profile names the encode profile that supplied the settings (AC-A10), so a
// reader holding the row and the configuration can resolve what ran. What it costs is
// stated in docs/requeue.md and in inputs.go rather than papered over - editing an
// encode profile re-opens nothing, and `holdfast requeue` is the lever for that.
func TestRegressS0079F10_ADoneRowRecordsTheInputsItsReDerivationCompares(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	// A real libx265 encode taken under the PROFILE's crf: baseCfg is crf 22, the
	// profile that matches this file says 30, and buildArgs is handed the job's
	// effective settings - so 30 is the number that reached ffmpeg.
	profiles := []config.EncodeProfile{{Name: "bulk", Match: "*.mkv", CRF: intp(30)}}
	cfg := baseCfg(root)
	cfg.EncodeProfiles = profiles
	ts := run(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.EncodeProfiles = profiles })

	path := ts.findPath(t, "film.mkv")
	if path == "" {
		t.Fatal("no row for film.mkv")
	}
	row := rowFor(t, ts, path)
	if row.Status != store.Done {
		t.Fatalf("film.mkv is %q/%q, want done - the fixture never reached the swap, so this case proves nothing",
			row.Status, row.Outcome.Reason)
	}

	// 1. THE ATTRIBUTION. The row names the encode profile that supplied this job's
	// settings, which is the half of "what ran" the inputs deliberately do not carry.
	if row.Outcome.Profile != "bulk" {
		t.Fatalf("the row records profile %q, want %q - without the attribution the narrowing below really would lose what ran",
			row.Outcome.Profile, "bulk")
	}

	// 2. THE RECORD. crf is the library root's, which is the value both comparisons ask
	// about.
	got, ok := row.Outcome.DecisionInputs.Value(InputCRF)
	if !ok {
		t.Fatalf("the done row recorded no crf at all: %+v", row.Outcome.DecisionInputs)
	}
	if got != "22" {
		t.Errorf("the done row records crf=%q, want %q - the library root's value, which is what "+
			"store.Claim and the ledger survey both compare a row against. The profile's own crf is "+
			"attributed by the profile column beside it", got, "22")
	}

	// 3. THE PROPERTY. The row re-derives under the very configuration that wrote it, so
	// the next scan leaves the file alone and `validate` reports it as moved zero times.
	current := DecisionInputsFor(cfg)
	if !row.Outcome.DecisionInputs.StillMatches(current) {
		t.Errorf("the done row does not re-derive under the configuration that wrote it "+
			"(recorded %q, current %q) - so every scan would offer this file back and every "+
			"report would count it as moved, for ever",
			row.Outcome.DecisionInputs.Encode(), current.Encode())
	}

	// 4. THE FALSIFICATION. The reading this test refuses, driven for real: a record
	// carrying the crf the encode was taken under, compared exactly as Claim and the
	// survey compare it. If this ever passes, the argument above has stopped being true
	// and the narrowing should be revisited rather than kept.
	naive := store.InputsRead(map[string]string{InputCRF: "30"})
	if naive.StillMatches(current) {
		t.Errorf("a row recording the encode profile's own crf (30) still matches the configuration "+
			"in force (%q), so recording the job's value would cost nothing and this narrowing is no "+
			"longer justified", current.Encode())
	}
}
