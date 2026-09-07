package engine

// The undo window (UNDO-6): a bounded period in which the one irreversible act this
// tool performs can be walked back.
//
// The swap is a rename over the source, so the original is destroyed by the swap
// itself and nothing keeps it. Every gate in front of that rename is an ESTIMATE -
// a very good one, measured against the source, but still a verdict taken by a
// machine about whether a file looks the same. The delete is not an estimate.
//
// What closes the gap is `link(2)`: a second name for the SAME data, taken before
// the rename. It costs no additional space (there is one inode either way) and it
// keeps the original's bytes alive after the rename has taken its only other name
// away. `unlink(2)` then frees that data when - and only when - the last name for it
// goes, which is what the release below reports honestly rather than assuming.
//
// Two properties of a link bound the design and are not negotiable:
//
//   - A link cannot cross a mount point (`link(2)` fails with EXDEV when the two
//     paths "are not on the same mounted filesystem"), so the retention area is a
//     directory INSIDE the source's own directory. Same directory, same filesystem,
//     by construction - the identical argument the same-directory temp already rests
//     on.
//   - A link raises the source's link count, and this tool SKIPS a file with more
//     than one link (an *arr import that is also an active seed). A retention left
//     behind by an interrupted run would therefore park the file it was protecting.
//     The hardlink guard discounts links this tool itself holds; a foreign link still
//     skips exactly as it did before.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
	"github.com/NSchatz/holdfast/internal/probe"
	"github.com/NSchatz/holdfast/internal/store"
)

// UndoMarker is the fixed infix in a retained original's name, so a retained file is
// always identifiable from its NAME alone - which is what the scan and the startup
// walk both need, since they decide what to enumerate without opening anything. It
// mirrors TempMarker and serves the same purpose: this tool's own working files are
// never mistaken for library media, whatever extension they carry.
const UndoMarker = "__undo__"

// UndoDirName is the per-directory retention area. It lives INSIDE the source's own
// directory because a hard link cannot cross a filesystem boundary, and a directory
// beside the file is the only location guaranteed to be on the same one.
const UndoDirName = ".holdfast-undo"

// isUndoName reports whether a basename is a retained original.
func isUndoName(base string) bool { return strings.Contains(base, "."+UndoMarker) }

// undoDirFor returns the retention area for files in dir.
func undoDirFor(dir string) string { return filepath.Join(dir, UndoDirName) }

// retainedPathFor is the retained original's full path: the source's basename, the
// source's fingerprint, and the marker. The fingerprint is in the name so two
// retentions of the same path (a swap, a restore, a later swap) can never collide on
// one name, and so a caller can compute the name a given source WOULD get.
func retainedPathFor(src, fingerprint string) string {
	base := filepath.Base(src)
	safe := strings.NewReplacer(":", "-", "/", "-").Replace(fingerprint)
	return filepath.Join(undoDirFor(filepath.Dir(src)), base+"."+safe+"."+UndoMarker)
}

// errRetentionExists is returned when the retention area already holds a DIFFERENT
// file at the name this retention wants. It is a refusal, not a collision to resolve:
// the tool does not know what those other bytes are, and overwriting them would be
// destroying something to make room for a safety net.
var errRetentionExists = errors.New("a different file already occupies the retained name")

// UndoWindow is the retention/restore/release surface, over the same store the engine
// writes through. It is deliberately separable from Engine: a restore needs the config
// and the ledger and nothing else - no ffmpeg, no encoder capability check, no probe -
// so `holdfast restore` can be a cheap local command rather than a daemon start.
type UndoWindow struct {
	Cfg   config.Config
	Store store.Store
	Log   *slog.Logger

	// now is a test seam for the clock. Production leaves it nil and uses time.Now;
	// a test uses it to place a retention's expiry in the past without sleeping.
	now func() time.Time
}

// NewUndoWindow builds the undo surface over a store.
func NewUndoWindow(cfg config.Config, st store.Store, log *slog.Logger) *UndoWindow {
	if log == nil {
		log = slog.Default()
	}
	return &UndoWindow{Cfg: cfg, Store: st, Log: log}
}

// undo returns the engine's own view of the window, so ProcessFile and RunOneshot use
// exactly the code path `holdfast restore` does.
func (e *Engine) undo() *UndoWindow {
	u := NewUndoWindow(e.Cfg, e.Store, e.Log)
	u.now = e.undoNow
	return u
}

