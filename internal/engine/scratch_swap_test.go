package engine

import (
	"context"
	"errors"
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

// The swap, with a scratch directory configured. Everything here is about the one
// rule the whole feature was designed around: the finalizing rename ALWAYS reads a
// path in the source's own directory, and nothing is ever renamed or moved out of the
// scratch directory onto a source or into its directory - whether or not the two share
// a filesystem.

// renameLog records every rename the swap attempts, so a criterion about WHAT was
// renamed FROM WHERE is graded on the calls themselves rather than on the wreckage.
type renameLog struct {
	mu   sync.Mutex
	from []string
	to   []string
	// scratchPresentAt records, per call, whether the scratch working file was still
	// on disk at the moment of the rename. A swap that had moved it out would find
	// it gone.
	scratchPresentAt []bool
}

func (l *renameLog) record(old, new string, scratchWork string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.from = append(l.from, old)
	l.to = append(l.to, new)
	l.scratchPresentAt = append(l.scratchPresentAt, scratchWork != "" && exists(scratchWork))
}

func (l *renameLog) calls() ([]string, []string, []bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.from...), append([]string(nil), l.to...), append([]bool(nil), l.scratchPresentAt...)
}

// AC-B3: with a scratch_dir configured, the swap renames onto the source a path in
// the SOURCE's own directory, and never calls rename (or any move) on a path under
// the scratch directory with a destination under the source's directory - whether or
// not the two are on the same filesystem.
func TestScratchSwap_TheRenameAlwaysReadsAPathInTheSourcesOwnDirectory(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng := buildEngine(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
	rec := &realEncode{enc: FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}}
	eng.Enc = rec

	var log renameLog
	var work string
	eng.afterCopyBack = func(string) error { return nil }
	eng.renameFn = func(oldpath, newpath string) error {
		log.record(oldpath, newpath, work)
		return os.Rename(oldpath, newpath)
	}
	// The working path is only known once the encoder has been asked for it, and the
	// rename happens after that, so capturing it here is early enough.
	rec.before = func(in, out string) { work = out }

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	from, to, present := log.calls()
	if len(from) != 1 {
		t.Fatalf("the swap made %d rename(s), want exactly 1: %v -> %v", len(from), from, to)
	}
	if filepath.Dir(from[0]) != root {
		t.Fatalf("the swap renamed %s, which is not in the source's own directory %s", from[0], root)
	}
	if strings.HasPrefix(from[0], scratch+string(filepath.Separator)) {
		t.Fatalf("the swap renamed a path UNDER THE SCRATCH DIRECTORY onto the source: %s -> %s", from[0], to[0])
	}
	if to[0] != src {
		t.Fatalf("the swap renamed onto %s, want the source %s", to[0], src)
	}
	if !present[0] {
		t.Fatalf("the scratch working file %s was gone at the moment of the rename - it was MOVED rather than copied", work)
	}
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatalf("the source was not replaced by the encode")
	}
	if got := listDir(t, scratch); len(got) != 0 {
		t.Fatalf("the scratch directory holds %v after the job, want nothing", got)
	}
}

// AC-B4, first limb: with the scratch directory on a DIFFERENT filesystem from the
// source, the job completes by copying the accepted result into the source's
// directory, making it durable, and then performing the existing atomic rename.
//
// The boundary is injected rather than mounted, and that is not a shortcut: a CI
// runner cannot conjure a second real filesystem, and this repository already carries
// three seams for exactly that reason (see the Engine's FILESYSTEM-1 seams). The
// injection is the SHARP form of the criterion: any rename whose source is under the
// scratch directory fails with EXDEV, the error rename(2) gives across a mount
// boundary - so an implementation that tried to rename out of scratch could not
// possibly pass this, while the one that copies back never attempts it.
func TestScratchSwap_ACrossFilesystemScratchCompletesByCopyingBackAndRenamingBesideTheSource(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })

	var fsyncs []string
	var fmu sync.Mutex
	eng.fsyncPath = func(p string) error {
		fmu.Lock()
		fsyncs = append(fsyncs, p)
		fmu.Unlock()
		return fsyncPath(p)
	}
	var copied string
	eng.afterCopyBack = func(p string) error { copied = p; return nil }
	eng.renameFn = func(oldpath, newpath string) error {
		if strings.HasPrefix(oldpath, scratch+string(filepath.Separator)) {
			// The scratch directory is on another filesystem: rename(2) across the
			// boundary is EXDEV, and holdfast reports that cause rather than working
			// around it.
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EXDEV}
		}
		return os.Rename(oldpath, newpath)
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if _, status, ok := outcomeFor(t, ts, src); !ok || status != store.Done {
		t.Fatalf("status = %q (found=%v), want done - a scratch on another filesystem must still complete", status, ok)
	}
	if codecOf(t, ffprobe, src) != "hevc" {
		t.Fatalf("the source was not replaced")
	}
	if copied == "" {
		t.Fatal("no copy was made beside the source")
	}
	if filepath.Dir(copied) != root {
		t.Fatalf("the copy went to %s, not into the source's own directory %s", copied, root)
	}
	fmu.Lock()
	sawCopyFsync := false
	for _, p := range fsyncs {
		if p == copied {
			sawCopyFsync = true
		}
	}
	fmu.Unlock()
	if !sawCopyFsync {
		t.Fatalf("the copy beside the source was never made durable: fsyncs were %v", fsyncs)
	}
	if got := listDir(t, scratch); len(got) != 0 {
		t.Fatalf("the scratch directory holds %v after the job, want nothing", got)
	}
}

