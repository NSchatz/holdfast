package engine

// The RECORD-FREE hold-back at the temp construction (AC15i), and the crash-safety it
// must not cost.
//
//	AC15i - IF a file holdfast wrote as a replacement is still present at a path inside
//	a library root after the job that created it stopped progressing - ... or never
//	recorded at all because AC15h fired - THEN THE SYSTEM SHALL NOT enumerate that path
//	as a source and SHALL NOT encode, swap or delete the file at it, in that run or in
//	any later run. Where a record survives, AC15d is how this holds; where none does,
//	holding it SHALL NOT depend on one, since the write that failed is exactly what
//	denied it. The record-free basis SHALL be the build's OWN construction of
//	replacement paths: a path is held back on its name only when that construction could
//	have produced it, matched exactly and never by a widened temp-or-dotfile pattern.
//
// The tension these tests exist to hold open, from both ends at once: a gate-passed
// replacement stranded at a temp path must survive every later run, AND an ordinary
// orphaned temp must still be swept, or a killed run's half-written encodes accumulate
// for ever. The name alone cannot decide it - both files carry the same marker - so the
// content decides, using the verify gate's own checks.
//
// The second block of tests below is the direction the first one missed: a "no" that is
// not about the file. The hold used to ask whether the file was at the codec the RUNNING
// engine targets, using a probe that reports "this is not video" and "ffprobe did not
// run" with the same empty string - so an operator changing `encoder:`, or a SIGTERM, or
// a missing binary each turned a gate-passed replacement into a deletion. Each of those
// arms is a test here.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
)

// mkHevcFrom encodes src to path with libx265. A non-empty seconds truncates the encode
// to that many seconds, which is the shape a KILLED run leaves behind: measured on real
// ffmpeg in this container, an interrupted libx265 encode of a 20-second source lands at
// 3.6 seconds, reports codec "hevc" and DECODES CLEANLY - so it is a complete, valid,
// short file, not a corrupt one, and only its LENGTH gives it away.
func mkHevcFrom(t *testing.T, ffmpeg, src, path, seconds string) {
	t.Helper()
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-i", src}
	if seconds != "" {
		args = append(args, "-t", seconds)
	}
	args = append(args, "-c:v", "libx265", "-x265-params", "log-level=error", "-crf", "30",
		"-preset", "ultrafast", "-pix_fmt", "yuv420p10le", "--", path)
	ff(t, ffmpeg, args...)
}

// mkH264From encodes the WHOLE of src to path with libx264 - a complete, decodable file
// of the source's own length at a codec NO encoder in this build's registry produces. It
// is the control on the codec question: everything else about it says "finished".
func mkH264From(t *testing.T, ffmpeg, src, path string) {
	t.Helper()
	ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", src,
		"-c:v", "libx264", "-crf", "30", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "--", path)
}

// TestStrayTemp_TheSweepKeepsAFinishedReplacementAndStillTakesAPartialEncode is the
// controlled experiment behind the whole fix, and it is deliberately built so that the
// two files differ in ONE way.
//
// Both sit at a path this build's temp construction produced. Both are real libx265
// output. Both report codec "hevc" and both decode cleanly - asserted here as
// preconditions, because that is what rules out the two cheaper rules somebody might
// reach for instead (hold everything at the target codec; hold everything that decodes).
// The only difference is length: one is the whole of its source, one is a fragment. The
// sweep keeps the first and takes the second.
func TestStrayTemp_TheSweepKeepsAFinishedReplacementAndStillTakesAPartialEncode(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	ctx := context.Background()
	prober := probe.New(ffmpeg, ffprobe)

	// Two sources, each with a temp beside it under the build's own construction.
	finishedSrc := filepath.Join(d, "finished.mkv")
	partialSrc := filepath.Join(d, "partial.mkv")
	mkH264Long(t, ffmpeg, finishedSrc, "4M")
	mkH264Long(t, ffmpeg, partialSrc, "4M")

	finished := tempPath(d, "finished", "mkv", 0)
	partial := tempPath(d, "partial", "mkv", 0)
	mkHevcFrom(t, ffmpeg, finishedSrc, finished, "") // the whole source: a replacement
	mkHevcFrom(t, ffmpeg, partialSrc, partial, "2")  // a fragment: work in progress
	finishedMD5 := md5f(t, finished)

	// Preconditions. Without these the experiment is not controlled.
	for _, p := range []string{finished, partial} {
		if got := codecOf(t, ffprobe, p); got != "hevc" {
			t.Fatalf("precondition: %s is %q, not hevc - both arms must be real target-codec output", p, got)
		}
		if !prober.DecodeOK(ctx, p) {
			t.Fatalf("precondition: %s does not decode cleanly - the partial arm must be VALID, not corrupt, "+
				"or this test would only be proving that broken files are swept", p)
		}
	}
	dFull, ok1 := prober.DurationSec(ctx, finished)
	dPart, ok2 := prober.DurationSec(ctx, partial)
	if !ok1 || !ok2 || !(dPart < dFull-1) {
		t.Fatalf("precondition: the fragment (%.3fs, ok=%v) is not clearly shorter than the whole (%.3fs, ok=%v)",
			dPart, ok2, dFull, ok1)
	}

	e := buildEngine(t, ffmpeg, ffprobe, d, nil, nil)
	e.held.Store(e.loadHoldBacks(ctx))
	e.cleanStaleTemps(ctx)

	if !exists(finished) {
		t.Errorf("AC15i: the sweep DELETED a finished replacement holdfast wrote (%s), which no record survived to name", finished)
	} else if md5f(t, finished) != finishedMD5 {
		t.Errorf("the sweep modified %s", finished)
	}
	if exists(partial) {
		t.Errorf("crash-safety: an ordinary orphaned temp (%s) was NOT swept - a killed run's half-written "+
			"encodes would accumulate for ever", partial)
	}
	// And neither source was touched.
	for _, p := range []string{finishedSrc, partialSrc} {
		if !exists(p) {
			t.Errorf("the sweep removed a source (%s)", p)
		}
	}
}

