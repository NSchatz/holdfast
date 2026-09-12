package engine

// S0085, the half of the contract that needs a substitution.
//
// This gate runs as an unprivileged uid with no CAP_CHOWN, so a real chown(2) to somebody
// else's uid is not exercisable here and never will be: a test that needed one would be
// skipped, and for a data-safety proof a skip is a false green. The four seams below are
// the same pattern the engine already carries for exactly this reason (fsyncPath,
// renameFn, restatFn, hookAfterRetain), and each one is here because of a question no
// unprivileged process on ordinary local storage can otherwise ask:
//
//	chownFn         what uid and gid the swap REQUESTS (AC3), and what it does when the
//	                kernel answers EPERM (AC4) - which it will not do for a chown to the
//	                uid this process already is.
//	chmodFn         a mode that cannot be applied (AC8). A process that owns the file can
//	                always chmod it, so the failure has to be injected.
//	chtimesFn       a modification time that cannot be applied (AC8), same reason.
//	statMetadataFn  a source whose metadata cannot be READ (AC8) while the source is still
//	                there to be asserted byte-for-byte intact afterwards.
//
// Every other fact here is real: a real ffmpeg encode, the real verify gate, the real
// store, real files, and the real rename. In particular the rename is NEVER substituted in
// this file, because "did the swap happen" is exactly what these criteria are about and a
// substituted rename would be the test answering its own question.

import (
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
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// metadataEngine builds an engine over root with a config mutation and a logger of the
// caller's choosing. buildEngineWithStore takes neither, and both are needed here: the
// mtime criteria turn on a config key and the once-per-run criterion is about what was
// REPORTED, which is the one thing in S0085 that is not readable from a file.
func metadataEngine(t *testing.T, ffmpeg, ffprobe, root string, log *slog.Logger, mutate func(*config.Config)) (*Engine, *testStore) {
	t.Helper()
	cfg := baseCfg(root)
	if mutate != nil {
		mutate(&cfg)
	}
	ts := newTestStore(t, root)
	prober := probe.New(ffmpeg, ffprobe)
	if log == nil {
		log = discardLogger()
	}
	return New(cfg, prober, FFmpegEncoder{FFmpeg: ffmpeg, Cfg: cfg, Probe: prober}, ts, log), ts
}

// chownRequest is one ownership change the swap asked for.
type chownRequest struct {
	path     string
	uid, gid int
}

// recordingChown records every ownership request AND performs the real one, so the test
// asserts both what was asked for and what the file ended up with. err, when non-nil, is
// returned INSTEAD of performing it - which is how the kernel's "you may not" is put in
// front of a process that would in fact be allowed.
func recordingChown(err error, out *[]chownRequest, mu *sync.Mutex) func(string, int, int) error {
	return func(path string, uid, gid int) error {
		mu.Lock()
		*out = append(*out, chownRequest{path: path, uid: uid, gid: gid})
		mu.Unlock()
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	}
}

// ---- AC3: the ownership the swap REQUESTS ------------------------------------

// TestSwap_ReplacementCarriesTheSourcesOwnerWhenPermitted is AC3. The assertion that
// carries it is the REQUEST: the uid and gid handed to the ownership change are the
// source's, on the replacement, exactly once. The on-disk owner is asserted too, from a
// chown this process really performed.
//
// RED at the pin: nothing on the swap path changes a file's owner at all, so no request
// is ever made.
func TestSwap_ReplacementCarriesTheSourcesOwnerWhenPermitted(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	wantUID, wantGID := statOwner(t, src)

	var mu sync.Mutex
	var got []chownRequest
	eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, nil, nil)
	eng.chownFn = recordingChown(nil, &got, &mu)
	runOneshot(t, eng)

	assertSwapped(t, ts, ffprobe, src)

	if len(got) != 1 {
		t.Fatalf("the swap made %d ownership request(s), want exactly 1: %+v", len(got), got)
	}
	if got[0].uid != wantUID || got[0].gid != wantGID {
		t.Errorf("the swap requested owner %d:%d, want the source's %d:%d",
			got[0].uid, got[0].gid, wantUID, wantGID)
	}
	if base := filepath.Base(got[0].path); !IsTempConstructionName(base) {
		t.Errorf("the ownership was requested on %s, which is not the replacement this build wrote - "+
			"the source's own ownership is never holdfast's to change", got[0].path)
	}
	if uid, gid := statOwner(t, src); uid != wantUID || gid != wantGID {
		t.Errorf("the published replacement is owned by %d:%d, want the source's %d:%d", uid, gid, wantUID, wantGID)
	}
}

