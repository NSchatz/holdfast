package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// The scratch half of S0079, driven end to end through RunOneshot over real ffmpeg
// fixtures. The load-bearing claims here are all about WHERE files appear and WHEN,
// so every one of them is graded by observing the filesystem at the moment it
// matters rather than by asking the engine what it did.

// scratchRun builds a temp library root and a scratch directory beside it (a
// SIBLING of the root, never under it - an overlapping scratch is a startup refusal).
func scratchDirs(t *testing.T) (root, scratch string) {
	t.Helper()
	d := t.TempDir()
	root = filepath.Join(d, "media")
	scratch = filepath.Join(d, "scratch")
	for _, p := range []string{root, scratch} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root, scratch
}

// realEncode is the production encoder wrapped so a test can observe the path it was
// asked to write to, and inspect the world at the moment it is called. It runs the
// REAL FFmpegEncoder, so the output is a real encode the real gates then judge.
type realEncode struct {
	enc  FFmpegEncoder
	mu   sync.Mutex
	outs map[string]string // source -> the path the encoder was told to write
	// before, when non-nil, is called with (in, out) BEFORE the encode runs - the
	// one moment at which "nothing has been written under the source's directory
	// yet" is a question with an answer.
	before func(in, out string)
}

func (r *realEncode) Encode(ctx context.Context, in, out string, props *probe.VideoProps) error {
	r.mu.Lock()
	if r.outs == nil {
		r.outs = map[string]string{}
	}
	r.outs[in] = out
	r.mu.Unlock()
	if r.before != nil {
		r.before(in, out)
	}
	return r.enc.Encode(ctx, in, out, props)
}

func (r *realEncode) outFor(t *testing.T, in string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.outs[in]
	if !ok {
		t.Fatalf("the encoder was never asked to encode %s", in)
	}
	return p
}

// buildEngineWithStore is buildEngine plus the store it was given, for a test that
// drives RunOneshot itself (because it has a seam to set first) and still has to
// assert on the ledger row the run wrote. It takes a config mutation and an encoder,
// which is what separates it from durability_test.go's fixed-shape
// buildEngineWithStore.
func buildEngineAndStore(t *testing.T, ffmpeg, ffprobe, root string, enc Encoder, mutate func(*config.Config)) (*Engine, *testStore) {
	t.Helper()
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	if enc == nil {
		enc = FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}
	}
	return New(cfg, prober, enc, ts, discardLogger()), ts
}

// listDir returns the sorted basenames of the files in dir, which is the "directory
// listing" several criteria are stated in terms of.
func listDir(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

// inodeOf is how "the same file" is asserted rather than "the same bytes". A copy
// reproduces the bytes and cannot reproduce the inode, which is the only way to tell
// a path that renames the encoder's own output from one that copies it first.
func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("stat %s: the platform reported no inode", path)
	}
	return st.Ino
}

// AC-B1: with no scratch_dir the encode's working file is created BESIDE THE SOURCE
// under today's construction and today's swap runs - with no copy step.
//
// "No copy step" is proved by IDENTITY, not by absence of evidence: the file the swap
// renames is the very file the encoder wrote, same inode. A copy would produce a
// different inode however carefully it reproduced the bytes, so this assertion cannot
// pass on a path that copies.
func TestScratch_AbsentScratchDirKeepsTodaysInPlaceEncodeAndSwap(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, _ := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, nil)
	rec := &realEncode{enc: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}}
	eng.Enc = rec

	var renamedFrom string
	var renamedInode uint64
	eng.renameFn = func(oldpath, newpath string) error {
		renamedFrom = oldpath
		renamedInode = inodeOf(t, oldpath)
		return os.Rename(oldpath, newpath)
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	wrote := rec.outFor(t, src)
	wantTemp := tempPath(root, "film", "mkv", 0)
	if wrote != wantTemp {
		t.Fatalf("the encoder wrote to %s, want the in-place construction %s", wrote, wantTemp)
	}
	if renamedFrom != wrote {
		t.Fatalf("the swap renamed %s, but the encoder wrote %s - a copy step appeared on the default path",
			renamedFrom, wrote)
	}
	final := filepath.Join(root, "film.mkv")
	if inodeOf(t, final) != renamedInode {
		t.Fatalf("the file at %s is not the inode the encoder wrote - a copy happened where none should", final)
	}
	if got := listDir(t, root); len(got) != 1 || got[0] != "film.mkv" {
		t.Fatalf("the source's directory holds %v, want just the swapped file", got)
	}
}