// TestStrayTemp_HeldOnlyWhereTheConstructionCouldHaveProducedTheName bounds the
// record-free basis. AC15i says a path is held on its name "only when that construction
// could have produced it, matched exactly and never by a widened temp-or-dotfile
// pattern" - so a file with identical CONTENT at a name the construction cannot produce
// is not held, and the sweep still takes it.
func TestStrayTemp_HeldOnlyWhereTheConstructionCouldHaveProducedTheName(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	ctx := context.Background()

	src := filepath.Join(d, "movie.mkv")
	mkH264Long(t, ffmpeg, src, "4M")

	construction := tempPath(d, "movie", "mkv", 0)
	mkHevcFrom(t, ffmpeg, src, construction, "")
	// Byte-identical content at a name tempPath could NOT have produced. isTempName
	// still matches it (the sweep looks at it), so this is the widened-pattern case and
	// not a file the sweep never reaches.
	widened := construction + ".part"
	b, err := os.ReadFile(construction)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(widened, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if !isTempName(filepath.Base(widened)) {
		t.Fatalf("precondition: the sweep does not even look at %s, so this proves nothing", widened)
	}

	e := buildEngine(t, ffmpeg, ffprobe, d, nil, nil)
	e.held.Store(e.loadHoldBacks(ctx))

	if why := e.strayReplacementHold(ctx, construction); why == "" {
		t.Error("a finished replacement at the build's own temp construction was not held back")
	}
	if why := e.strayReplacementHold(ctx, widened); why != "" {
		t.Errorf("%s is held back on its name (%q), but the construction cannot produce that name - "+
			"the record-free basis has been widened", widened, why)
	}

	e.cleanStaleTemps(ctx)
	if !exists(construction) {
		t.Error("the sweep took the file at the build's own construction")
	}
	if exists(widened) {
		t.Error("the sweep left a file at a name the construction cannot produce")
	}
}

// TestTempConstructionName_MatchesOnlyWhatTheConstructionProduces mirrors the retained
// half's bound. Everything this matches is a candidate for being withheld from a
// library, so it is matched EXACTLY - the one thing worse than deleting a file holdfast
// wrote is refusing to reclaim one it did not.
func TestTempConstructionName_MatchesOnlyWhatTheConstructionProduces(t *testing.T) {
	for n := 0; n < 3; n++ {
		p := tempPath("/lib/tv", "Show", "mkv", n)
		if !IsTempConstructionName(filepath.Base(p)) {
			t.Errorf("the construction produced %q and the matcher does not recognise it", p)
		}
	}
	held := []string{
		"Show." + TempMarker + ".mkv",
		"Show." + TempMarker + ".12.mkv",
		"Show.S01E01.1080p." + TempMarker + ".mp4",
	}
	for _, base := range held {
		if !IsTempConstructionName(base) {
			t.Errorf("%q should be matched - the construction can produce it", base)
		}
	}
	free := []string{
		"Show.mkv",
		"Show.tmp.mkv",
		".Show.mkv",
		"Show." + RetainedMarker + ".mkv", // the OTHER construction, matched by the other matcher
		TempMarker + ".mkv",               // no stem
		"Show." + TempMarker + ".mkv.part",
		"Show." + TempMarker + ".x1.mkv",
		"Show." + TempMarker + ".",
	}
	for _, base := range free {
		if IsTempConstructionName(base) {
			t.Errorf("%q must NOT be matched - nothing but the construction is", base)
		}
	}
	// The two constructions are disjoint, which is what keeps a retained replacement out
	// of the sweep's sight entirely and a work-in-progress temp out of enumeration.
	for _, base := range []string{"Show." + TempMarker + ".mkv", "Show." + RetainedMarker + ".mkv"} {
		if IsTempConstructionName(base) && IsRetainedReplacementName(base) {
			t.Errorf("%q is matched by BOTH constructions", base)
		}
	}
}

// TestStrandedReplacement_AWholeLaterRunLeavesItAloneAndStillDoesItsWork is AC15i's own
// Given, end to end, on the production path rather than on the sweep alone: "Given an
// unwritable store, a run that leaves a replacement beside its source, and a later run
// with a writable store, that later run SHALL leave both files alone".
//
// The staging is the one root cause: the library directory goes read-only the instant
// before the swap, so the REAL os.Rename fails for the swap AND for the move to a
// retained name, while the state directory on the same mount refuses the incident write
// (AC15h). Nothing is injected but the moment the mount flips.
//
// The later run is a FULL RunOneshot with a writable store and a writable directory, and
// it has ordinary work to do - which is what distinguishes "it left that file alone" from
// "it did nothing at all". It must: leave the stranded replacement byte-identical, not
// enumerate it, step AROUND its path when it picks a temp for the fresh encode of the
// same source (the second route to the same deletion), and still reclaim both sources.
func TestStrandedReplacement_AWholeLaterRunLeavesItAloneAndStillDoesItsWork(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	ctx := context.Background()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	srcMD5 := md5f(t, src)

	real := newTestStore(t, d)
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)
	logs := &capturedLog{}
	broken := unwritableIncidents{Store: real, err: errors.New("simulated: the state dir is on the mount that went read-only")}

	first := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, broken, logs.logger())
	first.fsLookup = lookups("nfs")
	t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
	first.fsyncPath = func(p string) error {
		if err := fsyncPath(p); err != nil {
			return err
		}
		if strings.Contains(filepath.Base(p), TempMarker) {
			if err := os.Chmod(d, 0o555); err != nil {
				t.Errorf("chmod: %v", err)
			}
		}
		return nil
	}
	if err := first.RunOneshot(ctx); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}

	stranded := tempPath(d, "movie", "mkv", 0)
	if !exists(stranded) {
		t.Fatalf("precondition: nothing stranded at %s; the directory holds %v\nlog:\n%s", stranded, lsDir(t, d), logs.String())
	}
	strandedMD5 := md5f(t, stranded)
	if got := codecOf(t, ffprobe, stranded); got != "hevc" {
		t.Fatalf("precondition: %s is %q, not the gate-passed hevc replacement", stranded, got)
	}
	if len(retainedFiles(t, d)) != 0 {
		t.Fatalf("precondition: the retain SUCCEEDED, so the case under test did not arise: %v", retainedFiles(t, d))
	}
	if in := allIncidents(t, real); len(in) != 0 {
		t.Fatalf("precondition: an incident was recorded, so AC15h did not fire: %+v", in)
	}
	if md5f(t, src) != srcMD5 {
		t.Fatal("precondition: the source moved; this case is about the replacement")
	}

	// An ORDINARY source appears beside it, so "left that file alone" is distinguishable
	// from "the run did nothing".
	other := filepath.Join(d, "other.mkv")
	mkH264(t, ffmpeg, other, "8M")

	next := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, real, discardLogger())
	if err := next.RunOneshot(ctx); err != nil {
		t.Fatalf("later RunOneshot: %v", err)
	}

	// 1. The stranded replacement: still there, byte for byte.
	if !exists(stranded) {
		t.Fatalf("AC15i: the later run DELETED the stranded replacement %s\ndirectory now holds %v", stranded, lsDir(t, d))
	}
	if md5f(t, stranded) != strandedMD5 {
		t.Errorf("AC15i: the later run MODIFIED the stranded replacement %s", stranded)
	}
	// 2. It was never a job of its own: not enumerated, not encoded, not swapped.
	rows, err := real.List(ctx, nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, row := range rows {
		if row.Path == stranded {
			t.Errorf("AC15i: the stranded replacement was enumerated as a source (job row %+v)", row)
		}
	}
	// 3. The run still did its work - both sources reclaimed, at the target codec.
	for _, p := range []string{src, other} {
		if got := codecOf(t, ffprobe, p); got != "hevc" {
			t.Errorf("the later run did not reclaim %s (codec %q) - a stranded replacement must withhold "+
				"ITSELF from the run and nothing else", p, got)
		}
	}
	// 4. The fresh encode of the same source stepped AROUND the stranded path rather
	// than clearing it (pickTempPath, the second route to the same deletion), and left
	// no temp of its own behind.
	if nTemp(t, d) != 1 {
		t.Errorf("temps under %s = %v, want exactly the one stranded replacement", d, lsDir(t, d))
	}
}