// ---- AC4: the rootless case --------------------------------------------------

// TestSwap_OwnershipNotPermittedProceedsAndIsLoggedOncePerRun is AC4, and it is the
// criterion that decides whether this feature is usable at all: the shipped deployment is
// a container running as an ordinary `user:` with no CAP_CHOWN, so EPERM from chown is the
// NORMAL case and not an error condition. The swap must still happen, the rest of the
// metadata must still be carried, and the operator must be told - once for the run, not
// once per file, because a library-wide first pass would otherwise bury the log under one
// identical line per film.
//
// Two files, so "once per run" is measurable at all.
func TestSwap_OwnershipNotPermittedProceedsAndIsLoggedOncePerRun(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	first := filepath.Join(d, "first.mkv")
	second := filepath.Join(d, "second.mkv")
	mkH264(t, ffmpeg, first, "8M")
	mkH264(t, ffmpeg, second, "8M")

	wantMtime := aFixedPastTime()
	for _, f := range []string{first, second} {
		if err := os.Chmod(f, 0o640); err != nil {
			t.Fatalf("chmod %s: %v", f, err)
		}
		setModTime(t, f, wantMtime)
	}
	hostileUmask(t)

	log := &capturedLog{}
	var mu sync.Mutex
	var got []chownRequest
	eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, log.logger(), nil)
	// EPERM is what the kernel answers a process without CAP_CHOWN. It is the ONE
	// ownership error that is not a failed swap.
	eng.chownFn = recordingChown(syscall.EPERM, &got, &mu)
	runOneshot(t, eng)

	// Both swaps happened, with the rest of the metadata carried.
	for _, f := range []string{first, second} {
		assertSwapped(t, ts, ffprobe, f)
		if mode := modeOf(t, f).Perm(); mode != 0o640 {
			t.Errorf("%s is at mode %04o after an unprivileged run, want the source's 0640 - "+
				"an ownership this process may not change must not cost the mode as well", f, mode)
		}
		if mt := mtimeOf(t, f); !mt.Equal(wantMtime) {
			t.Errorf("%s carries mtime %s after an unprivileged run, want the source's %s", f, mt, wantMtime)
		}
	}
	if len(got) != 2 {
		t.Errorf("the run made %d ownership request(s) over 2 files, want one per swap: %+v", len(got), got)
	}

	if n := strings.Count(log.String(), ownershipNotPreservedNotice); n != 1 {
		t.Errorf("the unpreserved ownership was reported %d time(s) over a 2-file run, want EXACTLY 1.\n"+
			"log:\n%s", n, log.String())
	}
}

// ---- AC6: the modification time, both ways -----------------------------------

// TestSwap_PreserveMtimeCarriesTheSourcesModTime is AC6 in both directions. The default is
// ON (the conductor ruling of 2026-09-11), so the first subtest is also what an operator
// who has never heard of the key gets.
//
// RED at the pin: there is no key, and the replacement always carries the encode's clock.
func TestSwap_PreserveMtimeCarriesTheSourcesModTime(t *testing.T) {
	t.Run("on: the replacement carries the source's modification time", func(t *testing.T) {
		ffmpeg, ffprobe := tools(t)
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")
		want := aFixedPastTime()
		setModTime(t, src, want)

		eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
			c.PreserveMtime = boolPtr(true)
		})
		runOneshot(t, eng)
		assertSwapped(t, ts, ffprobe, src)

		if got := mtimeOf(t, src); !got.Equal(want) {
			t.Errorf("the replacement carries mtime %s, want the source's %s", got, want)
		}
	})

	t.Run("absent: the key defaults ON", func(t *testing.T) {
		ffmpeg, ffprobe := tools(t)
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")
		want := aFixedPastTime()
		setModTime(t, src, want)

		// No mutation at all: the value a configuration that never names the key resolves
		// to. Preserving is the least-surprising behaviour (cp -p, rsync -a, mv all do it)
		// and RESETTING the mtime is the side effect.
		eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, nil, nil)
		runOneshot(t, eng)
		assertSwapped(t, ts, ffprobe, src)

		if got := mtimeOf(t, src); !got.Equal(want) {
			t.Errorf("with preserve_mtime unset the replacement carries mtime %s, want the source's %s - "+
				"the shipped default must preserve it", got, want)
		}
	})

	t.Run("off: the replacement keeps the modification time the encode wrote", func(t *testing.T) {
		ffmpeg, ffprobe := tools(t)
		d := t.TempDir()
		src := filepath.Join(d, "movie.mkv")
		mkH264(t, ffmpeg, src, "8M")
		sourceMtime := aFixedPastTime()
		setModTime(t, src, sourceMtime)
		before := time.Now().Add(-2 * time.Second)

		eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
			c.PreserveMtime = boolPtr(false)
		})
		runOneshot(t, eng)
		assertSwapped(t, ts, ffprobe, src)

		got := mtimeOf(t, src)
		if got.Equal(sourceMtime) {
			t.Errorf("preserve_mtime: false still took the source's mtime (%s) - an operator who turns the key "+
				"off is asking for the mtime to say when the bytes were written", got)
		}
		if got.Before(before) {
			t.Errorf("with preserve_mtime off the replacement carries mtime %s, which predates the run (%s) - "+
				"it should be the one the encode wrote", got, before)
		}
	})
}