// AC-B2: with a scratch_dir configured the encoder's output is created UNDER THAT
// DIRECTORY, and NO file is created under the source's directory until the acceptance
// gates have accepted.
//
// The second half is observed at the only moment it can be: from inside the encoder,
// before it has written anything, and again from a run whose encode the gates REJECT
// - where the source's directory must be untouched at the end, because the gates
// never accepted at all.
func TestScratch_TheEncoderWritesToScratchAndNothingReachesTheSourceUntilAcceptance(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	t.Run("accepted: nothing beside the source until the gates pass", func(t *testing.T) {
		root, scratch := scratchDirs(t)
		src := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, src, "8M")

		eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
		var atEncodeTime []string
		rec := &realEncode{
			enc:    FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe},
			before: func(in, out string) { atEncodeTime = listDir(t, filepath.Dir(in)) },
		}
		eng.Enc = rec
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}

		wrote := rec.outFor(t, src)
		if filepath.Dir(wrote) != scratch {
			t.Fatalf("the encoder wrote to %s, which is not under the scratch directory %s", wrote, scratch)
		}
		if len(atEncodeTime) != 1 || atEncodeTime[0] != "film.mkv" {
			t.Fatalf("at encode time the source's directory held %v, want only the source - "+
				"something was created beside the source before the gates had accepted", atEncodeTime)
		}
		if got := listDir(t, root); len(got) != 1 || got[0] != "film.mkv" {
			t.Fatalf("after the swap the source's directory holds %v, want just the swapped file", got)
		}
		if got := listDir(t, scratch); len(got) != 0 {
			t.Fatalf("the scratch directory holds %v after a completed job, want nothing", got)
		}
	})

	t.Run("rejected: the source's directory is untouched", func(t *testing.T) {
		root, scratch := scratchDirs(t)
		src := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, src, "8M")
		before := md5f(t, src)

		// An encode the size gate must reject: a lossless re-encode of an already
		// small clip comes out LARGER, and reclaiming space is the whole point.
		tooBig := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
			ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", in,
				"-c:v", "libx265", "-x265-params", "log-level=error", "-preset", "ultrafast",
				"-x265-params", "log-level=error:lossless=1", "-pix_fmt", "yuv420p", "--", out)
			return nil
		})
		ts := run(t, ffmpeg, ffprobe, root, tooBig, func(c *config.Config) { c.ScratchDir = scratch })

		if !ledgerHas(t, ts, store.Failed, "film.mkv") {
			t.Fatalf("a rejected encode did not fail the job")
		}
		if md5f(t, src) != before {
			t.Fatalf("the source was modified by a rejected encode")
		}
		if got := listDir(t, root); len(got) != 1 || got[0] != "film.mkv" {
			t.Fatalf("a REJECTED encode left %v in the source's directory, want just the source - "+
				"the whole point of a scratch location is that a failed transcode never reaches the array", got)
		}
		if got := listDir(t, scratch); len(got) != 0 {
			t.Fatalf("the scratch directory holds %v after a rejected encode, want nothing", got)
		}
	})
}

