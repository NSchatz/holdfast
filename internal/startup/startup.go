// Package startup implements the whole-run start-or-refuse decision holdfast takes before
// it does anything at all. The no-loss promise is stated for a LOCAL filesystem - an atomic
// same-filesystem rename whose failure means it did not happen, a stat that can see a
// concurrent rewrite, a working SQLite WAL - and none of that holds on a network filesystem,
// so this establishes what storage EVERY path it will act on sits on, reports each one, and
// takes exactly ONE ordered decision over the whole set (Result.Row, and the rows in
// Decide). Fail-safe by construction: only a POSITIVE local identification counts as local
// and everything else is `undetermined`, which refuses exactly as a detected NAS does,
// because a false warning costs one opt-in line and a false clear costs a film.
package startup

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrMountInfoUnavailable reports mount information absent, unreadable or unparseable. It is a
// classification input, never a coverage or termination one: the walk still covers and terminates.
var ErrMountInfoUnavailable = errors.New("mount information is absent, unreadable or unparseable")

// Region identifies the DIRECTORY OF UNDERLYING STORAGE a path exposes: two paths are the
// same region when they expose the same directory of the same filesystem, however each was
// reached; that makes the walk terminate over any mount and link layout and stops a source
// being enumerated twice. FS is NOT the per-mount device number: one filesystem mounted
// twice carries two of those, and two regions would queue a swap against a replaced source.
type Region struct {
	FS   string
	Node string
}

type Info struct {
	// IsDir reports whether the path is a directory, after following links.
	IsDir bool
	// DevID identifies the MOUNTED FILESYSTEM the path is on, which is how a directory on
	// its parent's is known not to be a separate checked path.
	DevID string
	// Region is the directory of underlying storage the path exposes.
	Region Region
}

type Entry struct {
	Name string
	// IsDir is the kind the LISTING reports, no link followed: a symlink is not a directory.
	IsDir  bool
	IsLink bool
	// ResolvesToDir reports that FOLLOWING this entry reaches a directory. A
	// Platform leaves it false - a listing does not follow links - and the walk
	// fills it in from the inspection it already makes of every link and every
	// subdirectory it meets.
	ResolvesToDir bool
}

// Platform is the substitutable view of the host the startup check reads, and the seam the
// tests need: the gate a test runs on has neither a network mount nor a second real
// filesystem. Every method reports a missing path as fs.ErrNotExist and a refused look as
// fs.ErrPermission, and flattening those two moves which row of Decide fires.
type Platform interface {
	// A path that cannot be inspected refuses the run. Links are followed.
	Inspect(path string) (Info, error)

	// ReadDir lists dir, sorted by name. A listing that fails PARTWAY returns the entries
	// read so far TOGETHER WITH a non-nil error, and the walk enumerates nothing from it.
	ReadDir(path string) ([]Entry, error)

	// Resolve reports fs.ErrNotExist when path does not exist, which is what resolvedForm's
	// longest-existing-prefix rule rests on.
	Resolve(path string) (string, error)

	// FSType names the filesystem type at path; an empty name with a nil error means none.
	FSType(path string) (string, error)

	// MountPoint reports whether path is itself the root of a mounted filesystem. It MAY
	// consult the mount table and reports ErrMountInfoUnavailable when that cannot be read.
	MountPoint(path string) (bool, error)

	// FreeBytes reports the space available to this process on the filesystem
	// holding path. It is read for the SCRATCH DIRECTORY alone - no library root
	// and no state directory is ever refused for want of space here - and a
	// lookup that fails is a refusal, never a guessed number, because both
	// directions of a guess are wrong: zero refuses a device that is fine and a
	// large number clears one that is full.
	FreeBytes(path string) (uint64, error)

	// ProbeWritable establishes that this process can CREATE AND REMOVE a file in
	// dir, and returns the reason it cannot.
	//
	// It is the ONE method on this interface that writes anything, and it writes
	// only in the scratch directory - never under a library root, never in or
	// under the state directory. A mode check would not answer the question: a
	// read-only mount, a full filesystem, an NFS export squashing the writing
	// uid and an SELinux denial are all decided at open() and none of them is
	// visible in the permission bits. It creates one zero-length file under a
	// name this build constructs and removes it again, so a directory it was
	// asked about is left exactly as it was found.
	ProbeWritable(dir string) error
}

func cleanPath(p string) string { return filepath.Clean(p) }

// lexicallyBeneath reports whether path is LEXICALLY beneath root: over the two texts
// alone, with no link resolution and no filesystem access at all.
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

// withinResolved reports whether path is AT or beneath root, both already resolved.
// Equality counts here, unlike lexicallyBeneath: a link pointing at a configured root has
// not left the library.
func withinResolved(path, root string) bool {
	p, r := cleanPath(path), cleanPath(root)
	return p == r || lexicallyBeneath(p, r)
}
