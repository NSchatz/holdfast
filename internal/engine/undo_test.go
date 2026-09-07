package engine

// The UNDO-6 proof: the window in which a swap can be walked back.
//
// This suite is written the way the rest of this repo's safety proofs are - against
// REAL ffmpeg fixtures, on real files, through the real ProcessFile - because every
// property here is a property of the filesystem and not of a data structure. A link
// that is really a second name for the same inode, a removal that really does or
// does not return space, a scan that really does not enumerate a retained original:
// none of those can be established by a mock.
//
// Two of these tests carry an explicit ANTI-VACUITY control (a copy-based retention
// that must be seen to grow the disk, a fixture with the blockage removed that must
// be seen to swap). A measurement that cannot fail is not evidence, and both of these
// measurements have an obvious way to be silently blind.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// ---- helpers ----------------------------------------------------------------

// sha256f is the byte-for-byte identity the undo criteria are stated in.
func sha256f(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// undoCfg is baseCfg with the window open.
func undoCfg(root string, hours int) config.Config {
	c := baseCfg(root)
	c.UndoWindowHours = hours
	return c
}

// undoEngine builds an engine over root with the undo window open and returns it with
// the store behind it, so a test can drive several passes over ONE ledger (a retention
// that did not survive the pass that took it would prove nothing).
func undoEngine(t *testing.T, ffmpeg, ffprobe, root string, hours int, enc Encoder) (*Engine, *testStore) {
	t.Helper()
	return undoEngineWithLog(t, ffmpeg, ffprobe, root, hours, enc, discardLogger())
}

func undoEngineWithLog(t *testing.T, ffmpeg, ffprobe, root string, hours int, enc Encoder, log *slog.Logger) (*Engine, *testStore) {
	t.Helper()
	cfg := undoCfg(root, hours)
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	return New(cfg, prober, enc, ts, log), ts
}

// retainedRows is every live retention in the ledger.
func retainedRows(t *testing.T, ts *testStore) []store.Retained {
	t.Helper()
	rows, err := ts.ListRetained(context.Background())
	if err != nil {
		t.Fatalf("ListRetained: %v", err)
	}
	return rows
}

// onlyRetained asserts there is exactly one live retention and returns it.
func onlyRetained(t *testing.T, ts *testStore) store.Retained {
	t.Helper()
	rows := retainedRows(t, ts)
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d retained original(s), want exactly 1: %+v", len(rows), rows)
	}
	return rows[0]
}

// treeUsage is the disk this directory tree occupies, counting each INODE once - the
// measurement `du` makes, and the only one that can tell a second link from a second
// copy. Summing st_blocks per directory entry would count a hard link twice and read
// exactly like a copy, which is the mistake this function exists to not make.
//
// It is measured over the fixture's own tree rather than from statfs, so a parallel
// test writing elsewhere on the same filesystem cannot move the number.
func treeUsage(t *testing.T, root string) int64 {
	t.Helper()
	seen := make(map[[2]uint64]bool)
	var total int64
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		fi, serr := os.Stat(path)
		if serr != nil {
			return nil
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("no syscall.Stat_t for %s - this proof needs the inode", path)
		}
		key := [2]uint64{uint64(st.Dev), uint64(st.Ino)}
		if seen[key] {
			return nil // a second NAME for data already counted: costs nothing
		}
		seen[key] = true
		total += st.Blocks * 512
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return total
}

// nlinkOf reads a file's hard-link count, failing the test when it cannot.
func nlinkOf(t *testing.T, path string) uint64 {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return probe.NLink(path)
}

// ---- AC1: the original survives the swap, on both swap shapes ----------------

