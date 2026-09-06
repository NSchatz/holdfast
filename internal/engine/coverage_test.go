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
	got := e.enumerate()
	want := []string{
		filepath.Join(root, "Top.mkv"),
		filepath.Join(root, "walked", "Walked.mkv"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("enumerate() = %v, want %v", got, want)
	}

	// Without a coverage set (an Engine built with no startup check) the old
	// recursive behaviour is unchanged.
	plain := coverageEngine(t, root, nil)
	if n := len(plain.enumerate()); n != 4 {
		t.Fatalf("unbounded enumerate() found %d sources, want the 4 in the tree", n)
	}

	// And an EMPTY bound is not an absent one. nil means "no startup check
	// ran"; a non-nil empty slice means "that check traversed nothing, so this
	// run may enumerate nothing". Confusing the two would un-bound the scan at
	// exactly the moment the bound matters most, which is why the startup check
	// never returns a nil Coverage (see startup.Result.Coverage).
	if got := coverageEngine(t, root, []string{}).enumerate(); len(got) != 0 {
		t.Fatalf("an empty coverage set enumerated %v, want nothing at all", got)
	}
}

// TestEnumerate_CoverageThatNoLongerExistsIsSkipped: the walk ran before the
// scan, and the world can move underneath it. A directory that has since gone is
// skipped, never a crash and never an abort of the whole scan.
func TestEnumerate_CoverageThatNoLongerExistsIsSkipped(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "Top.mkv"))
	e := coverageEngine(t, root, []string{root, filepath.Join(root, "gone")})
	if got, want := e.enumerate(), []string{filepath.Join(root, "Top.mkv")}; !reflect.DeepEqual(got, want) {
		t.Fatalf("enumerate() = %v, want %v", got, want)
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
