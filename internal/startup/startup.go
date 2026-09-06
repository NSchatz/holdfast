// Package startup implements the whole-run start-or-refuse decision holdfast
// takes before it does anything at all (FILESYSTEM-1).
//
// holdfast's promise is that it never destroys a source until the replacement is
// provably faithful, and that promise is stated for a LOCAL filesystem: an
// atomic same-filesystem rename, a rename whose failure means it did not happen,
// a stat that can see a concurrent rewrite, and a SQLite WAL that works at all.
// None of those hold on a network filesystem, and until this package existed the
// tool never checked which one it was on.
//
// So: before the first encode, before the job store is opened and before
// anything is created, renamed or removed, the system establishes what storage
// EVERY path it will act on sits on (the configured library roots, the state
// directory, and every mounted filesystem found beneath a root by the startup
// walk), reports each one, and then takes exactly ONE decision over the whole
// set. The decision is ordered and closed (see Result.Row and the rows in
// Decide): a malformed declaration, a missing root, a path that cannot be
// inspected or a root that cannot be listed, storage that is not local with no
// opt-in covering it, and otherwise the run starts.
//
// Fail-safe by construction: only a POSITIVE local identification counts as
// local. An unrecognised type, a lookup that fails, absent mount information, an
// overlay, an in-memory filesystem and anything in user space (FUSE) are all
// `undetermined`, which is NOT local and refuses just as a detected NAS does. A
// false warning costs the operator one opt-in line; a false clear costs them a
// film.
package startup

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrMountInfoUnavailable reports that the mount information a lookup would have
// read is absent, unreadable or unparseable. It is a classification input and
// never a coverage or termination input: a walk still visits every directory and
// still terminates without it, and every path whose TYPE only the mount table
// could have settled becomes `undetermined` (which refuses unless opted in)
// rather than being guessed at.
var ErrMountInfoUnavailable = errors.New("mount information is absent, unreadable or unparseable")

// Region identifies the DIRECTORY OF UNDERLYING STORAGE that a path exposes. Two
// paths are the same region when they expose the same directory of the same
// filesystem, however each was reached - which is what makes the startup walk
// terminate over any mount and link layout, and what stops one source being
// enumerated twice through two spellings.
//
// FS identifies the FILESYSTEM and Node the directory within it. FS is
// deliberately NOT the per-mount device number: one filesystem mounted twice can
// carry two different device numbers (separate superblocks), and treating those
// as two regions would walk the same directory twice and queue a second swap
// against a source the first already replaced. See the platform implementation
// for what it derives FS from and for the residual case it cannot collapse.
type Region struct {
	FS   string
	Node string
}

// Info is what one inspection of a path establishes.
type Info struct {
	// IsDir reports whether the path is a directory (after following links).
	IsDir bool
	// DevID identifies the MOUNTED FILESYSTEM the path is on. Two paths with the
	// same DevID are on the same mounted filesystem, which is the test AC2d1's
	// converse limb needs: a directory on the same mounted filesystem as its
	// parent is not a separate checked path.
	DevID string
	// Region identifies the directory of underlying storage the path exposes.
	Region Region
}

// Entry is one entry of a directory listing.
type Entry struct {
	Name   string
	IsDir  bool
	IsLink bool
}

// Platform is the substitutable view of the host that the startup check reads.
// The two seams the tests need are both here, because the gate a test runs on
// has neither a network mount nor a second real filesystem: the filesystem-type
// lookup (FSType), and the mount layout plus directory tree the walk reads
// (Inspect, ReadDir, Resolve, MountPoint).
//
// Every method reports "does not exist" as an error satisfying
// errors.Is(err, fs.ErrNotExist) and "the process may not look" as one
// satisfying errors.Is(err, fs.ErrPermission); the check distinguishes those two
// from every other failure and from each other, so an implementation that
// flattens them would move which row of the decision fires.
type Platform interface {
	// Inspect establishes that path exists and reports its storage facts,
	// following symbolic links. This is "to inspect" in the sense the decision
	// uses: a path that cannot be inspected refuses the run.
	Inspect(path string) (Info, error)

	// ReadDir lists dir, sorted by name. A listing that fails PARTWAY returns the
	// entries read so far TOGETHER WITH a non-nil error; the walk treats that as
	// a failed traversal and enumerates nothing from it.
	ReadDir(path string) ([]Entry, error)

	// Resolve returns path with symbolic links resolved. It reports
	// fs.ErrNotExist when the path does not exist, which is how the general
	// longest-existing-prefix rule (resolvedForm) is built on top of it.
	Resolve(path string) (string, error)

	// FSType names the filesystem type at path. An empty name with a nil error
	// means the platform reported no type at all.
	FSType(path string) (string, error)

	// MountPoint reports whether path is itself the root of a mounted
	// filesystem. It MAY consult the mount table, and reports
	// ErrMountInfoUnavailable when that information cannot be read: the walk
	// then falls back to the mounted-filesystem identity alone, so absent mount
	// information can move a classification but never coverage or termination.
	MountPoint(path string) (bool, error)
}

// cleanPath normalises a path the way both "beneath" tests require: drop "."
// segments, resolve ".." away, and remove any trailing separator. It is purely
// lexical and touches no storage.
func cleanPath(p string) string { return filepath.Clean(p) }

// lexicallyBeneath reports whether path is LEXICALLY beneath root: over the two
// texts alone, normalised, with no link resolution and no filesystem access at
// all, root is a proper prefix of path ending at a separator boundary. So
// /srv/mediaX is not beneath /srv/media, and /srv/media/tv/../tv is.
func lexicallyBeneath(path, root string) bool {
	p, r := cleanPath(path), cleanPath(root)
	if p == r {
		return false // beneath is PROPER: the root is not beneath itself
	}
	if r == string(filepath.Separator) {
		return strings.HasPrefix(p, r)
	}
	return strings.HasPrefix(p, r+string(filepath.Separator))
}

// withinResolved reports whether path is AT or beneath root, both already in
// resolved form. Equality counts here, unlike lexicallyBeneath: a symbolic link
// pointing at a configured root has not left the library, so the walk owes it
// the region rule's report rather than the "leaves the library roots" one.
func withinResolved(path, root string) bool {
	p, r := cleanPath(path), cleanPath(root)
	return p == r || lexicallyBeneath(p, r)
}