// TestUndo_RetainsTheOriginalBeforeTheSwap is UNDO-6's first criterion. Both swap
// shapes are graded, because they are different code: the in-place rename destroys
// the source in ONE syscall, and the container-changing one renames to a new name and
// then removes the source. A retention that covered only the first would leave every
// mp4 in a library unprotected while reporting a window that was open.
func TestUndo_RetainsTheOriginalBeforeTheSwap(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, tc := range []struct {
		name, src string
		final     string
	}{
		{"extension unchanged (in-place rename)", "movie.mkv", "movie.mkv"},
		{"extension changed (rename then remove)", "movie.mp4", "movie.mkv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, tc.src)
			mkH264(t, ffmpeg, src, "8M")
			before := sha256f(t, src)
			beforeSize := probe.FileSize(src)

			eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			final := filepath.Join(d, tc.final)
			if got := codecOf(t, ffprobe, final); got != "hevc" {
				t.Fatalf("the swap did not happen: %s is %q, want hevc", tc.final, got)
			}
			if tc.src != tc.final && exists(src) {
				t.Errorf("the container-changing swap left the source %s behind", src)
			}

			r := onlyRetained(t, ts)
			if r.SourcePath != src {
				t.Errorf("retention source_path = %q, want %q", r.SourcePath, src)
			}
			if r.SwappedPath != final {
				t.Errorf("retention swapped_path = %q, want %q", r.SwappedPath, final)
			}
			if r.SourceBytes != beforeSize {
				t.Errorf("retention source_bytes = %d, want %d", r.SourceBytes, beforeSize)
			}
			if fp := probe.Fingerprint(final); r.SwappedFingerprint != fp {
				t.Errorf("retention swapped_fingerprint = %q, want the post-swap %q", r.SwappedFingerprint, fp)
			}
			if !exists(r.RetainedPath) {
				t.Fatalf("the retained original is not on disk at %s", r.RetainedPath)
			}
			// THE criterion: the bytes that were there before the swap are still
			// retrievable after it.
			if got := sha256f(t, r.RetainedPath); got != before {
				t.Errorf("the retained original is not the pre-swap source:\n  got  %s\n  want %s", got, before)
			}
		})
	}
}

// ---- AC8: retention writes no second copy -----------------------------------

// TestUndo_RetentionCostsNoSpace is UNDO-6's eighth criterion, and it is measured
// twice over: the retained original and the source are the SAME INODE, and the disk
// the tree occupies is unchanged by taking the retention.
//
// The control is the point. A `du`-style measurement is easy to write in a way that
// cannot tell a link from a copy, so the same measurement is run against a real COPY
// of the same file and must be seen to grow by its size. Without that, a broken
// measurement would report "no growth" for a retention that doubled the disk.
func TestUndo_RetentionCostsNoSpace(t *testing.T) {
	ffmpeg, _ := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	size := probe.FileSize(src)

	u := NewUndoWindow(undoCfg(d, 24), nil, discardLogger())
	before := treeUsage(t, d)
	retained, err := u.retain(src, probe.Fingerprint(src))
	if err != nil {
		t.Fatalf("retain: %v", err)
	}
	after := treeUsage(t, d)

	if after != before {
		t.Errorf("retaining the original grew the tree by %d bytes (before %d, after %d) - "+
			"a retention must be a second NAME for the same data, never a second copy",
			after-before, before, after)
	}
	if !sameFile(src, retained) {
		t.Errorf("the retained original %s is not the same inode as the source %s", retained, src)
	}
	if n := nlinkOf(t, src); n != 2 {
		t.Errorf("the source has %d links after retention, want 2 (itself and the retained original)", n)
	}

	// The control: the identical measurement over a real copy MUST grow. If this does
	// not fire, the assertion above proves nothing.
	copied := filepath.Join(d, "copy.bin")
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copied, b, 0o644); err != nil {
		t.Fatal(err)
	}
	grown := treeUsage(t, d)
	if grown-after < size/2 {
		t.Fatalf("the space measurement is blind: copying a %d-byte file grew it by only %d bytes, "+
			"so it could not have detected a copy-based retention either", size, grown-after)
	}
}