// ---- a "no" that is not about the file ---------------------------------------
//
// Everything below is one property, asked from five directions: the sweep needs a
// POSITIVE finding that a file is work in progress, and anything it could not establish
// HOLDS. AC15i's consequent is unconditional and names deletion, and it goes out of its
// way to say the hold must not depend on something the stranding failure could also have
// taken away - so a hold that releases on a question it could not ask, or on a config
// key an operator may edit, is the criterion inverted rather than a gap in it.

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

// brokenFFprobe writes an executable that RUNS and exits non-zero for every question -
// a half-installed build, or one whose shared library is gone. It matters because it is
// the shape that most resembles a legitimate refusal: ffprobe reading a file and saying
// "this is not media" exits non-zero too.
func brokenFFprobe(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "broken-ffprobe")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestStrayTemp_TheCodecQuestionAsksWhatThisBuildCouldHaveWrittenNotWhatIsConfigured.
//
// `encoder:` is an ordinary config key and svtav1 is a shipped value, so if the content
// test asks whether a file is at the target codec of THE RUN DOING THE ASKING, an
// operator switching encoders while a replacement is stranded has it deleted by the next
// sweep - in a later run, exactly as AC15i forbids. The question has to be "could some
// encoder this build ships have written this", which a stranded replacement always
// passes whatever the current setting is.
//
// The control arm is what makes it an experiment: the same fixture under the encoder
// that wrote it is held byte-identical, so the test measures the encoder key and nothing
// else.
func TestStrayTemp_TheCodecQuestionAsksWhatThisBuildCouldHaveWrittenNotWhatIsConfigured(t *testing.T) {
	dir, _, stranded, md5, ffmpeg, ffprobe := strandedFixture(t)
	ctx := context.Background()

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
	if md5f(t, stranded) != md5 {
		t.Errorf("AC15i: the later run MODIFIED the stranded replacement %s", stranded)
	}
}

