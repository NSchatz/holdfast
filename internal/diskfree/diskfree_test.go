package diskfree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The exported surface, over the REAL filesystem this test is running on. Two parts of
// holdfast refuse work on the strength of this number - the startup floor and the
// per-job pre-check - and both are graded through a seam, which is the right way to
// grade a refusal but leaves the call that actually answers unexecuted.
//
// It asserts what is true wherever this runs rather than a figure: a number no reading
// of a live filesystem can be pinned to, and the absence of one is the failure mode
// that matters (a caller comparing against 0 refuses everything, and one comparing
// against a fabricated large number refuses nothing).
//
// On a platform this build has no free-space call for, the documented answer is a
// REFUSAL, and that is asserted rather than skipped: a grader that skips is a false
// green, and "holdfast cannot establish the free space here" is a real obligation.
func TestBytes_ReportsTheSpaceOnTheFilesystemHoldingAPath(t *testing.T) {
	dir := t.TempDir()

	got, err := Bytes(dir)
	if runtime.GOOS != "linux" {
		if err == nil {
			t.Fatalf("Bytes on GOOS=%s returned %d and no error; this build has no free-space "+
				"call there and must say so rather than answer from nothing", runtime.GOOS, got)
		}
		return
	}
	if err != nil {
		t.Fatalf("Bytes(%s): %v", dir, err)
	}
	if got == 0 {
		t.Fatal("Bytes reported 0 bytes available on the filesystem holding a directory this " +
			"test just wrote into, which every caller reads as a full device")
	}

	// The same answer for a FILE as for the directory holding it: statfs(2) is a question
	// about the filesystem, not about the inode, and the per-job pre-check asks it about a
	// source path. Both readings are of one live filesystem, so they are compared for
	// their ORDER of magnitude rather than for equality.
	f := filepath.Join(dir, "source.mkv")
	if err := os.WriteFile(f, []byte("not really a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	viaFile, err := Bytes(f)
	if err != nil {
		t.Fatalf("Bytes(%s): %v", f, err)
	}
	if viaFile < got/2 || viaFile > got*2 {
		t.Errorf("Bytes reported %d for a file and %d for the directory holding it; these are "+
			"the same filesystem and the two readings are seconds apart", viaFile, got)
	}
}

// A path that cannot be inspected is an ERROR and never a number. This is the fail-safe
// direction stated in the package comment: 0 would make the startup floor refuse every
// run and a large number would make the per-job pre-check pass on a full device.
func TestBytes_APathThatCannotBeInspectedIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no", "such", "directory")
	got, err := Bytes(missing)
	if err == nil {
		t.Fatalf("Bytes(%s) returned %d and no error for a path that does not exist", missing, got)
	}
	if got != 0 {
		t.Errorf("Bytes returned %d alongside its error; a refusal carries no measurement", got)
	}
}