// ---- AC7: the preserved mtime is still a fresh identity ----------------------

// TestSwap_PreserveMtimeStillProducesAFreshFingerprint is AC7, and it is what makes the ON
// default safe rather than a resume bug.
//
// probe.Fingerprint is size:mtime. With the mtime carried across the swap, only the SIZE
// half moves - so the whole of "the post-swap key is a fresh identity" now rests on the
// size, and the size is guaranteed to move because the verify gate refuses an output that
// is not strictly smaller. If that ever stopped holding, the done row would key under the
// SOURCE's identity and the next scan would re-encode the replacement as if it were the
// untouched source. This test reds the moment either half of that stops being true.
func TestSwap_PreserveMtimeStillProducesAFreshFingerprint(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	setModTime(t, src, aFixedPastTime())

	preKey := probe.Fingerprint(src)
	preAttrs, err := probe.StatAttributes(src)
	if err != nil {
		t.Fatalf("StatAttributes(source): %v", err)
	}

	eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.PreserveMtime = boolPtr(true)
	})
	runOneshot(t, eng)
	assertSwapped(t, ts, ffprobe, src)

	postKey := probe.Fingerprint(src)
	postAttrs, err := probe.StatAttributes(src)
	if err != nil {
		t.Fatalf("StatAttributes(replacement): %v", err)
	}

	if postKey == preKey {
		t.Fatalf("the post-swap fingerprint (%s) equals the pre-swap one - the done row would key under the "+
			"SOURCE's identity and the next scan would re-encode the replacement as the untouched source", postKey)
	}
	// The load-bearing half, asserted rather than assumed: the mtime did NOT move, so the
	// size is the whole of the difference.
	if postAttrs.MTimeUnix != preAttrs.MTimeUnix {
		t.Errorf("the mtime moved across the swap (%d -> %d) - this test is no longer measuring what it says it is",
			preAttrs.MTimeUnix, postAttrs.MTimeUnix)
	}
	if postAttrs.SizeBytes >= preAttrs.SizeBytes {
		t.Errorf("the replacement is %d bytes against the source's %d - with the mtime carried, a size that did "+
			"not shrink is a post-swap key equal to the pre-swap one", postAttrs.SizeBytes, preAttrs.SizeBytes)
	}

	// The terminal row is keyed under the POST-swap fingerprint, and the superseded
	// pre-swap row is gone.
	row, ok := jobRow(t, ts, src, postKey)
	if !ok {
		t.Fatalf("no row keyed under the post-swap fingerprint %s", postKey)
	}
	if row.Status != store.Done {
		t.Errorf("the row under the post-swap fingerprint is %s, want done", row.Status)
	}
	if _, stale := jobRow(t, ts, src, preKey); stale {
		t.Errorf("the superseded pre-swap row (%s) is still in the ledger", preKey)
	}
}

// ---- AC8: a metadata failure is a FAILED swap, source intact -----------------

