//go:build linux

package diskfree

import (
	"errors"
	"syscall"
	"testing"
)

// The arithmetic, over filesystem shapes no test machine can be made to have.
//
// Two decisions live in three lines, and both are invisible on the filesystems a test
// actually runs on: a tmpfs reserves nothing for root, so f_bavail and f_bfree are
// equal there and a reading of either passes; and no filesystem in the container
// answers with an unusable block size at all. Constructing the struct is the only way
// to execute either, and both are decisions an operator's original rests on.
func TestFromStatfs_CountsTheBlocksAvailableToAnUnprivilegedProcess(t *testing.T) {
	// A default ext4's shape: 5% of the device held back for root. Bavail and Bfree
	// differ, so the two readings differ by the whole reserve and only one of them is
	// space holdfast can write into.
	st := syscall.Statfs_t{Bsize: 4096, Blocks: 250_000, Bfree: 100_000, Bavail: 87_500}

	got, err := fromStatfs(st)
	if err != nil {
		t.Fatalf("fromStatfs: %v", err)
	}
	if want := uint64(87_500 * 4096); got != want {
		t.Errorf("fromStatfs = %d, want %d", got, want)
	}
	if reserved := uint64(100_000 * 4096); got == reserved {
		t.Errorf("fromStatfs counted the blocks a filesystem reserves for root (%d); holdfast "+
			"cannot write into those, so a floor measured against them passes on a device the "+
			"encode then fails on", reserved)
	}
}

// No usable block size is a REFUSAL and not a number. There is no arithmetic to do from
// it: 0 would read to every caller as a full device and a large number as plenty of
// room, and both are inventions about storage nobody measured.
func TestFromStatfs_RefusesAFilesystemWithNoUsableBlockSize(t *testing.T) {
	for _, bsize := range []int64{0, -1, -4096} {
		st := syscall.Statfs_t{Bavail: 1_000_000}
		st.Bsize = int64(bsize)

		got, err := fromStatfs(st)
		if !errors.Is(err, errNoBlockSize) {
			t.Errorf("f_bsize=%d: fromStatfs = (%d, %v), want the no-block-size refusal", bsize, got, err)
		}
		if got != 0 {
			t.Errorf("f_bsize=%d: fromStatfs returned %d alongside its refusal", bsize, got)
		}
	}
}
