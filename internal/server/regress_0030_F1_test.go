// Refuter artifact for S0030-holdfast-dash-8, impl gate ordinal 1, finding F1.
// This file documents a defect; it is not a fix. Delete it only when the behaviour
// it pins is corrected upstream.
package server

import (
	"context"
	"testing"

	"github.com/NSchatz/holdfast/internal/engine"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestRegress0030F1_VerifyingRowCarriesTheFinishedEncodesProgress shows that the last
// figure the ENCODER reported survives the transition out of `encoding` and is published
// on the `verifying` row, where nothing is measuring it any more.
//
// The spec puts that outside this item on purpose:
//
//	Out (explicit):
//	- A progress percentage for the verify/VMAF phase. `internal/vmaf` keeps its
//	  current invocation; the verifying state is covered by elapsed alone (AC1).
//
// and the page's own renderer claims the property this breaks: "There is no
// interpolation, no estimate from elapsed time, and no carry-over of the last figure -
// an absent measurement is displayed as absent." (index.html, progressCell).
//
// Hub.Observe drops a live entry only on a TERMINAL status, and liveProgressFor prunes
// only against the active list - a verifying job is still active, so its stale encode
// figure rides every snapshot for the whole verify/VMAF phase, which on a feature-length
// source is minutes to hours of a frozen percentage next to a state it does not describe.
func TestRegress0030F1_VerifyingRowCarriesTheFinishedEncodesProgress(t *testing.T) {
	h := newHarness(t, "")
	ctx := context.Background()

	// The encoder reports 300s of a 400s source, then the encode ends.
	dur := 400.0
	h.hub.Observe(progressEvent("/lib/active.mkv", 300, &dur))

	// The job moves on to the verify gate. This is a normal, non-terminal transition:
	// exactly what ProcessFile emits between the encode and the swap.
	if err := h.st.Advance(ctx, "/lib/active.mkv", "1:1", store.Verifying); err != nil {
		t.Fatalf("Advance to verifying: %v", err)
	}
	h.hub.Observe(engine.Event{Path: "/lib/active.mkv", Status: store.Verifying, Worker: "w0"})

	row := queueRowFor(t, snapshotOf(t, h.hub), "/lib/active.mkv")
	if row.Status != string(store.Verifying) {
		t.Fatalf("fixture did not reach the verifying state: status = %q", row.Status)
	}
	if row.ProgressFraction != nil {
		t.Errorf("a verifying row publishes progress_fraction = %v; the verifying state is covered by elapsed alone, and this figure measures an encode that has already finished",
			*row.ProgressFraction)
	}
	if row.ProgressSeconds != nil {
		t.Errorf("a verifying row publishes progress_seconds = %v - a carried-over position from the finished encode",
			*row.ProgressSeconds)
	}
	if h.hub.liveProgressCount() != 0 {
		t.Errorf("the hub still holds %d live progress entries for a job whose encoder has exited",
			h.hub.liveProgressCount())
	}
}