// retainedLinks is how many of f's hard links this tool holds through the undo
// window. It is 0 - with no ledger read at all - when the window is disabled, which
// is the default, so the hardlink guard costs a configuration without the window
// exactly what it cost before.
func (e *Engine) retainedLinks(ctx context.Context, f, fingerprint string) uint64 {
	if !e.Cfg.UndoEnabled() {
		return 0
	}
	return e.undo().heldLinks(ctx, f, fingerprint)
}

func (u *UndoWindow) clock() time.Time {
	if u.now != nil {
		return u.now()
	}
	return time.Now()
}

// Enabled reports whether the window is open at all.
func (u *UndoWindow) Enabled() bool { return u.Cfg.UndoEnabled() }

// retain takes the second reference to src's data and records it, returning the
// retained path. It runs BEFORE the rename, and its failure is the reason a file is
// skipped rather than swapped: a swap this tool cannot undo is not one it takes while
// the operator has asked for a window in which to undo it.
//
// It is idempotent against its own leftovers. An interrupted run can leave a retained
// link on disk (and a record beside it) for a source that was never swapped; retaining
// again must then REUSE that link rather than fail on `os.Link`'s EEXIST, or the very
// first crash would park the file for as long as the record lived.
func (u *UndoWindow) retain(src, fingerprint string) (string, error) {
	dst := retainedPathFor(src, fingerprint)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", fmt.Errorf("create the retention area: %w", err)
	}
	err := os.Link(src, dst)
	if err == nil {
		return dst, nil
	}
	if !errors.Is(err, os.ErrExist) {
		// EXDEV lands here when the retention area is not on the source's filesystem,
		// which the same-directory layout makes impossible - but a bind mount over
		// the retention directory would do it, and the honest answer is the same as
		// for a permission failure: this file cannot be retained, so it is not
		// swapped.
		return "", fmt.Errorf("link the original: %w", err)
	}
	// Something is already at the retained name. If it is the SAME data (our own
	// link, from a run that was interrupted between the link and the rename) the
	// retention is already taken and there is nothing to do. If it is anything else,
	// refuse - see errRetentionExists.
	if sameFile(src, dst) {
		return dst, nil
	}
	return "", errRetentionExists
}

// discard removes a retained link this run took but will not use, because the swap it
// was protecting did not happen. Best effort and logged: a leftover retained link is
// never a loss (the source is intact and the link points at the same data), and the
// release sweep has no record to act on, so the worst case is one orphan file that
// this same function removes on the next attempt via retain's EEXIST-same-file path.
func (u *UndoWindow) discard(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		u.Log.Warn("could not remove the retained original after abandoning the swap it was for",
			"retained", path, "err", err)
	}
	pruneUndoDir(filepath.Dir(path))
}

// pruneUndoDir removes the retention area when it is empty. os.Remove on a directory
// fails unless it is empty, which is exactly the test wanted - so this needs no
// listing and cannot race a concurrent worker's retention into deletion.
func pruneUndoDir(dir string) {
	if filepath.Base(dir) != UndoDirName {
		return
	}
	_ = os.Remove(dir)
}

// sameFile reports whether two paths name the same inode on the same device - the
// question "is this retained file another name for that source", which is the whole
// of what distinguishes a link this tool holds from a foreign one.
func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// record writes the retention to the ledger once the swap has happened, stamping the
// fingerprint of what the swap left behind so a later restore can refuse to overwrite
// content this tool did not write.
func (u *UndoWindow) record(ctx context.Context, sourcePath, swappedPath, retainedPath string, sourceBytes int64) error {
	nowT := u.clock()
	return u.Store.Retain(ctx, store.Retained{
		SourcePath:         sourcePath,
		SwappedPath:        swappedPath,
		RetainedPath:       retainedPath,
		SourceBytes:        sourceBytes,
		SwappedFingerprint: probe.Fingerprint(swappedPath),
		RetainedAt:         nowT.Unix(),
		ExpiresAt:          nowT.Add(u.Cfg.UndoWindow()).Unix(),
	})
}

