package engine

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// regress_0155_F1 - [AC-1] refuter artifact, S0155 impl gate ordinal 1.
// (see the finding text for the byte arithmetic)
func TestRegress0155F1_LongNamedCoverArtSourceSwaps(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	cases := []struct {
		name    string
		stem    int
		scratch bool
	}{
		{"beside the source", 230, false},
		{"scratch directory", 220, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stem := strings.Repeat("x", tc.stem)
			cfg := func(c *config.Config) {
				everyGate(c)
				if tc.scratch {
					c.ScratchDir = t.TempDir()
				}
			}

			cd := t.TempDir()
			mkReportMatroska(t, ffmpeg, ffprobe, filepath.Join(cd, stem+".mkv"), reportShape{})
			if row := onlyRow(t, run(t, ffmpeg, ffprobe, cd, nil, cfg)); row.Status != store.Done {
				t.Fatalf("control: the same %d-byte stem with no pictures did not swap (%s), so this "+
					"case proves nothing about the picture carriage", tc.stem, row.Outcome.Reason)
			}

			d, side := t.TempDir(), t.TempDir()
			src := filepath.Join(d, stem+".mkv")
			mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
			wantPics := picturesOf(t, ffmpeg, ffprobe, src, side, "source")

			ts := run(t, ffmpeg, ffprobe, d, nil, cfg)

			if row := onlyRow(t, ts); row.Status != store.Done {
				t.Fatalf("AC-1: the report-shaped source with a %d-byte stem did not swap, where the "+
					"same name without pictures does (status %q): %s", tc.stem, row.Status, row.Outcome.Reason)
			}
			doneWithVmaf(t, ts)
			assertPicturesCarried(t, wantPics, picturesOf(t, ffmpeg, ffprobe, src, side, "output"))
		})
	}
}