// TestUndo_TheRetainedOriginalIsTheSourceInodeDuringTheSwap proves the same identity
// through the REAL swap path, in the only window where both names exist at once: after
// the retention and before the rename. Production leaves the seam nil.
func TestUndo_TheRetainedOriginalIsTheSourceInodeDuringTheSwap(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng, _ := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
	var observed, sameInode, linksSeen = false, false, uint64(0)
	eng.hookAfterRetain = func(retained string) error {
		observed = true
		sameInode = sameFile(src, retained)
		linksSeen = probe.NLink(src)
		return nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if !observed {
		t.Fatal("the retention never ran - the seam was not reached, so this test asserted nothing")
	}
	if !sameInode {
		t.Error("during the swap the retained original was not another name for the source's inode")
	}
	if linksSeen != 2 {
		t.Errorf("the source carried %d links between the retention and the rename, want 2", linksSeen)
	}
}

// ---- AC4: a retention that cannot be taken skips the file --------------------

// TestUndo_SkipsTheFileWhenTheOriginalCannotBeRetained is UNDO-6's fourth criterion.
// The window's promise is that a swap can be walked back; a swap this tool could not
// walk back is not one it performs while that promise is in force.
//
// Both blockages are REAL filesystem conditions rather than an injected error, and
// neither depends on permission bits (a suite that runs as root would silently stop
// exercising a chmod-based one): the retention area is occupied by a regular file, so
// creating it fails with ENOTDIR; or the retained NAME is already taken by a different
// file, which link(2) refuses with EEXIST and this tool will not resolve by
// overwriting bytes it cannot identify.
func TestUndo_SkipsTheFileWhenTheOriginalCannotBeRetained(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	block := map[string]func(t *testing.T, dir, src string){
		"the retention area cannot be created": func(t *testing.T, dir, src string) {
			// A regular file where the retention directory must go: MkdirAll fails.
			if err := os.WriteFile(filepath.Join(dir, UndoDirName), []byte("not a directory"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"the retained name is occupied by different data": func(t *testing.T, dir, src string) {
			want := retainedPathFor(src, probe.Fingerprint(src))
			if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(want, []byte("somebody else's bytes"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
	}

	for name, blockIt := range block {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			before := sha256f(t, src)
			blockIt(t, d, src)

			eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			// No rename ran: the source is byte-for-byte what it was, and it is still
			// the codec it was (a swap would have made it hevc).
			if got := sha256f(t, src); got != before {
				t.Errorf("the source changed after a retention failure:\n  got  %s\n  want %s", got, before)
			}
			if got := codecOf(t, ffprobe, src); got != "h264" {
				t.Errorf("the source is %q - a swap ran despite the retention failing", got)
			}
			if n := nTemp(t, d); n != 0 {
				t.Errorf("%d temp file(s) survived the retention failure", n)
			}
			if got := skipReason(t, ts, "movie.mkv"); got != SkipUndoRetentionFailed {
				t.Errorf("skip reason = %q, want %q", got, SkipUndoRetentionFailed)
			}
			if rows := retainedRows(t, ts); len(rows) != 0 {
				t.Errorf("a retention was recorded for a retention that failed: %+v", rows)
			}
		})
	}

	// The control: the identical fixture with NOTHING blocking the retention swaps.
	// Without it, a fixture that simply never reached the encoder would pass every
	// assertion above.
	t.Run("control: the same fixture swaps when the retention can be taken", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")
		eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if got := codecOf(t, ffprobe, src); got != "hevc" {
			t.Fatalf("the unblocked fixture did not swap (codec %q) - the blocked cases above prove nothing", got)
		}
		if len(retainedRows(t, ts)) != 1 {
			t.Error("the unblocked fixture swapped without recording a retention")
		}
	})
}

// TestUndo_ARetentionFailureIsReEvaluatedOnTheNextScan proves the retention guard is
// MUTABLE, exactly as the hardlink guard is. A full disk or an unwritable retention
// area is a condition somebody fixes; a permanent skip row would park the file for
// ever, so the file must be reclaimed on the next scan once the blockage is gone.
func TestUndo_ARetentionFailureIsReEvaluatedOnTheNextScan(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	blocker := filepath.Join(d, UndoDirName)
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if got := skipReason(t, ts, "movie.mkv"); got != SkipUndoRetentionFailed {
		t.Fatalf("scan 1: reason = %q, want %q", got, SkipUndoRetentionFailed)
	}

	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Errorf("scan 2: the file was not reclaimed after the blockage was cleared (codec %q) - "+
			"the retention skip parked it permanently", got)
	}
}

// TestUndo_TurningTheWindowOffClearsAStaleRetentionFailureSkip is the third strand of
// the same rule the release sweep and the hardlink discount follow: undo_window_hours
// governs whether a NEW retention is taken and nothing else, so turning it off must not
// strand what the window left behind.
//
// With the window off no retention is attempted at all, which means a retention failure
// has stopped being a reason to skip anything. A skip row left over from when it was on
// would park that file for as long as the setting stayed off - and the operator who
// turned the key to 0 to stop paying for the window is exactly the one who would never
// look for a guard named after it.
func TestUndo_TurningTheWindowOffClearsAStaleRetentionFailureSkip(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	// A file where the retention area has to go: the retention cannot be taken, and the
	// blockage is left in place for the whole test, because with the window off it is
	// not a blockage at all.
	if err := os.WriteFile(filepath.Join(d, UndoDirName), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	if got := skipReason(t, ts, "movie.mkv"); got != SkipUndoRetentionFailed {
		t.Fatalf("scan 1: reason = %q, want %q - the fixture is not in the state this test is about", got, SkipUndoRetentionFailed)
	}

	eng.Cfg.UndoWindowHours = 0
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("scan 2: %v", err)
	}

	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Errorf("the file was not reclaimed after the window was turned off (codec %q) - a skip about a "+
			"retention this configuration no longer takes parked it", got)
	}
	for _, row := range skippedRows(t, ts) {
		if row.Outcome.Reason == SkipUndoRetentionFailed {
			t.Errorf("a stale %q skip survived the window being turned off: %s", SkipUndoRetentionFailed, row.Path)
		}
	}
}

// ---- AC6/AC7: the undo window and the hardlink guard -------------------------

// TestUndo_OurOwnRetainedLinkDoesNotTripTheHardlinkGuard is UNDO-6's sixth criterion.
//
// A retention raises the source's link count, and this tool skips a file with more
// than one link as an active seed. So a run interrupted between the link and the
// rename leaves the source looking hardlinked - and without this, the undo window
// would permanently park the very file it exists to protect.
//
// Both shapes of leftover are graded: with the retention RECORDED (the interrupted
// run got as far as the ledger) and without it (it did not). The second matters
// because the record is written after the swap, so the unrecorded state is the one a
// crash actually leaves behind.
func TestUndo_OurOwnRetainedLinkDoesNotTripTheHardlinkGuard(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, recorded := range []bool{true, false} {
		name := "retention recorded"
		if !recorded {
			name = "retention not yet recorded (killed before the ledger write)"
		}
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")

			eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
			// Leave the state an interrupted run leaves: the source present, a second
			// link to it in the retention area.
			u := eng.undo()
			retained, err := u.retain(src, probe.Fingerprint(src))
			if err != nil {
				t.Fatalf("retain: %v", err)
			}
			if recorded {
				if err := u.record(context.Background(), src, src, retained, probe.FileSize(src)); err != nil {
					t.Fatalf("record: %v", err)
				}
			}
			if n := nlinkOf(t, src); n != 2 {
				t.Fatalf("the fixture has %d links, want 2 - it is not the state this test is about", n)
			}

			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			if got := codecOf(t, ffprobe, src); got != "hevc" {
				t.Errorf("the file was not processed (codec %q) - our own retained link was read as a foreign one", got)
			}
			for _, row := range skippedRows(t, ts) {
				if row.Outcome.Reason == SkipHardlinked {
					t.Errorf("a hardlinked skip was recorded for %s, whose only extra link this tool holds", row.Path)
				}
			}
		})
	}
}