// heldLinks counts how many of f's hard links are retained originals THIS TOOL holds:
// a live retention record whose retained file is another name for f itself.
//
// It is what keeps the undo window from tripping the hardlink guard. A retention taken
// before a rename that then never happened (an interrupted run) leaves the source at
// two links, and without this the next scan would skip it as "an active seed" - the
// window would have parked the file it exists to protect. Discounting only links this
// tool can PROVE are its own (same inode, live record) means a foreign link still
// skips exactly as it did before.
func (u *UndoWindow) heldLinks(ctx context.Context, f string, fingerprint string) uint64 {
	seen := make(map[string]bool, 2)
	var n uint64
	count := func(path string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		if sameFile(f, path) {
			n++
		}
	}

	// The link this tool WOULD have taken for this exact source. It is checked without
	// consulting the ledger at all, because the record is written after the swap and a
	// run killed between the link and that write leaves a real retained link with no
	// row behind it. The name is proof enough on its own: it carries this tool's
	// marker and the source's own fingerprint, and it is only counted when it is
	// literally another name for this inode.
	count(retainedPathFor(f, fingerprint))

	rows, err := u.Store.ListRetained(ctx)
	if err != nil {
		// Fail safe: with the ledger unreadable, discount only what the name proved.
		// Anything else is treated as foreign, which is the pre-undo-window behaviour
		// and never a swap this tool could not undo.
		u.Log.Warn("could not read the retained originals (treating every unproven extra link as foreign)", "file", f, "err", err)
		return n
	}
	for _, r := range rows {
		count(r.RetainedPath)
	}
	return n
}

// ReleaseReport is what one release sweep did: how many retentions it released, how
// many bytes that ACTUALLY returned to the filesystem, and how many records named a
// retained original that was no longer there.
//
// Released and BytesReturned are separate figures and neither implies the other.
// Removing a name frees the data only when it was the last name for it (`unlink(2)`:
// "if that name was the last link to a file ... the space it was using is made
// available for reuse"), so a retention whose data survives under a third link is
// released and returns nothing - and says so, rather than reporting a reclaim that
// did not happen.
type ReleaseReport struct {
	Released      int
	BytesReturned int64
	Unrestorable  int
}

// ReleaseExpired releases every retention whose window has closed. It runs at the
// start of each scan pass, which is what makes the window self-closing: nothing else
// has to be scheduled, and a daemon that is never asked to scan is also never
// reclaiming, so there is nothing to release.
func (u *UndoWindow) ReleaseExpired(ctx context.Context) ReleaseReport {
	var rep ReleaseReport
	rows, err := u.Store.ListRetained(ctx)
	if err != nil {
		u.Log.Warn("could not read the retained originals (nothing released this pass)", "err", err)
		return rep
	}
	nowUnix := u.clock().Unix()
	for _, r := range rows {
		if ctx.Err() != nil {
			break
		}
		if r.ExpiresAt > nowUnix {
			continue
		}
		bytes, kind := u.releaseOne(r)
		switch kind {
		case releasedFreed, releasedShared:
			rep.Released++
			rep.BytesReturned += bytes
			if err := u.Store.DropRetained(ctx, r.SourcePath); err != nil {
				u.Log.Warn("released the retained original but could not drop its record", "path", r.SourcePath, "err", err)
			}
		case releasedMissing:
			rep.Unrestorable++
			if err := u.Store.DropRetained(ctx, r.SourcePath); err != nil {
				u.Log.Warn("could not drop the record of a retained original that is no longer on disk", "path", r.SourcePath, "err", err)
			}
		case releasedFailed:
			// Left in place deliberately: the record still describes a real retained
			// original, so the next pass tries again rather than forgetting a file the
			// operator can still restore.
		}
	}
	if rep.Released > 0 || rep.Unrestorable > 0 {
		u.Log.Info("undo window: released retained original(s)",
			"released", rep.Released, "bytes_returned", rep.BytesReturned, "unrestorable", rep.Unrestorable)
	}
	return rep
}

// the outcomes of releasing one retention.
type releaseKind int

const (
	releasedFreed   releaseKind = iota // the last name went; the space came back
	releasedShared                     // the name went; the data survives elsewhere, so nothing came back
	releasedMissing                    // there was nothing there to release
	releasedFailed                     // the name could not be removed; the record stays
)

