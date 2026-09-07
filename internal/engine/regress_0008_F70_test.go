package engine

// regress_0008_F70 - S0008-holdfast-swap-1, impl gate ordinal 2, finding F70.
//
// THE DEFECT THIS FILE PINS. When the output container extension differs from the
// source's (`container_ext` forced to a different value - the ext-CHANGING swap this
// repository already ships and already tests in durability_test.go), the swap's rename
// target is NOT the source path. If that rename reports an error but was nonetheless
// applied - the rename(2)/NFS hazard this whole phase exists for - then:
//
//   - the replacement is at `final` (movie.mkv), a path inside a library root;
//   - the source at `f` (movie.mp4) is untouched, so the re-stat of the SOURCE path
//     returns the source's own pre-swap record and, on storage that is not local, the
//     outcome is AC14a case (c): indeterminate, and the job is PARKED under AC15;
//   - `retainReplacement` then tried to move the TEMP to its retained name, but the temp
//     was gone (the rename moved it to `final`), so the move failed and the recorded
//     replacement path fell back to the temp path, where there was no file at all.
//
// The parked record therefore named a path with nothing at it, while the actual
// replacement sat unrecorded at `final` under an ordinary source name - held back by
// neither hold-back this item creates (AC15d is keyed to the RECORDED replacement path,
// and the record-free basis matches only `*.__holdfast-replacement__.*`).
//
// Criteria violated:
//
//	AC15a - "WHEN a job is parked indeterminate THE SYSTEM SHALL record with it,
//	         retrievable after a restart, the source path, THE PATH THE REPLACEMENT IS
//	         AT, ... so both files can be identified without logs."
//	AC15i - "IF a file holdfast wrote as a replacement is still present at a path inside
//	         a library root after the job that created it stopped progressing - parked
//	         under AC15 ... - THEN THE SYSTEM SHALL NOT enumerate that path as a source."
//
// THE FIX AND WHAT THESE TESTS NOW ASK. `replacementIsAt` follows the file: when the
// temp path is empty, the target is not the source path, and what is at the target
// carries the attributes recorded for the replacement before the swap, the replacement
// is at the TARGET. The indeterminate branch then retains it from there, exactly as it
// always did for the in-place shape, and records where it moved it to.
//
// One assertion from the probe as it was left has changed, and this is the whole of the
// change: it demanded the recorded replacement path be literally `final` (movie.mkv).
// That would forbid the only fix that satisfies AC15i as well as AC15a. AC15i's
// record-free basis "SHALL be the build's OWN construction of replacement paths ... and
// NOTHING else is held back on a name", so an ordinary media name like movie.mkv can
// never be held back on its name - which means a replacement LEFT there is unprotected
// the moment no record survives (AC15h, the third origin in AC15i's own list). The file
// therefore has to move to a name the construction produces, and AC15a's clause is "the
// path the replacement IS at", which after that move is the retained path. So the
// assertion below asks the clause instead of a fixed string: the recorded path must have
// the gate-passed replacement at it, and nothing holdfast wrote may be left anywhere the
// record does not name. The AC15i assertion is unchanged, and a second test now drives
// the same shape through an unwritable store, where only the name can hold it.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

	// Preconditions: the job really is parked, and the ext-changing rename really did
	// take effect. If either is false the scenario did not arise and everything below
	// would be vacuous.
	in := onlyIncident(t, ts)
	if !in.Parked() {
		t.Fatalf("precondition: the job is not parked (outcome %q, resolution %q)", in.Outcome, in.Resolution)
	}
	if nTemp(t, d) != 0 {
		t.Fatalf("precondition: a file is still at a temp path, so the rename did not apply; dir holds %v", lsDir(t, d))
	}

	// ---- AC15a: the record must name THE PATH THE REPLACEMENT IS AT ----------------
	if _, err := os.Lstat(in.ReplacementPath); os.IsNotExist(err) {
		t.Fatalf("AC15a: the parked record's replacement path %q has NO FILE at it. "+
			"An operator running `holdfast resolve` is told that path is absent and can "+
			"never identify the second file from the record. (dir holds %v)",
			in.ReplacementPath, lsDir(t, d))
	}
	if got := codecOf(t, ffprobe, in.ReplacementPath); got != "hevc" {
		t.Errorf("AC15a: the file at the recorded replacement path %q is %q, want the gate-passed hevc replacement",
			in.ReplacementPath, got)
	}
	gotAttrs, err := probe.StatAttributes(in.ReplacementPath)
	if err != nil {
		t.Fatal(err)
	}
	if gotAttrs.String() != in.ReplacementAttrs {
		t.Errorf("AC15a: the file at the recorded replacement path is %q but the record's pre-swap replacement attributes are %q - the two do not identify one file",
			gotAttrs.String(), in.ReplacementAttrs)
	}
	// ... and nothing holdfast wrote may be left at a path the record does not name.
	// `final` is where the applied rename put it; after the fix the replacement has been
	// moved from there to a held-back name, so a file still at `final` means a SECOND
	// copy nobody records.
	if exists(final) {
		t.Errorf("AC15a: a file is still at %q, which the record does not name (it names %q); dir holds %v",
			final, in.ReplacementPath, lsDir(t, d))
	}
	// The construction, asked directly rather than through the name matcher: the matcher
	// is one of the things under test here, so finding the file with it would be asking
	// the accused for an alibi.
	if want := retainedReplacementPath(d, "movie", "mkv", 0); in.ReplacementPath != want {
		t.Errorf("recorded replacement path = %q, want %q (the build's own construction of a retained replacement path, which is what the record-free hold-back recognises)",
			in.ReplacementPath, want)
	}

	// ---- AC15i: that path must not be enumerated as a source by a LATER run --------
	//
	// Asked of a FRESH engine over the same store and roots, which is what a restart is.
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, ts, discardLogger())
	next.held.Store(next.loadHoldBacks(context.Background()))

	for _, p := range next.enumerate() {
		if p == final || p == in.ReplacementPath {
			t.Errorf("AC15i: a later run ENUMERATES %q as a source. It is a file holdfast "+
				"wrote as a replacement, still present inside a library root, for a job "+
				"parked under AC15 - one of AC15i's three named origins - so it must not "+
				"be enumerated at all.", p)
		}
	}

	// The source itself must still be held (it is a parked job's recorded source path),
	// and it must still be the original file. This is the control: it shows the parked
	// hold-back is working for the path the record DOES name.
	if _, held := next.heldBack(src); !held {
		t.Errorf("control: the parked job's recorded source path %q is not held back", src)
	}
	if _, held := next.heldBack(in.ReplacementPath); !held {
		t.Errorf("control: the parked job's recorded replacement path %q is not held back", in.ReplacementPath)
	}
	if row, ok := jobRow(t, ts, src, probe.Fingerprint(src)); ok && row.Status != store.Indeterminate {
		t.Errorf("control: the parked job's row is %q, want %q", row.Status, store.Indeterminate)
	}
	if got := codecOf(t, ffprobe, src); got != "h264" {
		t.Errorf("control: the source at %q is %q, want the untouched h264 source", src, got)
	}
}

