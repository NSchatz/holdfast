package engine

// Impl-gate finding F1 on S0100-holdfast-bounded-run, ordinal 1: `--limit N` overshoots
// its bound whenever a swap does not complete cleanly.
//
// AC-4 binds two halves: "SHALL stop offering files once N files have reached a terminal
// outcome in that run" and "SHALL exit 0 having recorded no more than N terminal
// outcomes". The bound counts Engine.terminal, which is raised in finishStore and at the
// two pre-claim RecordSkip sites. It is NOT raised by recordIncident, and recordIncident
// is the writer of the two swap-failure outcomes store.Status.Terminal() calls terminal:
// Indeterminate and AppliedDespiteError. Neither is re-claimable, so each IS "a state the
// ledger records as final" in this repository's own vocabulary and in the spec's
// Definitions.
//
// The consequence is the accident this item exists to prevent. An operator who asked for
// one decision gets every eligible file in the library carried through the encoder, the
// gates and the swap, because the bound never observes a single outcome being recorded.
// The AppliedDespiteError branch is the sharp one: there the rename took effect, so the
// overshoot is a source replaced on a file the operator did not ask this run to touch.
//
// The fixture forces the Indeterminate branch through the engine's own seams - a rename
// that reports an error, over storage the filesystem lookup answers "nfs" for - which is
// exactly how swap_test.go's S1 case reaches the same outcome.

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/store"
)

// allTerminalStatuses is every status store.Status.Terminal() answers yes for, so the
// count below is of what the LEDGER calls final rather than of the subset the other
// bounded-run cases happen to list.
var allTerminalStatuses = []store.Status{
	store.Done, store.Skipped, store.Failed,
	store.WouldTranscode, store.Indeterminate, store.AppliedDespiteError,
}

func countTerminal(t *testing.T, ts *testStore) []store.Job {
	t.Helper()
	rows, err := ts.List(context.Background(), allTerminalStatuses, 0)
	if err != nil {
		t.Fatalf("List(every terminal status): %v", err)
	}
	return rows
}

// TestRegress0100F1_ALimitedRunOvershootsItsBoundWhenASwapIsIndeterminate is the finding.
// Three eligible sources, `--limit 1`, and every swap answering indeterminate: the run
// must record at most one terminal outcome and records three.
func TestRegress0100F1_ALimitedRunOvershootsItsBoundWhenASwapIsIndeterminate(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		mkH264(t, ffmpeg, filepath.Join(root, "film"+strconv.Itoa(i)+".mkv"), "8M")
	}
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = 1 })
	eng.renameFn = failingRename(errSwap)
	eng.fsLookup = lookups("nfs")
	ts := eng.Store.(*testStore)

	if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
		t.Fatalf("RunBounded(limit 1): %v", err)
	}

	rows := countTerminal(t, ts)
	// The fixture has to actually reach the branch, or the assertion below is vacuous.
	incidents := 0
	for _, r := range rows {
		if r.Status == store.Indeterminate || r.Status == store.AppliedDespiteError {
			incidents++
		}
	}
	if incidents == 0 {
		t.Fatalf("no swap-incident row was produced, so this fixture asserts nothing; rows: %+v", rows)
	}
	if len(rows) > 1 {
		paths := make([]string, 0, len(rows))
		for _, r := range rows {
			paths = append(paths, filepath.Base(r.Path)+"="+string(r.Status))
		}
		t.Fatalf("a --limit 1 run recorded %d terminal outcomes (%v), want no more than 1: "+
			"recordIncident writes Indeterminate/AppliedDespiteError rows without raising "+
			"Engine.terminal, so the bound never sees a decision being taken and the pass "+
			"carries the whole library", len(rows), paths)
	}
}

// TestRegress0100F1_TheSameBoundHoldsWhenTheSwapCompletes is the anti-vacuity arm: the
// identical fixture with the rename seam removed records exactly one outcome, so the
// failure above is about the uncounted incident row and not about the bound being broken
// for every run.
func TestRegress0100F1_TheSameBoundHoldsWhenTheSwapCompletes(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	for i := 0; i < 3; i++ {
		mkH264(t, ffmpeg, filepath.Join(root, "film"+strconv.Itoa(i)+".mkv"), "8M")
	}
	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = 1 })
	ts := eng.Store.(*testStore)

	if err := eng.RunBounded(context.Background(), Bound{Limit: 1}); err != nil {
		t.Fatalf("RunBounded(limit 1): %v", err)
	}
	if rows := countTerminal(t, ts); len(rows) != 1 {
		t.Fatalf("a --limit 1 run over three clean sources recorded %d terminal outcomes, want 1", len(rows))
	}
}

// TestRegress0100F1_TheBoundHoldsWithSeveralWorkers is the second anti-vacuity arm, and
// it is also the leg AC-4 was never executed against: the implementer's own AC-4 case
// runs at the default single worker, and the multi-worker half of the criterion rests on
// budget.admit's in-flight accounting. Decision 26 refuses to rubber-stamp a testable
// claim on reasoning, so it is executed here rather than reasoned about.
func TestRegress0100F1_TheBoundHoldsWithSeveralWorkers(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	for _, limit := range []int{1, 2, 3} {
		t.Run("limit="+strconv.Itoa(limit), func(t *testing.T) {
			root := t.TempDir()
			for i := 0; i < 8; i++ {
				mkHevc(t, ffmpeg, filepath.Join(root, "f"+strconv.Itoa(i)+".mkv"), "800k")
			}
			eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.Workers = 4 })
			ts := eng.Store.(*testStore)
			if err := eng.RunBounded(context.Background(), Bound{Limit: limit}); err != nil {
				t.Fatalf("RunBounded(limit %d): %v", limit, err)
			}
			if rows := countTerminal(t, ts); len(rows) != limit {
				t.Fatalf("a --limit %d run over eight eligible files with four workers recorded %d "+
					"terminal outcomes, want exactly %d", limit, len(rows), limit)
			}
		})
	}
}
