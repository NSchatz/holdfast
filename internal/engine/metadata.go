package engine

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"time"
)

// The metadata a swap carries from the SOURCE onto the replacement it publishes (S0085).
//
// # The defect this exists to fix
//
// The replacement is written by ffmpeg, under the daemon's umask and the daemon's uid:gid,
// and the rename then publishes it under the source's name. Nothing carried the source's
// own attributes across, so every file holdfast touched silently changed its permissions,
// its owner and its age. Both consequences are library-wide and neither is announced:
//
//   - ACCESS. On a library with mixed ownership - the common shape when an *arr, a
//     download client and a human have all written into the same tree - every transcoded
//     file comes out owned by the holdfast uid with the holdfast umask, and whatever else
//     read those files may stop being able to.
//   - ORDERING. A new mtime resets the file's age, so "Recently Added" in Plex and
//     Jellyfin, and every date-based sort and smart collection built on it, sees a first
//     pass over a library as the entire library arriving at once. That is a real, visible
//     and unrecoverable change to somebody's media server, produced as a side effect of
//     reclaiming disk.
//
// # Where it runs, and why exactly there
//
// carrySourceMetadata is called from ProcessFile immediately BEFORE the temp's durability
// barrier (the fsync of the temp that precedes the rename), and therefore before the
// TRANSCODE-16 source re-fingerprint as well. Both placements are constraints rather than
// preferences:
//
//   - before the durability barrier, so the attributes the rename publishes are as durable
//     as the bytes it publishes. Metadata applied after the fsync would be a mode and an
//     mtime living only in the page cache behind a name that is already on disk.
//   - outside the re-fingerprint window, because that guard's whole promise is that its
//     TOCTOU window is the microseconds between its stat and the rename syscall. A
//     syscall added inside it widens the window this repository spent a phase narrowing -
//     the same reason the temp fsync and the undo retention are both hoisted out of it.
//
// # The failure rule
//
// This code sits inside the safety envelope named on the repository's own card: a source
// is never destroyed until the replacement is provably faithful, and a wrong verdict is
// unrecoverable. A metadata step that half-applied and then let the swap proceed would
// publish a file whose permissions or owner nobody chose, with the source already gone.
// So every metadata failure - a source whose metadata cannot be READ included - is a
// FAILED SWAP: no rename, the temp discarded, the source byte-for-byte intact, and a
// recorded reason naming the step. There is exactly ONE exemption and it is narrow: an
// ownership change refused because this process lacks the privilege to make one. That is
// the shipped deployment (a container running as an ordinary user:, with no CAP_CHOWN),
// so it is the normal case and not an error condition - the swap proceeds with everything
// else carried, and the operator is told once for the run.
//
// # What is deliberately NOT carried
//
// ACLs and xattrs. Copying them correctly is a filesystem-specific problem this does not
// open, and docs/docker.md says so outright so an operator relying on POSIX ACLs is not
// left to infer it. The undo window's retained original is untouched too: it is a second
// hard link to the SOURCE inode, so it already has the source's metadata by construction
// and `restore` keeps publishing exactly the bytes and attributes it retained.

// metadataModeBits are the mode bits os.Chmod honours, and therefore the whole of what
// "the same file mode as the source" can mean here: the nine permission bits plus setuid,
// setgid and sticky. Masking to a SUBSET would be the original defect restated in
// miniature - a source at 04640 replaced by a file at 0640 has still had its mode silently
// changed by holdfast.
const metadataModeBits = fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky

// The metadata steps, named. Each name is written into the recorded reason of a failed
// swap, so an operator reading a failed row learns WHICH step could not be completed
// rather than that "the metadata" failed; they are read back by the fixtures that prove
// each failure is refused.
const (
	metadataStepRead    = "read the source's metadata"
	metadataStepOwner   = "apply the source's ownership to the replacement"
	metadataStepMode    = "apply the source's mode to the replacement"
	metadataStepModTime = "apply the source's modification time to the replacement"
)

