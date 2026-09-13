package engine

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// The metadata a swap carries from the SOURCE onto the replacement it publishes (S0085):
// mode, ownership and modification time. ffmpeg writes the replacement under the daemon's
// umask and uid:gid, so carrying nothing across means every transcoded file changes owner,
// permissions and age: access breaks on a library an *arr, a download client and a human
// have all written into, and a fresh mtime makes a first pass read to Plex or Jellyfin as
// the whole library arriving at once.
//
// # Where it runs, and why exactly there
//
// carrySourceMetadata is called from ProcessFile immediately BEFORE the temp's durability
// barrier (the fsync of the temp that precedes the rename), and therefore before the
// TRANSCODE-16 source re-fingerprint as well. Both placements are constraints rather than
// preferences: before the barrier, so the attributes the rename publishes are as durable as
// the bytes it publishes rather than a mode living in the page cache behind a name already
// on disk; outside the re-fingerprint window, because that guard's whole promise is a TOCTOU
// window of the microseconds between its stat and the rename syscall, and a syscall added
// inside it widens what a phase was spent narrowing - the same reason the temp fsync and the
// undo retention are both hoisted out of it.
//
// # The failure rule
//
// Every metadata failure - a source whose metadata cannot be READ included - is a FAILED
// SWAP: no rename, the temp discarded, the source byte-for-byte intact, and a recorded
// reason naming the step. A step that half-applied and let the swap proceed would publish a
// file whose permissions or owner nobody chose, with the source already gone. There is
// exactly ONE exemption and it is narrow: an ownership change refused because this process
// lacks the privilege to make one. That is the shipped deployment (a container running as an
// ordinary user:, with no CAP_CHOWN), so the swap proceeds with everything else carried and
// the operator is told once for the run.
//
// ACLs and xattrs are deliberately NOT carried: copying them correctly is a
// filesystem-specific problem this does not open, and docs/docker.md says so outright. The
// undo window's retained original is a second hard link to the SOURCE inode, so it already
// has the source's metadata by construction.

// metadataModeBits are the mode bits os.Chmod honours, and therefore the whole of what "the
// same file mode as the source" can mean here. Masking to a SUBSET is the original defect in
// miniature: a source at 04640 replaced by a file at 0640 still had its mode changed.
const metadataModeBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// The metadata steps, named. Each name is written into the recorded reason of a failed swap,
// so a failed row says WHICH step could not be completed rather than that "the metadata"
// failed, and the fixtures that prove each failure is refused read the same names back.
const (
	metadataStepRead    = "read the source's metadata"
	metadataStepOwner   = "apply the source's ownership to the replacement"
	metadataStepMode    = "apply the source's mode to the replacement"
	metadataStepModTime = "apply the source's modification time to the replacement"
)

// ownershipNotPreservedNotice is the report an unprivileged run owes its operator, emitted
// EXACTLY ONCE per run however many files that run swaps: not once per file, because a
// container running as an ordinary `user:` cannot chown at all and a library-wide first pass
// would write one identical line per film; not once per process, because `serve` scans every
// scan_interval_sec and an operator who started the daemon a week ago would never hear it
// again. A constant, so the fixture that counts it counts what the engine emits.
const ownershipNotPreservedNotice = "OWNERSHIP NOT PRESERVED: this process may not change a file's owner " +
	"(chown answered EPERM - it has neither CAP_CHOWN nor root), so every replacement this run publishes is " +
	"owned by the holdfast uid:gid rather than by the source's. The mode and the modification time ARE carried " +
	"and the swap is unaffected. Reported once for this run. Run holdfast as a user that already owns the media, " +
	"or grant it CAP_CHOWN, to carry ownership across as well."

// fileMetadata is what a swap carries from the source onto the replacement: the mode already
// masked to the bits os.Chmod honours, the modification time, and the ownership where the
// platform has one. HasOwner is false where this build cannot read an ownership at all (see
// metadata_other.go), which is not an error and is not reported: there is nothing to carry.
type fileMetadata struct {
	Mode     fs.FileMode
	ModTime  time.Time
	UID, GID int
	HasOwner bool
}

