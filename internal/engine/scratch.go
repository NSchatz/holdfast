package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/NSchatz/holdfast/internal/diskfree"
)

// The configurable working location (scratch_dir), and the ONE rule that governs
// every line in this file: the file the finalizing rename reads is ALWAYS a path in
// the SOURCE's own directory.
//
// What a scratch directory buys is real and is worth stating exactly, because it is
// routinely overstated. A spinning disk no longer interleaves a large sequential
// read and a large sequential write on one spindle for the whole encode; a parity
// array (unRAID, SnapRAID, RAIDZ) takes ONE sequential copy back instead of hours of
// dribbling encoder writes, each of which is a read-modify-write of a parity stripe;
// a transcode that fails or is aborted never puts its wasted bytes on the array at
// all; and an NFS or SMB share sees one streamed copy rather than an encoder's write
// pattern. What it does NOT do is change how many bytes the source's drive handles:
// the source is read once and the result written once either way. It ADDS a full
// write-plus-read cycle on the scratch device, which on an SSD is finite write
// endurance spent at video-file sizes.
//
// And what it must never do is change the swap. holdfast finalizes with rename(2)
// into the source's own directory on purpose, and swap.go's EXDEV branch REFUSES to
// work around a filesystem boundary rather than copying across it, because a
// cross-filesystem copy is a different operation with a different, non-atomic
// safety story. So an accepted encode is copied back into a temp beside the source,
// proved to be the accepted bytes, made durable, and handed to the existing rename -
// uniformly, whether or not the scratch and the source share a filesystem. A second
// swap shape that only appeared on some mounts would be a path nobody tests and
// everybody trusts.

// scratchTagBytes is how much of the source path's digest goes into a working
// file's name. Six bytes (twelve hex characters) is what distinguishes two sources
// that share a basename in different directories, which is the collision this
// exists for; it is not a security property and nothing depends on it being
// unguessable.
const scratchTagBytes = 6

// maxBaseName bounds a constructed working-file name. Linux's NAME_MAX is 255
// BYTES, and a source basename may already be close to it - so a construction that
// simply appended to the stem would produce ENAMETOOLONG on exactly the files whose
// names are longest, which is a per-file failure with no useful message.
const maxBaseName = 255

// sourceTag is the per-source component of a working file's name: a short digest of
// the source's absolute path.
//
// It is what makes AC-B14 hold - two sources that share a basename in different
// library directories get different working paths, so neither job can read or
// remove the other's file - and it is derived from the PATH rather than from a
// counter or the pid because it must be the same string for the same source on
// every worker, in every run, with no shared state to consult.
func sourceTag(source string) string {
	abs := source
	if a, err := filepath.Abs(source); err == nil {
		abs = a
	}
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return hex.EncodeToString(sum[:scratchTagBytes])
}

// scratchWorkPath is this build's own construction of a job's working file inside
// the scratch directory, and it is the whole of it.
//
// It carries TempMarker, so everything that already recognises a work-in-progress
// temp on its name recognises this one too: isTempName for the sweep,
// splitConstruction (and therefore IsTempConstructionName) for the record-free
// hold-back, and the retained-replacement construction stays distinct from it by
// carrying a different marker. The source tag sits in the stem, where
// splitConstruction reads it as part of the stem and nothing has to learn a second
// shape.
func scratchWorkPath(scratch, source, ext string, n int) string {
	base := filepath.Base(source)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	tail := "." + sourceTag(source) + "." + TempMarker + suffix(n) + "." + ext
	if room := maxBaseName - len(tail); len(stem) > room {
		if room < 1 {
			// Pathological: the extension alone fills the name. Keep a one-character
			// stem so splitConstruction still sees a stem at all.
			stem = "w"
		} else {
			stem = stem[:room]
		}
	}
	return filepath.Join(scratch, stem+tail)
}

