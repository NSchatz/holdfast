package engine

// FILESYSTEM-1 - the honest outcome of a FAILED swap, proven on ordinary local storage.
//
// The gate has no network mount and no second real filesystem, so three substitutions
// carry the whole contract: the filesystem-type LOOKUP, the RENAME itself, and the
// RE-STAT that follows a failed rename. Everything else here is real - a real ffmpeg
// encode, the real verify gate, real files on disk, the real SQLite store - and every
// assertion about a file is made by looking at the file, never by reading a log line.
//
// Three named scenarios, and every expectation below belongs to exactly one of them:
//
//	S1  failed WITHOUT applying - the injected rename returns an error and does not
//	    rename, so the source is still at the source path and the (real) re-stat returns
//	    the SOURCE's pre-swap record.
//	S2  applied - the injected rename PERFORMS the rename and then returns an error, so
//	    the replacement is at the source path and the re-stat returns the REPLACEMENT's
//	    pre-swap record. This is the shape rename(2) documents for NFS and it is the
//	    single most important case in the phase.
//	S3  applied behind a stale cache - as S2, but the substituted re-stat returns the
//	    SOURCE's pre-swap record, as a client attribute cache populated before the swap
//	    would. Not local by construction, so S3 has no local variant.
//
// The one thing no substitution can produce is S3: with the rename genuinely applied,
// only a lying stat returns the source's attributes - which is why the re-stat is a seam
// in its own right and not a convenience.

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/fsclass"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// errSwap is the deterministic error an injected rename reports. It is deliberately NOT
// EXDEV: the cross-filesystem cause has its own tests and must never be attributed to a
// failure that is not one.
var errSwap = errors.New("simulated swap failure")

// ---- seam builders -----------------------------------------------------------

// failingRename is S1: report an error, rename nothing.
func failingRename(err error) func(string, string) error {
	return func(_, _ string) error { return err }
}

// applyingRename is S2: perform the rename, THEN report an error.
func applyingRename(err error) func(string, string) error {
	return func(oldpath, newpath string) error {
		if rerr := os.Rename(oldpath, newpath); rerr != nil {
			return rerr
		}
		return err
	}
}

// copyingRename is S2 with a contradiction: the replacement lands at the target AND a
// file is left at the recorded replacement path, which a rename that took effect cannot
// account for.
func copyingRename(err error) func(string, string) error {
	return func(oldpath, newpath string) error {
		b, rerr := os.ReadFile(oldpath)
		if rerr != nil {
			return rerr
		}
		fi, rerr := os.Stat(oldpath)
		if rerr != nil {
			return rerr
		}
		if rerr := os.WriteFile(newpath, b, 0o644); rerr != nil {
			return rerr
		}
		// The copy has to carry the replacement's own attributes, or the re-stat sees a
		// third file rather than the replacement and the contradiction under test never
		// arises.
		if rerr := os.Chtimes(newpath, fi.ModTime(), fi.ModTime()); rerr != nil {
			return rerr
		}
		return err
	}
}

// lookups returns a substituted filesystem-type lookup that answers with each type in
// turn and then repeats the last: the engine looks up twice per failed swap (once when
// the GUARD runs, once at SWAP time), so lookups("ext4", "nfs") is "local when the run
// gets going, network by the time the swap happens". An empty string means the lookup
// FAILED, which is not local either.
func lookups(types ...string) fsclass.Lookup {
	var mu sync.Mutex
	i := 0
	return func(string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		t := types[i]
		if i < len(types)-1 {
			i++
		}
		if t == "" {
			return "", errors.New("statfs: permission denied")
		}
		return t, nil
	}
}

// ---- assertion helpers -------------------------------------------------------

// allIncidents walks ids from 1 until one is missing, which is every incident a fresh
// test store can hold (AUTOINCREMENT, nothing deletes them).
func allIncidents(t *testing.T, ts *testStore) []store.SwapIncident {
	t.Helper()
	var out []store.SwapIncident
	for id := int64(1); ; id++ {
		in, ok, err := ts.IncidentByID(context.Background(), id)
		if err != nil {
			t.Fatalf("IncidentByID(%d): %v", id, err)
		}
		if !ok {
			return out
		}
		out = append(out, in)
	}
}

func onlyIncident(t *testing.T, ts *testStore) store.SwapIncident {
	t.Helper()
	all := allIncidents(t, ts)
	if len(all) != 1 {
		t.Fatalf("want exactly one swap incident, got %d: %+v", len(all), all)
	}
	return all[0]
}

// jobRow returns the row keyed by path+fingerprint given (NOT re-derived from the file
// on disk: after an applied swap the file's fingerprint has moved and the row that
// records the attempt is the one under the pre-swap key).
func jobRow(t *testing.T, ts *testStore, path, fingerprint string) (store.Job, bool) {
	t.Helper()
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, r := range rows {
		if r.Path == path && r.Fingerprint == fingerprint {
			return r, true
		}
	}
	return store.Job{}, false
}