// releaseOne removes one retained name and reports what that actually returned.
//
// The link count is read BEFORE the removal, because afterwards there is nothing left
// to ask. More than one link means the data survives under another name and the
// removal returned NOTHING - the figure this reports is the space the filesystem got
// back, not the size of the file whose name was removed. (`unlink(2)` also holds the
// space while any process still has the file open; that is not observable from here,
// so a returned figure is an upper bound on space available immediately, and the tool
// never claims otherwise.)
func (u *UndoWindow) releaseOne(r store.Retained) (int64, releaseKind) {
	fi, err := os.Stat(r.RetainedPath)
	if err != nil {
		u.Log.Warn("the retained original is no longer on disk - nothing to release",
			"path", r.SourcePath, "retained", r.RetainedPath)
		return 0, releasedMissing
	}
	links := probe.NLink(r.RetainedPath)
	size := fi.Size()
	if err := os.Remove(r.RetainedPath); err != nil {
		u.Log.Warn("could not release the retained original (will retry next pass)",
			"path", r.SourcePath, "retained", r.RetainedPath, "err", err)
		return 0, releasedFailed
	}
	pruneUndoDir(filepath.Dir(r.RetainedPath))
	if links > 1 {
		u.Log.Info("undo window: released a retained original whose data survives under another link - no space returned",
			"path", r.SourcePath, "links", links)
		return 0, releasedShared
	}
	return size, releasedFreed
}

// RestoreResult is what a restore did, so the CLI can report it and a test can assert
// on it without parsing prose.
type RestoreResult struct {
	// Record is the retention that was acted on (zero-valued when none was found).
	Record store.Retained
	// RestoredAt is the unix second stamped on the ledger, 0 when nothing was restored.
	RestoredAt int64
	// Removed is the file the swap had produced and the restore took away, "" when the
	// restore was an in-place rename over it.
	Removed string
}

// Restore-refusal sentinels. Each is a REFUSAL, never a partial action: on every one
// of them the filesystem is exactly as it was.
var (
	// ErrNothingRetained means there is no live retention for that path: never one,
	// or one already released when its window closed, or one already restored.
	ErrNothingRetained = errors.New("no retained original for that path")
	// ErrUnrestorable means the record's retained original is no longer on disk.
	ErrUnrestorable = errors.New("the retained original is no longer on disk")
	// ErrSwappedFileMoved means the file at the restore target has changed since the
	// swap, so putting the original back would destroy content this tool never wrote.
	ErrSwappedFileMoved = errors.New("the file at that path is not the one holdfast swapped in")
	// ErrOriginalPathOccupied means a file has appeared at the original's own path
	// since a container-changing swap removed it.
	ErrOriginalPathOccupied = errors.New("a file already exists at the original's path")
)

// List returns the live retentions, oldest expiry first - what `holdfast restore`
// with no argument prints.
func (u *UndoWindow) List(ctx context.Context) ([]store.Retained, error) {
	return u.Store.ListRetained(ctx)
}