// pickScratchPath returns a free working path for one job inside the scratch
// directory, clearing anything stale sitting at it.
//
// It clears rather than holds, and the difference from pickTempPath is the whole
// reason this is a separate function. A file at a temp path BESIDE A SOURCE may be a
// gate-passed replacement that could not be moved to its retained name, which is why
// strayReplacementHold examines it before anything deletes it. A file at a scratch
// working path can never be that: with a scratch directory configured, a replacement
// only ever reaches the swap as a COPY beside the source, and the scratch file is
// removed after the copy - so an orphan here is either an encode that never passed a
// gate or a duplicate of bytes that are already beside the source. In both cases the
// source (or the file the swap left in its place) is still there, so reclaiming it
// costs an encode and never the only copy.
//
// The record-based hold-backs still apply: a path a live record names is left alone
// wherever it is.
func (e *Engine) pickScratchPath(scratch, source, ext string) (string, error) {
	for n := 0; n < maxPathCandidates; n++ {
		p := scratchWorkPath(scratch, source, ext, n)
		if why, ok := e.heldBack(p); ok {
			e.Log.Info("not using a scratch working path a record holds back", "path", p, "why", why)
			continue
		}
		_ = os.Remove(p)
		return p, nil
	}
	return "", fmt.Errorf("no free working path for %s in the scratch directory %s after %d candidates",
		source, scratch, maxPathCandidates)
}

// free reports the space available on the filesystem holding path, through the test
// seam when one is set.
func (e *Engine) free(path string) (uint64, error) {
	if e.freeBytes != nil {
		return e.freeBytes(path)
	}
	return diskfree.Bytes(path)
}

// scratchRoomFor refuses a job whose source will not fit in what is left of the
// scratch filesystem, BEFORE the encoder writes a byte.
//
// The source's own size is the bar, and it is a deliberately conservative one: a
// transcode is only ever taken when the output is SMALLER than the source (that is
// gate 4), so a device with room for the source has room for any output holdfast
// would accept. Nobody has the output's size before the encode, and inventing a
// ratio would be a prediction dressed as a check.
//
// A lookup that FAILS does not fail the job. The reason is stated rather than
// assumed: a scratch write that fails anyway is an ordinary encode failure, which
// already leaves the source untouched and records the error - so refusing here on
// the strength of a broken statfs would cost an operator every file in the run to
// protect them from an outcome that is already safe.
func (e *Engine) scratchRoomFor(scratch, source string, sourceBytes int64) error {
	free, err := e.free(scratch)
	if err != nil {
		e.Log.Warn("could not establish the free space on the scratch filesystem (continuing: a scratch write that fails is an ordinary encode failure, and leaves the source untouched)",
			"scratch_dir", scratch, "err", err)
		return nil
	}
	if sourceBytes >= 0 && free < uint64(sourceBytes) {
		return fmt.Errorf("not enough room in the scratch directory %s for %s: %d byte(s) available, the source is %d byte(s). "+
			"Free space there, point scratch_dir at a larger device, or unset scratch_dir to encode beside the source",
			scratch, source, free, sourceBytes)
	}
	return nil
}

// copyBackBesideSource puts the ACCEPTED encode into the source's own directory and
// establishes that what is now there is exactly the bytes the gates passed.
//
// Re-establishing that identity is not belt and braces, it is the whole reason a
// copy is allowed to exist at all. Without a scratch directory the file the gates
// accepted IS the file the swap renames, and the no-loss argument rests on that
// being one file. A copy breaks the identity, so it has to be put back: the digest
// is taken from the accepted file AS IT IS READ, and then the file that was written
// is READ BACK OFF DISK and digested again. Comparing the write against itself would
// prove nothing about what landed; comparing against a re-read catches a short
// write, a truncation, a device that accepted bytes it did not keep, and anything
// that altered the file between the write and the rename.
//
// It is made durable BEFORE the re-read, so the re-read is answered by a file that
// has been flushed rather than by the page cache alone.
//
// Any failure returns an error and leaves the caller to remove the copy. Nothing is
// renamed, the source is not touched, and the copy never had an ordinary media name
// to be mistaken for one.
func (e *Engine) copyBackBesideSource(work, dst string) error {
	accepted, n, err := copyDigest(work, dst)
	if err != nil {
		return fmt.Errorf("copying the accepted encode from %s to %s: %w", work, dst, err)
	}
	if err := e.fsync(dst); err != nil {
		return fmt.Errorf("could not make the accepted encode durable at %s: %w", dst, err)
	}
	if e.afterCopyBack != nil {
		if err := e.afterCopyBack(dst); err != nil {
			return fmt.Errorf("aborted after copying the accepted encode to %s: %w", dst, err)
		}
	}
	got, m, err := fileDigest(dst)
	if err != nil {
		return fmt.Errorf("could not re-read the accepted encode at %s to establish it is the file the gates passed: %w", dst, err)
	}
	if m != n || got != accepted {
		return fmt.Errorf("the file at %s is NOT the file the acceptance gates passed - refusing the swap: "+
			"the gates accepted %d byte(s) sha256 %s (%s), and %d byte(s) sha256 %s were found beside the source",
			dst, n, accepted, work, m, got)
	}
	return nil
}

