package engine

// Which hold-backs the DOOR judges a file against (S0093).
//
// The two halves of that question are in tension and getting either one wrong is a file
// this tool is not allowed to touch, so they are pinned together here.
//
// A scan pass publishes one snapshot and reads it from end to end. A targeted submission
// has no pass: in the deployment this endpoint exists for (scan_interval_sec: 0) there is
// never another pass at all, so a snapshot published once is frozen for the life of the
// process, and a swap that parks an incident after it was taken records two paths the
// submission route would then walk straight past. A recorded replacement path is not
// always a name this build can recognise - swap.go's applied-despite-error branch records
// where the replacement WAS, and retainReplacement records the path it could not move the
// file off - so for those files the record is the only thing holding them back.
//
// The second half is what may NOT be done about it: a pass in flight must keep the
// snapshot it is reading. Refreshing under it would change what the scan already in
// progress holds back, half way through.

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestHoldBacksTheDoorUsesAreTheOnesInForce is the decision itself.
// TestRegress0093F1SubmissionMissesAHoldBackParkedAfterTheQueueStarted exercises the same
// first half through the real queue and a real encoder; this case fails by name if a later
// change quietly reintroduces a frozen snapshot, or takes the freshness by overwriting a
// pass's.
func TestHoldBacksTheDoorUsesAreTheOnesInForce(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	ts := newTestStore(t, root)
	eng := engineOver(t, ffmpeg, ffprobe, root, ts, nil, nil)
	ctx := context.Background()

	park := func(t *testing.T, source, replacement string) {
		t.Helper()
		if err := ts.RecordSwapIncident(ctx, store.SwapIncident{
			SourcePath:        source,
			SourceFingerprint: probe.Fingerprint(source),
			ReplacementPath:   replacement,
			Outcome:           store.Indeterminate,
			SwapError:         "simulated: the swap outcome could not be established",
		}); err != nil {
			t.Fatalf("RecordSwapIncident: %v", err)
		}
	}

	t.Run("between passes the door reads the store", func(t *testing.T) {
		// The startup snapshot, published before anything is parked - which is exactly
		// what a daemon with no periodic scan has, for ever.
		eng.EnsureHoldBacks(ctx)
		startup := eng.held.Load()

		replacement := filepath.Join(root, "parked-after-startup.mkv")
		park(t, filepath.Join(root, "source-after-startup.mkv"), replacement)

		if _, ok := startup.held(replacement); ok {
			t.Fatal("the startup snapshot already holds the path, so this case establishes nothing about a " +
				"hold-back recorded after it was taken")
		}
		if _, ok := eng.holdBacksInForce(ctx).held(replacement); !ok {
			t.Error("the door did not hold back a path a swap parked after the last snapshot was published; " +
				"with scan_interval_sec: 0 no later pass ever publishes another one, so that file would be " +
				"encoded and swapped for the life of the process")
		}
	})

	t.Run("during a pass the door reads that pass's snapshot and never replaces it", func(t *testing.T) {
		pinned := filepath.Join(root, "parked-before-the-pass.mkv")
		park(t, filepath.Join(root, "source-before-the-pass.mkv"), pinned)

		// A pass, opened exactly as RunOneshot opens one: publish the snapshot, then
		// raise the in-flight count.
		pass := eng.readHoldBacks(ctx)
		eng.held.Store(pass)
		eng.passes.Add(1)
		defer eng.passes.Add(-1)

		late := filepath.Join(root, "parked-during-the-pass.mkv")
		park(t, filepath.Join(root, "source-during-the-pass.mkv"), late)

		if _, ok := eng.holdBacksInForce(ctx).held(pinned); !ok {
			t.Fatal("the pass's snapshot does not hold what it was built holding, so this case establishes " +
				"nothing about which snapshot the door read")
		}
		if _, ok := eng.holdBacksInForce(ctx).held(late); ok {
			t.Error("a decision taken mid-pass was judged against hold-backs this pass's own workers do not " +
				"see, so the scan route and the submission route disagree about the file beside them")
		}
		if eng.held.Load() != pass {
			t.Error("the snapshot a pass is reading from end to end was replaced while the pass was reading it")
		}
	})
}

// TestRunOneshotMarksItsPassInFlight is the link between the decision above and the real
// scan: holdBacksInForce answers "is there a pass to agree with", and this is what puts a
// pass there. Without it the first half would be correct and unreachable, because every
// caller would look like a caller with no pass.
func TestRunOneshotMarksItsPassInFlight(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root := t.TempDir()
	// One enumerable file, so the feed loop runs at all. It is never processed: Paused
	// answers true, which stops the feed before anything is handed to a worker.
	mkH264(t, ffmpeg, filepath.Join(root, "film.mkv"), "1M")

	ts := newTestStore(t, root)
	eng := engineOver(t, ffmpeg, ffprobe, root, ts, nil, nil)

	var insidePass int64
	var asked bool
	eng.Paused = func() bool {
		// Called by the feed loop, which is inside the pass by construction.
		asked = true
		insidePass = eng.passes.Load()
		return true
	}

	if got := eng.passes.Load(); got != 0 {
		t.Fatalf("passes = %d before any pass ran, want 0", got)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if !asked {
		t.Fatal("the feed loop never ran, so nothing was observed from inside the pass")
	}
	if insidePass <= 0 {
		t.Errorf("passes = %d from inside a running pass, want > 0: every caller of ProcessFile would be "+
			"judged against a fresh read instead of this pass's own snapshot", insidePass)
	}
	if got := eng.passes.Load(); got != 0 {
		t.Errorf("passes = %d after the pass returned, want 0: a pass that never closes would freeze the "+
			"snapshot every later submission is judged against", got)
	}
}
