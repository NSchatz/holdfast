package engine

import (
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0079 F10 - a terminal row records a configuration value its decision did not read.
//
// This is the same root cause as F9 seen from the other end, and it is advisory
// because no acceptance criterion in the spec binds decision_inputs. What it
// contradicts is text this repository SHIPS, in docs/requeue.md:
//
//	"every terminal row records the configuration values the decision that wrote
//	 it actually read, and no others"
//	"| `done` | `target_codec`, `encoder`, `crf`, `preset` - what the encode was
//	 taken under |"
//
// After this diff the encode is taken under config.Transcode (the root's profile
// overlaid with the matching encode profile), and the row records
// Engine.inputsRead(prof, ...) - the ROOT's profile alone. When an encode profile
// overrides any of those four keys, the permanent record of an irreversible swap
// names settings that did not run, and the row contradicts its own `encoder` and
// `profile` columns, which DO carry the job's values.
//
// The consequence beyond the record: editing that encode profile's crf, preset or
// encoder moves nothing the row recorded, so no scan ever offers the file back.
func TestRegressS0079F10_ADoneRowRecordsTheSettingsTheEncodeWasActuallyTakenUnder(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	// A real libx265 encode, taken under the PROFILE's crf rather than the root's:
	// baseCfg is crf 22, the profile that matches this file says 30, and buildArgs is
	// handed config.Transcode - so 30 is the number that reached ffmpeg.
	ts := run(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.EncodeProfiles = []config.EncodeProfile{
			{Name: "bulk", Match: "*.mkv", CRF: intp(30)},
		}
	})

	path := ts.findPath(t, "film.mkv")
	if path == "" {
		t.Fatal("no row for film.mkv")
	}
	row := rowFor(t, ts, path)
	if row.Status != store.Done {
		t.Fatalf("film.mkv is %q/%q, want done - the fixture never reached the swap, so this case proves nothing",
			row.Status, row.Outcome.Reason)
	}
	if row.Outcome.Profile != "bulk" {
		t.Fatalf("the row records profile %q, want %q - the profile did not supply this job's settings",
			row.Outcome.Profile, "bulk")
	}

	got, ok := row.Outcome.DecisionInputs.Value(InputCRF)
	if !ok {
		t.Fatalf("the done row recorded no crf at all: %+v", row.Outcome.DecisionInputs)
	}
	if got != "30" {
		t.Errorf("the done row records crf=%q; the encode was taken under crf 30, which is what the encode "+
			"profile named on the row (%q) supplied. docs/requeue.md says a terminal row records \"the "+
			"configuration values the decision that wrote it actually read, and no others\" and lists crf among "+
			"what a done row records as \"what the encode was taken under\". The row records the LIBRARY ROOT's "+
			"value instead, so it names a setting that did not run - and editing that profile's crf moves nothing "+
			"the row recorded, so no scan ever offers the file back",
			got, row.Outcome.Profile)
	}
}
