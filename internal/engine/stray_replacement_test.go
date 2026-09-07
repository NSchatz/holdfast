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

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
