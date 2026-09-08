package engine

// Where UNDO-6's retention meets FILESYSTEM-1's failed-swap outcomes.
//
// The two phases were built on branches that forked before either landed, and their
// merge had exactly one decision in it: what happens to the retained original when the
// rename FAILS. Every other path that declines to swap drops the retention, because a
// second link with no swap behind it is an orphan raising the source's link count for
// nothing. A failed rename is not such a path - whether the swap happened is precisely
// the question handleFailedSwap exists to answer - so the rule is:
//
//   - case (b), the source ESTABLISHED untouched: drop it. The source is provably still
//     at its own path, so the retained link is not the only name for those bytes.
//   - indeterminate, and applied-despite-error: HOLD it. If the rename did take effect,
//     the retained link is the ONLY remaining name for the original's bytes, and
//     dropping it on a verdict this tool could not reach would destroy the one file the
//     phase exists to protect.
//
// These are graded on real files through the real ProcessFile, because "the second link
// is still on disk" is a property of the filesystem. Both arms run the SAME fixture and
// differ only in the filesystem classification the swap sees, so the outcome is the only
// thing that can produce the difference - a hold that held everything would fail the
// first arm, and a drop that dropped everything would fail the second.

import (
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/store"
)

// retentionAfterFailedSwap runs one failed swap over an engine with the undo window
// open, under the given filesystem classification, and reports the job's outcome and
// what is left in the retention area on disk.
//
// The observable is the FILE and not a ledger row, deliberately: the retention row is
// written only once the swap has SUCCEEDED (UndoWindow.record stamps the fingerprint of
// what the swap left behind), so after a failed swap the ledger is empty either way and
// an assertion against it would pass whatever the merge decided. The link on disk is
// what "the original's bytes still have a name" actually means.
func retentionAfterFailedSwap(t *testing.T, fsType string) (status store.Status, root string) {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng, ts := undoEngine(t, ffmpeg, ffprobe, d, 24, nil)
	eng.renameFn = failingRename(errSwap)
	eng.fsLookup = lookups(fsType)
	if err := eng.RunOneshot(t.Context()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	return rowFor(t, ts, src).Status, d
}

// TestSwapUndo_AnUntouchedSourceReleasesTheRetentionItNoLongerNeeds is the control arm,
// and it is what stops the test below from passing for the wrong reason. On storage
// positively identified as local a failed rename resolves to case (b): the source is
// established intact, so the retention is an orphan and is dropped, exactly as every
// other refusal in ProcessFile drops it.
func TestSwapUndo_AnUntouchedSourceReleasesTheRetentionItNoLongerNeeds(t *testing.T) {
	status, root := retentionAfterFailedSwap(t, "ext4")

	if status != store.Failed {
		t.Fatalf("status = %q, want %q - this arm must reach case (b) or it grades nothing",
			status, store.Failed)
	}
	if files := retainedOriginals(t, root); len(files) != 0 {
		t.Errorf("a retained original is still on disk after an outcome that established the source "+
			"untouched; a retention with no swap behind it raises the source's link count for nothing: %v", files)
	}
}

// TestSwapUndo_AnIndeterminateOutcomeHOLDSTheRetainedOriginal is the arm that matters.
// On storage this run could not identify as local, a failed rename is INDETERMINATE: the
// rename may have taken effect, in which case the source path now carries the replacement
// and the retained link is the only remaining name for the original's bytes. Dropping it
// there would be the unrecoverable deletion this whole phase exists to prevent, so it is
// held - and held is the fail-safe answer whether or not the rename actually applied.
func TestSwapUndo_AnIndeterminateOutcomeHOLDSTheRetainedOriginal(t *testing.T) {
	status, root := retentionAfterFailedSwap(t, "nfs")

	if status != store.Indeterminate {
		t.Fatalf("status = %q, want %q - this arm must reach the indeterminate outcome or it grades nothing",
			status, store.Indeterminate)
	}
	files := retainedOriginals(t, root)
	if len(files) != 1 {
		t.Fatalf("the retention area holds %d file(s) after an INDETERMINATE swap, want exactly 1. "+
			"Nobody can say the rename did not take effect, so the retained link may be the only "+
			"remaining name for the original's bytes and dropping it would be the unrecoverable "+
			"deletion this phase exists to prevent: %v", len(files), files)
	}
	// It has to be a second NAME for the original, not some other file that happens to be
	// sitting in the retention area - that is the whole of what makes it a rescue.
	if !sameFile(files[0], filepath.Join(root, "movie.mkv")) {
		t.Errorf("the file held at %s is not another name for the source's inode, so it is not the "+
			"original's bytes and restoring it would not put the source back", files[0])
	}
}

// retainedOriginals lists every file the undo window is holding under root, found by
// walking the retention area rather than by reading the ledger - so a test can tell a
// record that survived from a FILE that survived.
func retainedOriginals(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	ents, err := filepath.Glob(filepath.Join(root, UndoDirName, "*"))
	if err != nil {
		t.Fatalf("glob the retention area: %v", err)
	}
	out = append(out, ents...)
	return out
}
