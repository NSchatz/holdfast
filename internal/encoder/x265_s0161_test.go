package encoder

import (
	"fmt"
	"testing"
)

// TestS0161_AC2_EveryBudgetDerivesAFrameCountX265Accepts grades AC-2's range clause over
// every budget the x265_cpus key accepts and well past it: the pool takes the budget, and
// the frame-thread count is always one libx265 documents as accepted ("any value between
// 0 and 16") and never 0, which is libx265's own auto-detect rather than a count.
func TestS0161_AC2_EveryBudgetDerivesAFrameCountX265Accepts(t *testing.T) {
	for cpus := 1; cpus <= 4096; cpus++ {
		p := X265ParallelismFor(cpus)
		if p.CPUs != cpus || p.Pools != cpus {
			t.Fatalf("X265ParallelismFor(%d) = %+v: the pool must take the whole budget", cpus, p)
		}
		if p.FrameThreads < 1 || p.FrameThreads > X265MaxFrameThreads {
			t.Fatalf("X265ParallelismFor(%d) derived frame-threads=%d, outside 1..%d", cpus,
				p.FrameThreads, X265MaxFrameThreads)
		}
		if want := fmt.Sprintf(":pools=%d:frame-threads=%d", cpus, p.FrameThreads); p.Params() != want {
			t.Fatalf("Params() = %q, want %q", p.Params(), want)
		}
	}
	if X265MaxFrameThreads != 16 {
		t.Errorf("X265MaxFrameThreads = %d, want the 16 libx265 documents", X265MaxFrameThreads)
	}
}

// TestS0161_AC2_TheFrameCountFollowsX265sOwnTable pins the derivation notes.md states:
// the frame-thread count libx265 auto-detects for a WPP encode on a machine of that many
// CPUs, at every boundary of that table.
func TestS0161_AC2_TheFrameCountFollowsX265sOwnTable(t *testing.T) {
	for _, tc := range []struct{ cpus, want int }{
		{1, 1}, {3, 1}, {4, 2}, {7, 2}, {8, 3}, {15, 3}, {16, 4}, {24, 4}, {31, 4}, {32, 5}, {1024, 5},
	} {
		if got := X265ParallelismFor(tc.cpus).FrameThreads; got != tc.want {
			t.Errorf("frame-threads for %d CPUs = %d, want %d", tc.cpus, got, tc.want)
		}
	}
}

// TestS0161_AC5_NoBudgetPassesNothing grades the argv half of AC-5 at its source: no
// budget derives the zero value, which adds nothing to -x265-params, so an encode without
// a figure keeps libx265's own defaults and the argument list it always had.
func TestS0161_AC5_NoBudgetPassesNothing(t *testing.T) {
	for _, cpus := range []int{0, -1, -24} {
		p := X265ParallelismFor(cpus)
		if p != (X265Parallelism{}) || p.Set() || p.Params() != "" {
			t.Errorf("X265ParallelismFor(%d) = %+v, Set=%v, Params=%q: want the zero value, false, \"\"",
				cpus, p, p.Set(), p.Params())
		}
	}
	if (X265Parallelism{}).Params() != "" {
		t.Error("the zero value adds to -x265-params")
	}
	if !X265ParallelismFor(1).Set() {
		t.Error("a budget of 1 CPU reports itself unset")
	}
}
