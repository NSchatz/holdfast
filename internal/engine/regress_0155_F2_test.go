package engine

import (
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// regress_0155_F2 - [AC-9] (and [AC-1]) refuter artifact, S0155 impl gate ordinal 1.
// (see the finding text for the mechanism)
func TestRegress0155F2_PatternInNameLeavesNoPictureBehind(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	const name = "The 100%d Club (2025).mkv"

	cd := t.TempDir()
	mkReportMatroska(t, ffmpeg, ffprobe, filepath.Join(cd, name), reportShape{})
	controlBefore := dirListing(t, cd)
	if row := onlyRow(t, run(t, ffmpeg, ffprobe, cd, nil, nil)); row.Status != store.Done {
		t.Fatalf("control: %q with no pictures did not swap (%s), so this case proves nothing "+
			"about the picture carriage", name, row.Outcome.Reason)
	}
	if got := dirListing(t, cd); !equalStrings(got, controlBefore) {
		t.Fatalf("control: the source directory changed: before %v, after %v", controlBefore, got)
	}

	d := t.TempDir()
	src := filepath.Join(d, name)
	mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
	before := dirListing(t, d)

	ts := run(t, ffmpeg, ffprobe, d, nil, nil)

	row := onlyRow(t, ts)
	if got := dirListing(t, d); !equalStrings(got, before) {
		t.Errorf("AC-9: the job (status %q) left the source directory as %v, it was %v - a file "+
			"the job wrote remains. Reason: %s", row.Status, got, before, row.Outcome.Reason)
	}
	if row.Status != store.Done {
		t.Errorf("AC-1: the report-shaped source named %q did not swap, where the same name "+
			"without pictures does: %s", name, row.Outcome.Reason)
	}
}