// AC-B4, second limb: the source's fingerprint moving during the encode still refuses
// the swap, with today's reason and the source left untouched - on the scratch path
// exactly as on the in-place one.
//
// The mtime is moved and the CONTENT is not, deliberately: every acceptance gate
// compares the output against the source's content, so a fixture whose content
// changed would be rejected by a gate and would prove nothing about the guard that
// runs after them.
func TestScratchSwap_TheSourceRefingerprintGuardStillRefusesOnTheScratchPath(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
	real := FFmpegEncoder{FFmpeg: ffmpeg, Cfg: eng.Cfg, Probe: eng.Probe}
	eng.Enc = EncoderFunc(func(ctx context.Context, in, out string, props *probe.VideoProps) error {
		if err := real.Encode(ctx, in, out, props); err != nil {
			return err
		}
		// Something else rewrote the source while holdfast was encoding. Same bytes,
		// new mtime - which is exactly what the guard compares.
		moved := time.Now().Add(2 * time.Hour)
		return os.Chtimes(in, moved, moved)
	})
	var renames int
	eng.renameFn = func(oldpath, newpath string) error {
		renames++
		return os.Rename(oldpath, newpath)
	}

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	if renames != 0 {
		t.Fatalf("the swap renamed %d time(s) after the source's fingerprint moved", renames)
	}
	out, status, ok := outcomeFor(t, ts, src)
	if !ok || status != store.Failed {
		t.Fatalf("status = %q (found=%v), want failed", status, ok)
	}
	if !strings.Contains(out.Reason, "source changed during encode") {
		t.Fatalf("the reason is not today's: %q", out.Reason)
	}
	if md5f(t, src) != before {
		t.Fatalf("the source's bytes changed")
	}
	if got := listDir(t, root); len(got) != 1 || got[0] != "film.mkv" {
		t.Fatalf("the source's directory holds %v, want just the source", got)
	}
	if got := listDir(t, scratch); len(got) != 0 {
		t.Fatalf("the scratch directory holds %v, want nothing", got)
	}
}

// AC-B4, third limb: the failed-swap machinery is reached UNCHANGED from the scratch
// path. The rename fails on storage this run positively classifies as local and the
// re-stat finds the source's own pre-swap attributes, which is case (b) - the one
// outcome allowed to say "untouched", and the one that proves both pre-swap attribute
// records were taken, because the decision is made by comparing against them.
func TestScratchSwap_AFailedRenameStillReachesTheFourCaseDecision(t *testing.T) {
	ffmpeg, ffprobe := tools(t)
	root, scratch := scratchDirs(t)
	src := filepath.Join(root, "film.mkv")
	mkH264(t, ffmpeg, src, "8M")
	before := md5f(t, src)

	eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
	// Case (b) is the only outcome allowed to say "untouched", and it needs a
	// POSITIVE local identification - which the gate's own temp directory is not
	// guaranteed to give. The type lookup is substituted for the same reason the rest
	// of this suite substitutes it: the decision under test is the four-case one, not
	// what the runner's /tmp happens to be mounted as.
	eng.fsLookup = lookups("ext4")
	eng.renameFn = func(oldpath, newpath string) error { return errors.New("injected swap failure") }

	if err := eng.RunOneshot(context.Background()); err != nil {
		t.Fatalf("RunOneshot: %v", err)
	}

	out, status, ok := outcomeFor(t, ts, src)
	if !ok || status != store.Failed {
		t.Fatalf("status = %q (found=%v), want failed (case b: the source is established untouched)", status, ok)
	}
	if !strings.Contains(out.Reason, "the source is untouched") {
		t.Fatalf("the four-case decision did not reach case (b): %q", out.Reason)
	}
	if md5f(t, src) != before {
		t.Fatalf("the source's bytes changed")
	}
	if got := listDir(t, root); len(got) != 1 || got[0] != "film.mkv" {
		t.Fatalf("the source's directory holds %v after a failed swap on the scratch path, want just the source", got)
	}
}