// TestStrayTemp_AProbeThatCannotBeRunHoldsRatherThanReleases.
//
// The content test is decided by ffprobe, and probe.VideoCodec returns "" for "this is
// not video" and for "ffprobe did not run" alike. So any failure to probe used to answer
// "not a replacement" and the sweep DELETED the file. The failure direction has to be
// the same one the no-source branch already takes: hold.
func TestStrayTemp_AProbeThatCannotBeRunHoldsRatherThanReleases(t *testing.T) {
	dir, _, stranded, md5, ffmpeg, _ := strandedFixture(t)
	ctx := context.Background()

	missing := filepath.Join(t.TempDir(), "no-such-ffprobe")
	broken := buildEngine(t, ffmpeg, missing, dir, nil, nil)
	broken.held.Store(broken.loadHoldBacks(ctx))
	broken.cleanStaleTemps(ctx)

	if !exists(stranded) {
		t.Fatalf("AC15i: with the content probe unable to answer, the sweep DELETED the gate-passed "+
			"replacement at %s. A probe that cannot answer is read as 'not a replacement', so the "+
			"record-free hold fails OPEN; the same function's no-source-beside branch fails CLOSED. "+
			"(ffprobe was %s)", stranded, missing)
	}
	if md5f(t, stranded) != md5 {
		t.Errorf("the sweep MODIFIED %s", stranded)
	}
	// And it holds for THIS reason. The operator has two different things to go and fix
	// depending on which it was, and the two branches are independently removable, so
	// the reason is asserted rather than only the file's survival.
	if why := broken.strayReplacementHold(ctx, stranded); !strings.Contains(why, "could not be asked about") {
		t.Errorf("the hold reports %q; a probe that never ran must be reported as one", why)
	}
}

// TestStrayTemp_AnFfprobeThatAnswersNothingAtAllHoldsToo is the harder half of the same
// property, and the reason the hold asks Prober.Usable before acting on a refusal.
//
// A MISSING ffprobe is easy: exec fails and no verdict was reached. An ffprobe that runs
// and exits non-zero for every question is not: on the wire it is identical to a working
// ffprobe reading a file and rejecting it, which is the answer that legitimately licenses
// the sweep. Only a question with a known answer - "what version are you" - separates
// them, and a hold that skipped it would delete every stranded replacement on a host
// whose ffprobe is half-installed.
func TestStrayTemp_AnFfprobeThatAnswersNothingAtAllHoldsToo(t *testing.T) {
	dir, _, stranded, md5, ffmpeg, _ := strandedFixture(t)
	ctx := context.Background()

	e := buildEngine(t, ffmpeg, brokenFFprobe(t), dir, nil, nil)
	e.held.Store(e.loadHoldBacks(ctx))
	e.cleanStaleTemps(ctx)

	if !exists(stranded) {
		t.Fatalf("AC15i: with an ffprobe that exits non-zero for EVERY question, the sweep DELETED the "+
			"gate-passed replacement at %s. Its refusal was evidence about the host, not about the file.", stranded)
	}
	if md5f(t, stranded) != md5 {
		t.Errorf("the sweep MODIFIED %s", stranded)
	}
	if why := e.strayReplacementHold(ctx, stranded); !strings.Contains(why, "answering nothing at all on this host") {
		t.Errorf("the hold reports %q; an ffprobe that answers nothing must be reported as one, because "+
			"it is a DIFFERENT thing for an operator to fix from a missing binary", why)
	}
}