// AC-B6: the gates reject an encode while a scratch_dir is configured AND the
// source's own directory is not writable. The job still records the GATE's rejection
// reason - not a write or permission error - and the directory listing is unchanged.
//
// This is what D3's ordering actually buys, stated as a test: nothing is attempted
// under the source's directory on the reject path, so a directory that would refuse a
// write is never asked to accept one, and the recorded reason is about the ENCODE
// rather than about the disk.
func TestScratch_AGateRejectionIsRecordedAsTheGatesEvenWhenTheSourceDirIsReadOnly(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)
	beforeList := listDir(t, root)

	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	tooBig := EncoderFunc(func(ctx context.Context, in, out string, _ *probe.VideoProps) error {
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", in,
			"-c:v", "libx265", "-preset", "ultrafast",
			"-x265-params", "log-level=error:lossless=1", "-pix_fmt", "yuv420p", "--", out)
		return nil
	})
	ts := run(t, ffmpeg, ffprobe, root, tooBig, func(c *config.Config) { c.ScratchDir = scratch })

	out, status, ok := outcomeFor(t, ts, src)
	if !ok || status != store.Failed {
		t.Fatalf("status = %q (found=%v), want failed", status, ok)
	}
	if !strings.Contains(out.Reason, "size-increase reject") {
		t.Fatalf("the recorded reason is not the GATE's: %q - a read-only source directory must not be able to "+
			"replace a gate's verdict with a write error", out.Reason)
	}
	for _, bad := range []string{"permission denied", "read-only"} {
		if strings.Contains(strings.ToLower(out.Reason), bad) {
			t.Fatalf("the recorded reason is a write error rather than the gate's: %q", out.Reason)
		}
	}
	if md5f(t, src) != before {
		t.Fatalf("the source was modified")
	}
	if got := listDir(t, root); !equalStrings(got, beforeList) {
		t.Fatalf("the source's directory listing changed from %v to %v", beforeList, got)
	}
}

// AC-B14: two sources that share a basename in different library directories, one
// scratch directory, one run. Each job gets a DISTINCT working path and both swaps
// complete, neither job reading or removing the other's working file.
//
// The two fixtures have different DURATIONS on purpose. If the working paths
// collided, one job would verify or swap the other's bytes - and the duration of the
// file left on disk is what would say so, where a size or a checksum of two encodes
// of the same fixture would not.
func TestScratch_TwoSourcesSharingABasenameGetDistinctWorkingPaths(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	a := filepath.Join(root, "showA")
	b := filepath.Join(root, "showB")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	srcA := filepath.Join(a, "film.mkv")
	srcB := filepath.Join(b, "film.mkv")
	mkH264(t, ffmpeg, srcA, "8M")     // 2 seconds
	mkH264Long(t, ffmpeg, srcB, "8M") // 10 seconds

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) {
		c.ScratchDir = scratch
		c.Workers = 2
	})
	rec := &realEncode{enc: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}}
	eng.Enc = rec
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	wa, wb := rec.outFor(t, srcA), rec.outFor(t, srcB)
	if wa == wb {
		t.Fatalf("both jobs were given the SAME working path %s - two sources sharing a basename collide in scratch", wa)
	}
	if filepath.Dir(wa) != scratch || filepath.Dir(wb) != scratch {
		t.Fatalf("working paths are not both in the scratch directory: %s, %s", wa, wb)
	}

	prober := probe.New(ffmpeg, ffprobe)
	ctx := context.Background()
	for _, tc := range []struct {
		path string
		want float64
	}{{srcA, 2}, {srcB, 10}} {
		if codecOf(t, ffprobe, tc.path) != "hevc" {
			t.Fatalf("%s was not swapped for an hevc encode (codec %q)", tc.path, codecOf(t, ffprobe, tc.path))
		}
		got, ok := prober.VideoProps(ctx, tc.path).DurationSec()
		if !ok {
			t.Fatalf("%s reports no duration", tc.path)
		}
		if diff := got - tc.want; diff > 1 || diff < -1 {
			t.Fatalf("%s is %.2fs, want about %.0fs - a job swapped the OTHER job's working file", tc.path, got, tc.want)
		}
	}
	if got := listDir(t, scratch); len(got) != 0 {
		t.Fatalf("the scratch directory holds %v after both jobs, want nothing", got)
	}
}