// rowFor returns the single row whose Path matches, failing if there is not exactly one.
func rowFor(t *testing.T, ts *testStore, path string) store.Job {
	t.Helper()
	rows, err := ts.List(context.Background(), nil, 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var found []store.Job
	for _, r := range rows {
		if r.Path == path {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one row for %s, got %d: %+v", path, len(found), found)
	}
	return found[0]
}

// retainedFiles lists every retained-replacement file under dir, by the build's own
// construction - the record-free hold-back's matcher, used here as the observer.
func retainedFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && IsRetainedReplacementName(filepath.Base(p)) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// lsDir lists a directory's entries for a failure message.
func lsDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func onlyRetained(t *testing.T, dir string) string {
	t.Helper()
	files := retainedFiles(t, dir)
	if len(files) != 1 {
		t.Fatalf("want exactly one retained replacement under %s, got %v", dir, files)
	}
	return files[0]
}

// capturedLog is a logger whose output a test can read back. It is used ONLY where the
// criterion is about REPORTING (the unpersistable outcome, which by construction has no
// record to assert against); every other assertion here looks at files and rows.
type capturedLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (c *capturedLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *capturedLog) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(c, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// unwritableIncidents wraps a real store and refuses exactly one write: the one that
// persists a failed swap's outcome. Everything else behaves normally, which is what an
// unwritable-at-that-moment store looks like from the engine's side.
type unwritableIncidents struct {
	store.Store
	err error
}

func (u unwritableIncidents) RecordSwapIncident(context.Context, store.SwapIncident) error {
	return u.err
}

// swapFixture is one h264 source under a fresh root, with an engine wired to it. The
// source is .mkv and the config forces "mkv", so the swap is IN PLACE (final == the
// source path) - the shape every criterion here is written against.
type swapFixture struct {
	dir    string
	src    string
	srcMD5 string
	eng    *Engine
	ts     *testStore
	ffmpeg string
	fprobe string
}

func newSwapFixture(t *testing.T) *swapFixture {
	t.Helper()
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M") // inflated, so a real libx265 encode really is smaller
	eng, ts := buildEngineWithStore(t, ffmpeg, ffprobe, d)
	return &swapFixture{dir: d, src: src, srcMD5: md5f(t, src), eng: eng, ts: ts, ffmpeg: ffmpeg, fprobe: ffprobe}
}

func (f *swapFixture) run(t *testing.T) {
	t.Helper()
	if err := f.eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
}

// assertSourceIntact is the assertion that matters most in this file: the bytes at the
// source path are the bytes the test wrote there.
func (f *swapFixture) assertSourceIntact(t *testing.T) {
	t.Helper()
	if !exists(f.src) {
		t.Fatal("the source is GONE")
	}
	if md5f(t, f.src) != f.srcMD5 {
		t.Fatal("the source was modified")
	}
}

// ---- S1: the swap failed and did not apply -----------------------------------

// TestSwapS1_OnLocalStorageTheSourceIsConfirmedUntouched is AC14a case (b) and the ONLY
// route to "untouched": the re-stat returned the source's own pre-swap record AND the
// storage was positively identified as local. It also pins AC13a - the re-stat is not
// skipped because the storage is local.
//
// RED at the pin: the pin logs "swap error, source untouched" without re-statting
// anything, so the claim is unearned even when it is true.
func TestSwapS1_OnLocalStorageTheSourceIsConfirmedUntouched(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = failingRename(errSwap)
	f.eng.fsLookup = lookups("ext4")
	f.run(t)

	f.assertSourceIntact(t)
	if got := codecOf(t, f.fprobe, f.src); got != "h264" {
		t.Errorf("source codec = %q, want h264 (untouched)", got)
	}
	row := rowFor(t, f.ts, f.src)
	if row.Status != store.Failed {
		t.Fatalf("status = %q, want %q (a failure that left the source intact)", row.Status, store.Failed)
	}
	if !strings.Contains(row.Outcome.Reason, "untouched") {
		t.Errorf("reason does not report the source untouched: %q", row.Outcome.Reason)
	}
	if in := allIncidents(t, f.ts); len(in) != 0 {
		t.Errorf("an established, source-intact failure recorded a swap incident: %+v", in)
	}
	if nTemp(t, f.dir) != 0 {
		t.Error("the temp was not discarded after an established source-intact failure")
	}
	if got := retainedFiles(t, f.dir); len(got) != 0 {
		t.Errorf("a source-intact failure retained a replacement: %v", got)
	}
}

// TestSwapS1_OnNetworkStorageTheOutcomeIsIndeterminateNotUntouched is AC14a case (c) and
// AC14b, and it is the headline: the SAME observation that reads "untouched" on ext4
// proves nothing on NFS, because a client attribute cache populated before the swap
// returns exactly the pre-swap answer whether or not the rename was applied.
//
// It also pins AC15/AC15a: the job is parked, both files are kept, and the record
// carries both paths and both pre-swap attribute records.
//
// RED at the pin: the pin reports "source untouched" here.
func TestSwapS1_OnNetworkStorageTheOutcomeIsIndeterminateNotUntouched(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = failingRename(errSwap)
	f.eng.fsLookup = lookups("nfs")
	f.run(t)

	f.assertSourceIntact(t)
	row := rowFor(t, f.ts, f.src)
	if row.Status != store.Indeterminate {
		t.Fatalf("status = %q, want %q", row.Status, store.Indeterminate)
	}
	if strings.Contains(row.Outcome.Reason, "untouched") {
		t.Errorf("the outcome claims the source is untouched on network storage: %q", row.Outcome.Reason)
	}

	// AC15a: both files identifiable from the record alone, after a restart.
	in := onlyIncident(t, f.ts)
	if !in.Parked() {
		t.Fatalf("the incident is not parked: %+v", in)
	}
	if in.SourcePath != f.src {
		t.Errorf("recorded source path = %q, want %q", in.SourcePath, f.src)
	}
	retained := onlyRetained(t, f.dir)
	if in.ReplacementPath != retained {
		t.Errorf("recorded replacement path = %q, but the replacement is at %q", in.ReplacementPath, retained)
	}
	wantSrc, err := probe.StatAttributes(f.src)
	if err != nil {
		t.Fatal(err)
	}
	if in.SourceAttrs != wantSrc.String() {
		t.Errorf("recorded source attributes = %q, want %q", in.SourceAttrs, wantSrc.String())
	}
	gotRepl, err := probe.StatAttributes(retained)
	if err != nil {
		t.Fatal(err)
	}
	if in.ReplacementAttrs != gotRepl.String() {
		t.Errorf("recorded replacement attributes = %q, but the retained file is %q", in.ReplacementAttrs, gotRepl.String())
	}
	// The replacement is KEPT: a parked job holds both files.
	if !exists(retained) {
		t.Error("the replacement was deleted on the indeterminate path")
	}
	if got := codecOf(t, f.fprobe, retained); got != "hevc" {
		t.Errorf("the retained replacement is %q, want the hevc encode", got)
	}
}

// ---- S2: the rename took effect and reported an error anyway ------------------

// TestSwapS2_AppliedIsReportedAppliedWhateverTheStorage is AC14a case (a) and AC14e.
// Case (a) does NOT consult the classification - the replacement's attributes at the
// source path are not something a stale cache can invent, because the cache was
// populated while the SOURCE was there - so local and network must reach the same
// answer, and neither may say "untouched" or "indeterminate".
func TestSwapS2_AppliedIsReportedAppliedWhateverTheStorage(t *testing.T) {
	for _, fsType := range []string{"ext4", "nfs"} {
		t.Run(fsType, func(t *testing.T) {
			f := newSwapFixture(t)
			f.eng.renameFn = applyingRename(errSwap)
			f.eng.fsLookup = lookups(fsType)
			preSwapKey := probe.Fingerprint(f.src)
			f.run(t)

			row, ok := jobRow(t, f.ts, f.src, preSwapKey)
			if !ok {
				t.Fatalf("no row under the pre-swap key %q", preSwapKey)
			}
			if row.Status != store.AppliedDespiteError {
				t.Fatalf("status = %q, want %q", row.Status, store.AppliedDespiteError)
			}
			if strings.Contains(row.Outcome.Reason, "untouched") {
				t.Errorf("an applied swap was reported as leaving the source untouched: %q", row.Outcome.Reason)
			}

			// The file at the source path IS the replacement, and it is NOT deleted.
			if !exists(f.src) {
				t.Fatal("the file at the source path was removed after an applied swap")
			}
			if got := codecOf(t, f.fprobe, f.src); got != "hevc" {
				t.Errorf("the file at the source path is %q, want the hevc replacement", got)
			}

			in := onlyIncident(t, f.ts)
			if in.Outcome != store.AppliedDespiteError {
				t.Errorf("incident outcome = %q, want %q", in.Outcome, store.AppliedDespiteError)
			}
			if in.Parked() {
				t.Error("an applied-despite-error job is parked - it needs no operator action")
			}
			// The evidence case (a) matched on, and the error the rename returned.
			if in.ObservedAttrs != in.ReplacementAttrs {
				t.Errorf("the recorded evidence (%q) is not the replacement record it matched (%q)",
					in.ObservedAttrs, in.ReplacementAttrs)
			}
			if !strings.Contains(in.SwapError, errSwap.Error()) {
				t.Errorf("the recorded outcome does not carry the rename's error: %q", in.SwapError)
			}
			if got := retainedFiles(t, f.dir); len(got) != 0 {
				t.Errorf("an applied swap retained a replacement file: %v", got)
			}
		})
	}
}

// TestSwapS2_AFileStillAtTheReplacementPathIsParkedNotApplied is AC14e's contradiction
// clause: a rename that took effect cannot account for BOTH files. The pair is parked.
func TestSwapS2_AFileStillAtTheReplacementPathIsParkedNotApplied(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = copyingRename(errSwap) // lands at the target AND stays at the temp
	f.eng.fsLookup = lookups("ext4")
	preSwapKey := probe.Fingerprint(f.src)
	f.run(t)

	row, ok := jobRow(t, f.ts, f.src, preSwapKey)
	if !ok {
		t.Fatalf("no row under the pre-swap key %q", preSwapKey)
	}
	if row.Status != store.Indeterminate {
		t.Fatalf("status = %q, want %q (two files cannot both be accounted for)", row.Status, store.Indeterminate)
	}
	in := onlyIncident(t, f.ts)
	if !in.Parked() {
		t.Fatal("the contradicted applied case did not park the job")
	}
	if !strings.Contains(in.SwapError, "STILL present") {
		t.Errorf("the recorded reason does not name the contradiction: %q", in.SwapError)
	}
}

// ---- S3: applied, behind a stale attribute cache ------------------------------

// TestSwapS3_AppliedBehindAStaleCacheIsIndeterminate is the case only the third seam can
// produce: the rename REALLY took effect, and the re-stat returns the SOURCE's pre-swap
// attributes because the client answered from a cache populated before the swap. The
// honest answer is that the outcome is unknown - never "untouched", which is what the
// observation alone would have said.
func TestSwapS3_AppliedBehindAStaleCacheIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	srcRec, err := probe.StatAttributes(f.src)
	if err != nil {
		t.Fatal(err)
	}
	f.eng.renameFn = applyingRename(errSwap)
	f.eng.restatFn = func(string) (probe.Attributes, error) { return srcRec, nil } // the stale cache
	f.eng.fsLookup = lookups("nfs")                                                // not local by construction
	preSwapKey := probe.Fingerprint(f.src)
	f.run(t)

	row, ok := jobRow(t, f.ts, f.src, preSwapKey)
	if !ok {
		t.Fatalf("no row under the pre-swap key %q", preSwapKey)
	}
	if row.Status != store.Indeterminate {
		t.Fatalf("status = %q, want %q", row.Status, store.Indeterminate)
	}
	if strings.Contains(row.Outcome.Reason, "untouched") {
		t.Errorf("a swap that WAS applied was reported as leaving the source untouched: %q", row.Outcome.Reason)
	}
	if !onlyIncident(t, f.ts).Parked() {
		t.Error("the job was not parked")
	}
	// The rename really happened, so the file at the source path is the replacement.
	if got := codecOf(t, f.fprobe, f.src); got != "hevc" {
		t.Errorf("the file at the source path is %q, want the applied hevc replacement", got)
	}
}

// ---- AC14d: the classification is the SWAP's, never an earlier record ---------

// TestSwap_TheClassificationIsTakenAtSwapTime pins AC14d with the case that makes it
// matter: a root that was local when the run began, with a NAS mounted beneath it while
// the encode ran. An older record is wrong exactly when it matters most, so the decision
// re-looks. S1 is the only scenario where the classification decides the outcome.
func TestSwap_TheClassificationIsTakenAtSwapTime(t *testing.T) {
	// The engine looks up twice per failed swap: once when the guard runs, once at swap
	// time. "ext4" then "nfs" is local at the first and network by the second.
	t.Run("local at the guard, network at the swap", func(t *testing.T) {
		f := newSwapFixture(t)
		f.eng.renameFn = failingRename(errSwap)
		f.eng.fsLookup = lookups("ext4", "nfs")
		f.run(t)

		f.assertSourceIntact(t)
		row := rowFor(t, f.ts, f.src)
		if row.Status != store.Indeterminate {
			t.Fatalf("status = %q, want %q - the swap-time classification must decide", row.Status, store.Indeterminate)
		}
		if strings.Contains(row.Outcome.Reason, "untouched") {
			t.Errorf("a stale local classification produced an untouched claim: %q", row.Outcome.Reason)
		}
	})

	// A swap-time lookup that fails or is denied is not local (Definitions), so the
	// outcome is case (c) or (d) and never untouched.
	t.Run("the swap-time lookup is denied", func(t *testing.T) {
		f := newSwapFixture(t)
		f.eng.renameFn = failingRename(errSwap)
		f.eng.fsLookup = lookups("ext4", "") // "" = the lookup errors
		f.run(t)

		f.assertSourceIntact(t)
		row := rowFor(t, f.ts, f.src)
		if row.Status != store.Indeterminate {
			t.Fatalf("status = %q, want %q", row.Status, store.Indeterminate)
		}
	})
}

// ---- AC14 / AC14a(d) / AC14c: what the re-stat could not establish ------------

// TestSwap_ARestatThatFailsIsIndeterminate - nothing was established, so nothing is
// reported: not success, not untouched, not applied.
func TestSwap_ARestatThatFailsIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = failingRename(errSwap)
	f.eng.restatFn = func(string) (probe.Attributes, error) {
		return probe.Attributes{}, errors.New("simulated: stat refused")
	}
	f.eng.fsLookup = lookups("ext4") // local, and STILL not untouched
	f.run(t)

	f.assertSourceIntact(t)
	row := rowFor(t, f.ts, f.src)
	if row.Status != store.Indeterminate {
		t.Fatalf("status = %q, want %q", row.Status, store.Indeterminate)
	}
	if strings.Contains(row.Outcome.Reason, "untouched") {
		t.Errorf("an uncompletable re-stat still produced an untouched claim: %q", row.Outcome.Reason)
	}
	if !onlyIncident(t, f.ts).Parked() {
		t.Error("the job was not parked")
	}
}

// TestSwap_AnObservationMatchingNeitherRecordIsIndeterminate is AC14a case (d).
func TestSwap_AnObservationMatchingNeitherRecordIsIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = failingRename(errSwap)
	f.eng.restatFn = func(string) (probe.Attributes, error) {
		return probe.Attributes{SizeBytes: 424242, MTimeUnix: 1}, nil // neither record
	}
	f.eng.fsLookup = lookups("ext4")
	f.run(t)

	f.assertSourceIntact(t)
	row := rowFor(t, f.ts, f.src)
	if row.Status != store.Indeterminate {
		t.Fatalf("status = %q, want %q", row.Status, store.Indeterminate)
	}
	if !strings.Contains(row.Outcome.Reason, "matches neither") {
		t.Errorf("the reason does not say the observation matched neither record: %q", row.Outcome.Reason)
	}
}

