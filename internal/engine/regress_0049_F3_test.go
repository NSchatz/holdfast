package engine

// Refuter artifact for S0049-holdfast-undo-6, finding F3. Report-only.
//
// AC6: "IF a source carries an extra hard link that is an original THIS SYSTEM
// retained THEN THE SYSTEM SHALL process it on the normal path rather than skip it as
// hardlinked, and SHALL record no hardlinked skip for it."
//
// Engine.retainedLinks short-circuits to 0 when the window is disabled, so the
// discount that AC6 requires disappears the moment an operator sets
// undo_window_hours back to 0. A run interrupted between the link and the rename
// leaves exactly the state AC6 names - source present, one extra link, this tool's
// own - and with the window off the guard reads that link as a foreign seed and parks
// the file with a `hardlinked` skip that nothing can ever clear, because nothing ever
// removes the link.
//
// Same root cause as F1: the undo window's cleanup and compensation paths are gated
// on the window still being enabled, so turning it off strands whatever it left
// behind.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
)

func TestRegress0049F3_OurOwnRetainedLinkTripsTheHardlinkGuardOnceTheWindowIsOff(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	// The state an interrupted run leaves: the source present, plus this tool's own
	// second link in the retention area. Recorded in the ledger, which is the more
	// favourable of the two shapes the AC6 grader drives.
	eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
	u := eng.undo()
	retained, err := u.retain(src, probe.Fingerprint(src))
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	if err := u.record(context.Background(), src, src, retained, probe.FileSize(src)); err != nil {
		t.Fatalf("record: %v", err)
	}
	if n := nlinkOf(t, src); n != 2 {
		t.Fatalf("the fixture has %d links, want 2", n)
	}

	// The operator turns the window off. The link this tool took is still there.
	eng.Cfg.UndoWindowHours = 0

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Errorf("AC6: the file was not processed (codec %q) - this tool's own retained link was read as a foreign one", got)
	}
	for _, row := range skippedRows(t, ts) {
		if row.Outcome.Reason == SkipHardlinked {
			t.Errorf("AC6: a hardlinked skip was recorded for %s, whose only extra link this tool holds", row.Path)
		}
	}
}