// TestRegress0008F70_AnExtChangingAppliedSwapIsHeldWithNoRecordAtAll is the same shape
// with the job store refusing the write that would have recorded it - AC15h, and AC15i's
// third named origin ("never recorded at all because AC15h fired"). It is the case that
// settles WHERE the replacement has to end up: with no record, the only thing that can
// hold a file back is the build's own construction of a replacement path, and an
// ext-changing rename that took effect leaves the file at an ordinary media name, which
// nothing may hold back on its name. Unless it is moved, it is unprotected.
func TestRegress0008F70_AnExtChangingAppliedSwapIsHeldWithNoRecordAtAll(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mp4")
	final := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	real := newTestStore(t, d)
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)
	logs := &capturedLog{}
	broken := unwritableIncidents{Store: real, err: errors.New("simulated: the job store cannot be written")}
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, broken, logs.logger())
	eng.renameFn = applyingRename(errSwap)
	eng.fsLookup = lookups("nfs")
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// Nothing was persisted, so nothing but the NAME can hold the replacement back.
	if got := allIncidents(t, real); len(got) != 0 {
		t.Fatalf("precondition: an incident was recorded despite the store refusing the write: %+v", got)
	}
	retained := retainedReplacementPath(d, "movie", "mkv", 0)
	if !exists(retained) {
		t.Fatalf("the replacement was not moved to a held-back name; it is unprotected wherever it is. Dir holds %v", lsDir(t, d))
	}
	if exists(final) {
		t.Errorf("the replacement was left at %q, an ordinary media name that nothing may hold back on its name (AC15i)", final)
	}
	if got := codecOf(t, ffprobe, retained); got != "hevc" {
		t.Errorf("the retained file is %q, want the gate-passed hevc replacement", got)
	}
	retainedMD5 := md5f(t, retained)
	if !exists(src) {
		t.Fatal("the source was deleted when the outcome could not be persisted")
	}

	// It was REPORTED with the path the file is actually at, so an operator holds what
	// the parked record would have carried (AC15h).
	out := logs.String()
	for _, want := range []string{"COULD NOT PERSIST", src, retained} {
		if !strings.Contains(out, want) {
			t.Errorf("the unpersistable outcome was not reported with %q; log:\n%s", want, out)
		}
	}

	// AC15i: a LATER run with a WRITABLE store leaves it alone - not enumerated, not
	// encoded, not deleted, not swept as a stale temp.
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, real, discardLogger())
	if err := next.RunOneshot(context.Background()); err != nil {
		t.Fatalf("later RunOneshot: %v", err)
	}
	if !exists(retained) {
		t.Fatal("the later run DELETED a replacement holdfast wrote and no record survived to protect")
	}
	if md5f(t, retained) != retainedMD5 {
		t.Error("the later run RE-ENCODED the retained replacement")
	}
	if row, ok := jobRow(t, real, retained, probe.Fingerprint(retained)); ok {
		t.Errorf("the later run gave the retained replacement a job row (%q) - it was offered to the pipeline", row.Status)
	}
}

