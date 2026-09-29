package main

// S0177, at the command surface: the retained original's new name as `holdfast run` takes
// it and `holdfast restore` lists and restores it, a retention an earlier build recorded
// under its earlier name, and the census's account of a working file under its new name.
// Everything is graded through the real command, as the rest of the undo window's operator
// half is (restore_test.go).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// s0177DefaultVideoExts is the shipped default video_exts, which the undo fixtures run with.
var s0177DefaultVideoExts = []string{"mkv", "mp4", "avi", "mov", "m4v", "ts", "m2ts", "wmv", "flv"}

// s0177RetainedName is the name a retention of src at fingerprint takes, spelled out here
// from the criterion rather than borrowed from the engine: `<stem>.<fingerprint>.__undo__.
// <ext>`, plus the suffix when earlier is false.
func s0177RetainedName(src, fingerprint string, earlier bool) string {
	base := filepath.Base(src)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext) + "." + strings.NewReplacer(":", "-", "/", "-").Replace(fingerprint) +
		"." + engine.UndoMarker + ext
	if !earlier {
		name += engine.UndoSuffix
	}
	return filepath.Join(filepath.Dir(src), engine.UndoDirName, name)
}

// s0177Files is every regular file under root.
func s0177Files(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// s0177Row is the ledger's row for path.
func s0177Row(t *testing.T, state, path string) store.Job {
	t.Helper()
	st := openStore(t, state)
	defer func() { _ = st.Close() }()
	rows, err := st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("the ledger has no row for %s", path)
	return store.Job{}
}

// s0177Run runs `holdfast run` in-process and fails the test on a non-zero exit.
func s0177Run(t *testing.T, cfgPath string) {
	t.Helper()
	if code, _, errOut := cli(t, "run", "--config", cfgPath); code != 0 {
		t.Fatalf("run exited %d: %s", code, errOut)
	}
}

// s0177EarlierRetention swaps src and then leaves its retention exactly as a build before
// S0177 would have: the retained original under the earlier name, and the record naming
// it. It returns that record.
func s0177EarlierRetention(t *testing.T, cfgPath, state, src string) store.Retained {
	t.Helper()
	s0177Run(t, cfgPath)
	st := openStore(t, state)
	defer func() { _ = st.Close() }()
	r := onlyRetention(t, st)
	earlier := strings.TrimSuffix(r.RetainedPath, engine.UndoSuffix)
	if earlier == r.RetainedPath {
		t.Fatalf("the retention at %s is not under this build's name", r.RetainedPath)
	}
	if err := os.Rename(r.RetainedPath, earlier); err != nil {
		t.Fatal(err)
	}
	r.RetainedPath = earlier
	if err := st.Retain(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestS0177AC7_TheOriginalIsHeldUnderItsNewFormRetainedName grades AC-7: a completed swap
// with the undo window on holds the original at
// `.holdfast-undo/<stem>.<fingerprint>.__undo__.<ext>.holdfast-undo`, the same inode as the
// pre-swap original; `holdfast restore` with no path lists that path; and nothing in the
// retention area ends in a configured video extension.
func TestS0177AC7_TheOriginalIsHeldUnderItsNewFormRetainedName(t *testing.T) {
	cfgPath, lib, state, src := undoLibrary(t, 24, "")
	original, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256File(t, src)
	want := s0177RetainedName(src, probe.Fingerprint(src), false)

	s0177Run(t, cfgPath)

	if sha256File(t, src) == sum {
		t.Fatal("the swap did not happen, so there is no retention to grade")
	}
	held, err := os.Stat(want)
	if err != nil {
		t.Fatalf("the original is not held at %s: %v (the library holds %v)", want, err, s0177Files(t, lib))
	}
	if !os.SameFile(original, held) {
		t.Errorf("%s is not the same inode as the pre-swap original", want)
	}
	st := openStore(t, state)
	r := onlyRetention(t, st)
	_ = st.Close()
	if r.RetainedPath != want {
		t.Errorf("the retention record names %s, want %s", r.RetainedPath, want)
	}
	code, out, errOut := cli(t, "restore", "--config", cfgPath)
	if code != 0 {
		t.Fatalf("restore (list) exited %d: %s", code, errOut)
	}
	if !strings.Contains(out, want) {
		t.Errorf("the retention `holdfast restore` lists does not name %s:\n%s", want, out)
	}
	for _, p := range s0177Files(t, filepath.Join(lib, engine.UndoDirName)) {
		for _, ext := range s0177DefaultVideoExts {
			if strings.HasSuffix(strings.ToLower(p), "."+ext) {
				t.Errorf("%s in the retention area ends in the configured video extension .%s", p, ext)
			}
		}
	}
}

// TestS0177AC8_RestorePutsANewFormRetentionBackUnderItsOriginalName grades AC-8: `holdfast
// restore <path>` inside the window puts a new-form retention back at the original's own
// path under its own name, byte for byte, leaves no `.holdfast-undo` file for it, removes
// the emptied retention area, and the next scan does not re-encode the file - for an
// in-place swap, and for a container-changing one, whose encode at the other name goes too.
func TestS0177AC8_RestorePutsANewFormRetentionBackUnderItsOriginalName(t *testing.T) {
	for _, tc := range []struct {
		name  string
		extra string
		mp4   bool
	}{
		{"an in-place swap", "", false},
		{"a container-changing swap", "container_ext: mkv\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, lib, state, src := undoLibrary(t, 24, tc.extra)
			swapped := src
			if tc.mp4 {
				mp4 := filepath.Join(lib, "movie.mp4")
				if out, err := exec.Command(envOr("HOLDFAST_FFMPEG", "ffmpeg"), "-hide_banner", "-loglevel", "error",
					"-y", "-i", src, "-c", "copy", "--", mp4).CombinedOutput(); err != nil {
					t.Fatalf("build the mp4 fixture: %v\n%s", err, out)
				}
				if err := os.Remove(src); err != nil {
					t.Fatal(err)
				}
				src = mp4
			}
			before := sha256File(t, src)

			s0177Run(t, cfgPath)

			st := openStore(t, state)
			r := onlyRetention(t, st)
			_ = st.Close()
			if !strings.HasSuffix(r.RetainedPath, engine.UndoSuffix) || !fileExists(r.RetainedPath) {
				t.Fatalf("the retention is not a new-form one on disk: %s", r.RetainedPath)
			}
			if tc.mp4 && (!fileExists(swapped) || fileExists(src)) {
				t.Fatalf("the container-changing swap did not happen: %v", s0177Files(t, lib))
			}

			code, out, errOut := cli(t, "restore", "--config", cfgPath, src)
			if code != 0 {
				t.Fatalf("restore exited %d: %s", code, errOut)
			}
			if !strings.Contains(out, src) {
				t.Errorf("the restore report does not name %s: %s", src, out)
			}
			if got := sha256File(t, src); got != before {
				t.Errorf("the file at %s is not the pre-swap original", src)
			}
			for _, p := range s0177Files(t, lib) {
				if strings.HasSuffix(p, engine.UndoSuffix) {
					t.Errorf("a retained file is left after the restore: %s", p)
				}
			}
			if _, err := os.Stat(filepath.Join(lib, engine.UndoDirName)); !os.IsNotExist(err) {
				t.Errorf("the emptied retention area was not removed (%v)", err)
			}
			if tc.mp4 && fileExists(swapped) {
				t.Errorf("the encode the restore replaced is still at %s", swapped)
			}

			s0177Run(t, cfgPath)
			if got := sha256File(t, src); got != before {
				t.Error("the scan after the restore re-encoded the restored file")
			}
		})
	}
}

// TestS0177AC10_AnEarlierBuildsRetentionKeepsItsNameThroughRestoreReleaseAndTheGuard grades
// AC-10: a retention record an earlier build wrote, naming its retained file under the
// earlier name (no suffix), is restored as AC-8 restores, released at its recorded expiry
// as AC-9 releases, and discounted by the hardlink guard - and none of those renames it
// first.
func TestS0177AC10_AnEarlierBuildsRetentionKeepsItsNameThroughRestoreReleaseAndTheGuard(t *testing.T) {
	t.Run("restored under the original's own name", func(t *testing.T) {
		cfgPath, lib, state, src := undoLibrary(t, 24, "")
		before := sha256File(t, src)
		r := s0177EarlierRetention(t, cfgPath, state, src)

		code, _, errOut := cli(t, "restore", "--config", cfgPath, src)
		if code != 0 {
			t.Fatalf("restore exited %d: %s", code, errOut)
		}
		if got := sha256File(t, src); got != before {
			t.Error("the restored file is not the pre-swap original")
		}
		if fileExists(r.RetainedPath) {
			t.Errorf("the earlier retention is still at %s", r.RetainedPath)
		}
		if _, err := os.Stat(filepath.Join(lib, engine.UndoDirName)); !os.IsNotExist(err) {
			t.Errorf("the emptied retention area was not removed (%v)", err)
		}
		s0177Run(t, cfgPath)
		if got := sha256File(t, src); got != before {
			t.Error("the scan after the restore re-encoded the restored file")
		}
	})

	t.Run("released at its recorded expiry and not before", func(t *testing.T) {
		cfgPath, _, state, src := undoLibrary(t, 24, "")
		r := s0177EarlierRetention(t, cfgPath, state, src)

		// A pass inside the window leaves it exactly where it is.
		s0177Run(t, cfgPath)
		if !fileExists(r.RetainedPath) {
			t.Fatalf("a pass inside the window moved or removed %s", r.RetainedPath)
		}
		st := openStore(t, state)
		r.ExpiresAt = time.Now().Add(-time.Minute).Unix()
		if err := st.Retain(context.Background(), r); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()

		code, out := cliProcess(t, "run", "--config", cfgPath)
		if code != 0 {
			t.Fatalf("run exited %d:\n%s", code, out)
		}
		if fileExists(r.RetainedPath) {
			t.Errorf("the retention at %s outlived its window", r.RetainedPath)
		}
		reported := false
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, `msg="undo window: released retained original(s)"`) &&
				strings.Contains(line, " released=1 ") &&
				strings.Contains(line, " bytes_returned="+strconv.FormatInt(r.SourceBytes, 10)+" ") {
				reported = true
			}
		}
		if !reported {
			t.Errorf("the pass's release line does not report the release of %d byte(s):\n%s", r.SourceBytes, out)
		}
		st = openStore(t, state)
		defer func() { _ = st.Close() }()
		if held, err := st.HeldByUndoWindow(context.Background()); err != nil || held != 0 {
			t.Errorf("after the release the undo window holds %d byte(s) (%v), want 0", held, err)
		}
	})

	t.Run("discounted by the hardlink guard under its earlier name", func(t *testing.T) {
		cfgPath, _, state, src := undoLibrary(t, 24, "")
		before := sha256File(t, src)
		fp := probe.Fingerprint(src)
		earlier := s0177RetainedName(src, fp, true)
		if err := os.MkdirAll(filepath.Dir(earlier), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Link(src, earlier); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(src)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		rec := store.Retained{SourcePath: src, SwappedPath: src, RetainedPath: earlier, SourceBytes: fi.Size(),
			SwappedFingerprint: fp, RetainedAt: now.Unix(), ExpiresAt: now.Add(24 * time.Hour).Unix()}
		st := openStore(t, state)
		if err := st.Retain(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
		_ = st.Close()

		s0177Run(t, cfgPath)

		if got := s0177Row(t, state, src).Outcome.Reason; got == engine.SkipHardlinked {
			t.Errorf("the source was skipped as hardlinked, though its only extra link is a retention holdfast recorded")
		}
		held, err := os.Stat(earlier)
		if err != nil {
			t.Fatalf("the earlier retention is gone from %s: %v", earlier, err)
		}
		if !os.SameFile(fi, held) {
			t.Errorf("%s is no longer the original", earlier)
		}
		if sha256File(t, src) != before {
			t.Error("the source changed")
		}
		st = openStore(t, state)
		defer func() { _ = st.Close() }()
		if r := onlyRetention(t, st); r.RetainedPath != earlier || r.ExpiresAt != rec.ExpiresAt {
			t.Errorf("the earlier retention's record changed: %+v, was %+v", r, rec)
		}
	})
}

// TestS0177AC16_TheCensusAttributesANewFormWorkingFileToItsMarker grades AC-16: `holdfast
// analyze` counts a working file under this build's name - which ends in no video extension
// - under the __transcoding__ mechanism and not under video_exts, and the census still
// creates, renames or removes nothing under the library root or the state directory.
func TestS0177AC16_TheCensusAttributesANewFormWorkingFileToItsMarker(t *testing.T) {
	cfgPath, lib, state := censusLibrary(t, "")
	writeCensusFile(t, filepath.Join(lib, "show", "ep2."+engine.TempMarker+".mkv"+engine.TempSuffix),
		"work in progress under this build's name\n")
	libBefore, stateBefore := treeSnapshot(t, lib), treeSnapshot(t, state)

	c := analyzeJSON(t, cfgPath)

	named := map[string]int64{}
	for _, m := range c.Total.Withheld {
		named[m.Name] = m.Files
	}
	// The fixture's two working files - the earlier name and this build's - and its two
	// files no configured extension matches.
	if named[mechTemp] != 2 || named[mechExt] != 2 {
		t.Errorf("the census counts %d file(s) under %s and %d under %s, want 2 and 2 (all: %+v)",
			named[mechTemp], mechTemp, named[mechExt], mechExt, named)
	}
	assertSameTree(t, "the library root", libBefore, treeSnapshot(t, lib))
	assertSameTree(t, "the state directory", stateBefore, treeSnapshot(t, state))
}