// AC-B5: if the file in the source's directory does not carry exactly the bytes that
// passed the acceptance gates, there is NO rename, the job records a failure whose
// reason names the mismatch, the source is byte-for-byte unchanged, and nothing this
// tool wrote is left at an ordinary media name.
//
// A copy breaks the identity the whole no-loss argument rests on - that the file the
// gates accepted IS the file the swap renames - so the identity has to be
// re-established, and a check that cannot be shown to fail is not a check. The seam
// puts different bytes at that path at the one instant where nothing else could:
// after the copy has been made durable, before the identity is re-established.
func TestScratchSwap_ACopyThatIsNotTheAcceptedBytesRefusesTheSwap(t *testing.T) {
	ffmpeg, ffprobe := tools(t)

	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, path string)
	}{
		{
			name: "a byte appended",
			mutate: func(t *testing.T, path string) {
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Write([]byte{0}); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			},
		},
		{
			name: "a byte changed in place, same length",
			mutate: func(t *testing.T, path string) {
				f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
				if err != nil {
					t.Fatal(err)
				}
				// Well past any header, so the file is still structurally a video and
				// only its CONTENT differs from what the gates measured.
				if _, err := f.WriteAt([]byte{0xAA}, 512); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, scratch := scratchDirs(t)
			src := filepath.Join(root, "film.mkv")
			mkH264(t, ffmpeg, src, "8M")
			before := md5f(t, src)

			eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
			eng.afterCopyBack = func(p string) error { tc.mutate(t, p); return nil }
			var renames int
			eng.renameFn = func(oldpath, newpath string) error {
				renames++
				return os.Rename(oldpath, newpath)
			}

			if err := eng.RunOneshot(context.Background()); err != nil {
				t.Fatalf("RunOneshot: %v", err)
			}

			if renames != 0 {
				t.Fatalf("the swap renamed %d time(s) a file that is not the bytes the gates accepted", renames)
			}
			out, status, ok := outcomeFor(t, ts, src)
			if !ok || status != store.Failed {
				t.Fatalf("status = %q (found=%v), want failed", status, ok)
			}
			if !strings.Contains(out.Reason, "NOT the file the acceptance gates passed") {
				t.Fatalf("the reason does not name the mismatch: %q", out.Reason)
			}
			if !strings.Contains(out.Reason, "sha256") {
				t.Fatalf("the reason does not report what was compared: %q", out.Reason)
			}
			if md5f(t, src) != before {
				t.Fatalf("the source's bytes changed")
			}
			// Nothing at an ordinary media name. The source is one, and it is the only
			// thing in the directory; anything holdfast wrote is gone.
			got := listDir(t, root)
			if len(got) != 1 || got[0] != "film.mkv" {
				t.Fatalf("the source's directory holds %v, want just the source - a file holdfast wrote survived", got)
			}
			for _, name := range got {
				if IsSourceName(name, eng.Cfg.VideoExts) && name != "film.mkv" {
					t.Fatalf("holdfast left %s at an ordinary media name after refusing the swap", name)
				}
			}
			if got := listDir(t, scratch); len(got) != 0 {
				t.Fatalf("the scratch directory holds %v, want nothing", got)
			}
		})
	}

	// The control arm. Without the mutation the identical run SWAPS - so the two
	// failures above are about the bytes and not about the seam existing.
	t.Run("control: the unmutated copy is accepted and swapped", func(t *testing.T) {
		root, scratch := scratchDirs(t)
		src := filepath.Join(root, "film.mkv")
		mkH264(t, ffmpeg, src, "8M")

		eng, ts := buildEngineAndStore(t, ffmpeg, ffprobe, root, nil, func(c *config.Config) { c.ScratchDir = scratch })
		eng.afterCopyBack = func(string) error { return nil }
		if err := eng.RunOneshot(context.Background()); err != nil {
			t.Fatalf("RunOneshot: %v", err)
		}
		if _, status, ok := outcomeFor(t, ts, src); !ok || status != store.Done {
			t.Fatalf("status = %q (found=%v), want done", status, ok)
		}
		if codecOf(t, ffprobe, src) != "hevc" {
			t.Fatalf("the source was not replaced")
		}
	})
}