// TestUndo_AForeignExtraLinkIsStillSkipped is UNDO-6's seventh criterion: the undo
// window must never weaken the hardlink guard. Graded with the window both closed and
// open, because the discount only exists in the second and a guard that is right in
// one configuration and wrong in the other is not a guard.
func TestUndo_AForeignExtraLinkIsStillSkipped(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, hours := range []int{0, 24} {
		t.Run(fmt.Sprintf("undo_window_hours=%d", hours), func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			seed := filepath.Join(d, "seed.mkv")
			if err := os.Link(src, seed); err != nil {
				t.Fatalf("hardlink: %v", err)
			}
			before := sha256f(t, src)

			eng, ts := undoEngine(t, ffmpeg, ffprobe, d, hours, nil)
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			if got := sha256f(t, src); got != before {
				t.Error("a hardlinked source was modified")
			}
			if got := codecOf(t, ffprobe, src); got != "h264" {
				t.Errorf("a hardlinked source was transcoded (codec %q)", got)
			}
			if got := skipReason(t, ts, "movie.mkv"); got != SkipHardlinked {
				t.Errorf("skip reason = %q, want %q - the undo window weakened the hardlink guard", got, SkipHardlinked)
			}
		})
	}
}

// skippedRows is every skipped row in the ledger, for assertions about what is NOT in
// the skip breakdown.
func skippedRows(t *testing.T, ts *testStore) []store.Job {
	t.Helper()
	rows, err := ts.List(context.Background(), []store.Status{store.Skipped}, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return rows
}

// ---- AC9: a retained original is never a source ------------------------------

// TestUndo_ARetainedOriginalIsNeverEnumerated is UNDO-6's ninth criterion, and the
// hazard it closes is the sharpest one in the phase: a retention area the scan walked
// would hand the encoder the very bytes an operator was given a window to recover,
// re-encode them, and swap the result over them.
//
// The fixture is deliberately a `.mkv` - a scanned extension - so the exclusion cannot
// be passing by accident on a name the scan would have ignored anyway.
//
// BOTH enumeration paths are graded. `run` and `serve` always set Coverage (the
// directories the startup walk traversed, which INCLUDES the retention area, because
// the walk descends into it), so that is the production path; the direct root walk is
// what an engine built without the startup check uses, including every other test in
// this package. An exclusion present in only one of them would be an exclusion that is
// absent exactly where it ships.
func TestUndo_ARetainedOriginalIsNeverEnumerated(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, withCoverage := range []bool{false, true} {
		name := "walking the roots directly"
		if withCoverage {
			name = "bounded by the startup walk's coverage (the production path)"
		}
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")

			// Everything the encoder is handed, so "never enumerated" is asserted
			// against what actually reached the encode and not only against the ledger.
			var handed []string
			spy := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
				handed = append(handed, in)
				return errFake // never actually swap: this test is about what is SCANNED
			})

			eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, spy)
			u := eng.undo()
			retained, err := u.retain(src, probe.Fingerprint(src))
			if err != nil {
				t.Fatalf("retain: %v", err)
			}
			if err := u.record(context.Background(), src, src, retained, probe.FileSize(src)); err != nil {
				t.Fatalf("record: %v", err)
			}
			// The retained original really is a name the scan would otherwise
			// enumerate: it ENDS in a configured video extension. Without this the
			// exclusion below could be passing because the name carried no video
			// extension at all, which would prove nothing about a retention area.
			if !strings.HasSuffix(retained, ".mkv") {
				t.Fatalf("the retained name %q does not end in a scanned extension - the fixture is not the hazard", retained)
			}
			if !matchesVideoExt(filepath.Base(retained), eng.Cfg.VideoExts) {
				t.Fatalf("the retained name %q is not one the extension rule would match - the fixture is not the hazard", retained)
			}
			if withCoverage {
				// Exactly what the startup walk produces: every directory it traversed,
				// the retention area among them.
				eng.Coverage = []string{d, filepath.Dir(retained)}
			}

			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			if len(handed) == 0 {
				t.Fatal("the encoder was handed nothing at all - this scan enumerated no file, so it cannot show what it excluded")
			}
			for _, in := range handed {
				if in == retained || strings.Contains(in, UndoDirName) || isUndoName(filepath.Base(in)) {
					t.Errorf("the encoder was handed a retained original: %s", in)
				}
			}
			if _, _, found, err := ts.Get(context.Background(), retained, probe.Fingerprint(retained)); err != nil {
				t.Fatal(err)
			} else if found {
				t.Errorf("the scan claimed a job for the retained original %s", retained)
			}
			// And it is still there, which is the whole point of it.
			if !exists(retained) {
				t.Fatal("the retained original was removed by the scan")
			}
		})
	}

	// The name rule the scan and the startup walk SHARE, asserted directly: the walk
	// decides what is media from the basename alone, so this is the only place the two
	// can be held in agreement.
	t.Run("the shared name rule", func(t *testing.T) {
		for _, base := range []string{
			"movie.100-200." + UndoMarker + ".mkv",
			"movie.100-200." + UndoMarker + ".mp4",
		} {
			if IsSourceName(base, []string{"mkv", "mp4"}) {
				t.Errorf("IsSourceName(%q) = true - a retained original would be enumerated as a source", base)
			}
		}
		// The control: the same names WITHOUT the marker are sources, so the rule above
		// is the marker and not the shape of the name.
		for _, base := range []string{"movie.mkv", "movie.100-200.mp4"} {
			if !IsSourceName(base, []string{"mkv", "mp4"}) {
				t.Errorf("IsSourceName(%q) = false - the exclusion is too broad and would hide real media", base)
			}
		}
	})
}

