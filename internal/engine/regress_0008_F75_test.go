package engine

// Impl-gate ordinal 4 probes for S0008-holdfast-swap-1 (finding F75, plus the AC13b
// acceptance check the shipped suite leaves to substituted lookups).
//
//	AC15i - IF a file holdfast wrote as a replacement is still present at a path inside
//	a library root after the job that created it stopped progressing - ... or never
//	recorded at all because AC15h fired - THEN THE SYSTEM SHALL NOT enumerate that path
//	as a source and SHALL NOT encode, swap or DELETE the file at it, in that run or in
//	any later run. Where a record survives, AC15d is how this holds; where none does,
//	holding it SHALL NOT depend on one, since the write that failed is exactly what
//	denied it.
//
// The build's record-free hold (engine.strayReplacementHold) holds a file only when its
// NAME is exactly what tempPath constructs AND its CONTENT answers two questions: is it
// at the RUNNING engine's target codec, and does its length match the source beside it.
// Both content questions can answer "no" for reasons that have nothing to do with the
// file, and every "no" is a DELETION.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/fsclass"
	"github.com/NSchatz/holdfast/internal/store"
)

// localLibraryDir returns a scratch directory the REAL classification calls `local`.
// t.TempDir() is preferred; where TMPDIR is tmpfs (undetermined, and so not local by
// the phase's own fail-safe) it falls back to a directory beside the package source,
// which is on the repository's own filesystem.
func localLibraryDir(t *testing.T) string {
	t.Helper()
	if d := t.TempDir(); fsclass.Of(nil, d).IsLocal() {
		return d
	}
	d, err := os.MkdirTemp(".", "probe-local-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	abs, err := filepath.Abs(d)
	if err != nil {
		t.Fatalf("Abs: %v", err)
	}
	if cls := fsclass.Of(nil, abs); !cls.IsLocal() {
		t.Skipf("no recognised-local scratch storage on this gate (%s classifies %s)", abs, cls)
	}
	return abs
}

// TestProbe_AC13bGivenOverTheRealClassification runs one S1 failed swap with NOTHING
// substituted but the rename. The classification is the real statfs on real local
// storage, so this is the only place the enumeration is load-bearing end to end.
//
// This one PASSES against the branch under review; it is here as the acceptance check
// the spec's Verification notes ask for ("one test substitutes NOTHING"), not as a
// finding.
func TestProbe_AC13bGivenOverTheRealClassification(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	dir := localLibraryDir(t)
	cls := fsclass.Of(nil, dir)
	src := filepath.Join(dir, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	srcMD5 := md5f(t, src)

	eng, ts := buildEngineWithStore(t, ffmpeg, ffprobe, dir)
	eng.renameFn = failingRename(errSwap) // S1: the rename fails and does not apply
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if !exists(src) || md5f(t, src) != srcMD5 {
		t.Fatal("the source is gone or was modified")
	}
	row := rowFor(t, ts, src)
	if row.Status != store.Failed {
		t.Fatalf("AC13b: over the REAL classification (%s) the outcome is %q, want %q (untouched)",
			cls, row.Status, store.Failed)
	}
	if in := allIncidents(t, ts); len(in) != 0 {
		t.Fatalf("AC13b: the swap was parked despite a demonstrably intact source on %s: %+v", cls, in)
	}
	if row.Outcome.GuardResidualWindow != store.ResidualWindowLocal {
		t.Errorf("AC22: the guard record's window field is %q on %s, want %q",
			row.Outcome.GuardResidualWindow, cls, store.ResidualWindowLocal)
	}
}

// strandedFixture is AC15h's aftermath, staged directly: a gate-passed hevc replacement
// sitting at a path THIS BUILD'S temp construction produced, its source beside it, and
// no record of either anywhere - because the store write that would have made one is
// exactly what failed. AC15i: no later run may enumerate, encode, swap or DELETE it.
func strandedFixture(t *testing.T) (dir, src, stranded, md5, ffmpeg, ffprobe string) {
	t.Helper()
	ffmpeg, ffprobe = tools(t)
	dir = t.TempDir()
	src = filepath.Join(dir, "movie.mkv")
	mkH264Long(t, ffmpeg, src, "4M")
	stranded = tempPath(dir, "movie", "mkv", 0)
	mkHevcFrom(t, ffmpeg, src, stranded, "") // the WHOLE source: a finished replacement
	return dir, src, stranded, md5f(t, stranded), ffmpeg, ffprobe
}

// TestProbe_AC15i_ALaterRunWithADifferentTargetCodecSweepsTheStrandedReplacement.
//
// The codec half of the content test is asked against the target codec of THE RUN DOING
// THE ASKING, not the one the file was written to. `encoder:` is an ordinary config key
// and svtav1 is a shipped value, so a later run under a different encoder asks a
// question the stranded replacement cannot pass - and the sweep deletes a gate-passed
// file holdfast wrote, in a later run, exactly as AC15i forbids.
func TestProbe_AC15i_ALaterRunWithADifferentTargetCodecSweepsTheStrandedReplacement(t *testing.T) {
	dir, _, stranded, md5, ffmpeg, ffprobe := strandedFixture(t)
	ctx := context.Background()

	// Control: the same run under the encoder that wrote it holds the file.
	same := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	same.held.Store(same.loadHoldBacks(ctx))
	same.cleanStaleTemps(ctx)
	if !exists(stranded) {
		t.Fatalf("control arm: the same-encoder run already swept %s - the fixture is not the case under test", stranded)
	}
	if md5f(t, stranded) != md5 {
		t.Fatalf("control arm: %s was modified", stranded)
	}

	// The only change: the operator switched encoders. svtav1 is a shipped `encoder:`
	// value (internal/encoder), so its target codec is av1 rather than hevc.
	other := buildEngine(t, ffmpeg, ffprobe, dir, nil, func(c *config.Config) { c.Encoder = "svtav1" })
	if other.targetCodec == same.targetCodec {
		t.Fatalf("precondition: both engines target %q - the encoder switch did not move the target codec", other.targetCodec)
	}
	other.held.Store(other.loadHoldBacks(ctx))
	other.cleanStaleTemps(ctx)

	if !exists(stranded) {
		t.Fatalf("AC15i: a later run under encoder %q (target %q) DELETED the gate-passed replacement at %s, "+
			"which no record survived to name. The record-free hold's content test is asked against the "+
			"asking run's target codec, so a config key an operator may change at any time releases a file "+
			"AC15i says may never be deleted in any later run.",
			"svtav1", other.targetCodec, stranded)
	}
}

// TestProbe_AC15i_TheHoldIsReleasedWhenTheContentProbeCannotAnswer.
//
// The content test is decided by ffprobe, and probe.Prober.VideoCodec returns "" for
// "this is not video" and for "ffprobe did not run" alike. So any failure to probe - an
// unrunnable ffprobe, a fork that cannot be made, a context cancelled while the sweep is
// inside the coverage-bounded loop (which checks ctx per DIRECTORY, not per entry) -
// answers "not the target codec" and the sweep DELETES the file. The failure direction
// is unsafe, where the sibling branch of the same function ("no source beside it to
// measure against") deliberately holds.
func TestProbe_AC15i_TheHoldIsReleasedWhenTheContentProbeCannotAnswer(t *testing.T) {
	dir, _, stranded, _, ffmpeg, _ := strandedFixture(t)
	ctx := context.Background()

	missing := filepath.Join(dir, "no-such-ffprobe")
	broken := buildEngine(t, ffmpeg, missing, dir, nil, nil)
	broken.held.Store(broken.loadHoldBacks(ctx))
	broken.cleanStaleTemps(ctx)

	if !exists(stranded) {
		t.Fatalf("AC15i: with the content probe unable to answer, the sweep DELETED the gate-passed "+
			"replacement at %s. A probe that cannot answer is read as 'not a replacement', so the "+
			"record-free hold fails OPEN; the same function's no-source-beside branch fails CLOSED. "+
			"(ffprobe was %s)", stranded, missing)
	}
}

// TestProbe_AC15i_TheNoSourceFailSafeIsUnreachableWhenTheCodecQuestionAnswersNo is what
// makes the two arms above a data-safety finding rather than a lost encode.
//
// strayReplacementHold asks its questions in this order: name, CODEC, source-beside,
// length. The "no source beside it to measure against" branch is the function's own
// declared fail-safe ("Holding is the fail-safe answer") - but it sits BEHIND the codec
// check, so it is never reached when the codec question answers no, including when it
// answers no because it could not be asked. A gate-passed replacement with NO source
// beside it - the case where it may be the only faithful copy there is - is deleted.
func TestProbe_AC15i_TheNoSourceFailSafeIsUnreachableWhenTheCodecQuestionAnswersNo(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		ffprobe string
		mutate  func(*config.Config)
	}{
		{"the content probe cannot answer", "", nil},
		{"a later run targets a different codec", ffprobe, func(c *config.Config) { c.Encoder = "svtav1" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Build the replacement from a source, then take the source away: an
			// operator moved the film, or a media manager did. Nothing is left beside
			// the replacement to measure it against, which is the branch that holds.
			seed := filepath.Join(dir, "seed.mkv")
			mkH264Long(t, ffmpeg, seed, "4M")
			stranded := tempPath(dir, "movie", "mkv", 0)
			mkHevcFrom(t, ffmpeg, seed, stranded, "")
			if err := os.Remove(seed); err != nil {
				t.Fatal(err)
			}

			probeBin := tc.ffprobe
			if probeBin == "" {
				probeBin = filepath.Join(dir, "no-such-ffprobe")
			}
			e := buildEngine(t, ffmpeg, probeBin, dir, nil, tc.mutate)
			e.held.Store(e.loadHoldBacks(ctx))
			e.cleanStaleTemps(ctx)

			if !exists(stranded) {
				t.Fatalf("AC15i: the sweep DELETED %s - a gate-passed replacement holdfast wrote, with NO "+
					"record naming it and NO source beside it. strayReplacementHold's own no-source "+
					"fail-safe is unreachable here, because the codec check runs first and answered no.",
					stranded)
			}
		})
	}
}