// TestSwap_IndistinguishablePreSwapRecordsAreIndeterminate is AC14c's second half, and
// it drives the REAL failed-swap handler (not a decision helper) over real files and the
// real store: when the two pre-swap records are identical, a re-stat cannot tell the two
// files apart whatever it returns, so neither the applied case nor the untouched case is
// establishable.
//
// It calls handleFailedSwap directly because the condition cannot be reached through
// ProcessFile: the verify gate requires a strictly SMALLER output, so a real encode can
// never carry the source's own size. Everything else is real - the files exist, the
// store is the SQLite one, and the assertions are made against both.
func TestSwap_IndistinguishablePreSwapRecordsAreIndeterminate(t *testing.T) {
	f := newSwapFixture(t)
	tmp := filepath.Join(f.dir, "movie."+TempMarker+".mkv")
	if err := os.WriteFile(tmp, []byte("a replacement that got as far as the swap"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The same record for both files: a re-stat returning it identifies nothing.
	same := probe.Attributes{SizeBytes: 100, MTimeUnix: 1700000000}
	key := probe.Fingerprint(f.src)
	if _, err := f.ts.Claim(context.Background(), f.src, key, "w0", 3); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	f.eng.fsLookup = lookups("ext4") // local, and still not untouched
	f.eng.handleFailedSwap(context.Background(), f.src, key, tmp, f.src, same, same, errSwap, &store.Outcome{})

	row, ok := jobRow(t, f.ts, f.src, key)
	if !ok {
		t.Fatal("no job row was written")
	}
	if row.Status != store.Indeterminate {
		t.Fatalf("status = %q, want %q", row.Status, store.Indeterminate)
	}
	if !strings.Contains(row.Outcome.Reason, "identical pre-swap attributes") {
		t.Errorf("the reason does not name the indistinguishable records: %q", row.Outcome.Reason)
	}
	f.assertSourceIntact(t)
	if !exists(onlyRetained(t, f.dir)) {
		t.Error("the replacement was not kept")
	}
}

// ---- AC17 / AC18 / AC19: the cross-filesystem cause ---------------------------

// TestSwap_CrossFilesystemCauseIsReportedDistinctlyAndNamesBothPaths is AC17 and AC18.
// The distinct cause is a stable token on the row (a UI cannot key off a sentence), the
// report names the temp and the target, and the state of the source is reported per the
// outcome decision rather than asserted.
func TestSwap_CrossFilesystemCauseIsReportedDistinctlyAndNamesBothPaths(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
	}
	f.eng.fsLookup = lookups("ext4")
	f.run(t)

	f.assertSourceIntact(t)
	row := rowFor(t, f.ts, f.src)
	if row.Outcome.SwapCause != store.SwapCauseCrossFilesystem {
		t.Fatalf("swap cause = %q, want %q", row.Outcome.SwapCause, store.SwapCauseCrossFilesystem)
	}
	if !strings.Contains(row.Outcome.Reason, "not on the same mounted filesystem") {
		t.Errorf("the report does not name the cause: %q", row.Outcome.Reason)
	}
	// AC18: BOTH paths named - the temp the replacement was at, and the target.
	if !strings.Contains(row.Outcome.Reason, TempMarker) {
		t.Errorf("the report does not name the temp path: %q", row.Outcome.Reason)
	}
	if !strings.Contains(row.Outcome.Reason, f.src) {
		t.Errorf("the report does not name the target path: %q", row.Outcome.Reason)
	}
	// AC18 again: the source's state is REPORTED (established by the re-stat), not
	// asserted by the cross-filesystem branch itself.
	if !strings.Contains(row.Outcome.Reason, "re-stat") {
		t.Errorf("the report does not state what the re-stat established: %q", row.Outcome.Reason)
	}
}

// TestSwap_AnotherFailureIsNotAttributedToTheCrossFilesystemCause is AC19.
func TestSwap_AnotherFailureIsNotAttributedToTheCrossFilesystemCause(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = func(oldpath, newpath string) error {
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EACCES}
	}
	f.eng.fsLookup = lookups("ext4")
	f.run(t)

	row := rowFor(t, f.ts, f.src)
	if row.Outcome.SwapCause != "" {
		t.Errorf("a permission failure was given the swap cause %q", row.Outcome.SwapCause)
	}
	if strings.Contains(row.Outcome.Reason, "cross-filesystem") {
		t.Errorf("a permission failure was reported as cross-filesystem: %q", row.Outcome.Reason)
	}
}