// ---- AC3 / AC14: releasing, and what a release actually returns --------------

// TestUndo_ReleasesAnExpiredRetentionAndReportsWhatItReturned is UNDO-6's third
// criterion, and its second half - the report - carries the fourteenth: removing a
// name returns the file's space only when it was the LAST name for that data
// (`unlink(2)`), so a retention whose data survives under a third link is released and
// returns NOTHING, and must say so rather than claim a reclaim it did not perform.
func TestUndo_ReleasesAnExpiredRetentionAndReportsWhatItReturned(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, tc := range []struct {
		name      string
		thirdLink bool
	}{
		{"the last link goes, so the space comes back", false},
		{"the data survives under another link, so nothing comes back", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			size := probe.FileSize(src)

			// A window of an hour, then a clock an hour and a half later: the retention
			// is genuinely past its own expiry, with no ledger row rewritten to fake it.
			clock := time.Now()
			eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 1, nil)
			eng.undoNow = func() time.Time { return clock }
			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}
			r := onlyRetained(t, ts)
			if !exists(r.RetainedPath) {
				t.Fatalf("nothing was retained at %s", r.RetainedPath)
			}
			if r.SourceBytes != size {
				t.Fatalf("retention records %d bytes, want the source's %d", r.SourceBytes, size)
			}

			var extra string
			if tc.thirdLink {
				extra = filepath.Join(d, "another-name.bin")
				if err := os.Link(r.RetainedPath, extra); err != nil {
					t.Fatalf("third link: %v", err)
				}
			}

			clock = clock.Add(90 * time.Minute)
			rep := eng.ReleaseExpired(context.Background())

			if rep.Released != 1 {
				t.Errorf("released %d retention(s), want 1", rep.Released)
			}
			wantBytes := size
			if tc.thirdLink {
				wantBytes = 0
			}
			if rep.BytesReturned != wantBytes {
				t.Errorf("the release reported %d bytes returned, want %d", rep.BytesReturned, wantBytes)
			}
			if exists(r.RetainedPath) {
				t.Error("the retained original is still on disk after its window closed")
			}
			if rows := retainedRows(t, ts); len(rows) != 0 {
				t.Errorf("the ledger still holds %d retention(s) after the release: %+v", len(rows), rows)
			}
			if tc.thirdLink {
				if !exists(extra) {
					t.Error("the release removed a link it did not take")
				}
				if got := sha256f(t, extra); got == "" {
					t.Error("the surviving link is unreadable")
				}
			}
		})
	}
}

