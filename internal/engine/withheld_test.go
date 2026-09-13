package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The operator's own withholding, at the door into the pipeline.
//
// A withheld path is one an operator has taken out of the pipeline from the surface that
// showed them the file failing. The three things that makes it are all graded here: the
// file is never offered to the encoder, the row it leaves NAMES the withholding rather
// than some guard that never looked at the file, and removing the withholding makes the
// path eligible again on the very next scan.
//
// No ffmpeg is needed and none is used: every guard this exercises fires BEFORE the probe
// snapshot, so the prober is pointed at a program that does not exist and the encoder is
// one that fails the test if anything ever reaches it. That is not a convenience, it is
// half the assertion - a withheld file must not reach the encoder, and an encoder that
// would fail the test if it did is how that is said.

// refusingEncoder fails the test the moment anything is offered to it.
type refusingEncoder struct{ t *testing.T }

func (e refusingEncoder) Encode(_ context.Context, in, _ string, _ *probe.VideoProps) error {
	e.t.Errorf("a file was offered to the encoder: %q. Nothing in this case may reach it", in)
	return nil
}

// withheldEngine builds an engine over root with no real tooling behind it.
func withheldEngine(t *testing.T, root string, st store.Store) *Engine {
	t.Helper()
	cfg := baseCfg(root)
	return New(cfg, probe.New(filepath.Join(t.TempDir(), "no-such-ffmpeg"),
		filepath.Join(t.TempDir(), "no-such-ffprobe")), refusingEncoder{t}, st, discardLogger())
}

// withheldRowFor returns the ledger row for path, and whether there is one at all.
func withheldRowFor(t *testing.T, st store.Store, path string) (store.Job, bool) {
	t.Helper()
	rows, err := st.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("store.List: %v", err)
	}
	for _, r := range rows {
		if r.Path == path {
			return r, true
		}
	}
	return store.Job{}, false
}

// Criterion 9. A withheld path is held out of the pipeline on every subsequent scan, and
// the row it leaves states the withholding as its reason rather than a guard that did not
// decide it.
func TestExcludedPathIsWithheldWithItsOwnReason(t *testing.T) {
	root := t.TempDir()
	withheld := filepath.Join(root, "withheld.mkv")
	eligible := filepath.Join(root, "eligible.mkv")
	for _, p := range []string{withheld, eligible} {
		if err := os.WriteFile(p, []byte("not really a video, and nothing here ever probes it"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
	}

	ts := newTestStore(t, root)
	ctx := context.Background()
	if added, err := ts.ExcludePath(ctx, withheld); err != nil || !added {
		t.Fatalf("ExcludePath: added=%v err=%v", added, err)
	}

	eng := withheldEngine(t, root, ts)
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	row, ok := withheldRowFor(t, ts, withheld)
	if !ok {
		t.Fatalf("the withheld path left NO row at all. A file that silently stops being worked on, "+
			"with nothing on the ledger saying why, is the dead end this whole surface exists to end: %q", withheld)
	}
	if row.Status != store.Skipped {
		t.Errorf("the withheld path is recorded as %q, want %q", row.Status, store.Skipped)
	}
	if row.Outcome.Reason != SkipOperatorExcluded {
		t.Errorf("the withheld path's row records the reason %q, want %q. A row that names some other "+
			"guard reports a verdict nothing reached: no guard below the withholding ever looked at this file",
			row.Outcome.Reason, SkipOperatorExcluded)
	}
	// And it is not any of the guards that did not decide it. Named one at a time so a
	// failure says WHICH verdict was fabricated.
	for _, other := range []string{
		SkipAlreadyTargetCodec, SkipLowBitrate, SkipHardlinked, SkipInterlaced, SkipDolbyVision,
		SkipHDR10Plus, SkipIncompleteHDRMetadata, SkipExoticPixelFormat, SkipTargetExists,
		SkipSymlink, SkipMultiVideoStream, SkipUndoRetentionFailed, SkipRestoredOriginal,
		FailUnreadable,
	} {
		if row.Outcome.Reason == other {
			t.Errorf("the withheld path's row reports %q, a guard that never ran on it", other)
		}
	}

	// The withholding is per path. The file beside it went the ordinary way, which here
	// means it reached the probe and was recorded unreadable - the point being only that
	// the run happened and the guard did not swallow the library.
	if other, ok := withheldRowFor(t, ts, eligible); !ok {
		t.Errorf("the file beside the withheld one left no row, so the withholding took more than the path it named")
	} else if other.Outcome.Reason == SkipOperatorExcluded {
		t.Errorf("a path nobody withheld was recorded as withheld: %q", eligible)
	}

	// EVERY SUBSEQUENT SCAN, not just the first: a second pass must still withhold it.
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot (second pass): %v", err)
	}
	row, ok = withheldRowFor(t, ts, withheld)
	if !ok || row.Status != store.Skipped || row.Outcome.Reason != SkipOperatorExcluded {
		t.Errorf("after a second scan the withheld path reads status=%q reason=%q (exists=%v), "+
			"want a skipped row naming the withholding", row.Status, row.Outcome.Reason, ok)
	}
}

// Criterion 12's engine half. Removing the withholding makes the path eligible again on
// the next scan: the stale row goes and the file re-enters the ordinary path.
//
// This is what keeps a wrongly recorded withholding from being a file that silently stops
// being worked on for ever, which is the one residual risk the whole feature carries.
func TestExcludedPathIsEligibleAgainOnceTheWithholdingIsRemoved(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "film.mkv")
	if err := os.WriteFile(p, []byte("not really a video"), 0o644); err != nil {
		t.Fatalf("writing %s: %v", p, err)
	}

	ts := newTestStore(t, root)
	ctx := context.Background()
	if _, err := ts.ExcludePath(ctx, p); err != nil {
		t.Fatalf("ExcludePath: %v", err)
	}
	eng := withheldEngine(t, root, ts)
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if row, ok := withheldRowFor(t, ts, p); !ok || row.Outcome.Reason != SkipOperatorExcluded {
		t.Fatalf("the fixture is wrong: the path was not withheld (exists=%v reason=%q), so removing "+
			"the withholding proves nothing", ok, row.Outcome.Reason)
	}

	if gone, err := ts.UnexcludePath(ctx, p); err != nil || !gone {
		t.Fatalf("UnexcludePath: gone=%v err=%v", gone, err)
	}
	if err := eng.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot (after the withholding was removed): %v", err)
	}
	row, ok := withheldRowFor(t, ts, p)
	if !ok {
		t.Fatalf("the path left no row at all after the withholding was removed")
	}
	if row.Outcome.Reason == SkipOperatorExcluded {
		t.Errorf("the path still carries the withholding's own skip row after the withholding was "+
			"removed, so it is excluded by a record nobody can see any more: %+v", row)
	}
}
