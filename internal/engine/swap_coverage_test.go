package engine

// The hold-backs on the PRODUCTION enumeration path.
//
// `cmd/holdfast` builds the engine with the startup walk's Coverage set, so that is the
// branch of enumerate() and cleanStaleTemps() a real run takes - the recursive fallback
// exists for an engine built without the startup check. A hold-back proven only on the
// fallback would be a hold-back nobody's install has.
//
// These tests substitute nothing but the store: no ffmpeg, no seams, no encode. They
// assert what the two record-based hold-backs (a parked job's two recorded paths, and a
// recorded replacement path whose disposition still excludes it) do to the list of files
// a run would hand its workers, and that the record-free name basis holds where no
// record survived.

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// heldEngine builds an engine over a real store with Coverage set, publishes this run's
// hold-backs exactly as RunOneshot does, and returns it.
func heldEngine(t *testing.T, root string, st store.Store, coverage []string) *Engine {
	t.Helper()
	cfg := config.Config{LibraryRoots: []string{root}, VideoExts: []string{"mkv", "mp4"}}
	e := New(cfg, nil, nil, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	e.Coverage = coverage
	e.held.Store(e.loadHoldBacks(context.Background()))
	return e
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// TestCoverageBoundedEnumerate_HoldsBackExactlyTheRecordedPaths is AC15c and AC15d on the
// path a real run takes. Exactly two files are withheld - a parked job's source and its
// replacement - and everything else in the same covered directories is still offered, so
// a parked job costs the library two files and never narrows the run.
func TestCoverageBoundedEnumerate_HoldsBackExactlyTheRecordedPaths(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	defer func() { _ = ts.Close() }()

	parkedSrc := filepath.Join(root, "Parked.mkv")
	replacement := filepath.Join(root, "Parked."+RetainedMarker+".mkv")
	other := filepath.Join(root, "Other.mkv")
	deep := filepath.Join(root, "sub", "Deep.mkv")
	for _, p := range []string{parkedSrc, replacement, other, deep} {
		mustWrite(t, p)
	}

	if err := ts.RecordSwapIncident(context.Background(), store.SwapIncident{
		SourcePath: parkedSrc, SourceFingerprint: "1:1", ReplacementPath: replacement,
		SourceAttrs: "1:1", ReplacementAttrs: "2:2", Outcome: store.Indeterminate,
	}); err != nil {
		t.Fatalf("record incident: %v", err)
	}

	e := heldEngine(t, root, ts, []string{root, filepath.Join(root, "sub")})
	got := e.enumerate()

	if contains(got, parkedSrc) {
		t.Errorf("the parked job's SOURCE was enumerated: %v", got)
	}
	if contains(got, replacement) {
		t.Errorf("the parked job's REPLACEMENT was enumerated: %v", got)
	}
	for _, want := range []string{other, deep} {
		if !contains(got, want) {
			t.Errorf("%s was withheld; a parked job may withhold exactly two files and no more: %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("enumerate() = %v, want exactly the two ordinary sources", got)
	}
}

// TestCoverageBoundedEnumerate_ARetainedReplacementIsHeldWithNoRecordAtAll is AC15i's
// record-free basis on the same path: the store write that failed is precisely what
// denied the record, so a hold-back that needed one would be absent exactly when it
// matters. Nothing else is held back on its name.
func TestCoverageBoundedEnumerate_ARetainedReplacementIsHeldWithNoRecordAtAll(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	defer func() { _ = ts.Close() }()

	orphan := filepath.Join(root, "Film."+RetainedMarker+".mkv")
	ordinary := filepath.Join(root, "Film.mkv")
	lookalike := filepath.Join(root, "Film.holdfast-replacement.mkv") // NOT the construction
	for _, p := range []string{orphan, ordinary, lookalike} {
		mustWrite(t, p)
	}

	e := heldEngine(t, root, ts, []string{root})
	got := e.enumerate()

	if contains(got, orphan) {
		t.Errorf("a replacement holdfast wrote, with no record surviving, was enumerated: %v", got)
	}
	if !contains(got, ordinary) {
		t.Errorf("an ordinary source in the same root was withheld: %v", got)
	}
	if !contains(got, lookalike) {
		t.Errorf("a file the build's own construction could NOT have produced was held back on its name: %v", got)
	}
}

// TestCoverageBoundedSweep_LeavesATempARecordNames. The stale-temp sweep runs under the
// same Coverage bound, and it must not take a file a live record calls a job's
// replacement - which is what a replacement that could not be MOVED to its retained name
// looks like. A work-in-progress temp nothing records is still swept, unchanged.
func TestCoverageBoundedSweep_LeavesATempARecordNames(t *testing.T) {
	root := t.TempDir()
	ts := newTestStore(t, root)
	defer func() { _ = ts.Close() }()

	recorded := filepath.Join(root, "Held."+TempMarker+".mkv")
	orphaned := filepath.Join(root, "Abandoned."+TempMarker+".mkv")
	mustWrite(t, recorded)
	mustWrite(t, orphaned)

	if err := ts.RecordSwapIncident(context.Background(), store.SwapIncident{
		SourcePath: filepath.Join(root, "Held.mkv"), SourceFingerprint: "1:1",
		ReplacementPath: recorded, SourceAttrs: "1:1", ReplacementAttrs: "2:2",
		Outcome: store.Indeterminate,
	}); err != nil {
		t.Fatalf("record incident: %v", err)
	}

	e := heldEngine(t, root, ts, []string{root})
	e.cleanStaleTemps(context.Background())

	if _, err := os.Stat(recorded); err != nil {
		t.Errorf("the sweep deleted a replacement a live record names: %v", err)
	}
	if _, err := os.Stat(orphaned); err == nil {
		t.Error("an orphaned work-in-progress temp survived the sweep - the pre-existing behaviour changed")
	}
}