// ---- AC20 / AC21 / AC22 / AC22a: the guard's granularity record ---------------

// TestGuard_RecordsWhatItComparedAndWhichWindowApplies is the whole granularity
// obligation in one test, run against three classifications.
//
// The two MEASURED facts are the attributes compared and the resolution of the timestamp
// compared - and the resolution is REQUIRED to be a duration: it is what says how sharp
// the guard actually is, and a build that dropped it (or spelled it as a word) would have
// thrown that away. The WINDOW is the opposite: a class label naming which of the two
// documented windows applies, and never a duration, because the network window belongs to
// the client's attribute cache and has no value anyone can honestly state.
func TestGuard_RecordsWhatItComparedAndWhichWindowApplies(t *testing.T) {
	cases := []struct {
		name   string
		fsType string
		want   string
	}{
		{"local storage takes the local window", "ext4", store.ResidualWindowLocal},
		{"network storage takes the network window", "nfs", store.ResidualWindowNetwork},
		{"undetermined storage takes the NETWORK window", "some-unrecognised-fs", store.ResidualWindowNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwapFixture(t)
			f.eng.fsLookup = lookups(tc.fsType)
			f.run(t) // a real, successful swap: the guard runs on the happy path too

			row := rowFor(t, f.ts, f.src) // keyed by the post-swap file
			if row.Status != store.Done {
				t.Fatalf("status = %q, want %q", row.Status, store.Done)
			}
			if row.Outcome.GuardAttributes != probe.AttributeNames {
				t.Errorf("compared attributes = %q, want %q", row.Outcome.GuardAttributes, probe.AttributeNames)
			}
			// AC21's field is REQUIRED to carry a measured duration.
			d, err := time.ParseDuration(row.Outcome.GuardTimeResolution)
			if err != nil {
				t.Fatalf("the recorded time resolution %q is not a duration: %v", row.Outcome.GuardTimeResolution, err)
			}
			if d != time.Second {
				t.Errorf("recorded time resolution = %v, want 1s (the whole-second mtime this build compares)", d)
			}
			// AC22 + AC22a: the class label, and no other window.
			if row.Outcome.GuardResidualWindow != tc.want {
				t.Fatalf("recorded window = %q, want %q", row.Outcome.GuardResidualWindow, tc.want)
			}
			other := store.ResidualWindowLocal
			if tc.want == store.ResidualWindowLocal {
				other = store.ResidualWindowNetwork
			}
			if strings.Contains(row.Outcome.GuardResidualWindow, other) {
				t.Errorf("the record names both windows: %q", row.Outcome.GuardResidualWindow)
			}
			// The window field holds a LABEL: no duration, and no figure of any kind.
			if _, err := time.ParseDuration(row.Outcome.GuardResidualWindow); err == nil {
				t.Errorf("the window field parsed as a duration (%q) - it must be a class label", row.Outcome.GuardResidualWindow)
			}
			if strings.ContainsAny(row.Outcome.GuardResidualWindow, "0123456789") {
				t.Errorf("the window field carries a figure: %q", row.Outcome.GuardResidualWindow)
			}
		})
	}
}

// TestGuard_TheWindowFollowsTheStorageAtTHEMOMENTTHEGUARDRUNS is AC22's Given: a library
// root whose storage was local when the run began, with a NAS mounted beneath it hours
// later. The record must describe the storage the check actually ran against.
//
// The flip is driven off the temp fsync, which the swap performs immediately before the
// guard - so the lookup answers "ext4" for the whole run and "nfs" from the instant the
// guard runs.
func TestGuard_TheWindowFollowsTheStorageAtTHEMOMENTTHEGUARDRUNS(t *testing.T) {
	f := newSwapFixture(t)
	var mu sync.Mutex
	mounted := false
	f.eng.fsyncPath = func(p string) error {
		if strings.Contains(p, TempMarker) {
			mu.Lock()
			mounted = true // the NAS appears beneath the root, right before the guard
			mu.Unlock()
		}
		return fsyncPath(p)
	}
	f.eng.fsLookup = func(string) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if mounted {
			return "nfs", nil
		}
		return "ext4", nil
	}
	f.run(t)

	row := rowFor(t, f.ts, f.src)
	if row.Outcome.GuardResidualWindow != store.ResidualWindowNetwork {
		t.Fatalf("recorded window = %q, want %q - the guard ran against the NAS",
			row.Outcome.GuardResidualWindow, store.ResidualWindowNetwork)
	}
}

// ---- AC16: the regression anchor ---------------------------------------------

// TestSwap_ASuccessfulSwapReportsExactlyWhatItReportedBefore is AC16. None of the checks
// this phase adds may turn a successful swap into a failed or an indeterminate one, or
// change what a successful swap reports.
func TestSwap_ASuccessfulSwapReportsExactlyWhatItReportedBefore(t *testing.T) {
	f := newSwapFixture(t)
	before := probe.FileSize(f.src)
	f.run(t) // no seams at all: real rename, real lookup, real re-stat

	row := rowFor(t, f.ts, f.src)
	if row.Status != store.Done {
		t.Fatalf("status = %q, want %q", row.Status, store.Done)
	}
	if got := codecOf(t, f.fprobe, f.src); got != "hevc" {
		t.Errorf("the swapped file is %q, want hevc", got)
	}
	if row.Outcome.Reason != "" {
		t.Errorf("a successful swap recorded a reason: %q", row.Outcome.Reason)
	}
	if row.Outcome.SwapCause != "" {
		t.Errorf("a successful swap recorded a swap cause: %q", row.Outcome.SwapCause)
	}
	if row.Outcome.SourceBytes == nil || *row.Outcome.SourceBytes != before {
		t.Errorf("source bytes = %v, want %d", row.Outcome.SourceBytes, before)
	}
	if row.Outcome.OutputBytes == nil || *row.Outcome.OutputBytes >= before {
		t.Errorf("output bytes = %v, want strictly smaller than %d", row.Outcome.OutputBytes, before)
	}
	if in := allIncidents(t, f.ts); len(in) != 0 {
		t.Errorf("a successful swap recorded a swap incident: %+v", in)
	}
	if nTemp(t, f.dir) != 0 || len(retainedFiles(t, f.dir)) != 0 {
		t.Error("a successful swap left a file behind")
	}
}

