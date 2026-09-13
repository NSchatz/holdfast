package engine

// S0085 - the replacement carries the SOURCE's metadata across the swap.
//
// The defect these fixtures were written against: the replacement is created by ffmpeg
// under the daemon's umask and uid:gid, and the rename publishes it under the source's
// name with none of the source's metadata. Every file holdfast touched silently changed
// its permissions, its owner and its age.
//
// Everything in this file is measured by STATTING THE PUBLISHED FILE, never by reading a
// log line: the criteria are about what an operator (and Plex, and every other reader of
// that library) finds on disk afterwards. The encode, the verify gate, the store and the
// swap are all real; the only substitutions live in swap_metadata_seams_test.go, and each
// is there because this gate runs as an unprivileged uid with no CAP_CHOWN and a real
// ownership change is therefore not exercisable here.

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// ---- observers ---------------------------------------------------------------

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode()
}

func mtimeOf(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.ModTime()
}

// statOwner reads a path's uid and gid. It is the test's own reader of the platform's
// ownership model, deliberately separate from the production one, so a build whose
// production reader returned the wrong pair would be caught rather than confirmed.
func statOwner(t *testing.T, path string) (uid, gid int) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no syscall.Stat_t for %s - this proof needs the ownership", path)
	}
	return int(st.Uid), int(st.Gid)
}

// hostileUmask makes every file this process creates for the rest of the test come out
// at 0600 whatever mode was asked for. It is what turns "the replacement happens to have
// the right mode" into "the replacement was GIVEN the right mode": with it set, the temp
// ffmpeg writes cannot be 0640 by accident.
func hostileUmask(t *testing.T) {
	t.Helper()
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
}

// aFixedPastTime is a modification time no encode could have produced, so a replacement
// carrying it can only have taken it from the source. It is deliberately old enough to be
// the kind of age a media server's "Recently Added" sort is built on.
func aFixedPastTime() time.Time { return time.Date(2019, 3, 4, 5, 6, 7, 0, time.UTC) }

// setModTime sets a path's modification time and leaves its access time alone.
func setModTime(t *testing.T, path string, mt time.Time) {
	t.Helper()
	if err := os.Chtimes(path, time.Time{}, mt); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// ---- AC1: the mode, under a hostile umask ------------------------------------

// TestSwap_ReplacementCarriesTheSourcesMode is AC1 and the headline defect: a source at
// 0640 must be replaced by a file at 0640, whatever umask the process is running under.
//
// RED at the pin: nothing on the swap path reads the source's mode or writes it to the
// temp, so the published file comes out at whatever ffmpeg's creat(2) produced under the
// umask - 0600 here - and the library's permissions are silently rewritten file by file.
func TestSwap_ReplacementCarriesTheSourcesMode(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv") // .mkv + ContainerExt "mkv" = the IN-PLACE shape
	mkH264(t, ffmpeg, src, "8M")

	const want = os.FileMode(0o640)
	if err := os.Chmod(src, want); err != nil {
		t.Fatalf("chmod the source: %v", err)
	}
	hostileUmask(t)

	eng, ts := buildEngineWithStore(t, ffmpeg, ffprobe, d)
	runOneshot(t, eng)

	// The swap must actually have happened, or a mode that never changed proves nothing.
	assertSwapped(t, ts, ffprobe, src)

	if got := modeOf(t, src).Perm(); got != want {
		t.Errorf("the published replacement is at mode %04o, want the source's %04o - "+
			"every file this tool touches is silently re-permissioned", got, want)
	}
}

// ---- AC2: the container-changing shape ---------------------------------------

// TestSwap_ContainerChangingSwapCarriesTheSourcesMetadata is AC2: the shape where the
// replacement is published under a NEW name (movie.mp4 -> movie.mkv) and the original
// source file is removed afterwards. The metadata has to be carried there too, and it is
// the shape most likely to be missed, because the file the operator ends up with was never
// the source's name at all.
//
// RED at the pin: the mode and the modification time are both the encode's.
func TestSwap_ContainerChangingSwapCarriesTheSourcesMetadata(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mp4") // .mp4 + ContainerExt "mkv" = the EXT-CHANGING shape
	final := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	const wantMode = os.FileMode(0o640)
	if err := os.Chmod(src, wantMode); err != nil {
		t.Fatalf("chmod the source: %v", err)
	}
	wantMtime := aFixedPastTime()
	setModTime(t, src, wantMtime)
	wantUID, wantGID := statOwner(t, src)
	hostileUmask(t)

	eng, ts := buildEngineWithStore(t, ffmpeg, ffprobe, d)
	runOneshot(t, eng)

	if exists(src) {
		t.Fatal("the ext-changing swap left the original source behind")
	}
	assertSwapped(t, ts, ffprobe, final)

	if got := modeOf(t, final).Perm(); got != wantMode {
		t.Errorf("the replacement at %s is at mode %04o, want the source's %04o", final, got, wantMode)
	}
	if uid, gid := statOwner(t, final); uid != wantUID || gid != wantGID {
		t.Errorf("the replacement at %s is owned by %d:%d, want the source's %d:%d", final, uid, gid, wantUID, wantGID)
	}
	if got := mtimeOf(t, final); !got.Equal(wantMtime) {
		t.Errorf("the replacement at %s carries mtime %s, want the source's %s - a reset mtime moves the "+
			"file to the top of every date-based sort in the library", final, got, wantMtime)
	}
}

// ---- shared assertions -------------------------------------------------------

// runOneshot runs one real pass and fails loudly on an error, so a criterion is never
// asserted against a run that did not happen.
func runOneshot(t *testing.T, eng *Engine) {
	t.Helper()
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
}

// assertSwapped proves the file at path is the REPLACEMENT and not the source that was
// left alone: it is at the target codec and the ledger carries a done row for it. Every
// metadata assertion in this file stands on it - a mode that was never touched because
// the file was skipped would otherwise read as a mode that was carried.
func assertSwapped(t *testing.T, ts *testStore, ffprobe, path string) {
	t.Helper()
	if !exists(path) {
		t.Fatalf("no file at %s after the run", path)
	}
	if got := codecOf(t, ffprobe, path); got != "hevc" {
		t.Fatalf("the file at %s is %s, not the replacement - no swap happened, so nothing here is being measured",
			path, got)
	}
	if !ledgerHas(t, ts, store.Done, filepath.Base(path)) {
		t.Fatalf("no done row for %s - no swap happened, so nothing here is being measured", path)
	}
}
