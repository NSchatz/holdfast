package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// regress_0155_F3 - [AC-1] refuter artifact, S0155 impl gate ordinal 2.
//
// A picture file's path is its working output's path plus ".picture<i>", so where the
// working output's path fits PATH_MAX (4095 bytes and the terminating NUL) with fewer than
// 9 bytes to spare, the picture file's does not. Beside the source, the report's name puts
// the working output at directory + 44 bytes and the picture files at + 53: a 4046-byte
// source directory lands in between. In a scratch directory the working name carries the
// 13-byte source tag, putting them at + 57 and + 66: a 4034-byte scratch directory lands in
// between. No path component is longer than 250 bytes. In each case the same directory with
// no pictures is the control: it must swap first, or the case proves nothing about the
// picture carriage.
func TestRegress0155F3_PathThatFitsTheWorkingOutputSwapsWithPictures(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	const name = "The Secret Agent (2025).mkv"

	deepDir := func(t *testing.T, depth int) string {
		t.Helper()
		p := t.TempDir()
		for len(p) < depth {
			n := min(250, depth-len(p)-1)
			if n < 1 {
				p += "d"
				break
			}
			p = filepath.Join(p, strings.Repeat("d", n))
		}
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("make the %d-byte directory: %v", len(p), err)
		}
		if len(p) != depth {
			t.Fatalf("the directory is %d bytes, want %d", len(p), depth)
		}
		return p
	}

	cases := []struct {
		name    string
		depth   int
		scratch bool
	}{
		{"beside the source", 4046, false},
		{"scratch directory", 4034, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// lib is the library root; deep is the directory whose path is tc.depth bytes.
			lib := func() (lib, scratch string) {
				if tc.scratch {
					return t.TempDir(), deepDir(t, tc.depth)
				}
				return deepDir(t, tc.depth), ""
			}
			cfg := func(scratch string) func(c *config.Config) {
				return func(c *config.Config) {
					everyGate(c)
					if scratch != "" {
						c.ScratchDir = scratch
					}
				}
			}

			cd, cs := lib()
			mkReportMatroska(t, ffmpeg, ffprobe, filepath.Join(cd, name), reportShape{})
			controlBefore := dirListing(t, cd)
			if row := onlyRow(t, run(t, ffmpeg, ffprobe, cd, nil, cfg(cs))); row.Status != store.Done {
				t.Fatalf("control: %q with a %d-byte directory and no pictures did not swap (%s), so "+
					"this case proves nothing about the picture carriage", name, tc.depth,
					truncate(row.Outcome.Reason, 260))
			}
			if got := dirListing(t, cd); !equalStrings(got, controlBefore) {
				t.Fatalf("control: the source directory changed: before %v, after %v", controlBefore, got)
			}

			d, s := lib()
			side := t.TempDir()
			src := filepath.Join(d, name)
			mkReportMatroska(t, ffmpeg, ffprobe, src, reportShape{pictures: true})
			wantPics := picturesOf(t, ffmpeg, ffprobe, src, side, "source")
			before := dirListing(t, d)

			ts := run(t, ffmpeg, ffprobe, d, nil, cfg(s))

			row := onlyRow(t, ts)
			if got := dirListing(t, d); !equalStrings(got, before) {
				t.Errorf("AC-9: the job (status %q) left the source directory as %v, it was %v - a "+
					"file the job wrote remains", row.Status, got, before)
			}
			if s != "" {
				if got := dirListing(t, s); !equalStrings(got, []string{"."}) {
					t.Errorf("AC-9: the job (status %q) left the scratch directory holding %v",
						row.Status, got)
				}
			}
			if row.Status != store.Done {
				t.Fatalf("AC-1: the report-shaped source with a %d-byte directory did not swap, where "+
					"the same directory without pictures does (status %q): %s", tc.depth, row.Status,
					truncate(row.Outcome.Reason, 260))
			}
			doneWithVmaf(t, ts)
			assertPicturesCarried(t, wantPics, picturesOf(t, ffmpeg, ffprobe, src, side, "output"))
		})
	}
}