// ---- AC15c / AC15d: a restart, and exactly two files withheld -----------------

// TestRestart_TheParkedJobSurvivesAndWithholdsExactlyTwoFiles is AC15, AC15a, AC15c and
// AC15d in one pass, and it is deliberately run with a SECOND source present: "nothing
// happens at either path" has to be distinguishable from "nothing happens at all".
func TestRestart_TheParkedJobSurvivesAndWithholdsExactlyTwoFiles(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = failingRename(errSwap)
	f.eng.fsLookup = lookups("nfs")
	f.run(t)

	in := onlyIncident(t, f.ts)
	if !in.Parked() {
		t.Fatal("the first run did not park the job")
	}
	retained := onlyRetained(t, f.dir)
	retainedMD5 := md5f(t, retained)

	// A second source appears, and the run restarts: a NEW Engine over the SAME store,
	// with no seams at all, which is what a restart looks like.
	other := filepath.Join(f.dir, "other.mkv")
	mkH264(t, f.ffmpeg, other, "8M")
	cfg := baseCfg(f.dir)
	prober := probe.New(f.ffmpeg, f.fprobe)
	logs := &capturedLog{}
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: f.ffmpeg, Cfg: cfg, Probe: prober}, f.ts, logs.logger())
	if err := next.RunOneshot(context.Background()); err != nil {
		t.Fatalf("second RunOneshot: %v", err)
	}

	// The run REPORTS the parked job when it starts, naming both files - an operator
	// who never looks at the dashboard still learns that two files are being withheld
	// and what to run to release them.
	report := logs.String()
	for _, want := range []string{"PARKED", f.src, retained, in.SourceAttrs, in.ReplacementAttrs, "holdfast resolve"} {
		if !strings.Contains(report, want) {
			t.Errorf("the run-start report does not carry %q:\n%s", want, report)
		}
	}

	// The parked job is still parked, and still says which two files it is about.
	after := onlyIncident(t, f.ts)
	if !after.Parked() {
		t.Fatal("the parked job was resolved by a run rather than by an operator")
	}
	if after.SourcePath != in.SourcePath || after.ReplacementPath != in.ReplacementPath {
		t.Errorf("the recorded paths moved across the restart: %+v -> %+v", in, after)
	}
	if after.SourceAttrs != in.SourceAttrs || after.ReplacementAttrs != in.ReplacementAttrs {
		t.Error("the recorded pre-swap attributes did not survive the restart")
	}

	// Neither of the two files was touched.
	f.assertSourceIntact(t)
	if got := codecOf(t, f.fprobe, f.src); got != "h264" {
		t.Errorf("the parked source was re-encoded (codec %q)", got)
	}
	if !exists(retained) || md5f(t, retained) != retainedMD5 {
		t.Error("the parked replacement was deleted or modified by the next run")
	}
	// The replacement was not enumerated: no job row names it.
	if _, ok := jobRow(t, f.ts, retained, probe.Fingerprint(retained)); ok {
		t.Error("the retained replacement was enumerated as a source")
	}
	// The parked job was not re-queued: its row is still the parked one.
	row := rowFor(t, f.ts, f.src)
	if row.Status != store.Indeterminate {
		t.Errorf("the parked job's row is %q, want %q", row.Status, store.Indeterminate)
	}

	// ... and the run was NOT narrowed: the other source was encoded and swapped.
	if got := codecOf(t, f.fprobe, other); got != "hevc" {
		t.Fatalf("the second source was not swapped (codec %q) - a parked job narrowed the whole run", got)
	}
	if o := rowFor(t, f.ts, other); o.Status != store.Done {
		t.Errorf("the second source's row is %q, want %q", o.Status, store.Done)
	}
}

// ---- AC15h / AC15i: the store could not be written ---------------------------

// TestUnwritableStore_HoldsBothFilesReportsEverythingAndTheNextRunStillLeavesItAlone is
// AC15h and AC15i together, because they are two halves of one situation: the write that
// failed is exactly what denied the record, so the hold-back in the NEXT run cannot
// depend on one.
func TestUnwritableStore_HoldsBothFilesReportsEverythingAndTheNextRunStillLeavesItAlone(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	srcMD5 := md5f(t, src)

	real := newTestStore(t, d)
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)
	logs := &capturedLog{}
	broken := unwritableIncidents{Store: real, err: errors.New("simulated: the job store cannot be written")}
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, broken, logs.logger())
	renames := 0
	eng.renameFn = func(string, string) error { renames++; return errSwap }
	eng.fsLookup = lookups("nfs")
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// Neither file was deleted; the swap was not re-attempted. The replacement's path is
	// derived from the CONSTRUCTION rather than from the name matcher, deliberately: the
	// hold-back under test is the matcher, so an assertion that used it to find the file
	// would be asking the accused for an alibi.
	if !exists(src) || md5f(t, src) != srcMD5 {
		t.Fatal("the source was deleted or modified when the outcome could not be persisted")
	}
	retained := retainedReplacementPath(d, "movie", "mkv", 0)
	if !exists(retained) {
		t.Fatalf("the replacement was not retained at %s; the directory holds %v", retained, lsDir(t, d))
	}
	retainedMD5 := md5f(t, retained)
	if renames != 1 {
		t.Errorf("the swap was attempted %d times, want exactly 1", renames)
	}

	// It was REPORTED: an operator holds what the parked record would have carried.
	out := logs.String()
	wantSrc, err := probe.StatAttributes(src)
	if err != nil {
		t.Fatal(err)
	}
	replRec, err := probe.StatAttributes(retained)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"COULD NOT PERSIST", src, retained, wantSrc.String(), replRec.String(),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the unpersistable outcome was not reported with %q; log:\n%s", want, out)
		}
	}
	for _, forbidden := range []string{"DONE", "source confirmed untouched"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("the report claims %q; log:\n%s", forbidden, out)
		}
	}
	// Nothing was persisted: no incident, and no terminal row claiming an outcome.
	if in := allIncidents(t, real); len(in) != 0 {
		t.Errorf("an incident was recorded despite the store refusing the write: %+v", in)
	}

	// AC15i: a LATER run, with a writable store, still leaves the replacement alone -
	// it is recognised by the build's own construction of replacement paths, which is
	// all that is left when no record could be written.
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, real, discardLogger())
	if err := next.RunOneshot(context.Background()); err != nil {
		t.Fatalf("later RunOneshot: %v", err)
	}
	if !exists(retained) {
		t.Fatal("the later run DELETED a replacement holdfast wrote and no record survived to protect")
	}
	if md5f(t, retained) != retainedMD5 {
		t.Error("the later run modified the retained replacement")
	}
	if _, ok := jobRow(t, real, retained, probe.Fingerprint(retained)); ok {
		t.Error("the later run enumerated the retained replacement as a source")
	}
	// And the ordinary source in the same root is NOT held back by any of this: it is
	// encoded and swapped normally, which is what bounds the record-free rule.
	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Errorf("the later run did not process the ordinary source (codec %q)", got)
	}
}