// TestStrayTemp_NothingBesideItHoldsWhateverElseCouldNotBeEstablished is what makes the
// arms above a data-safety property rather than a lost encode.
//
// With NO source beside it there is nothing to measure the file against, and it may be
// the only faithful copy of the film there is - the case the file's own RetainedMarker
// comment says can never happen ("nothing in this program may ever delete it on its own
// initiative"). That fail-safe used to sit behind a codec question that answered "no"
// for reasons that were not about the file, so it was unreachable on exactly the paths
// that needed it. Every one of those paths is an arm here, and the question is now asked
// FIRST - by os.Lstat alone, with no subprocess and no configuration in it - so the arms
// are the ways the OLD ordering could be reached and not the ways the new one can fail.
//
// The last arm is the one that outlived the reordering's first attempt: an ffprobe that
// runs, is demonstrably a working binary, and exits non-zero because it could not OPEN
// the path. On the wire that is identical to a verdict about the file, and it is what a
// restrictive mode, a `user:` change, an NFS export squashing the writing uid, an
// SELinux denial or a transient EIO each produce.
func TestStrayTemp_NothingBesideItHoldsWhateverElseCouldNotBeEstablished(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		ffprobe string
		mutate  func(*config.Config)
		stage   func(*testing.T, string) // applied to the stranded file before the sweep
	}{
		{name: "the content probe cannot answer", ffprobe: ""},
		{name: "a later run targets a different codec", ffprobe: ffprobe,
			mutate: func(c *config.Config) { c.Encoder = "svtav1" }},
		{name: "nothing at all is wrong", ffprobe: ffprobe},
		{name: "this process cannot read the file", ffprobe: ffprobe, stage: denyRead},
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
			if got := codecOf(t, ffprobe, stranded); got != "hevc" {
				t.Fatalf("precondition: %s is %q, not hevc - the fixture is not a gate-passed replacement", stranded, got)
			}
			want := md5f(t, stranded)
			if tc.stage != nil {
				tc.stage(t, stranded)
			}

			probeBin := tc.ffprobe
			if probeBin == "" {
				probeBin = filepath.Join(t.TempDir(), "no-such-ffprobe")
			}
			e := buildEngine(t, ffmpeg, probeBin, dir, nil, tc.mutate)
			e.held.Store(e.loadHoldBacks(ctx))
			e.cleanStaleTemps(ctx)

			if !exists(stranded) {
				t.Fatalf("AC15i: the sweep DELETED %s - a gate-passed replacement holdfast wrote, with NO "+
					"record naming it and NO source beside it. The one question that cannot fail for a "+
					"reason that is not about the file - is there anything here to measure it against - "+
					"must be asked before any that can.", stranded)
			}
			allowRead(t, stranded)
			if got := md5f(t, stranded); got != want {
				t.Errorf("the sweep MODIFIED %s", stranded)
			}
		})
	}
}

// denyRead takes this process's read access to path away and gives it back at the end of
// the test. It is the ONE staging that produces an ffprobe refusal which is not about the
// file: ProcessState.Exited() is true whether ffprobe exited because the file is not
// media or because open() failed, so the two are the same answer on the wire.
//
// Under uid 0 a file mode denies nothing, so the case cannot be staged and the test skips
// rather than passing vacuously.
func denyRead(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: a file mode cannot deny this process a read, so the case cannot be staged")
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
}