// copyDigest copies src to dst and returns the digest and length of what it READ,
// which is what the acceptance gates measured.
func copyDigest(src, dst string) (string, int64, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return "", 0, err
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(out, h), in)
	// The Close error is the one that reports a write the filesystem deferred, so
	// it is checked and never discarded: a copy that failed at close is a copy whose
	// bytes are not all there, and this is the one function that must not let that
	// through.
	if closeErr := out.Close(); cerr == nil {
		cerr = closeErr
	}
	if cerr != nil {
		return "", 0, cerr
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// fileDigest reads a file back off disk and returns its digest and length.
func fileDigest(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// cleanScratch discards the working files a prior killed run left in the configured
// scratch directory, under the same hold-back exceptions the in-place sweep applies.
//
// The scratch directory is not under a library root, so the coverage-bounded sweep
// cannot reach it: coverage is the startup walk's record of the directories BENEATH
// THE ROOTS it traversed, and a working area outside them contributes none. It is
// therefore swept explicitly, here, from the same construction and with the same two
// exceptions AC-B12 names: a path a live record holds back, and a retained
// replacement, which is never anybody's to delete.
//
// It does NOT apply strayReplacementHold, and that is a decision rather than an
// omission. That hold asks "is there a source beside this file to measure it
// against", and in a scratch directory the answer is always no - there are no
// sources here, by construction, since a scratch directory that overlapped a library
// root refuses the run at startup. Applying it would hold every working file for
// ever and crash-safety would regress to nothing. What makes the unconditional sweep
// safe is the property that hold exists to protect: with a scratch directory
// configured, a replacement only ever reaches the swap as a COPY beside the source,
// so a file orphaned here is either an encode that never passed a gate or a
// duplicate of bytes that already reached the source's directory. The source, or the
// file the swap left in its place, is still there in both cases.
func (e *Engine) cleanScratch(ctx context.Context) {
	dir := strings.TrimSpace(e.Cfg.ScratchDir)
	if dir == "" {
		return
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		e.Log.Warn("could not list the scratch directory to sweep it (continuing)", "scratch_dir", dir, "err", err)
		return
	}
	n := 0
	for _, ent := range ents {
		if ctx.Err() != nil {
			break
		}
		if ent.IsDir() {
			continue
		}
		p := filepath.Join(dir, ent.Name())
		if IsRetainedReplacementName(ent.Name()) {
			e.Log.Warn("leaving a file holdfast wrote in place (not an orphaned working file)",
				"file", p, "why", "a replacement holdfast retained, which nothing in this program deletes on its own initiative")
			continue
		}
		if !isTempName(ent.Name()) {
			continue
		}
		if why, ok := e.heldBack(p); ok {
			e.Log.Warn("leaving a file holdfast wrote in place (not an orphaned working file)", "file", p, "why", why)
			continue
		}
		if os.Remove(p) == nil {
			n++
		}
	}
	if n > 0 {
		e.Log.Info("discarded orphaned working file(s) from a prior run in the scratch directory", "count", n, "scratch_dir", dir)
	}
}
