package engine

// Refuter artifact for S0049-holdfast-undo-6, finding F1. Report-only: it documents a
// defect, it does not fix one.
//
// AC3: "WHEN a retained original passes its configured retention THE SYSTEM SHALL
// release it and SHALL report the space that release returned."
//
// RunOneshot gates the release sweep on e.Cfg.UndoEnabled(), so an operator who turns
// the window off - the documented way to stop paying for it, and the shipped default -
// strands every original already retained: the second link stays on disk for ever, the
// ledger row stays live for ever, and the space is never returned. The startup notice
// that same configuration prints says "nothing retains the original", which is then
// false.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegress0049F1_TurningTheWindowOffStrandsAnAlreadyRetainedOriginal(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	clock := time.Now()
	eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 1, nil)
	eng.undoNow = func() time.Time { return clock }

	// Pass 1: the window is open, so the swap retains the original.
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	r := onlyRetained(t, ts)
	if !exists(r.RetainedPath) {
		t.Fatalf("nothing was retained at %s", r.RetainedPath)
	}
	held, err := ts.HeldByUndoWindow(context.Background())
	if err != nil {
		t.Fatalf("HeldByUndoWindow: %v", err)
	}
	if held != r.SourceBytes {
		t.Fatalf("held = %d, want the retained original's %d", held, r.SourceBytes)
	}

	// The operator decides the window costs too much disk and turns it off - which is
	// exactly what config.example.yaml documents 0 as - and runs another pass, well
	// past the retention every held original was given.
	eng.Cfg.UndoWindowHours = 0
	clock = clock.Add(90 * time.Minute)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("scan 2: %v", err)
	}

	if exists(r.RetainedPath) {
		fi, serr := os.Stat(r.RetainedPath)
		var size int64
		if serr == nil {
			size = fi.Size()
		}
		t.Errorf("AC3: the retained original passed its configured retention and was NOT released: %s is still on disk holding %d byte(s)",
			r.RetainedPath, size)
	}
	if rows := retainedRows(t, ts); len(rows) != 0 {
		t.Errorf("AC3: the ledger still holds %d retention(s) whose window closed: %+v", len(rows), rows)
	}
	held, err = ts.HeldByUndoWindow(context.Background())
	if err != nil {
		t.Fatalf("HeldByUndoWindow: %v", err)
	}
	if held != 0 {
		t.Errorf("AC15: the held figure did not fall to zero after the window closed: still %d byte(s)", held)
	}
}
