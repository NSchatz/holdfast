package engine

// regress_0008_F73 - S0008-holdfast-swap-1, impl gate ordinal 3, finding F73.
//
// THE CLAIM. AC15i's record-free hold-back is not record-free: it depends on a WRITE to
// the media directory (the move to a `__holdfast-replacement__` name), and when that
// write fails for the same reason the store write failed, the gate-passed replacement is
// left at a `__transcoding__` path with nothing holding it back - and the very next run's
// stale-temp sweep DELETES it.
//
//	AC15i - IF a file holdfast wrote as a replacement is still present at a path inside
//	a library root after the job that created it stopped progressing - ... or never
//	recorded at all because AC15h fired - THEN THE SYSTEM SHALL NOT enumerate that path
//	as a source and SHALL NOT encode, swap or delete the file at it, in that run or in
//	any later run. Where a record survives, AC15d is how this holds; where none does,
//	holding it SHALL NOT depend on one, since the write that failed is exactly what
//	denied it.
//
// ONE ROOT CAUSE, BOTH FAULTS. This is not a contrived double failure. A library
// filesystem that remounts read-only on an I/O error (ext4's `errors=remount-ro`
// default) makes the SWAP rename fail - which is how the run reaches handleFailedSwap at
// all - and makes retainReplacement's rename, a rename in the same directory, fail for
// exactly the same reason; a state directory on that same mount then refuses the incident
// write, so AC15h fires. This test reproduces precisely that, with a real encode, a real
// rename against a read-only directory, and the real sweep.
//
// WHAT IT ASSERTS. After the run: the gate-passed hevc replacement is on disk at the temp
// path, both failures were reported, no record names it, and the source is intact. Then a
// later run - with a writable store and a writable directory, i.e. after the admin
// remounted rw - is asked to do the one thing AC15i forbids. It does it: the sweep
// removes a file that passed every gate.
//
// The equivalent case where the retain SUCCEEDS is already green
// (TestUnwritableStore_HoldsBothFilesReportsEverythingAndTheNextRunStillLeavesItAlone),
// so this test isolates the retain's own failure and nothing else.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
)

func TestRegress0008F73_ARetainThatFailsLeavesTheReplacementForTheSweepToDelete(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	srcMD5 := md5f(t, src)

	real := newTestStore(t, d)
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)
	logs := &capturedLog{}
	broken := unwritableIncidents{Store: real, err: errors.New("simulated: the state dir is on the mount that went read-only")}

	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, broken, logs.logger())
	eng.fsLookup = lookups("nfs")
	// The mount goes read-only the instant before the swap. No rename is injected: the
	// REAL os.Rename then fails with EACCES, which is what a read-only library directory
	// does to the swap AND to the retain - one cause, both writes.
	t.Cleanup(func() { _ = os.Chmod(d, 0o755) })
	eng.fsyncPath = func(p string) error {
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

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if err := os.Chmod(d, 0o755); err != nil {
		t.Fatal(err)
	}

	// Preconditions. Every one of them, or this test proves nothing.
	tmp := tempPath(d, "movie", "mkv", 0)
	if !exists(tmp) {
		t.Fatalf("precondition: no replacement at the temp path %s; the directory holds %v\nlog:\n%s",
			tmp, lsDir(t, d), logs.String())
	}
	replMD5 := md5f(t, tmp)
	// It really is the GATE-PASSED replacement and not a half-written encode: the file at
	// the temp path is the finished hevc output the verify/VMAF gate accepted, which is
	// what makes deleting it the thing AC15i forbids rather than the stale-temp sweep
	// doing its ordinary and correct job.
	if got := codecOf(t, ffprobe, tmp); got != "hevc" {
		t.Fatalf("precondition: the file at %s is %q, not the gate-passed hevc replacement", tmp, got)
	}
	if len(retainedFiles(t, d)) != 0 {
		t.Fatalf("precondition: the retain SUCCEEDED, so the failure under test did not happen: %v", retainedFiles(t, d))
	}
	if in := allIncidents(t, real); len(in) != 0 {
		t.Fatalf("precondition: the store recorded an incident, so AC15h did not fire: %+v", in)
	}
	// Both failures were REPORTED, which is how anyone knows this state exists at all.
	out := logs.String()
	for _, want := range []string{"could not move the replacement to a held-back name", "COULD NOT PERSIST"} {
		if !strings.Contains(out, want) {
			t.Fatalf("precondition: the run never reported %q; log:\n%s", want, out)
		}
	}
	if !exists(src) || md5f(t, src) != srcMD5 {
		t.Fatal("the source was modified - this test is about the replacement, and the source must be intact")
	}

	// A LATER run, store writable and directory writable again: exactly the situation
	// AC15i's Given describes. Only the sweep is driven, so the deletion cannot be
	// attributed to anything else.
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, real, discardLogger())
	ctx := context.Background()
	next.held.Store(next.loadHoldBacks(ctx))
	next.cleanStaleTemps(ctx)

	if !exists(tmp) {
		t.Fatalf("AC15i: the later run DELETED a replacement holdfast wrote (%s). No record survived to protect it "+
			"- the store write that would have made one is what failed - and the record-free hold-back did not "+
			"reach it, because it depends on a rename into the same read-only directory that denied the swap.", tmp)
	}
	if md5f(t, tmp) != replMD5 {
		t.Errorf("the later run modified the replacement at %s", tmp)
	}
}