// TestSwap_MetadataFailureFailsTheSwapWithTheSourceUntouched is AC8, and it is the
// criterion that keeps this change inside the safety envelope it sits in. The metadata
// steps run immediately before the only irreversible act this tool performs, so a step
// that half-applies and then swaps anyway publishes a file whose permissions or owner
// nobody chose - with the source already gone. Every metadata failure that is not "this
// process may not change an owner" is therefore a failed swap: no rename, temp discarded,
// source byte-for-byte intact, and a recorded reason naming the step that failed.
//
// The two permission errors in the table are deliberate. EACCES from chown is a path
// problem, not a privilege one, and must NOT take the AC4 exemption; EPERM from utimes
// means "not the owner" and must not take it either, because the exemption is scoped to
// the OWNERSHIP step alone.
func TestSwap_MetadataFailureFailsTheSwapWithTheSourceUntouched(t *testing.T) {
	cases := []struct {
		name   string
		inject func(*Engine)
		step   string
	}{
		{
			name: "the source's metadata cannot be read",
			inject: func(e *Engine) {
				e.statMetadataFn = func(string) (fileMetadata, error) {
					return fileMetadata{}, errors.New("simulated stat failure")
				}
			},
			step: metadataStepRead,
		},
		{
			name: "the ownership fails for a reason that is not a lack of privilege",
			inject: func(e *Engine) {
				e.chownFn = func(string, int, int) error { return syscall.EACCES }
			},
			step: metadataStepOwner,
		},
		{
			name: "the mode cannot be applied",
			inject: func(e *Engine) {
				e.chmodFn = func(string, os.FileMode) error { return syscall.EROFS }
			},
			step: metadataStepMode,
		},
		{
			name: "the modification time cannot be applied",
			inject: func(e *Engine) {
				e.chtimesFn = func(string, time.Time) error { return syscall.EPERM }
			},
			step: metadataStepModTime,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ffmpeg, ffprobe := tools(t)
			d := t.TempDir()
			src := filepath.Join(d, "movie.mkv")
			mkH264(t, ffmpeg, src, "8M")
			wantMD5 := md5f(t, src)
			wantMode := modeOf(t, src)
			wantMtime := mtimeOf(t, src)
			wantCodec := codecOf(t, ffprobe, src)

			eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, nil, nil)
			c.inject(eng)
			runOneshot(t, eng)

			// The source, byte for byte.
			if !exists(src) {
				t.Fatal("the source is GONE after a metadata failure")
			}
			if got := md5f(t, src); got != wantMD5 {
				t.Fatal("the source was modified after a metadata failure")
			}
			if got := codecOf(t, ffprobe, src); got != wantCodec {
				t.Fatalf("the file at the source path is %s, not the source's %s - the swap happened anyway", got, wantCodec)
			}
			if got := modeOf(t, src); got != wantMode {
				t.Errorf("the source's own mode moved (%04o -> %04o) - the source is never holdfast's to change",
					wantMode.Perm(), got.Perm())
			}
			if got := mtimeOf(t, src); !got.Equal(wantMtime) {
				t.Errorf("the source's own mtime moved (%s -> %s)", wantMtime, got)
			}

			// Nothing half-published, and nothing left behind.
			if n := nTemp(t, d); n != 0 {
				t.Errorf("%d temp file(s) survived a metadata failure: %v", n, lsDir(t, d))
			}
			if files := retainedFiles(t, d); len(files) != 0 {
				t.Errorf("a metadata failure retained a replacement: %v", files)
			}

			// The record, naming the step.
			row := rowFor(t, ts, src)
			if row.Status != store.Failed {
				t.Fatalf("the row is %s, want failed: %+v", row.Status, row)
			}
			if !strings.Contains(row.Outcome.Reason, c.step) {
				t.Errorf("the recorded reason does not name the metadata step that failed (%q): %q",
					c.step, row.Outcome.Reason)
			}
		})
	}
}