// TestUnwritableStore_TheAppliedOutcomeIsAlsoHeldAndReported is the same obligation for
// the OTHER outcome this phase adds. An applied-despite-error outcome that cannot be
// persisted must not delete the file now at the source path, must not re-attempt the
// swap, and must report both paths and both pre-swap records - and it must certainly not
// fall back to reporting success or an untouched source.
func TestUnwritableStore_TheAppliedOutcomeIsAlsoHeldAndReported(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	srcRec, err := probe.StatAttributes(src)
	if err != nil {
		t.Fatal(err)
	}

	real := newTestStore(t, d)
	cfg := baseCfg(d)
	prober := probe.New(ffmpeg, ffprobe)
	logs := &capturedLog{}
	broken := unwritableIncidents{Store: real, err: errors.New("simulated: the job store cannot be written")}
	eng := New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, broken, logs.logger())
	renames := 0
	eng.renameFn = func(oldpath, newpath string) error {
		renames++
		if err := os.Rename(oldpath, newpath); err != nil {
			return err
		}
		return errSwap
	}
	eng.fsLookup = lookups("ext4")
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// The rename took effect, so the file at the source path is the replacement - and
	// nothing deleted it.
	if !exists(src) {
		t.Fatal("the file at the source path was deleted when the outcome could not be persisted")
	}
	if got := codecOf(t, ffprobe, src); got != "hevc" {
		t.Errorf("the file at the source path is %q, want the applied replacement", got)
	}
	if renames != 1 {
		t.Errorf("the swap was attempted %d times, want exactly 1", renames)
	}
	out := logs.String()
	for _, want := range []string{"COULD NOT PERSIST", src, srcRec.String(), TempMarker} {
		if !strings.Contains(out, want) {
			t.Errorf("the unpersistable applied outcome was not reported with %q; log:\n%s", want, out)
		}
	}
	if strings.Contains(out, "source confirmed untouched") || strings.Contains(out, "DONE") {
		t.Errorf("the report claims success or an untouched source; log:\n%s", out)
	}
	if in := allIncidents(t, real); len(in) != 0 {
		t.Errorf("an incident was recorded despite the store refusing the write: %+v", in)
	}
}

// ---- AC14e / AC15d / AC15e: what LATER runs do -------------------------------

// TestApplied_ALaterRunOffersTheSourcePathAndTheORDINARYRuleSkipsIt is AC14e's release
// clause. The file at the source path IS the replacement, so no hold-back is placed on
// that path: a later run ENUMERATES it, and it is the ordinary already-at-target-codec
// rule - not an exclusion - that keeps it from being re-encoded. The distinction is the
// whole point, so the test asserts the ordinary REASON rather than merely "nothing
// happened".
func TestApplied_ALaterRunOffersTheSourcePathAndTheORDINARYRuleSkipsIt(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = applyingRename(errSwap)
	f.eng.fsLookup = lookups("ext4")
	preSwapKey := probe.Fingerprint(f.src)
	f.run(t)

	if row, _ := jobRow(t, f.ts, f.src, preSwapKey); row.Status != store.AppliedDespiteError {
		t.Fatalf("the first run did not record an applied-despite-error outcome (got %q)", row.Status)
	}
	appliedMD5 := md5f(t, f.src)

	cfg := baseCfg(f.dir)
	prober := probe.New(f.ffmpeg, f.fprobe)
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: f.ffmpeg, Cfg: cfg, Probe: prober}, f.ts, discardLogger())
	if err := next.RunOneshot(context.Background()); err != nil {
		t.Fatalf("later RunOneshot: %v", err)
	}

	// Offered to enumeration, and skipped by the ORDINARY rule with the ordinary reason.
	key := probe.Fingerprint(f.src)
	row, ok := jobRow(t, f.ts, f.src, key)
	if !ok {
		t.Fatal("the source path was NOT offered to enumeration - a hold-back was placed on it")
	}
	if row.Status != store.Skipped {
		t.Fatalf("the later run recorded %q for the released path, want %q", row.Status, store.Skipped)
	}
	if row.Outcome.Reason != SkipAlreadyTargetCodec {
		t.Errorf("skip reason = %q, want the ordinary %q", row.Outcome.Reason, SkipAlreadyTargetCodec)
	}
	// The job itself was not re-attempted: its record still stands under the old key.
	if old, ok := jobRow(t, f.ts, f.src, preSwapKey); !ok || old.Status != store.AppliedDespiteError {
		t.Errorf("the applied-despite-error record was re-attempted or lost (%+v)", old)
	}
	if md5f(t, f.src) != appliedMD5 {
		t.Error("the later run modified the file at the released source path")
	}
}

// TestResolved_TheNextRunDoesExactlyWhatTheDispositionsSay is AC15e's later-run half,
// for BOTH determinations. It also pins AC15d: the retained replacement stays out of
// enumeration for as long as the record lives, resolved or not.
func TestResolved_TheNextRunDoesExactlyWhatTheDispositionsSay(t *testing.T) {
	t.Run("the source is intact: the source is reclaimed, the replacement is left alone", func(t *testing.T) {
		f := newSwapFixture(t)
		f.eng.renameFn = failingRename(errSwap)
		f.eng.fsLookup = lookups("nfs")
		f.run(t)

		in := onlyIncident(t, f.ts)
		retained := onlyRetained(t, f.dir)
		retainedMD5 := md5f(t, retained)

		if err := f.ts.ResolveIncident(context.Background(), in.ID, store.Resolution{
			Determination:          store.SourceIsIntact,
			By:                     "operator",
			DispositionSource:      store.KeptInPlace,
			DispositionReplacement: store.RetainedExcluded,
		}); err != nil {
			t.Fatalf("ResolveIncident: %v", err)
		}

		cfg := baseCfg(f.dir)
		prober := probe.New(f.ffmpeg, f.fprobe)
		next := New(cfg, prober, FFmpegEncoder{FFmpeg: f.ffmpeg, Cfg: cfg, Probe: prober}, f.ts, discardLogger())
		if err := next.RunOneshot(context.Background()); err != nil {
			t.Fatalf("later RunOneshot: %v", err)
		}

		// kept-in-place means what it says: the source path re-enters enumeration, and
		// the swap that never happened happens now. This is how an un-transcoded source
		// is reclaimed rather than silently held forever.
		if got := codecOf(t, f.fprobe, f.src); got != "hevc" {
			t.Fatalf("a kept-in-place source resolved 'the source is intact' was not re-encoded (codec %q)", got)
		}
		if row := rowFor(t, f.ts, f.src); row.Status != store.Done {
			t.Errorf("the reclaimed source's row is %q, want %q", row.Status, store.Done)
		}
		// The retained replacement is untouched and still not enumerated.
		if !exists(retained) || md5f(t, retained) != retainedMD5 {
			t.Error("the retained-excluded replacement was deleted or modified")
		}
		if _, ok := jobRow(t, f.ts, retained, probe.Fingerprint(retained)); ok {
			t.Error("the retained-excluded replacement was enumerated as a source")
		}
		// A resolved job is not re-parked by a later run.
		if parked, err := f.ts.ParkedIncidents(context.Background()); err != nil || len(parked) != 0 {
			t.Errorf("a resolved job was re-parked (%d parked, err %v)", len(parked), err)
		}
	})

	t.Run("the swap was applied: the source path is skipped by the ordinary rule", func(t *testing.T) {
		f := newSwapFixture(t)
		srcRec, err := probe.StatAttributes(f.src)
		if err != nil {
			t.Fatal(err)
		}
		// S3: the rename really applied, but a stale cache hid it, so the job parked.
		f.eng.renameFn = applyingRename(errSwap)
		f.eng.restatFn = func(string) (probe.Attributes, error) { return srcRec, nil }
		f.eng.fsLookup = lookups("nfs")
		f.run(t)

		in := onlyIncident(t, f.ts)
		if !in.Parked() {
			t.Fatal("S3 did not park the job")
		}
		appliedMD5 := md5f(t, f.src) // the file at the source path is the replacement

		if err := f.ts.ResolveIncident(context.Background(), in.ID, store.Resolution{
			Determination:          store.SwapWasApplied,
			By:                     "operator",
			DispositionSource:      store.KeptInPlace,
			DispositionReplacement: store.Absent, // the rename consumed it
		}); err != nil {
			t.Fatalf("ResolveIncident: %v", err)
		}

		cfg := baseCfg(f.dir)
		prober := probe.New(f.ffmpeg, f.fprobe)
		next := New(cfg, prober, FFmpegEncoder{FFmpeg: f.ffmpeg, Cfg: cfg, Probe: prober}, f.ts, discardLogger())
		if err := next.RunOneshot(context.Background()); err != nil {
			t.Fatalf("later RunOneshot: %v", err)
		}

		key := probe.Fingerprint(f.src)
		row, ok := jobRow(t, f.ts, f.src, key)
		if !ok {
			t.Fatal("the kept-in-place source path was not offered to enumeration")
		}
		if row.Status != store.Skipped || row.Outcome.Reason != SkipAlreadyTargetCodec {
			t.Errorf("the later run recorded %q/%q, want a skip by the ordinary %q rule",
				row.Status, row.Outcome.Reason, SkipAlreadyTargetCodec)
		}
		if md5f(t, f.src) != appliedMD5 {
			t.Error("the file at the kept-in-place source path was modified")
		}
		if parked, err := f.ts.ParkedIncidents(context.Background()); err != nil || len(parked) != 0 {
			t.Errorf("a resolved job was re-parked (%d parked, err %v)", len(parked), err)
		}
	})
}

