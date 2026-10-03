package server

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// fileFacts is what a read must leave alone about one name under the root.
type fileFacts struct {
	Mode  fs.FileMode
	Links uint64
	Size  int64
	Mtime time.Time
	Inode uint64
}

// treeFacts records every name under root and what the filesystem says about each.
func treeFacts(t *testing.T, root string) map[string]fileFacts {
	t.Helper()
	out := map[string]fileFacts{}
	err := filepath.WalkDir(root, func(p string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		st := info.Sys().(*syscall.Stat_t)
		out[p] = fileFacts{Mode: info.Mode(), Links: uint64(st.Nlink), Size: info.Size(),
			Mtime: info.ModTime(), Inode: st.Ino}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// TestS0169_AC13_TheSummaryTouchesNoRetentionNoRowAndNoFile grades [AC-13].
//
// A retained original is a second HARD LINK to the source's bytes, and it is the only undo
// path a swap leaves. The summary reports how many bytes those links hold, so it is the
// one reporting read that has a reason to go near them - and it must not. The fixture is
// the real thing: a real file under a temporary root, a real second link to it in the
// retention area, a real swapped file, and the retention recorded through the store.
//
// The server reads through the WRITE handle here, on purpose: a handle the database
// refuses writes on would make "wrote nothing" true by construction and prove nothing
// about the handler.
func TestS0169_AC13_TheSummaryTouchesNoRetentionNoRowAndNoFile(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "show", "episode.mkv")
	swapped := filepath.Join(root, "show", "episode.mp4")
	retained := filepath.Join(root, "show", ".holdfast-undo", "episode.1-1.__undo__.mkv.holdfast-undo")
	if err := os.MkdirAll(filepath.Dir(retained), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("the original's bytes, which only the retained link keeps"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(source, retained); err != nil {
		t.Fatalf("link the retained name: %v", err)
	}
	if err := os.WriteFile(swapped, []byte("the replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	// An old mtime, so a re-dated file cannot land on the same second by accident.
	old := time.Unix(1_600_000_000, 0)
	for _, p := range []string{source, swapped, filepath.Dir(retained), filepath.Dir(source), root} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}

	st := emptyStore(t)
	ctx := context.Background()
	if err := st.Retain(ctx, store.Retained{
		SourcePath: source, SwappedPath: swapped, RetainedPath: retained, SourceBytes: 57,
		SwappedFingerprint: "15:1600000000", RetainedAt: 1_600_000_000, ExpiresAt: 1_600_086_400,
	}); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	finishRow(t, st, swapped, store.Done, root, i64p(57), i64p(15))
	finishRow(t, st, filepath.Join(root, "show", "next.mkv"), store.WouldTranscode, root, i64p(4_000), nil)
	mustClaim(t, st, filepath.Join(root, "show", "running.mkv"), "r:r")

	every := []store.Status{store.Pending, store.Probing, store.Encoding, store.Verifying, store.Done,
		store.Skipped, store.Failed, store.WouldTranscode, store.Indeterminate, store.AppliedDespiteError}
	type ledger struct {
		Summary  map[store.Status]int
		Rows     []store.Job
		Retained []store.Retained
		One      store.Retained
		OneOK    bool
	}
	read := func() ledger {
		t.Helper()
		var l ledger
		var err error
		if l.Summary, err = st.Summary(ctx); err != nil {
			t.Fatalf("Summary: %v", err)
		}
		if l.Rows, err = st.List(ctx, every, 1000); err != nil {
			t.Fatalf("List: %v", err)
		}
		if l.Retained, err = st.ListRetained(ctx); err != nil {
			t.Fatalf("ListRetained: %v", err)
		}
		if l.One, l.OneOK, err = st.GetRetained(ctx, source); err != nil {
			t.Fatalf("GetRetained: %v", err)
		}
		return l
	}

	filesBefore, ledgerBefore := treeFacts(t, root), read()
	if filesBefore[source].Links != 2 || filesBefore[retained].Links != 2 ||
		filesBefore[source].Inode != filesBefore[retained].Inode {
		t.Fatalf("the fixture's retained name is not a second hard link to the source: %+v, %+v",
			filesBefore[source], filesBefore[retained])
	}
	if len(ledgerBefore.Retained) != 1 || !ledgerBefore.OneOK || len(ledgerBefore.Rows) != 3 {
		t.Fatalf("the fixture's ledger is not what the test assumes: %+v", ledgerBefore)
	}

	s := newSizing(t, st, []string{root}, "", nil)
	for i := 0; i < 20; i++ {
		got, _ := s.summary(t)
		// The read under test did its work: it is the retention's bytes it reported.
		want(t, "bytes_held_by_undo_window", got.BytesHeldByUndoWindow, i64p(57))
		want(t, "root.bytes_held_by_undo_window", got.Roots[0].BytesHeldByUndoWindow, i64p(57))
		want(t, "root.candidate_bytes", got.Roots[0].CandidateBytes, i64p(4_000))
		if got.Roots[0].FreeBytes == nil {
			t.Fatal("free_bytes is null for a real root")
		}
	}

	filesAfter, ledgerAfter := treeFacts(t, root), read()
	if !reflect.DeepEqual(filesBefore, filesAfter) {
		for p, before := range filesBefore {
			if after, ok := filesAfter[p]; !ok {
				t.Errorf("%s was removed or renamed by a summary read", p)
			} else if after != before {
				t.Errorf("%s changed under a summary read:\n before %+v\n after  %+v", p, before, after)
			}
		}
		for p := range filesAfter {
			if _, ok := filesBefore[p]; !ok {
				t.Errorf("%s was created by a summary read", p)
			}
		}
	}
	for _, p := range []string{source, swapped, retained} {
		if _, ok := filesAfter[p]; !ok {
			t.Errorf("%s is gone after 20 summary reads", p)
		}
	}
	if !reflect.DeepEqual(ledgerBefore, ledgerAfter) {
		t.Errorf("the ledger changed under 20 summary reads:\n before %+v\n after  %+v", ledgerBefore, ledgerAfter)
	}
}