// ownershipNotPreservedNotice is the report an unprivileged run owes its operator, and it
// is emitted EXACTLY ONCE per run however many files that run swaps.
//
// Once per run, rather than once per file, because the deployment this describes is the
// NORMAL one: a container running as an ordinary `user:` cannot chown at all, so a
// library-wide first pass would otherwise write one identical line per film and bury
// everything else in the log. Once per run rather than once per process, because `serve`
// runs a scan every scan_interval_sec and an operator who started the daemon a week ago
// would otherwise never hear it again.
//
// It is a constant so the fixture that counts it counts the thing the engine emits, not a
// literal that can drift away from it.
const ownershipNotPreservedNotice = "OWNERSHIP NOT PRESERVED: this process may not change a file's owner " +
	"(chown answered EPERM - it has neither CAP_CHOWN nor root), so every replacement this run publishes is " +
	"owned by the holdfast uid:gid rather than by the source's. The mode and the modification time ARE carried " +
	"and the swap is unaffected. Reported once for this run. Run holdfast as a user that already owns the media, " +
	"or grant it CAP_CHOWN, to carry ownership across as well."

// fileMetadata is what a swap carries from the source onto the replacement: the mode
// already masked to the bits os.Chmod honours, the modification time, and the ownership
// where the platform has one.
//
// HasOwner is false where this build cannot read an ownership at all (a platform with no
// ownership model - see metadata_other.go). It is not an error there and nothing is
// reported: the shipped artefact is a linux/amd64 + linux/arm64 image, and where the
// platform has no ownership model there is nothing to carry and the current behaviour
// stands.
type fileMetadata struct {
	Mode     fs.FileMode
	ModTime  time.Time
	UID, GID int
	HasOwner bool
}

// statMetadata reads one file's carriable metadata.
//
// It uses os.Stat rather than os.Lstat deliberately: it is only ever asked about a SOURCE,
// and a symlinked source is refused long before the swap by the SkipSymlink guard, so
// there is no link here to follow or to refuse.
func statMetadata(path string) (fileMetadata, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return fileMetadata{}, err
	}
	md := fileMetadata{Mode: fi.Mode() & metadataModeBits, ModTime: fi.ModTime()}
	md.UID, md.GID, md.HasOwner = ownerOf(fi)
	return md, nil
}

// The four seams, routed exactly as rename, restat and fsync already are. Production
// leaves each nil and calls the real thing; the fixtures substitute them because this gate
// runs as an unprivileged uid with no CAP_CHOWN, and a real chown to another uid - or a
// chmod this process is not allowed to make on a file it owns - is not exercisable here.

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

// chtimes sets the modification time and LEAVES THE ACCESS TIME ALONE - a zero time.Time
// tells os.Chtimes to leave that field unchanged. The criterion is about the modification
// time, and inventing an access time (or copying the source's, which the read of the
// source has just moved) would be changing a second attribute nobody asked for.
func (e *Engine) chtimes(path string, mtime time.Time) error {
	if e.chtimesFn != nil {
		return e.chtimesFn(path, mtime)
	}
	return os.Chtimes(path, time.Time{}, mtime)
}

// notPermitted reports whether an error is the kernel saying THIS PROCESS LACKS THE
// PRIVILEGE, and nothing else.
//
// It matches EPERM alone, never fs.ErrPermission. The two are not the same question here:
// chown(2) answers EPERM for "the calling process has no CAP_CHOWN and is not the owner"
// and EACCES for "search permission is denied on a component of the path prefix", and Go
// maps BOTH onto fs.ErrPermission. Only the first is the rootless case AC4 exempts; the
// second is a real failure about the path, and swallowing it would publish a replacement
// whose owner nobody chose.
func notPermitted(err error) bool { return errors.Is(err, syscall.EPERM) }

// carrySourceMetadata puts the source's mode, ownership and modification time onto the
// replacement at `replacement`, and returns the NAME OF THE STEP that failed plus its
// error, or "" and nil when the whole set was applied.
//
// The caller treats any non-nil error as a failed swap. This function therefore never
// leaves a decision to the caller that it is better placed to make: the one failure that
// is not a failed swap (an ownership change this process is not privileged to make) is
// absorbed here, reported here, and returns no error.
func (e *Engine) carrySourceMetadata(src, replacement string) (string, error) {
	md, err := e.statMetadata(src)
	if err != nil {
		return metadataStepRead, err
	}

	// OWNERSHIP FIRST, and the order is load-bearing rather than arbitrary: chown(2)
	// clears a file's set-user-ID and set-group-ID bits. Applying the mode first would
	// therefore silently drop exactly the bits the source was carrying, on the step whose
	// whole job is to stop the mode being silently changed.
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

	// LAST, so nothing above can disturb what it sets. chown and chmod both move ctime
	// and neither moves mtime, so the order is a belt-and-braces rather than a
	// correctness requirement - but the fsync that follows is a read of the file, and the
	// rename after it does not touch mtime either, so this is the last write to the
	// attribute and that is where it belongs.
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