// TestUndo_TheScanPassReleasesAndReportsIt proves the release is not merely available
// but WIRED: the next scan pass performs it, and reports the count and the bytes it
// returned in the operator-facing log. A release nobody triggers is a window that
// never closes, and the space never comes back.
func TestUndo_TheScanPassReleasesAndReportsIt(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	size := probe.FileSize(src)

	var logs strings.Builder
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	clock := time.Now()
	eng, ts := undoEngineWithLog(t, ffmpeg, ffprobe, d, 1, nil, log)
	eng.undoNow = func() time.Time { return clock }

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	r := onlyRetained(t, ts)

	clock = clock.Add(90 * time.Minute)
	logs.Reset()
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("scan 2: %v", err)
	}

	if exists(r.RetainedPath) {
		t.Error("the scan pass did not release the expired retention from disk")
	}
	if rows := retainedRows(t, ts); len(rows) != 0 {
		t.Errorf("the scan pass did not drop the released record: %+v", rows)
	}
	out := logs.String()
	if !strings.Contains(out, "released=1") {
		t.Errorf("the scan pass did not report how many retentions it released:\n%s", out)
	}
	if !strings.Contains(out, fmt.Sprintf("bytes_returned=%d", size)) {
		t.Errorf("the scan pass did not report the %d bytes the release returned:\n%s", size, out)
	}
}