// TestResolved_ARetainedReplacementIsLeftAloneUnderTheOTHERDeterminationToo. The
// disposition, not the determination, is what governs the file: a replacement the
// operator chose to keep is out of enumeration under "the swap was applied" exactly as it
// is under "the source is intact", and the source path is handed back to the ordinary
// rules either way.
func TestResolved_ARetainedReplacementIsLeftAloneUnderTheOTHERDeterminationToo(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = failingRename(errSwap)
	f.eng.fsLookup = lookups("nfs")
	f.run(t)

	in := onlyIncident(t, f.ts)
	retained := onlyRetained(t, f.dir)
	retainedMD5 := md5f(t, retained)
	if err := f.ts.ResolveIncident(context.Background(), in.ID, store.Resolution{
		Determination:          store.SwapWasApplied,
		By:                     "operator",
		DispositionSource:      store.KeptInPlace,
		DispositionReplacement: store.RetainedExcluded,
	}); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}

	cfg := baseCfg(f.dir)
	prober := probe.New(f.ffmpeg, f.fprobe)
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: f.ffmpeg, Cfg: cfg, Probe: prober}, f.ts, discardLogger())
	if err := next.RunOneshot(context.Background()); err != nil {
		t.Fatalf("later RunOneshot: %v", err)
	}

	if !exists(retained) || md5f(t, retained) != retainedMD5 {
		t.Error("the retained-excluded replacement was deleted or modified")
	}
	if _, ok := jobRow(t, f.ts, retained, probe.Fingerprint(retained)); ok {
		t.Error("the retained-excluded replacement was enumerated as a source")
	}
	if parked, err := f.ts.ParkedIncidents(context.Background()); err != nil || len(parked) != 0 {
		t.Errorf("a resolved job was re-parked (%d parked, err %v)", len(parked), err)
	}
	// The source path went back to the ORDINARY rules: this file really is still h264,
	// so they select it. Which rule fires is the ordinary rules' business - the point is
	// that no hold-back stood in the way.
	if _, ok := jobRow(t, f.ts, f.src, probe.Fingerprint(f.src)); !ok {
		t.Error("the kept-in-place source path was not offered to enumeration")
	}
}

// TestRetained_AFreshEncodeNeitherOverwritesNorIsBlockedByARetainedReplacement is the
// collision the two constructions exist to prevent. A source resolved "the source is
// intact" is handed back to enumeration and encoded AGAIN while the replacement of the
// failed attempt is still sitting beside it: the fresh encode must neither write over
// that file nor be blocked by it.
func TestRetained_AFreshEncodeNeitherOverwritesNorIsBlockedByARetainedReplacement(t *testing.T) {
	f := newSwapFixture(t)
	f.eng.renameFn = failingRename(errSwap)
	f.eng.fsLookup = lookups("nfs")
	f.run(t)

	in := onlyIncident(t, f.ts)
	retained := onlyRetained(t, f.dir)
	retainedMD5 := md5f(t, retained)
	if err := f.ts.ResolveIncident(context.Background(), in.ID, store.Resolution{
		Determination:          store.SourceIsIntact,
		By:                     "operator",
		DispositionSource:      store.KeptInPlace,
		DispositionReplacement: store.RetainedExcluded,
	}); err != nil {
		t.Fatalf("ResolveIncident: %v", err)
	}

	// The fresh encode's temp path is the build's own construction, and so is the
	// retained name: the two cannot collide, and the retained file is not in the way.
	cfg := baseCfg(f.dir)
	prober := probe.New(f.ffmpeg, f.fprobe)
	next := New(cfg, prober, FFmpegEncoder{FFmpeg: f.ffmpeg, Cfg: cfg, Probe: prober}, f.ts, discardLogger())
	if err := next.RunOneshot(context.Background()); err != nil {
		t.Fatalf("later RunOneshot: %v", err)
	}
	if got := codecOf(t, f.fprobe, f.src); got != "hevc" {
		t.Errorf("the fresh encode was BLOCKED by the retained replacement (codec %q)", got)
	}
	if md5f(t, retained) != retainedMD5 {
		t.Error("the fresh encode OVERWROTE the retained replacement")
	}
	if nTemp(t, f.dir) != 0 {
		t.Error("the fresh encode left a work-in-progress temp behind")
	}
}

// TestRetainedName_MatchesOnlyWhatTheConstructionProduces bounds the record-free rule.
// It is matched EXACTLY against this build's own construction and never by a widened
// "looks temporary" pattern - because everything it matches is withheld from a library
// forever, and the one thing worse than enumerating a file holdfast wrote is refusing to
// enumerate a file it did not.
func TestRetainedName_MatchesOnlyWhatTheConstructionProduces(t *testing.T) {
	for n := 0; n < 3; n++ {
		p := retainedReplacementPath("/lib/tv", "Show", "mkv", n)
		if !IsRetainedReplacementName(filepath.Base(p)) {
			t.Errorf("the construction produced %q and the matcher does not recognise it", p)
		}
	}
	held := []string{
		"Show." + RetainedMarker + ".mkv",
		"Show." + RetainedMarker + ".12.mkv",
		"Show.S01E01.1080p." + RetainedMarker + ".mp4",
	}
	for _, base := range held {
		if !IsRetainedReplacementName(base) {
			t.Errorf("%q should be held back - the construction can produce it", base)
		}
	}
	free := []string{
		"Show.mkv",
		"Show.tmp.mkv",
		".Show.mkv",
		"Show." + TempMarker + ".mkv", // a work-in-progress temp is NOT a replacement
		RetainedMarker + ".mkv",       // no stem
		"Show." + RetainedMarker + ".mkv.part",
		"Show." + RetainedMarker + ".x1.mkv",
		"Show." + RetainedMarker + ".",
	}
	for _, base := range free {
		if IsRetainedReplacementName(base) {
			t.Errorf("%q must NOT be held back on its name - nothing but the construction is", base)
		}
	}
}