// AC-B15: at the moment a job is about to encode, the scratch filesystem's free
// space is below the size of that job's source. That job fails BEFORE any encoder
// output is written, the reason names the scratch path, the free space and the source
// size, the source is untouched, and the scan continues with the remaining files.
//
// The free-space lookup is substituted, because a CI runner cannot fill a filesystem
// on demand and a check that could only be proved by filling one would not be proved
// at all. The figure is fixed between the two fixtures' sizes, so the SAME run is
// both arms of the experiment: the big source is refused and the small one transcodes.
func TestScratch_AJobWhoseSourceWillNotFitFailsBeforeEncodingAndTheScanContinues(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	big := filepath.Join(root, "big.mkv")
	small := filepath.Join(root, "small.mkv")
	mkH264Long(t, ffmpeg, big, "8M")
	mkH264(t, ffmpeg, small, "8M")

	bigSize := probe.FileSize(big)
	smallSize := probe.FileSize(small)
	if !(smallSize < bigSize) {
		t.Fatalf("the fixtures are the wrong way round: small=%d big=%d", smallSize, bigSize)
	}
	free := uint64(bigSize) - 1
	if free < uint64(smallSize) {
		t.Fatalf("the fixture sizes leave no room between them: small=%d big=%d", smallSize, bigSize)
	}

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
	rec := &realEncode{enc: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}}
	eng.Enc = rec
	eng.freeBytes = func(path string) (uint64, error) {
		if path != scratch {
			t.Errorf("the free-space check asked about %s, want the scratch directory %s", path, scratch)
		}
		return free, nil
	}
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	// The big one: refused before the encoder ran.
	rec.mu.Lock()
	_, encoded := rec.outs[big]
	rec.mu.Unlock()
	if encoded {
		t.Fatalf("the encoder was called for a source that will not fit in the scratch filesystem")
	}
	if got := listDir(t, scratch); len(got) != 0 {
		t.Fatalf("the scratch directory holds %v, want nothing - encoder output was written despite the pre-check", got)
	}
	out, status, ok := outcomeFor(t, ts, big)
	if !ok || status != store.Failed {
		t.Fatalf("status = %q (found=%v), want failed", status, ok)
	}
	for _, want := range []string{scratch, fmt.Sprint(free), fmt.Sprint(bigSize)} {
		if !strings.Contains(out.Reason, want) {
			t.Errorf("the reason does not name %q: %q", want, out.Reason)
		}
	}
	if codecOf(t, ffprobe, big) != "h264" {
		t.Fatalf("the refused source was modified (codec %q)", codecOf(t, ffprobe, big))
	}

	// The small one: the scan continued and transcoded it.
	if codecOf(t, ffprobe, small) != "hevc" {
		t.Fatalf("the scan did not continue with the remaining files: %s is still %q",
			small, codecOf(t, ffprobe, small))
	}
}

// AC-B12: a run with a scratch_dir configured discards the working files a prior
// killed run left there, and leaves in place - with the existing warning - anything a
// record-based hold-back or the record-free retained-replacement rule protects.
func TestScratch_TheSweepDiscardsOrphansAndHoldsWhatTheExistingRulesHold(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)

	orphan := scratchWorkPath(scratch, filepath.Join(root, "gone.mkv"), "mkv", 0)
	held := scratchWorkPath(scratch, filepath.Join(root, "recorded.mkv"), "mkv", 0)
	retained := filepath.Join(scratch, "kept."+RetainedMarker+".mkv")
	stranger := filepath.Join(scratch, "someone-elses-file.txt")
	for _, p := range []string{orphan, held, retained, stranger} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
	// A live record naming one of the working paths, which is the first of the two
	// exceptions. Published the way RunOneshot publishes it, so the sweep reads it
	// through exactly the code path a real parked job reaches it by.
	eng.held.Store(&holdBacks{paths: map[string]string{
		resolvedForm(held): "a recorded replacement path",
	}})
	eng.cleanScratch(context.Background())

	if exists(orphan) {
		t.Errorf("a prior run's orphaned working file survived the sweep: %s", orphan)
	}
	if !exists(held) {
		t.Errorf("a working path a live record holds back was DELETED: %s", held)
	}
	if !exists(retained) {
		t.Errorf("a replacement holdfast retained was DELETED from the scratch directory: %s", retained)
	}
	if !exists(stranger) {
		t.Errorf("the sweep removed a file that is not this build's construction at all: %s", stranger)
	}
}