// TestRegress0008F70_AnExtChangingAppliedSwapOnLocalStorageReportsTheDuplicate is the
// LOCAL sibling of the case above, and it pins the ruling taken there. On storage
// positively identified as local the same observation is AC14a case (b) - the re-stat at
// the SOURCE path returns the source's own record, and the source really is untouched,
// because an ext-changing rename never targets it. Case (b) is exhaustive about the
// outcome ("the source is untouched, and THE SYSTEM SHALL report it so"), so the job is
// a plain failure and not one of AC15i's three origins.
//
// What must not happen is that the operator is told ONLY that: the gate-passed
// replacement is sitting beside the source, which is the same duplicate a crash between
// the rename and the source removal leaves and which this repository reconciles with the
// collision guard. It is reported durably, in the row, and neither file is deleted.
func TestRegress0008F70_AnExtChangingAppliedSwapOnLocalStorageReportsTheDuplicate(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mp4")
	final := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	srcMD5 := md5f(t, src)

	eng, ts := buildEngineWithStore(t, ffmpeg, ffprobe, d)
	eng.renameFn = applyingRename(errSwap)
	eng.fsLookup = lookups("ext4") // positively local: case (b)

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if !exists(src) || md5f(t, src) != srcMD5 {
		t.Fatal("the source was deleted or modified after a failed swap")
	}
	if !exists(final) {
		t.Fatalf("the replacement is gone: a gate-passed encode was deleted. Dir holds %v", lsDir(t, d))
	}
	row := rowFor(t, ts, src)
	if row.Status != store.Failed {
		t.Fatalf("status = %q, want %q (case (b): the source is established untouched)", row.Status, store.Failed)
	}
	if !strings.Contains(row.Outcome.Reason, "untouched") {
		t.Errorf("the reason does not report the source untouched: %q", row.Outcome.Reason)
	}
	if !strings.Contains(row.Outcome.Reason, final) {
		t.Errorf("the reason never names the second file at %q, so an operator told the source is untouched never learns it exists: %q",
			final, row.Outcome.Reason)
	}
	if got := allIncidents(t, ts); len(got) != 0 {
		t.Errorf("an established source-intact failure recorded a swap incident: %+v", got)
	}
}