// statMetadata reads one file's carriable metadata. os.Stat rather than os.Lstat is
// deliberate: it is only ever asked about a SOURCE, and a symlinked source is refused long
// before the swap by the SkipSymlink guard.
func statMetadata(path string) (fileMetadata, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileMetadata{}, err
	}
	md := fileMetadata{Mode: fi.Mode() & metadataModeBits, ModTime: fi.ModTime()}
	md.UID, md.GID, md.HasOwner = ownerOf(fi)
	return md, nil
}

// The four seams, routed exactly as rename, restat and fsync already are. Production leaves
// each nil and calls the real thing; the fixtures substitute them because this gate runs as an
// unprivileged uid with no CAP_CHOWN, so a real chown to another uid is not exercisable here.

func (e *Engine) statMetadata(path string) (fileMetadata, error) {
	if e.statMetadataFn != nil {
		return e.statMetadataFn(path)
	}
	return statMetadata(path)
}

func (e *Engine) chown(path string, uid, gid int) error {
	if e.chownFn != nil {
		return e.chownFn(path, uid, gid)
	}
	return os.Chown(path, uid, gid)
}

func (e *Engine) chmod(path string, mode fs.FileMode) error {
	if e.chmodFn != nil {
		return e.chmodFn(path, mode)
	}
	return os.Chmod(path, mode)
}

// chtimes sets the modification time and LEAVES THE ACCESS TIME ALONE - a zero time.Time tells
// os.Chtimes to leave that field unchanged. Inventing an access time, or copying the source's
// which the read has just moved, changes a second attribute nobody asked for.
func (e *Engine) chtimes(path string, mtime time.Time) error {
	if e.chtimesFn != nil {
		return e.chtimesFn(path, mtime)
	}
	return os.Chtimes(path, time.Time{}, mtime)
}

// notPermitted reports whether an error is the kernel saying THIS PROCESS LACKS THE PRIVILEGE,
// and nothing else. It matches EPERM alone, never fs.ErrPermission: chown(2) answers EPERM for
// "no CAP_CHOWN and not the owner" and EACCES for "search permission denied on a component of
// the path prefix", and Go maps BOTH onto fs.ErrPermission. Only the first is the rootless case
// AC4 exempts; swallowing the second would publish a replacement whose owner nobody chose.
func notPermitted(err error) bool { return errors.Is(err, syscall.EPERM) }

// carrySourceMetadata puts the source's mode, ownership and modification time onto the
// replacement, and returns the NAME OF THE STEP that failed plus its error, or "" and nil when
// the whole set was applied. The caller treats any non-nil error as a failed swap, so the one
// failure that is not a failed swap (an ownership change this process is not privileged to
// make) is absorbed here, reported here, and returns no error.
func (e *Engine) carrySourceMetadata(src, replacement string) (string, error) {
	md, err := e.statMetadata(src)
	if err != nil {
		return metadataStepRead, err
	}

	// OWNERSHIP FIRST, and the order is load-bearing: chown(2) clears a file's set-user-ID and
	// set-group-ID bits, so applying the mode first drops exactly the bits the source carried,
	// on the step whose whole job is to stop the mode being silently changed.
	if md.HasOwner {
		if err := e.chown(replacement, md.UID, md.GID); err != nil {
			if !notPermitted(err) {
				return metadataStepOwner, err
			}
			e.reportUnpreservedOwnership(replacement, md, err)
		}
	}

	if err := e.chmod(replacement, md.Mode); err != nil {
		return metadataStepMode, err
	}

	// LAST, so nothing above can disturb what it sets. chown and chmod move ctime and not
	// mtime, and neither the fsync nor the rename that follow touch mtime, so this is
	// belt-and-braces rather than a correctness requirement.
	if e.Cfg.PreserveMtimeEnabled() {
		if err := e.chtimes(replacement, md.ModTime); err != nil {
			return metadataStepModTime, err
		}
	}
	return "", nil
}

// reportUnpreservedOwnership emits the once-per-run notice. CompareAndSwap is what makes
// "exactly once" true under the worker pool: N workers swapping N files concurrently all
// reach this, and exactly one of them wins the flag.
func (e *Engine) reportUnpreservedOwnership(replacement string, md fileMetadata, err error) {
	if !e.ownershipNoticeGiven.CompareAndSwap(false, true) {
		return
	}
	e.Log.Warn(ownershipNotPreservedNotice,
		"replacement", replacement, "source_uid", md.UID, "source_gid", md.GID, "err", err)
}