// Restore puts a retained original back at its own path and records the restore.
//
// Every refusal below happens BEFORE anything is moved, and each names what it
// refused. The order is not arbitrary: nothing-retained first (the common miss), then
// the retained original's own existence (there is no restore to do without it), then
// what is at the destination - because overwriting a file this tool did not write is
// the one failure mode a restore can have that costs an operator data.
func (u *UndoWindow) Restore(ctx context.Context, path string) (RestoreResult, error) {
	var res RestoreResult
	r, ok, err := u.Store.GetRetained(ctx, path)
	if err != nil {
		return res, err
	}
	if !ok || r.RestoredAt != nil {
		return res, fmt.Errorf("%w: %s", ErrNothingRetained, path)
	}
	res.Record = r

	if _, err := os.Stat(r.RetainedPath); err != nil {
		// AC13: report it, drop the record, return nothing. A record promising a
		// restore it cannot perform is worse than no record: it is the ledger lying
		// about what is recoverable.
		if derr := u.Store.DropRetained(ctx, r.SourcePath); derr != nil {
			u.Log.Warn("could not drop the record of a retained original that is no longer on disk",
				"path", r.SourcePath, "err", derr)
		}
		return res, fmt.Errorf("%w: %s (recorded at %s; the record has been dropped and 0 bytes were returned)",
			ErrUnrestorable, r.RetainedPath, r.SourcePath)
	}

	// The file at the restore target must be the one the swap left there. A moved
	// size:mtime means something else wrote it - a re-download, an *arr upgrade, a
	// human - and putting the original back would silently destroy that newer content
	// exactly as a swap over a rewritten source would. Both fingerprints are named,
	// because "it changed" without saying from what to what is not something an
	// operator can act on.
	if cur := probe.Fingerprint(r.SwappedPath); cur != r.SwappedFingerprint {
		return res, fmt.Errorf("%w: %s (holdfast swapped in fingerprint %s, it is now %s) - refusing to overwrite the newer content",
			ErrSwappedFileMoved, r.SwappedPath, r.SwappedFingerprint, cur)
	}

	// A container-changing swap removed the original's own path. If something has
	// since appeared there, the rename below would clobber it - the same refusal the
	// swap's own collision guard takes, for the same reason.
	if r.SourcePath != r.SwappedPath {
		if _, err := os.Lstat(r.SourcePath); err == nil {
			return res, fmt.Errorf("%w: %s", ErrOriginalPathOccupied, r.SourcePath)
		}
	}

	// The restore itself. os.Rename is atomic and same-filesystem (the retention area
	// is a directory inside the original's own), so a reader sees either the encode or
	// the original and never nothing. For an in-place swap this single call also
	// unlinks the encode; for a container-changing one the original lands first and
	// the encode is removed after, so the failure mode is a duplicate and never a
	// loss.
	if err := os.Rename(r.RetainedPath, r.SourcePath); err != nil {
		return res, fmt.Errorf("restore %s: %w", r.SourcePath, err)
	}
	dir := filepath.Dir(r.SourcePath)
	if err := fsyncPath(dir); err != nil {
		u.Log.Warn("restored the original but could not fsync its directory (durability across a power loss is not guaranteed)",
			"path", r.SourcePath, "err", err)
	}
	pruneUndoDir(undoDirFor(dir))

	if r.SourcePath != r.SwappedPath {
		if err := os.Remove(r.SwappedPath); err != nil {
			u.Log.Warn("restored the original but could not remove the encode it replaced (a duplicate, never a loss)",
				"path", r.SwappedPath, "err", err)
		} else {
			res.Removed = r.SwappedPath
			if err := fsyncPath(dir); err != nil {
				u.Log.Warn("could not fsync the directory after removing the encode", "path", r.SwappedPath, "err", err)
			}
		}
	}

	res.RestoredAt = u.clock().Unix()
	if err := u.Store.MarkRestored(ctx, r.SourcePath, res.RestoredAt); err != nil {
		// The bytes are already back, which is the operator's actual goal; the ledger
		// is behind. Say so loudly rather than reporting a failure that would send
		// them looking for a file that is already restored.
		u.Log.Warn("restored the original but could not record the restore in the ledger",
			"path", r.SourcePath, "err", err)
	}
	u.recordRestoreInJobs(ctx, r)
	u.Log.Info("undo window: restored the original",
		"path", r.SourcePath, "from", r.RetainedPath, "bytes", r.SourceBytes)
	return res, nil
}

// recordRestoreInJobs reconciles the JOB ledger with what is now on disk: the swap's
// done row describes a file that no longer exists, and the restored original would
// otherwise be a fresh (path, fingerprint) the next scan would happily re-encode -
// undoing the undo, with the same gates that passed the encode the operator just
// rejected.
//
// So the done row goes and a skipped/`restored-original` row takes its place. That is
// what makes the restore visible to a ledger read for that path, and it is what stops
// the next pass from immediately swapping the file again. It parks that file until its
// fingerprint changes, which is the intended reading of a deliberate operator restore.
func (u *UndoWindow) recordRestoreInJobs(ctx context.Context, r store.Retained) {
	if err := u.Store.Delete(ctx, r.SwappedPath, r.SwappedFingerprint); err != nil {
		u.Log.Warn("could not prune the done row of the encode a restore removed", "path", r.SwappedPath, "err", err)
	}
	key := probe.Fingerprint(r.SourcePath)
	if _, err := u.Store.RecordSkip(ctx, r.SourcePath, key, SkipRestoredOriginal); err != nil {
		u.Log.Warn("could not record the restore in the job ledger", "path", r.SourcePath, "err", err)
	}
}
