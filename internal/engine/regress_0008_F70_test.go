package engine

// regress_0008_F70 - S0008-holdfast-swap-1, impl gate ordinal 2, finding F70.
//
// THE CLAIM UNDER TEST. When the output container extension differs from the source's
// (`container_ext` forced to a different value - the ext-CHANGING swap this repository
// already ships and already tests in durability_test.go), the swap's rename target is
// NOT the source path. If that rename reports an error but was nonetheless applied -
// the rename(2)/NFS hazard this whole phase exists for - then:
//
//   - the replacement is at `final` (movie.mkv), a path inside a library root;
//   - the source at `f` (movie.mp4) is untouched, so the re-stat of the SOURCE path
//     returns the source's own pre-swap record and, on storage that is not local, the
//     outcome is AC14a case (c): indeterminate, and the job is PARKED under AC15;
//   - `retainReplacement` then tries to move the temp to its retained name, but the
//     temp is gone (the rename moved it to `final`), so the move fails and the recorded
//     replacement path falls back to the TEMP path, where there is no file at all.
//
// The parked record therefore names a path with nothing at it, while the actual
// replacement sits unrecorded at `final` under an ordinary source name - held back by
// neither hold-back this item creates (AC15d is keyed to the RECORDED replacement path,
// and the record-free basis matches only `*.__holdfast-replacement__.*`).
//
// This test is EVIDENCE OF A DEFECT and is expected to FAIL against the branch under
// review. It documents the bug; fixing it is upstream's job.
//
// Criteria violated:
//
//	AC15a - "WHEN a job is parked indeterminate THE SYSTEM SHALL record with it,
//	         retrievable after a restart, the source path, THE PATH THE REPLACEMENT IS
//	         AT, ... so both files can be identified without logs."
//	AC15i - "IF a file holdfast wrote as a replacement is still present at a path inside
//	         a library root after the job that created it stopped progressing - parked
//	         under AC15 ... - THEN THE SYSTEM SHALL NOT enumerate that path as a source."

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// TestRegress0008F70_AnExtChangingAppliedSwapLosesTrackOfTheReplacement builds the exact
// fixture durability_test.go already uses for the ext-changing swap (a movie.mp4 source
// with baseCfg's ContainerExt "mkv"), injects S2's applying-rename (perform the rename,
// then report an error) and a network classification, and then asks the two questions
// AC15a and AC15i ask.
func TestRegress0008F70_AnExtChangingAppliedSwapLosesTrackOfTheReplacement(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mp4") // ext CHANGES: baseCfg forces "mkv"
	final := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng, ts := buildEngineWithStore(t, ffmpeg, ffprobe, d)
	eng.renameFn = applyingRename(errSwap) // S2: the rename took effect, then reported an error
	eng.fsLookup = lookups("nfs")          // not local, so the outcome is AC14a case (c)

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// Preconditions: the job really is parked, and the replacement really is on disk at
	// `final`. If either of these is false the scenario did not arise and the assertions
	// below would be vacuous.
	in := onlyIncident(t, ts)
	if !in.Parked() {
		t.Fatalf("precondition: the job is not parked (outcome %q, resolution %q)", in.Outcome, in.Resolution)
	}
	if !exists(final) {
		t.Fatalf("precondition: no replacement at %s; dir holds %v", final, lsDir(t, d))
	}
	if got := codecOf(t, ffprobe, final); got != "hevc" {
		t.Fatalf("precondition: the file at %s is %q, want the gate-passed hevc replacement", final, got)
	}

	// ---- AC15a: the record must name THE PATH THE REPLACEMENT IS AT ----------------
	if _, err := os.Lstat(in.ReplacementPath); os.IsNotExist(err) {
		t.Errorf("AC15a: the parked record's replacement path %q has NO FILE at it; "+
			"the replacement is actually at %q, which the record never names. "+
			"An operator running `holdfast resolve` is told that path is absent and can "+
			"never identify the second file from the record. (dir holds %v)",
			in.ReplacementPath, final, lsDir(t, d))
	}
	if in.ReplacementPath != final {
		t.Errorf("AC15a: recorded replacement path = %q, want %q (the path the replacement is at)",
			in.ReplacementPath, final)
	}

	// ---- AC15i: that path must not be enumerated as a source by a LATER run --------
	//
	// Asked of a FRESH engine over the same store and roots, which is what a restart is.
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, ts, discardLogger())
	next.held.Store(next.loadHoldBacks(context.Background()))

	for _, p := range next.enumerate() {
		if p == final {
			t.Errorf("AC15i: a later run ENUMERATES %q as a source. It is a file holdfast "+
				"wrote as a replacement, still present inside a library root, for a job "+
				"parked under AC15 - one of AC15i's three named origins - so it must not "+
				"be enumerated at all. Neither hold-back reaches it: the record names %q "+
				"instead, and the record-free basis matches only %q names.",
				final, in.ReplacementPath, RetainedMarker)
		}
	}

	// The source itself must still be held (it is a parked job's recorded source path),
	// and it must still be the original file. This is the control: it shows the parked
	// hold-back is working for the path the record DOES name, so the failures above are
	// about the path it does not.
	if _, held := next.heldBack(src); !held {
		t.Errorf("control: the parked job's recorded source path %q is not held back", src)
	}
	if row, ok := jobRow(t, ts, src, probe.Fingerprint(src)); ok && row.Status != store.Indeterminate {
		t.Errorf("control: the parked job's row is %q, want %q", row.Status, store.Indeterminate)
	}
}
