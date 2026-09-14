package engine

import (
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0079 F10 - what a terminal row records once an encode profile can supply a job's
// settings: the values THAT JOB's settings resolved to, for that row's own path.
//
// The advisory this pins asked for exactly that, and the narrowing that answered it then -
// record the library root's values, because both comparisons read the root's profile and
// neither could see a pattern match - was true of the machinery and not of the rule. A
// decision input has one job, to be COMPARED against the configuration in force, and both
// sides now resolve the same way for the same path:
//
//   - store.Claim asks DecisionInputs.StillMatches against Engine.inputsFor(prof, ts),
//     where ts is the job's effective settings for its own path;
//   - the survey behind the startup report and `validate` asks the same question per ROW,
//     about that row's own path (store.InputsForPath), rather than measuring the whole
//     ledger against one value with no path in hand.
//
// So the row records what the encode was taken under AND still re-derives: parts 3 and 4
// below drive both directions, because the property that makes this safe is symmetry and
// not the choice of layer. Recording the ROOT's crf would now be the reading that breaks -
// it is what part 4 falsifies.
//
// [AC-1] [AC-2] of S0122: the read-set rule, resolved per path on both sides.
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
	// settings, which is what a reader asks of a ledger once two settings can run in one
	// scan. It is unchanged by the record below and is not a substitute for it.
	if row.Outcome.Profile != "bulk" {
		t.Fatalf("the row records profile %q, want %q", row.Outcome.Profile, "bulk")
	}

	// 2. THE RECORD. crf is the one the encode was taken under, which is the one the
	// comparison resolves for this path.
	got, ok := row.Outcome.DecisionInputs.Value(InputCRF)
	if !ok {
		t.Fatalf("the done row recorded no crf at all: %+v", row.Outcome.DecisionInputs)
	}
	if got != "30" {
		t.Errorf("the done row records crf=%q, want %q - the value the encode profile that selected "+
			"this path supplied, which is what the decision read and what a later claim resolves for "+
			"the same path", got, "30")
	}

	// 3. THE PROPERTY. The row re-derives under the very configuration that wrote it, so
	// the next scan leaves the file alone and `validate` reports it as moved zero times.
	// This is symmetry, and it is what a per-path record would cost if only one side of the
	// comparison had been moved.
	current, rooted := DecisionInputsPerPath(cfg)(path)
	if !rooted {
		t.Fatalf("%s resolved to no configured library root, so this case is not asking about the "+
			"resolution it says it is", path)
	}
	if !row.Outcome.DecisionInputs.StillMatches(current) {
		t.Errorf("the done row does not re-derive under the configuration that wrote it "+
			"(recorded %q, current %q) - so every scan would offer this file back and every "+
			"report would count it as moved, for ever",
			row.Outcome.DecisionInputs.Encode(), current.Encode())
	}

	// 4. THE FALSIFICATION. The reading this test used to assert, driven for real: a record
	// carrying the LIBRARY ROOT's crf, compared exactly as Claim and the survey compare it.
	// It no longer matches, which is why the narrowing had to go rather than be kept - a row
	// written that way would be offered back on every scan for the life of the row.
	narrowed := store.InputsRead(map[string]string{InputCRF: "22"})
	if narrowed.StillMatches(current) {
		t.Errorf("a row recording the library root's own crf (22) still matches the configuration in "+
			"force for this path (%q), so the two readings are indistinguishable here and this case "+
			"grades nothing", current.Encode())
	}
}