// TestSwap_MetadataFailureOnTheContainerChangingShapeAlsoLeavesBothFilesAlone is AC8 on
// the OTHER swap shape. It is separate because the shapes fail differently: an in-place
// failure is "the file at the source path is still the source", but an ext-changing one is
// also "nothing was published at the target", and a swap that half-applied metadata and
// then renamed would leave a file there under an ordinary media name with the source
// deleted behind it.
func TestSwap_MetadataFailureOnTheContainerChangingShapeAlsoLeavesBothFilesAlone(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mp4")
	final := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")
	wantMD5 := md5f(t, src)

	eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, nil, nil)
	eng.chmodFn = func(string, os.FileMode) error { return syscall.EROFS }
	runOneshot(t, eng)

	if !exists(src) {
		t.Fatal("the source is GONE after a metadata failure on the ext-changing shape")
	}
	if got := md5f(t, src); got != wantMD5 {
		t.Fatal("the source was modified after a metadata failure on the ext-changing shape")
	}
	if exists(final) {
		t.Fatalf("a replacement was published at %s after a metadata failure: %v", final, lsDir(t, d))
	}
	if n := nTemp(t, d); n != 0 {
		t.Errorf("%d temp file(s) survived: %v", n, lsDir(t, d))
	}
	row := rowFor(t, ts, src)
	if row.Status != store.Failed {
		t.Fatalf("the row is %s, want failed: %+v", row.Status, row)
	}
	if !strings.Contains(row.Outcome.Reason, metadataStepMode) {
		t.Errorf("the recorded reason does not name the metadata step that failed (%q): %q",
			metadataStepMode, row.Outcome.Reason)
	}
}

// ---- the ordering the Constraints section pins -------------------------------

// TestSwap_MetadataIsAppliedBeforeTheDurabilityBarrierAndOutsideTheRefingerprintWindow is
// the Constraints section, asserted rather than asserted-in-prose. Two properties, one
// fixture:
//
//   - the metadata is applied BEFORE the temp's fsync, so the attributes the rename
//     publishes are as durable as the bytes it publishes;
//   - nothing new sits between the TRANSCODE-16 source re-fingerprint and the rename
//     syscall, whose whole promise is that its TOCTOU window is the microseconds between
//     those two. A metadata syscall inside it would widen the window this repository spent
//     a phase narrowing.
//
// The re-fingerprint is observed through the one seam that can see it: hookAfterRetain
// runs immediately after the retention and immediately BEFORE the re-fingerprint, so
// anything recorded after that mark and before the rename is inside the window.
func TestSwap_MetadataIsAppliedBeforeTheDurabilityBarrierAndOutsideTheRefingerprintWindow(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	d := t.TempDir()
	src := filepath.Join(d, "movie.mkv")
	mkH264(t, ffmpeg, src, "8M")

	var mu sync.Mutex
	var seq []string
	mark := func(what string) {
		mu.Lock()
		seq = append(seq, what)
		mu.Unlock()
	}

	eng, ts := metadataEngine(t, ffmpeg, ffprobe, d, nil, func(c *config.Config) {
		c.UndoWindowHours = 1 // so hookAfterRetain is reached at all
	})
	eng.chownFn = func(path string, uid, gid int) error { mark("chown"); return os.Chown(path, uid, gid) }
	eng.chmodFn = func(path string, mode os.FileMode) error { mark("chmod"); return os.Chmod(path, mode) }
	eng.chtimesFn = func(path string, mt time.Time) error {
		mark("chtimes")
		return os.Chtimes(path, time.Time{}, mt)
	}
	eng.fsyncPath = func(path string) error {
		if IsTempConstructionName(filepath.Base(path)) {
			mark("fsync-temp")
		}
		return fsyncPath(path)
	}
	eng.hookAfterRetain = func(string) error { mark("refingerprint-window-opens"); return nil }
	eng.renameFn = func(oldpath, newpath string) error { mark("rename"); return os.Rename(oldpath, newpath) }
	runOneshot(t, eng)
	assertSwapped(t, ts, ffprobe, src)

	order := strings.Join(seq, " ")
	for _, step := range []string{"chown", "chmod", "chtimes"} {
		if indexOf(seq, step) < 0 {
			t.Fatalf("the swap never performed %s: %s", step, order)
		}
		if indexOf(seq, step) > indexOf(seq, "fsync-temp") {
			t.Errorf("%s ran AFTER the temp's durability barrier, so the attributes the rename publishes are "+
				"not as durable as the bytes: %s", step, order)
		}
		if indexOf(seq, step) > indexOf(seq, "refingerprint-window-opens") {
			t.Errorf("%s ran inside the TRANSCODE-16 re-fingerprint -> rename window, widening the TOCTOU this "+
				"repository spent a phase narrowing: %s", step, order)
		}
	}
	if indexOf(seq, "rename") != len(seq)-1 {
		t.Errorf("the rename is not the last thing the swap did: %s", order)
	}
}

func indexOf(seq []string, want string) int {
	for i, s := range seq {
		if s == want {
			return i
		}
	}
	return -1
}