// AC-B13: the copy made beside the source is built by the EXISTING construction, so
// the stale-temp sweep, the hold-back rules and the never-enumerated-as-a-source rule
// all reach it with no additional exception.
//
// The first half is an identity - the path is the one pickTempPath produces - and the
// second drives the existing machinery over a file left at exactly that path, as a
// run killed between the copy and the rename would leave it.
func TestScratch_TheCopyBackTempIsTheExistingConstructionAndTheExistingRulesCoverIt(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
	var copied string
	eng.afterCopyBack = func(p string) error { copied = p; return nil }
	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}
	if copied == "" {
		t.Fatal("no copy was made beside the source")
	}

	want := tempPath(root, "film", "mkv", 0)
	if copied != want {
		t.Fatalf("the copy went to %s, want the EXISTING temp construction %s - a second construction would need "+
			"a second set of sweep and hold-back rules", copied, want)
	}
	base := filepath.Base(copied)
	if !IsTempConstructionName(base) {
		t.Errorf("%s is not a name this build's temp construction could have produced, so the record-free hold cannot reach it", base)
	}
	if !isTempName(base) {
		t.Errorf("%s is not a name the stale-temp sweep looks at", base)
	}
	if IsSourceName(base, eng.Cfg.VideoExts) {
		t.Errorf("%s would be ENUMERATED as a source", base)
	}

	// And the existing rules, driven over a file left at exactly that path. A
	// FINISHED encode beside its source is held (it may be the only faithful copy of
	// a source whose fate is unknown); a truncated one is swept. Both are the
	// in-place rules, unchanged, reached with no new exception.
	ctx := context.Background()
	t.Run("a finished copy left behind is held", func(t *testing.T) {
		d2, _ := scratchDirs(t)
		s2 := filepath.Join(d2, "film.mkv")
		mkH264(t, ffmpeg, s2, "8M")
		leftover := tempPath(d2, "film", "mkv", 0)
		ff(t, ffmpeg, "-hide_banner", "-loglevel", "error", "-y", "-i", s2,
			"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "log-level=error",
			"-crf", "35", "-pix_fmt", "yuv420p", "--", leftover)

		e2 := buildEngine(t, ffmpeg, ffprobe, d2, nil, nil)
		e2.held.Store(e2.loadHoldBacks(ctx))
		e2.cleanStaleTemps(ctx)
		if !exists(leftover) {
			t.Fatalf("a FINISHED copy left beside its source was swept: %s", leftover)
		}
	})
	t.Run("a truncated copy left behind is swept", func(t *testing.T) {
		d3, _ := scratchDirs(t)
		s3 := filepath.Join(d3, "film.mkv")
		mkH264(t, ffmpeg, s3, "8M")
		leftover := tempPath(d3, "film", "mkv", 0)
		if err := os.WriteFile(leftover, []byte("half an encode"), 0o644); err != nil {
			t.Fatal(err)
		}
		e3 := buildEngine(t, ffmpeg, ffprobe, d3, nil, nil)
		e3.held.Store(e3.loadHoldBacks(ctx))
		e3.cleanStaleTemps(ctx)
		if exists(leftover) {
			t.Fatalf("a truncated copy left beside its source was NOT swept: %s", leftover)
		}
	})
}

// scratchWorkPath must be a pure function of the SOURCE PATH, so the same source
// resolves to the same working file on every worker and in every run, and two sources
// sharing a basename never resolve to the same one.
func TestScratchWorkPath_IsStablePerSourceAndDistinctAcrossDirectories(t *testing.T) {
	a := scratchWorkPath("/scratch", "/media/showA/film.mkv", "mkv", 0)
	b := scratchWorkPath("/scratch", "/media/showB/film.mkv", "mkv", 0)
	if a == b {
		t.Fatalf("two sources sharing a basename resolve to one working path: %s", a)
	}
	if again := scratchWorkPath("/scratch", "/media/showA/film.mkv", "mkv", 0); again != a {
		t.Fatalf("the same source resolved to %s and then %s", a, again)
	}
	if filepath.Dir(a) != "/scratch" {
		t.Fatalf("the working path is not in the scratch directory: %s", a)
	}
	if !IsTempConstructionName(filepath.Base(a)) {
		t.Fatalf("%s is not a name this build's temp construction could have produced", filepath.Base(a))
	}

	// NAME_MAX is 255 bytes on Linux, and a source basename may already be near it.
	// A construction that simply appended would produce ENAMETOOLONG on exactly the
	// files whose names are longest.
	long := strings.Repeat("x", 250) + ".mkv"
	p := scratchWorkPath("/scratch", "/media/"+long, "mkv", 0)
	if len(filepath.Base(p)) > 255 {
		t.Fatalf("a long source basename produced a %d-byte working name", len(filepath.Base(p)))
	}
	if !IsTempConstructionName(filepath.Base(p)) {
		t.Fatalf("a truncated working name is no longer this build's construction: %s", filepath.Base(p))
	}
}
