package engine

import (
	"testing"

	"github.com/NSchatz/holdfast/internal/node"
)

// TestNodeCouldNotRun_TheReasonsThatAreNeverChargedToTheFile pins the closed set of typed
// reasons a worker fails a lease with that say nothing about the file: the server then
// encodes the job itself and max_failures does not count the lease
// (docs/design/nodes.md#endings). A reason outside it is a lease that was really attempted.
func TestNodeCouldNotRun_TheReasonsThatAreNeverChargedToTheFile(t *testing.T) {
	want := []string{
		"unmapped_source", "source_mismatch", "source_unreadable", "unsupported_encoder", "refused_plan",
		"worker_stopping", "source_download_failed", "work_dir_full", "source_withdrawn",
	}
	for _, reason := range want {
		if !nodeCouldNotRun[reason] {
			t.Errorf("%s is charged to the file; it says the node could not run the lease", reason)
		}
	}
	if len(nodeCouldNotRun) != len(want) {
		t.Errorf("nodeCouldNotRun holds %d reasons, want exactly %d: %v", len(nodeCouldNotRun), len(want), nodeCouldNotRun)
	}
	if !nodeCouldNotRun[node.ReasonSourceWithdrawn] {
		t.Error("the reason the hub leaves out of a node's cool-off is charged to the file")
	}
	for _, reason := range []string{"encode_failed", "output_too_large", "upload_refused", "source_digest_mismatch", "expired", ""} {
		if nodeCouldNotRun[reason] {
			t.Errorf("%q is not charged to the file; a lease that ended on it was really attempted", reason)
		}
	}
}
