package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/store"
)

// A retention record whose retained original is not at its recorded path is dropped as
// unrestorable - but only where the path lies under a configured library root. Under no
// root, the absence says nothing about the file: the commonest way to get there is to
// mount the same library at a new path and edit library_roots to match, and dropping the
// record then orphans the original for good (the scan never enumerates a retention area,
// and nothing else releases or restores one). So the release and the restore both KEEP
// that record, the release counts and logs it, and the restore refuses naming
// `holdfast reroot`.
//
// No encode is needed to ask this: the records are written straight into a real ledger
// and the files are plain files, which is all the release and the restore read.

type unrootedFixture struct {
	u        *UndoWindow
	st       *store.SQLite
	logs     *bytes.Buffer
	newRoot  string // the configured root
	oldRoot  string // a tree the configuration no longer names
	clockNow time.Time
}

func newUnrootedFixture(t *testing.T) *unrootedFixture {
	t.Helper()
	d := t.TempDir()
	f := &unrootedFixture{
		newRoot:  filepath.Join(d, "media", "new"),
		oldRoot:  filepath.Join(d, "media", "old"),
		logs:     &bytes.Buffer{},
		clockNow: time.Now(),
	}
	if err := os.MkdirAll(f.newRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(d, "state", "jobs.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f.st = st
	f.u = NewUndoWindow(undoCfg(f.newRoot, 1), st, slog.New(slog.NewJSONHandler(f.logs, nil)))
	f.u.now = func() time.Time { return f.clockNow }
	return f
}

// retain records an EXPIRED retention of src, creating its files on disk when onDisk.
func (f *unrootedFixture) retain(t *testing.T, src string, onDisk bool) store.Retained {
	t.Helper()
	r := store.Retained{
		SourcePath:         src,
		SwappedPath:        src,
		RetainedPath:       retainedPathFor(src, "10-1700000000"),
		SourceBytes:        10,
		SwappedFingerprint: "4:1700000001",
		RetainedAt:         f.clockNow.Add(-2 * time.Hour).Unix(),
		ExpiresAt:          f.clockNow.Add(-time.Hour).Unix(),
	}
	if onDisk {
		if err := os.MkdirAll(filepath.Dir(r.RetainedPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(r.RetainedPath, []byte("original!!"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(src, []byte("swap"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.st.Retain(context.Background(), r); err != nil {
		t.Fatalf("Retain: %v", err)
	}
	return r
}

func (f *unrootedFixture) kept(t *testing.T, src string) bool {
	t.Helper()
	r, ok, err := f.st.GetRetained(context.Background(), src)
	if err != nil {
		t.Fatalf("GetRetained: %v", err)
	}
	return ok && r.RestoredAt == nil
}

func TestUndo_ReleaseKeepsAMissingRetentionUnderNoConfiguredRoot(t *testing.T) {
	f := newUnrootedFixture(t)
	if err := os.MkdirAll(f.oldRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	moved := f.retain(t, filepath.Join(f.oldRoot, "moved.mkv"), false)
	gone := f.retain(t, filepath.Join(f.newRoot, "gone.mkv"), false)
	here := f.retain(t, filepath.Join(f.newRoot, "here.mkv"), true)

	rep := f.u.ReleaseExpired(context.Background())

	if rep.Unrooted != 1 || rep.UnrootedExample != moved.RetainedPath {
		t.Errorf("unrooted = %d (example %q), want 1 (%q)", rep.Unrooted, rep.UnrootedExample, moved.RetainedPath)
	}
	if !f.kept(t, moved.SourcePath) {
		t.Error("the record of a retained original under no configured root was DROPPED: nothing can release or " +
			"restore that original again once the root's new path is configured")
	}
	// Unchanged: under a configured root, a missing retained original is gone.
	if rep.Unrestorable != 1 || f.kept(t, gone.SourcePath) {
		t.Errorf("a missing retained original under a configured root: unrestorable = %d, kept = %v; want 1 and dropped",
			rep.Unrestorable, f.kept(t, gone.SourcePath))
	}
	if rep.Released != 1 || exists(here.RetainedPath) || f.kept(t, here.SourcePath) {
		t.Errorf("the reachable retention was not released: released = %d", rep.Released)
	}

	var warned bool
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["level"] == "WARN" && rec["records"] == float64(1) && rec["example"] == moved.RetainedPath &&
			strings.Contains(rec["next"].(string), "holdfast reroot") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("no WARN record counts the kept record, names it and points at holdfast reroot:\n%s", f.logs)
	}

	// The next pass keeps it again: it is held until its path is reachable, not for one pass.
	if rep := f.u.ReleaseExpired(context.Background()); rep.Unrooted != 1 || !f.kept(t, moved.SourcePath) {
		t.Errorf("a second pass did not keep the record (unrooted = %d)", rep.Unrooted)
	}
}

func TestUndo_RestoreRefusesAndKeepsAMissingRetentionUnderNoConfiguredRoot(t *testing.T) {
	f := newUnrootedFixture(t)
	moved := f.retain(t, filepath.Join(f.oldRoot, "moved.mkv"), false)
	gone := f.retain(t, filepath.Join(f.newRoot, "gone.mkv"), false)

	_, err := f.u.Restore(context.Background(), moved.SourcePath)
	if !errors.Is(err, ErrRetentionUnrooted) {
		t.Fatalf("Restore error = %v, want ErrRetentionUnrooted", err)
	}
	for _, want := range []string{moved.RetainedPath, "holdfast reroot", "kept"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not say %q: %v", want, err)
		}
	}
	if !f.kept(t, moved.SourcePath) {
		t.Error("a refused restore DROPPED the record of a retained original under no configured root")
	}

	// Unchanged: under a configured root, a missing retained original is unrestorable and
	// its record goes.
	if _, err := f.u.Restore(context.Background(), gone.SourcePath); !errors.Is(err, ErrUnrestorable) {
		t.Fatalf("Restore error = %v, want ErrUnrestorable", err)
	}
	if f.kept(t, gone.SourcePath) {
		t.Error("the record of a retained original missing under a configured root was kept")
	}
}