// ---- AC13: a record whose retained original is gone --------------------------

// TestUndo_ARecordWithNoRetainedOriginalIsUnrestorableAndReturnsNothing is UNDO-6's
// thirteenth criterion, driven through BOTH the restore and the release. A ledger that
// promises a restore it cannot perform, or reports a reclaim that returned nothing, is
// worse than one that says nothing at all: it is evidence that is wrong.
func TestUndo_ARecordWithNoRetainedOriginalIsUnrestorableAndReturnsNothing(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("restore", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")

		clock := time.Now()
		eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
		eng.undoNow = func() time.Time { return clock }
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		r := onlyRetained(t, ts)
		swapped := sha256f(t, r.SwappedPath)

		// Somebody removed the retained original out from under the record.
		if err := os.Remove(r.RetainedPath); err != nil {
			t.Fatal(err)
		}

		_, err := eng.Restore(context.Background(), src)
		if !errors.Is(err, ErrUnrestorable) {
			t.Fatalf("Restore error = %v, want ErrUnrestorable", err)
		}
		if !strings.Contains(err.Error(), r.RetainedPath) {
			t.Errorf("the refusal does not name the retained original that is missing: %v", err)
		}
		if rows := retainedRows(t, ts); len(rows) != 0 {
			t.Errorf("the unrestorable record was not dropped: %+v", rows)
		}
		if got := sha256f(t, r.SwappedPath); got != swapped {
			t.Error("the file at the restore target changed on an unrestorable restore")
		}
	})

	t.Run("release", func(t *testing.T) {
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")

		clock := time.Now()
		eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 1, nil)
		eng.undoNow = func() time.Time { return clock }
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		r := onlyRetained(t, ts)
		if err := os.Remove(r.RetainedPath); err != nil {
			t.Fatal(err)
		}

		clock = clock.Add(90 * time.Minute)
		rep := eng.ReleaseExpired(context.Background())
		if rep.Released != 0 {
			t.Errorf("released = %d, want 0 - there was nothing there to release", rep.Released)
		}
		if rep.BytesReturned != 0 {
			t.Errorf("bytes returned = %d, want 0 - removing nothing returns nothing", rep.BytesReturned)
		}
		if rep.Unrestorable != 1 {
			t.Errorf("unrestorable = %d, want 1", rep.Unrestorable)
		}
		if rows := retainedRows(t, ts); len(rows) != 0 {
			t.Errorf("the record of a missing retained original was not dropped: %+v", rows)
		}
	})
}
