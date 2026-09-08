// Regression probe for S0008-holdfast-swap-1, impl-gate ordinal 5, finding F77.
//
// The fifth pass closed three of the four ways strayReplacementHold's codec question
// could answer "no" for a reason that was not about the file (a changed `encoder:`, an
// ffprobe that cannot be started, an ffprobe that answers nothing at all) and left the
// conductor's requirement 1 - "reach the no-source fail-safe before the codec question"
// - deliberately unimplemented, on the argument that a question incapable of a non-file
// "no" makes the ordering cost nothing.
//
// A fourth way survives. VideoCodecAnswered reports answered=true for ANY ffprobe that
// exited of its own accord, including one that exited because it could not READ the
// path, and strayReplacementHold reads codec=="" from an answered probe as a positive
// finding that the file is work in progress. So a gate-passed replacement that is
// present but unreadable - a restrictive mode, a `user:` change of the kind
// docs/docker.md documents as "safe but useless", an NFS export that squashes the uid,
// a transient EIO - is released, and with the no-source fail-safe still sitting BEHIND
// the codec question it is released even when nothing is beside it: the case where it
// may be the only faithful copy of the film.
//
// AC15i's consequent is unconditional and names deletion.
package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
)

// TestRegress0008F77_AnUnreadableStrandedReplacementIsSweptWithNothingBesideIt.
//
// The experiment is controlled: ONE fixture, swept twice, and the only thing that
// changes between the two sweeps is whether the running process may read the file.
// Arm 1 is the fifth pass's own no-source arm and must hold. Arm 2 differs in nothing
// else and must hold for the same reason - there is still nothing beside it to measure
// it against, and holdfast has established nothing about the file at all.
func TestRegress0008F77_AnUnreadableStrandedReplacementIsSweptWithNothingBesideIt(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a file mode cannot deny this process a read, so the case cannot be staged")
	}
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()
	dir := t.TempDir()

	// A gate-passed hevc replacement at a path THIS build's temp construction produced,
	// with no record anywhere (the store write is what failed) and no source beside it
	// (the operator moved the film, or a media manager did). Exactly the fixture the
	// fifth pass's own TestStrayTemp_NothingBesideItHoldsWhateverElseCouldNotBe-
	// Established stages.
	seed := filepath.Join(dir, "seed.mkv")
	mkH264Long(t, ffmpeg, seed, "4M")
	stranded := tempPath(dir, "movie", "mkv", 0)
	mkHevcFrom(t, ffmpeg, seed, stranded, "")
	if err := os.Remove(seed); err != nil {
		t.Fatal(err)
	}
	if got := codecOf(t, ffprobe, stranded); got != "hevc" {
		t.Fatalf("precondition: %s is %q, not hevc - the fixture is not a gate-passed replacement", stranded, got)
	}
	want := md5f(t, stranded)

	// Arm 1, the control: readable, nothing beside it. This must hold, and it does.
	ctl := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	ctl.held.Store(ctl.loadHoldBacks(ctx))
	ctl.cleanStaleTemps(ctx)
	if !exists(stranded) {
		t.Fatalf("control arm: the sweep took %s while it was readable - the fixture is not the case under test", stranded)
	}

	// Arm 2: the same file, present and unchanged, that this process may not read.
	if err := os.Chmod(stranded, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stranded, 0o644) })

	// The precondition that IS the finding: ffprobe exits of its own accord, so the
	// probe reports "answered", and the answer it carries is the empty string - which
	// is what the hold reads as a positive finding of work in progress. The binary is
	// demonstrably fine, so Usable cannot separate the two either.
	prober := probe.New(ffmpeg, ffprobe)
	codec, answered := prober.VideoCodecAnswered(ctx, stranded)
	if !answered || codec != "" {
		t.Fatalf("precondition: on an unreadable file VideoCodecAnswered returned (%q, %v); "+
			"this probe assumes ffprobe exits non-zero having reached a verdict of its own", codec, answered)
	}
	if !prober.Usable(ctx) {
		t.Fatal("precondition: ffprobe must be a working binary, or the hold's own Usable check would catch this")
	}

	e := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	e.held.Store(e.loadHoldBacks(ctx))
	e.cleanStaleTemps(ctx)

	if !exists(stranded) {
		t.Fatalf("AC15i: the sweep DELETED %s - a gate-passed hevc replacement holdfast wrote, at a path "+
			"its own temp construction produced, with NO record naming it and NO source beside it. "+
			"The file was present and byte-identical; the only thing holdfast could not do was READ it, "+
			"and strayReplacementHold reads an empty codec from an ffprobe that exited on its own as a "+
			"positive finding that the file is work in progress. The no-source fail-safe never runs, "+
			"because the codec question is still asked first.", stranded)
	}
	if err := os.Chmod(stranded, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := md5f(t, stranded); got != want {
		t.Errorf("the sweep MODIFIED %s", stranded)
	}
}

// TestRegress0008F77_TheHoldReleasesAFileItCouldNotRead states the same defect at the
// one function that decides it, so the finding is not confused with a property of the
// sweep's loops. pickTempPath is the second route to the same deletion and asks the
// identical question, so a "" here is a deletion on both.
func TestRegress0008F77_TheHoldReleasesAFileItCouldNotRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a file mode cannot deny this process a read, so the case cannot be staged")
	}
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()
	dir := t.TempDir()

	seed := filepath.Join(dir, "seed.mkv")
	mkH264Long(t, ffmpeg, seed, "4M")
	stranded := tempPath(dir, "movie", "mkv", 0)
	mkHevcFrom(t, ffmpeg, seed, stranded, "")
	if err := os.Remove(seed); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stranded, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stranded, 0o644) })

	e := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	if why := e.strayReplacementHold(ctx, stranded); why == "" {
		t.Fatalf("AC15i: strayReplacementHold returns \"\" (sweep it) for %s, a file it could not read at "+
			"all and has nothing beside it to measure against. An unanswerable question is not "+
			"permission to delete, and this one was never answered: ffprobe reported on the process's "+
			"access to the path, not on the content of the file.", stranded)
	}
}