func allowRead(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestStrayTemp_AFileThisProcessCannotReadHoldsEvenWithItsSourceBesideIt is the same
// property WITHOUT the no-source fail-safe standing behind it, which is what makes the
// readability confirmation load-bearing rather than belt-and-braces.
//
// The source IS beside the file here, so every earlier hold is out of the way and the
// only thing that can hold it is the confirmation that ffprobe's refusal was about the
// FILE. The experiment is controlled: ONE fixture, swept twice, and the only thing that
// changes between the two sweeps is whether the running process may read it. The control
// arm keeps it (a readable, finished, length-matching replacement), so the second arm
// measures the access and nothing else.
//
// docs/docker.md documents the commonest cause as an operator knob and promises that
// getting `user:` wrong is "safe but useless: every encode fails at the write step, and
// every source is left byte-for-byte intact". Deleting a gate-passed replacement on that
// knob would make the sentence false.
func TestStrayTemp_AFileThisProcessCannotReadHoldsEvenWithItsSourceBesideIt(t *testing.T) {
	dir, src, stranded, md5, ffmpeg, ffprobe := strandedFixture(t)
	ctx := context.Background()

	// Arm 1, the control: readable, its source beside it. Held, byte for byte.
	ctl := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	ctl.held.Store(ctl.loadHoldBacks(ctx))
	ctl.cleanStaleTemps(ctx)
	if !exists(stranded) || md5f(t, stranded) != md5 {
		t.Fatalf("control arm: the sweep took or modified %s while it was readable - the fixture is not the case under test", stranded)
	}

	// Arm 2: the same file, present and unchanged, that this process may not read.
	denyRead(t, stranded)

	// The preconditions that ARE the finding, asserted rather than assumed: ffprobe
	// exits of its own accord (so the probe reports "answered"), the answer it carries is
	// the empty string, and the binary is demonstrably fine (so Usable cannot separate
	// the two either). Without these three the test would prove nothing about this route.
	prober := probe.New(ffmpeg, ffprobe)
	codec, answered := prober.VideoCodecAnswered(ctx, stranded)
	if !answered || codec != "" {
		t.Fatalf("precondition: on an unreadable file VideoCodecAnswered returned (%q, %v); this test "+
			"assumes ffprobe exits non-zero having reached a verdict of its own", codec, answered)
	}
	if !prober.Usable(ctx) {
		t.Fatal("precondition: ffprobe must be a working binary, or the hold's own Usable check would catch this")
	}
	if !exists(src) {
		t.Fatalf("precondition: the source %s is gone, so the no-source hold would carry this arm instead", src)
	}

	e := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
	e.held.Store(e.loadHoldBacks(ctx))
	e.cleanStaleTemps(ctx)

	if !exists(stranded) {
		t.Fatalf("AC15i: the sweep DELETED %s - a gate-passed hevc replacement holdfast wrote, at a path its "+
			"own temp construction produced, with NO record naming it. The file was present and "+
			"byte-identical; the only thing holdfast could not do was READ it, and an empty codec from an "+
			"ffprobe that exited on its own was read as a positive finding that the file is work in "+
			"progress. That answer is about the ACCESS to the path, not about the content of the file.", stranded)
	}
	allowRead(t, stranded)
	if got := md5f(t, stranded); got != md5 {
		t.Errorf("the sweep MODIFIED %s", stranded)
	}

	// And it holds for THIS reason. The operator has a different thing to go and fix
	// depending on which hold fired, and the branches are independently removable, so the
	// reason is asserted rather than only the file's survival.
	denyRead(t, stranded)
	if why := e.strayReplacementHold(ctx, stranded); !strings.Contains(why, "cannot read") {
		t.Errorf("the hold reports %q; a file this process cannot read must be reported as one - an "+
			"unanswerable question is not permission to delete, and this one was never answered", why)
	}
}

// TestStrayTemp_TheSourceBesideQuestionIsAskedBeforeAnythingThatCanFail is the ORDERING
// itself, graded where the ordering is the only thing that decides the answer.
//
// Every other hold in this file is now also carried by a later question, so removing the
// hoist would leave them all green - and the ordering is the property with the widest
// guarantee, because it is decided by os.Lstat alone: no subprocess, no configuration,
// nothing a failing host or a shrunken encoder registry can take away. It is what makes
// "anything the sweep could not establish HOLDS" a property of the function rather than
// an intention about the questions inside it.
//
// The two arms differ in ONE way and it is not the file: both are the same h264 encode at
// the build's own temp construction - complete, decodable, readable, with a working
// ffprobe answering about it, at a codec NO encoder here writes, so the codec question
// gives a POSITIVE and well-founded "not one of ours" for both. One has its source beside
// it and is swept; the other does not, and is held. Holding it costs disk and it is the
// trade this file already declares ("costs an operator some disk and never a file").
func TestStrayTemp_TheSourceBesideQuestionIsAskedBeforeAnythingThatCanFail(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	ctx := context.Background()

	for _, tc := range []struct {
		name       string
		keepSource bool
		wantSwept  bool
	}{
		{name: "with its source beside it the sweep still reclaims it", keepSource: true, wantSwept: true},
		{name: "with nothing beside it the sweep holds", keepSource: false, wantSwept: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "movie.mkv")
			mkH264Long(t, ffmpeg, src, "4M")
			orphan := tempPath(dir, "movie", "mkv", 0)
			mkH264From(t, ffmpeg, src, orphan)
			if got := codecOf(t, ffprobe, orphan); got != "h264" {
				t.Fatalf("precondition: %s is %q, not h264 - the codec question must answer a positive "+
					"'no encoder here writes this' for both arms", orphan, got)
			}
			if err := readableNow(orphan); err != nil {
				t.Fatalf("precondition: %s is not readable (%v), so the readability confirmation would "+
					"carry this arm instead of the ordering", orphan, err)
			}
			if !tc.keepSource {
				if err := os.Remove(src); err != nil {
					t.Fatal(err)
				}
			}

			e := buildEngine(t, ffmpeg, ffprobe, dir, nil, nil)
			e.held.Store(e.loadHoldBacks(ctx))
			e.cleanStaleTemps(ctx)

			switch {
			case tc.wantSwept && exists(orphan):
				t.Errorf("crash-safety: %s survived the sweep with its source beside it and a positive "+
					"finding about its content - the ordering must not have become a rule that holds "+
					"every temp", orphan)
			case !tc.wantSwept && !exists(orphan):
				t.Errorf("AC15i: the sweep DELETED %s, a file at this build's own temp construction with "+
					"NOTHING beside it to measure against. That question is answered by os.Lstat alone and "+
					"must be asked before any question that can fail for a reason which is not about the "+
					"file; behind one, the fail-safe is unreachable exactly when it is needed.", orphan)
			}
			if !tc.wantSwept {
				if why := e.strayReplacementHold(ctx, orphan); !strings.Contains(why, "no source beside it") {
					t.Errorf("the hold reports %q, want the no-source hold - an operator has a different "+
						"thing to go and look for depending on which one fired", why)
				}
			}
		})
	}
}

