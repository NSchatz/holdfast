package startup

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// Class is the storage classification of one checked path. There are exactly
// three, and only a POSITIVE local identification counts as local.
type Class string

const (
	// Local: the platform positively identified the storage as a
	// recognised-local type (see LocalTypes).
	Local Class = "local"
	// NonLocal: the platform identified a type this build knows is
	// network-backed, and the type has a name to report.
	NonLocal Class = "non-local"
	// Undetermined: the lookup failed, returned no type, or returned a type this
	// build does not recognise. Undetermined is NOT local, and refuses the run
	// exactly as a detected network filesystem does.
	Undetermined Class = "undetermined"
	// Unclassified is the ZERO Class: a checked path that carries no storage
	// classification at all, because there is no storage there to classify.
	// Exactly one thing has none - a configured library root that DOES NOT
	// EXIST. That path is reported as missing, distinctly from permission
	// denial and distinctly from an undetermined type, and it is NEVER
	// classified `undetermined`: a lookup that failed because the path is not
	// there says nothing about storage, and reporting it under the same word as
	// a type holdfast could not determine tells the operator to go looking for a
	// filesystem problem that does not exist.
	//
	// It is not a fourth classification: a classification is exactly one of the
	// three above, and this is the absence of one. It is not local either, so
	// nothing that reads IsLocal has to know about it.
	Unclassified Class = ""
)

// IsLocal reports whether c is the one classification that permits a run with no
// opt-in. "Not local" is non-local OR undetermined.
func (c Class) IsLocal() bool { return c == Local }

// localTypes is the complete set of filesystem types THIS BUILD classifies
// `local`, reported at startup and restated in the shipped documentation (the
// two are held in agreement by a test, so a build whose set moves and a
// documentation file left unedited fail the aggregate check target).
//
// A type qualifies only if, for EVERY file on storage of that type, the storage
// is attached to the host holdfast runs on and no other host can modify that
// file through it. That is what the no-loss contract rests on: an atomic
// same-filesystem rename whose failure means it did not happen, a stat that can
// see a concurrent rewrite, and a SQLite WAL that works at all.
//
// Deliberately absent: any union or overlay filesystem, any in-memory filesystem
// and anything in user space (FUSE), because the name of such a type does not by
// itself say what storage is underneath it (see notLocalByConstruction).
var localTypes = map[string]bool{
	"btrfs": true,
	"ext2":  true,
	"ext3":  true,
	"ext4":  true,
	"f2fs":  true,
	"jfs":   true,
	"xfs":   true,
	"zfs":   true,
}

// networkTypes are types this build KNOWS are network-backed. The set exists so
// a refusal can name the filesystem it detected; it is not a completeness claim
// and nothing rests on it being complete, because a type absent from BOTH sets
// is `undetermined`, which refuses just the same.
var networkTypes = map[string]bool{
	"9p": true, "afp": true, "afs": true, "beegfs": true, "ceph": true,
	"cifs": true, "coda": true, "davfs": true, "gfs2": true, "glusterfs": true,
	"lustre": true, "ncpfs": true, "nfs": true, "nfs4": true, "ocfs2": true,
	"orangefs": true, "pvfs2": true, "smb3": true, "smbfs": true, "sshfs": true,
}

// LocalTypes returns the complete set of filesystem types this build classifies
// `local`, sorted. Startup prints it and the shipped documentation states it, so
// two builds recognising different sets are distinguishable from what each
// prints without anyone reading source or rebuilding.
func LocalTypes() []string {
	out := make([]string, 0, len(localTypes))
	for t := range localTypes {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// notLocalByConstruction reports whether a type name is one whose name does not
// by itself determine the backing storage: a union or overlay filesystem, an
// in-memory one, or a user-space (FUSE) one. Such a path is `undetermined` -
// never `local` - however ordinary the files under it look, because what is
// underneath an overlay or a FUSE mount may be anything at all, including a NAS.
func notLocalByConstruction(t string) string {
	switch t {
	case "overlay", "overlayfs", "aufs", "unionfs":
		return "a union or overlay filesystem does not say what storage is underneath it"
	case "tmpfs", "ramfs":
		return "an in-memory filesystem is not durable storage"
	case "fuse", "fuseblk":
		return "a user-space (FUSE) filesystem does not say what storage is underneath it"
	}
	if strings.HasPrefix(t, "fuse.") {
		return "a user-space (FUSE) filesystem does not say what storage is underneath it"
	}
	return ""
}

// classification is the outcome of classifying one path.
type classification struct {
	Class  Class
	Type   string // the type matched, on a local or non-local record
	Reason string // why it is undetermined
	Denied bool   // the lookup failed because the process may not inspect the path
}

// classify turns one filesystem-type lookup into a classification. The lookup's
// FAILURE modes are as load-bearing as its answers, so each is kept distinct:
//
//   - the process may not inspect the path: `undetermined` FOR REPORTING, with
//     Denied set, because the run is decided by the permission row and not by
//     this one, and no opt-in can lift it;
//   - mount information absent, unreadable or unparseable: `undetermined` for
//     every path whose type it would have settled;
//   - any other failure, no type at all, or a type this build does not
//     recognise: `undetermined` with that reason, and never the name of a
//     filesystem it did not detect.
func classify(typeName string, err error) classification {
	if err != nil {
		switch {
		case errors.Is(err, fs.ErrPermission):
			return classification{Class: Undetermined, Denied: true,
				Reason: "the process is not permitted to inspect this path"}
		case errors.Is(err, ErrMountInfoUnavailable):
			return classification{Class: Undetermined, Reason: ErrMountInfoUnavailable.Error()}
		default:
			return classification{Class: Undetermined,
				Reason: fmt.Sprintf("the filesystem-type lookup failed: %v", err)}
		}
	}
	t := strings.TrimSpace(typeName)
	if t == "" {
		return classification{Class: Undetermined, Reason: "the platform reported no filesystem type"}
	}
	if why := notLocalByConstruction(t); why != "" {
		return classification{Class: Undetermined,
			Reason: fmt.Sprintf("filesystem type %q is not evidence of local storage: %s", t, why)}
	}
	if localTypes[t] {
		return classification{Class: Local, Type: t}
	}
	if networkTypes[t] {
		return classification{Class: NonLocal, Type: t}
	}
	return classification{Class: Undetermined,
		Reason: fmt.Sprintf("filesystem type %q is not one this build recognises as local", t)}
}
