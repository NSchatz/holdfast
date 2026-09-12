package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// S0079 F7 - AC-A10 is not satisfied for every terminal row.
//
// AC-A10: "WHEN a job reaches a terminal state, THE SYSTEM SHALL record on its
// ledger row the name of the profile that supplied its settings, empty when the
// top-level settings were used".
//
// The hardlink guard is a terminal state: it writes a `skipped` row through
// store.RecordSkip (engine.go, the e.Cfg.HardlinkSkip() block). RecordSkip's SQL
// sets `profile = NULL` unconditionally (internal/store/sqlite.go), and it takes no
// Outcome, so the row records "" however the profiles resolved. The very same guard
// EMITS becauseIn(ts, SkipHardlinked), which carries the profile name - so the live
// event and the durable ledger row disagree about the same job, and the branch's own
// docs/api-reference.md row ("`profile` | every terminal row") describes the event
// rather than the ledger.
//
// The same hole is at internal/engine/undo.go's SkipRestoredOriginal RecordSkip
// call; one case proves the class.
//
// This test documents the defect. It is expected to FAIL on
// sdd/S0079-holdfast-transcode-config at 5fa0683.
func TestRegressS0079F7_AHardlinkSkipRecordsTheProfileOnItsLedgerRow(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	// A second hard link, outside the library root so the scan never enumerates it
	// as a source of its own - the only thing it does is raise the source's link
	// count, which is what the hardlink guard reads.
	link := filepath.Join(t.TempDir(), "seed.mkv")
	if err := os.Link(src, link); err != nil {
		t.Skipf("this filesystem does not support hard links: %v", err)
	}

	ts := run(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.EncodeProfiles = []config.Profile{
			{Name: "bulk", Match: "*.mkv", Encoder: strp("svtav1")},
		}
	})

	out, status, ok := outcomeFor(t, ts, src)
	if !ok || status != store.Skipped {
		t.Fatalf("status = %q (found=%v), want skipped - the hardlink guard did not fire", status, ok)
	}
	if out.Reason != SkipHardlinked {
		t.Fatalf("skip reason = %q, want %q - a different guard fired and this case proves nothing", out.Reason, SkipHardlinked)
	}
	if out.Profile != "bulk" {
		t.Fatalf("AC-A10: the hardlinked skip's ledger row records profile %q, want %q. "+
			"store.RecordSkip writes `profile = NULL` and takes no Outcome, so the profile that "+
			"decided this terminal row is lost - while the event the same guard emits carries it.",
			out.Profile, "bulk")
	}
}