// TestStrayTemp_ACodecNoEncoderHereWritesIsStillSwept is the control on the codec
// question, and the reason widening it to the whole encoder registry did not turn it
// into a rule that holds everything.
//
// The two files differ in ONE way: both sit at the build's own temp construction, both
// are real, complete, decodable ffmpeg output of the same length as the source beside
// them - so length parity passes for both - and one is at a codec this build's encoders
// produce while the other is not. The first is held; the second is swept.
func TestStrayTemp_ACodecNoEncoderHereWritesIsStillSwept(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	ctx := context.Background()
	prober := probe.New(ffmpeg, ffprobe)

	oursSrc := filepath.Join(d, "ours.mkv")
	theirsSrc := filepath.Join(d, "theirs.mkv")
	mkH264Long(t, ffmpeg, oursSrc, "4M")
	mkH264Long(t, ffmpeg, theirsSrc, "4M")

	ours := tempPath(d, "ours", "mkv", 0)
	theirs := tempPath(d, "theirs", "mkv", 0)
	mkHevcFrom(t, ffmpeg, oursSrc, ours, "")             // hevc: something this build writes
	mkH264From(t, ffmpeg, theirsSrc, theirs)             // h264: nothing here writes it
	if got := codecOf(t, ffprobe, ours); got != "hevc" { // preconditions: the experiment is controlled
		t.Fatalf("precondition: %s is %q, not hevc", ours, got)
	}
	if got := codecOf(t, ffprobe, theirs); got != "h264" {
		t.Fatalf("precondition: %s is %q, not h264", theirs, got)
	}
	for _, p := range []string{ours, theirs} {
		if !prober.DecodeOK(ctx, p) {
			t.Fatalf("precondition: %s does not decode cleanly - both arms must be VALID output", p)
		}
	}
	dOurs, ok1 := prober.DurationSec(ctx, ours)
	dTheirs, ok2 := prober.DurationSec(ctx, theirs)
	if !ok1 || !ok2 || dOurs < 1 || dTheirs < 1 {
		t.Fatalf("precondition: both arms must be whole encodes (%.3fs ok=%v / %.3fs ok=%v)", dOurs, ok1, dTheirs, ok2)
	}

	e := buildEngine(t, ffmpeg, ffprobe, d, nil, nil)
	e.held.Store(e.loadHoldBacks(ctx))
	e.cleanStaleTemps(ctx)

	if !exists(ours) {
		t.Errorf("AC15i: the sweep deleted %s, which is at a codec this build's own encoders write", ours)
	}
	if exists(theirs) {
		t.Errorf("crash-safety: %s is at a codec NO encoder in this build produces, so it cannot be a "+
			"replacement holdfast wrote - widening the codec question to the registry must not become "+
			"a rule that holds every temp", theirs)
	}
}

// midLoopCancel is a context that is live for its first n Err() observations and
// cancelled from then on - a SIGTERM landing INSIDE a loop, deterministically.
//
// It is a test double rather than a real context because no real one can stage this:
// a context cancelled before the call is caught by the sweep's per-DIRECTORY check
// before an entry is ever read, and cancelling from another goroutine mid-loop is a
// race. The distinction it makes visible is exactly the finding's: whether the sweep
// re-asks on each entry or only when it moves to the next directory.
type midLoopCancel struct {
	context.Context
	live int
	done chan struct{}
	once sync.Once
}