// TestStaleTempSweep_StillDiscardsWorkInProgressButNeverARetainedReplacement is the
// distinction the two markers exist for. A killed run's partial encode is worth nothing
// and is swept, exactly as it always was; a retained replacement passed every gate and
// may be the only faithful copy of a source whose fate is unknown.
func TestStaleTempSweep_StillDiscardsWorkInProgressButNeverARetainedReplacement(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	mkHevc(t, ffmpeg, filepath.Join(d, "movie.mkv"), "400k") // already at target: no work
	orphan := filepath.Join(d, "movie."+TempMarker+".mkv")
	if err := os.WriteFile(orphan, []byte("half an encode"), 0o644); err != nil {
		t.Fatal(err)
	}
	kept := retainedReplacementPath(d, "other", "mkv", 0)
	if err := os.WriteFile(kept, []byte("a gate-passed replacement nobody may delete"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, ffmpeg, ffprobe, d, nil, nil)

	if exists(orphan) {
		t.Error("the orphaned work-in-progress temp was not discarded")
	}
	if !exists(kept) {
		t.Fatal("the sweep DELETED a retained replacement")
	}
}

// ---- the decision table, exhaustively ----------------------------------------

// TestDecideFailedSwap_TheFourCasesAreExhaustive walks the decision itself. The
// end-to-end tests above prove the engine reaches these answers through real files; this
// one proves the table has no hole, including the combinations a fixture cannot stage.
func TestDecideFailedSwap_TheFourCasesAreExhaustive(t *testing.T) {
	src := probe.Attributes{SizeBytes: 100, MTimeUnix: 10}
	repl := probe.Attributes{SizeBytes: 40, MTimeUnix: 20}
	other := probe.Attributes{SizeBytes: 7, MTimeUnix: 7}
	local := fsclass.ClassifyType("ext4", nil)
	network := fsclass.ClassifyType("nfs", nil)
	undetermined := fsclass.ClassifyType("some-unrecognised-fs", nil)

	cases := []struct {
		name     string
		observed probe.Attributes
		err      error
		cls      fsclass.Classification
		want     store.Status
		wantCase string
	}{
		{"(a) applied, local", repl, nil, local, store.AppliedDespiteError, "a"},
		{"(a) applied, network - the classification is not consulted", repl, nil, network, store.AppliedDespiteError, "a"},
		{"(a) applied, undetermined", repl, nil, undetermined, store.AppliedDespiteError, "a"},
		{"(b) untouched, local only", src, nil, local, store.Failed, "b"},
		{"(c) source's attributes but network", src, nil, network, store.Indeterminate, "c"},
		{"(c) source's attributes but undetermined", src, nil, undetermined, store.Indeterminate, "c"},
		{"(d) matches neither", other, nil, local, store.Indeterminate, "d"},
		{"re-stat failed", probe.Attributes{}, errors.New("nope"), local, store.Indeterminate, "restat-failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideFailedSwap(src, repl, tc.observed, tc.err, tc.cls)
			if got.Status != tc.want || got.Case != tc.wantCase {
				t.Fatalf("= %q/%q, want %q/%q", got.Status, got.Case, tc.want, tc.wantCase)
			}
			if got.Why == "" {
				t.Error("the decision carries no explanation")
			}
		})
	}

	// Indistinguishable records: neither the applied case nor the untouched case is
	// establishable, whatever the re-stat returns and whatever the storage is.
	for _, cls := range []fsclass.Classification{local, network, undetermined} {
		got := decideFailedSwap(src, src, src, nil, cls)
		if got.Status != store.Indeterminate || got.Case != "indistinguishable" {
			t.Errorf("indistinguishable records on %v = %q/%q, want indeterminate", cls.Class, got.Status, got.Case)
		}
	}
}

// TestResidualWindowFor_UndeterminedTakesTheNetworkWindow pins AC22's "exactly TWO
// windows and no third" against the fail-safe direction.
func TestResidualWindowFor_UndeterminedTakesTheNetworkWindow(t *testing.T) {
	if got := residualWindowFor(fsclass.ClassifyType("ext4", nil)); got != store.ResidualWindowLocal {
		t.Errorf("local storage -> %q", got)
	}
	for _, typ := range []string{"nfs", "cifs", "overlayfs", "", "who-knows"} {
		got := residualWindowFor(fsclass.ClassifyType(typ, nil))
		if got != store.ResidualWindowNetwork {
			t.Errorf("%q -> %q, want the network window", typ, got)
		}
	}
	if got := residualWindowFor(fsclass.ClassifyType("", errors.New("denied"))); got != store.ResidualWindowNetwork {
		t.Errorf("a failed lookup -> %q, want the network window", got)
	}
}

// ---- AC15j: the two states are reported AS THEMSELVES ------------------------

// TestObserver_TheTwoNewOutcomesAreEmittedAsThemselves is the engine's half of AC15j:
// the event a reporting surface receives carries the state the job is actually in.
func TestObserver_TheTwoNewOutcomesAreEmittedAsThemselves(t *testing.T) {
	for _, tc := range []struct {
		name   string
		rename func(string, string) error
		fsType string
		want   store.Status
	}{
		{"parked indeterminate", failingRename(errSwap), "nfs", store.Indeterminate},
		{"applied despite an error", applyingRename(errSwap), "ext4", store.AppliedDespiteError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwapFixture(t)
			var mu sync.Mutex
			var seen []store.Status
			f.eng.Observer = func(ev Event) {
				mu.Lock()
				seen = append(seen, ev.Status)
				mu.Unlock()
			}
			f.eng.renameFn = tc.rename
			f.eng.fsLookup = lookups(tc.fsType)
			f.run(t)

			mu.Lock()
			defer mu.Unlock()
			var terminal []store.Status
			for _, s := range seen {
				if s.Terminal() {
					terminal = append(terminal, s)
				}
			}
			if len(terminal) != 1 || terminal[0] != tc.want {
				t.Fatalf("terminal events = %v, want exactly [%s]", terminal, tc.want)
			}
			for _, s := range seen {
				if s == store.Done || s == store.Failed {
					t.Errorf("the outcome was reported as %q", s)
				}
			}
		})
	}
}

// ---- the temp-path construction ----------------------------------------------

// TestPickTempPath_SkipsAPathARecordHoldsBackAndNeverClearsIt covers the remaining
// collision: a recorded replacement that could not be MOVED to its retained name is
// still at a temp path, and the record is the only thing saying so. The next encode of
// that source must step around it rather than clear it, which is what every other temp
// path gets.
func TestPickTempPath_SkipsAPathARecordHoldsBackAndNeverClearsIt(t *testing.T) {
	d := t.TempDir()
	first := tempPath(d, "movie", "mkv", 0)
	if err := os.WriteFile(first, []byte("a recorded replacement stuck at a temp path"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := md5f(t, first)

	eng := New(config.Config{}, nil, nil, nil, discardLogger())
	eng.held.Store(&holdBacks{paths: map[string]string{resolvedForm(first): "a recorded replacement path"}})

	got, err := eng.pickTempPath(d, "movie", "mkv")
	if err != nil {
		t.Fatalf("pickTempPath: %v", err)
	}
	if got == first {
		t.Fatal("the held-back temp path was chosen for a fresh encode")
	}
	if !exists(first) || md5f(t, first) != before {
		t.Fatal("the held-back file was cleared")
	}
	if got != tempPath(d, "movie", "mkv", 1) {
		t.Errorf("temp path = %q, want the next candidate of the same construction", got)
	}
	// With nothing held back, the ordinary path is the one this repo has always used,
	// and a stale temp at it is still cleared.
	eng.held.Store(&holdBacks{paths: map[string]string{}})
	got, err = eng.pickTempPath(d, "movie", "mkv")
	if err != nil {
		t.Fatalf("pickTempPath: %v", err)
	}
	if got != first {
		t.Errorf("temp path = %q, want the unsuffixed %q", got, first)
	}
	if exists(first) {
		t.Error("a stale, unheld temp was not cleared")
	}
}

// TestResolvedForm_ComparesPathsTheWayTheDefinitionsDo - a parked job's hold-back is
// keyed on the recorded paths in resolved form, so a symlinked or dot-laden spelling of
// the same file is the same file.
func TestResolvedForm_ComparesPathsTheWayTheDefinitionsDo(t *testing.T) {
	d := t.TempDir()
	real := filepath.Join(d, "movie.mkv")
	if err := os.WriteFile(real, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(d, "link.mkv")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if resolvedForm(link) != resolvedForm(real) {
		t.Errorf("a symlink resolves to %q, want %q", resolvedForm(link), resolvedForm(real))
	}
	if resolvedForm(filepath.Join(d, ".", "sub", "..", "movie.mkv")) != resolvedForm(real) {
		t.Error("a dot-laden spelling did not resolve to the same path")
	}
	if resolvedForm(d+string(os.PathSeparator)) != resolvedForm(d) {
		t.Error("a trailing separator was not removed")
	}
	// A recorded path whose file is gone must still compare: EvalSymlinks cannot help
	// there, and a parked job whose file an operator deleted is exactly that case.
	gone := filepath.Join(d, "gone.mkv")
	if resolvedForm(gone) != filepath.Clean(gone) {
		t.Errorf("a missing path resolved to %q, want its lexical form", resolvedForm(gone))
	}
}
