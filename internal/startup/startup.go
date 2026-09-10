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

// ErrMountInfoUnavailable reports that mount information is absent, unreadable or
// unparseable. It is a classification input, never a coverage or termination one: the walk
// still visits every directory and terminates, and such a path is `undetermined`, not a guess.
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
	Name   string
	IsDir  bool
	IsLink bool
}

// Platform is the substitutable view of the host the startup check reads, and the seam the
// tests need: the gate a test runs on has neither a network mount nor a second real
// filesystem. Every method reports a missing path as an error satisfying fs.ErrNotExist and
// a refused look as one satisfying fs.ErrPermission, and the check distinguishes those two
// from each other and from every other failure, so flattening them moves which row of
// Decide fires.
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
	// consult the mount table and reports ErrMountInfoUnavailable when that cannot be read,
	// so absent mount information moves a classification but never coverage or termination.
	MountPoint(path string) (bool, error)
}

func cleanPath(p string) string { return filepath.Clean(p) }

// lexicallyBeneath reports whether path is LEXICALLY beneath root: over the two texts
// alone, normalised, with no link resolution and no filesystem access at all, root is a
// proper prefix of path ending at a separator boundary.
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
