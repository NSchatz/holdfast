package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
)

func coverageEngine(t *testing.T, root string, coverage []string) *Engine {
	t.Helper()
	cfg := config.Config{
		LibraryRoots: []string{root},
		VideoExts:    []string{"mkv", "mp4"},
	}
	e := New(cfg, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.Coverage = coverage
	return e
}

func mustWrite(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEnumerate_TheStartupWalkCoverageBoundsWhatTheScanCanSee. The startup walk
// decides what this run may touch, and the scan may not go behind it: it lists
// exactly the directories that walk traversed successfully, with no recursion of
// its own. A directory the walk declined (an already-walked region, a bind loop),
// could not read, or never reached yields no source at all - which is what stops
// one file being enumerated twice through two spellings, and what makes the scan
// terminate over a mount layout a recursive walk would follow for ever.
func TestEnumerate_TheStartupWalkCoverageBoundsWhatTheScanCanSee(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	mustWrite(t, filepath.Join(root, "walked", "Walked.mkv"))
	mustWrite(t, filepath.Join(root, "declined", "Declined.mkv"))
	mustWrite(t, filepath.Join(root, "walked", "deeper", "Deeper.mkv"))
	mustWrite(t, filepath.Join(root, "walked", "notes.txt"))
	mustWrite(t, filepath.Join(root, "walked", "Half.__transcoding__.mkv"))

	e := coverageEngine(t, root, []string{root, filepath.Join(root, "walked")})
	got, observed := e.enumerate()
	want := []string{
		filepath.Join(root, "Top.mkv"),
		filepath.Join(root, "walked", "Walked.mkv"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enumerate() = %v, want %v", got, want)
	}
	// The observed set is the same bound in its other form, and the retention pass
	// depends on it being exactly this: a directory holdfast did NOT list is one it
	// may draw no conclusion from, however absent a file under it looks.
	wantObserved := map[string]bool{root: true, filepath.Join(root, "walked"): true}
	if !reflect.DeepEqual(observed, wantObserved) {
		t.Fatalf("enumerate() observed %v, want exactly %v", observed, wantObserved)
	}

	// Without a coverage set (an Engine built with no startup check) the old
	// recursive behaviour is unchanged.
	plain := coverageEngine(t, root, nil)
	plainFiles, plainObserved := plain.enumerate()
	if n := len(plainFiles); n != 4 {
		t.Fatalf("unbounded enumerate() found %d sources, want the 4 in the tree", n)
	}
	for _, dir := range []string{root, filepath.Join(root, "walked"), filepath.Join(root, "declined"),
		filepath.Join(root, "walked", "deeper")} {
		if !plainObserved[dir] {
			t.Errorf("the unbounded walk listed %s and did not report observing it", dir)
		}
	}

	// And an EMPTY bound is not an absent one. nil means "no startup check
	// ran"; a non-nil empty slice means "that check traversed nothing, so this
	// run may enumerate nothing". Confusing the two would un-bound the scan at
	// exactly the moment the bound matters most, which is why the startup check
	// never returns a nil Coverage (see startup.Result.Coverage).
	if got, obs := coverageEngine(t, root, []string{}).enumerate(); len(got) != 0 || len(obs) != 0 {
		t.Fatalf("an empty coverage set enumerated %v and observed %v, want nothing at all", got, obs)
	}
}

// TestEnumerate_CoverageThatNoLongerExistsIsSkipped: the walk ran before the
// scan, and the world can move underneath it. A directory that has since gone is
// skipped, never a crash and never an abort of the whole scan.
func TestEnumerate_CoverageThatNoLongerExistsIsSkipped(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	e := coverageEngine(t, root, []string{root, filepath.Join(root, "gone")})
	got, observed := e.enumerate()
	if want := []string{filepath.Join(root, "Top.mkv")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enumerate() = %v, want %v", got, want)
	}
	// Skipped, and NOT reported as observed. A directory that has gone since the walk is
	// one this run has no evidence about - which is what stops the retention pass reading
	// an unmounted subtree as a library the operator deleted.
	if observed[filepath.Join(root, "gone")] {
		t.Fatal("a coverage entry that no longer exists was reported as a directory this run listed")
	}
	if !observed[root] {
		t.Fatal("the root was listed and not reported as observed")
	}
}

// TestCleanStaleTemps_IsBoundedByTheSameCoverage: the temp sweep mutates files
// under a library root, so it obeys the same bound. A directory the startup walk
// did not traverse is one this run touches in no way at all.
func TestCleanStaleTemps_IsBoundedByTheSameCoverage(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "walked", "Film.__transcoding__.mkv")
	outside := filepath.Join(root, "declined", "Film.__transcoding__.mkv")
	mustWrite(t, inside)
	mustWrite(t, outside)

	e := coverageEngine(t, root, []string{root, filepath.Join(root, "walked")})
	e.cleanStaleTemps(context.Background())

	if _, err := os.Stat(inside); err == nil {
		t.Fatal("a temp under a traversed directory survived the sweep")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a temp under a directory the walk did not traverse was removed: %v", err)
	}
}

// TestIsSourceName is the ONE definition of a media file this run would
// enumerate, shared by the scan and the startup walk, and it decides from the
// name alone - no file is opened to answer it.
func TestIsSourceName(t *testing.T) {
	exts := []string{"mkv", "mp4"}
	tests := []struct {
		base string
		want bool
	}{
		{"Film.mkv", true},
		{"Film.MKV", true},
		{"Film.mp4", true},
		{"notes.txt", false},
		{"Film", false},
		{"Film." + TempMarker + ".mkv", false},
	}
	for _, tc := range tests {
		if got := IsSourceName(tc.base, exts); got != tc.want {
			t.Fatalf("IsSourceName(%q) = %v, want %v", tc.base, got, tc.want)
		}
	}
}