func cancelAfter(n int) *midLoopCancel {
	return &midLoopCancel{Context: context.Background(), live: n, done: make(chan struct{})}
}

func (c *midLoopCancel) Err() error {
	if c.live > 0 {
		c.live--
		return nil
	}
	c.once.Do(func() { close(c.done) })
	return context.Canceled
}

func (c *midLoopCancel) Done() <-chan struct{} { return c.done }

// TestStrayTemp_ACancelledRunSweepsNothingAndStopsAtTheNextEntry.
//
// The production trigger that needs no operator at all: a SIGTERM lands while the sweep
// is inside the coverage-bounded entry loop. Every question below the name is an ffprobe
// subprocess and CommandContext kills it, so a cancelled run can establish nothing about
// any remaining file - and a "no" here is a deletion.
//
// Two things are asserted, and they are independent. (1) Nothing is deleted, by EITHER
// deletion route: the sweep and pickTempPath both ask strayReplacementHold, which now
// refuses to answer on a cancelled context. (2) The sweep stops at the next ENTRY rather
// than the next DIRECTORY - visible in the log, because a loop that carried on would ask
// about every remaining file and report holding each one.
func TestStrayTemp_ACancelledRunSweepsNothingAndStopsAtTheNextEntry(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()

	// Three ORDINARY orphans - work in progress, each with its source beside it. A live
	// run sweeps all three, so "nothing was deleted" cannot be confused with "there was
	// nothing to delete".
	var orphans []string
	for _, stem := range []string{"a", "b", "c"} {
		if err := os.WriteFile(filepath.Join(d, stem+".mkv"), []byte("a source"), 0o644); err != nil {
			t.Fatal(err)
		}
		p := tempPath(d, stem, "mkv", 0)
		if err := os.WriteFile(p, []byte("half an encode"), 0o644); err != nil {
			t.Fatal(err)
		}
		orphans = append(orphans, p)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	logs := &capturedLog{}
	cfg := baseCfg(d)
	e := New(cfg, probe.New(ffmpeg, ffprobe), nil, newTestStore(t, d), logs.logger())
	e.Coverage = []string{d} // the branch that used to ask ctx once per DIRECTORY
	e.held.Store(&holdBacks{paths: map[string]string{}})
	e.cleanStaleTemps(cancelled)

	for _, p := range orphans {
		if !exists(p) {
			t.Errorf("a cancelled run DELETED %s. Every question that decides a temp's fate is an "+
				"ffprobe the cancellation kills, so the sweep would be acting on answers it never got", p)
		}
	}

	// The same again with the cancellation landing INSIDE the entry loop, which is the
	// shape the finding named and the only one the per-directory check does not cover:
	// a context cancelled before the call never reaches an entry at all. cancelAfter(1)
	// lets the directory check through and is done by the first entry.
	midLoop := &capturedLog{}
	mid := New(cfg, probe.New(ffmpeg, ffprobe), nil, newTestStore(t, d), midLoop.logger())
	mid.Coverage = []string{d}
	mid.held.Store(&holdBacks{paths: map[string]string{}})
	mid.cleanStaleTemps(cancelAfter(1))

	for _, p := range orphans {
		if !exists(p) {
			t.Errorf("a run cancelled INSIDE the entry loop DELETED %s", p)
		}
	}
	if strings.Contains(midLoop.String(), "leaving a file holdfast wrote in place") {
		t.Errorf("the sweep carried on judging entries after the run was cancelled, one file at a time, "+
			"instead of stopping at the entry it was on:\n%s", midLoop.String())
	}

	// The hold itself refuses to answer, which is what protects the SECOND deletion
	// route - pickTempPath, which no sweep loop guards.
	if why := e.strayReplacementHold(cancelled, orphans[0]); !strings.Contains(why, "being cancelled") {
		t.Errorf("strayReplacementHold reports %q on a cancelled context, want a hold naming the cancellation", why)
	}
	got, err := e.pickTempPath(cancelled, d, "a", "mkv")
	if err != nil {
		t.Fatalf("pickTempPath: %v", err)
	}
	if got == orphans[0] || !exists(orphans[0]) {
		t.Errorf("pickTempPath chose or cleared %s on a cancelled context (it returned %q)", orphans[0], got)
	}

	// And the control: with a live context the same three files are ordinary orphans
	// and every one of them is reclaimed, so the sweep has not been disarmed.
	e.cleanStaleTemps(context.Background())
	for _, p := range orphans {
		if exists(p) {
			t.Errorf("crash-safety: %s survived a LIVE sweep - a killed run's half-written encodes "+
				"would accumulate for ever", p)
		}
	}
}
