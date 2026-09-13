// Package fsclass answers ONE question, at ONE moment: what kind of storage is this path
// on? The whole no-loss contract is specified against LOCAL POSIX rename semantics, and
// `rename(2)` is explicit about the gap: "On NFS filesystems, you can not assume that if
// the operation failed, the file was not renamed" - so a swap that reports an error on a
// network filesystem may nonetheless have replaced the source, and a "swap error, source
// untouched" log line could be simply false.
//
// A Classification is one point-in-time lookup, always ABOUT a moment, and is exactly one
// of Local (a positively identified recognised-local type), NonLocal (a type this build
// knows is network-backed, with a name to report) or Undetermined (the lookup failed,
// returned no type, returned one this build does not recognise, or was denied).
//
// The fail-safe is asymmetric on purpose: only a POSITIVE local identification counts as
// local, so NonLocal and Undetermined are both not local (see IsLocal). A false warning
// costs an operator an extra look; a false clear costs them a film.
//
// The two enumerations below are the WHOLE of what this build calls local and what it
// knows to be network-backed, and every classification the program makes reads them: the
// whole-run startup check, and the swap-time and guard-time lookups the engine makes for
// itself. Two sets that could drift would mean a run that started because startup called a
// path local and then parked every swap on it, or the reverse.
//
// Adding a type is a decision, not a tweak.
// recognisedLocal is AUTHORITATIVE and NOT EMPTY: a build consulting a set nobody
// populated would satisfy every rule here while never once reporting a source untouched.
// An ADDITION means a filesystem whose storage is normally attached to the host holdfast
// runs on rather than served to it over a network, and it judges additions only - a type
// name alone cannot say whether this host re-exports the filesystem or another host has it
// multi-attached, so no name in the list promises exclusivity. Adding one changes the
// list, the tests that pin it AND the shipped documentation that states it
// (docs/filesystem.md, held in agreement by the gate), and the reasoning applied SHALL be
// recorded beside the entry.
package fsclass

import (
	"fmt"
	"sort"
	"strings"
)

// Class is the storage classification of a path at one moment.
type Class string

// The three classes. Everything the platform cannot positively identify as
// recognised-local or known-network lands on Undetermined; there is no fourth.
const (
	Local        Class = "local"
	NonLocal     Class = "non-local"
	Undetermined Class = "undetermined"
)

// Classification is one point-in-time answer, carrying the type name the lookup produced
// so a report can NAME it: a bare "non-local" tells an operator nothing actionable. Type
// is "" only when the lookup produced no name at all.
type Classification struct {
	Class Class
	Type  string
	// Reason says WHY an undetermined classification is undetermined, so a report never
	// leaves the operator guessing.
	Reason string
	// Err is the lookup error when there was one, kept for reporting; it never changes
	// the Class, which is already Undetermined.
	Err error
}

// IsLocal reports whether this is a POSITIVE local identification. Everything else -
// network-backed, unrecognised, unreadable, denied - is not local, which is the fail-safe
// the whole package exists for.
func (c Classification) IsLocal() bool { return c.Class == Local }

// String renders the classification for a log line or a stored record.
func (c Classification) String() string {
	if c.Type == "" {
		return string(c.Class)
	}
	return string(c.Class) + " (" + c.Type + ")"
}

// recognisedLocal is the enumeration of filesystem types this build classifies Local. The
// floor is not a guess: docs/docker.md commits in the repository's own shipped words to
// ext4 / XFS / Btrfs / ZFS as the local side of holdfast's durability limitation, and
// ext2, ext3, f2fs and jfs sit beside them on the reasoning docs/filesystem.md states -
// for every file on storage of one of these types, the storage is attached to the host
// holdfast runs on rather than served to it over a network.
//
// overlay, tmpfs and anything in user space are deliberately in NEITHER enumeration (see
// NotLocalByConstruction): not network-backed, but not settled as host-attached either, so
// they fall to Undetermined and therefore to not-local. That is the fail-safe working.
var recognisedLocal = map[string]bool{
	"btrfs": true,
	"ext2":  true,
	"ext3":  true,
	"ext4":  true,
	"f2fs":  true,
	"jfs":   true,
	"xfs":   true,
	"zfs":   true,
}

// networkBacked is the enumeration of filesystem types this build KNOWS are served over a
// network. A type here is reported by name, because "your library is on nfs" is actionable
// and "your library is not local" is not. It is NOT a completeness claim and nothing rests
// on it being complete: a type absent from both enumerations is Undetermined, which is not
// local just the same. Every spelling a real mount can report is listed rather than one
// standing for its family.
var networkBacked = map[string]bool{
	"9p": true, "afp": true, "afs": true, "beegfs": true, "ceph": true,
	"cifs": true, "coda": true, "davfs": true, "gfs2": true, "glusterfs": true,
	"lustre": true, "ncpfs": true, "nfs": true, "nfs4": true, "ocfs2": true,
	"orangefs": true, "pvfs2": true, "smb2": true, "smb3": true, "smbfs": true,
	"sshfs": true,
}

// RecognisedLocalTypes returns the recognised-local enumeration, sorted. It exists so a
// test can assert the set is NON-EMPTY and names what the shipped documentation names - a
// silently emptied enumeration would otherwise pass every behavioural test while never
// reporting a source untouched - and so startup can print the set a binary carries.
func RecognisedLocalTypes() []string { return sortedKeys(recognisedLocal) }

// NetworkBackedTypes returns the network-backed enumeration, sorted.
func NetworkBackedTypes() []string { return sortedKeys(networkBacked) }

// IsRecognisedLocal reports whether this build classifies a type name Local.
func IsRecognisedLocal(typeName string) bool { return recognisedLocal[typeName] }

// IsNetworkBacked reports whether this build KNOWS a type name to be network-backed.
func IsNetworkBacked(typeName string) bool { return networkBacked[typeName] }

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// NotLocalByConstruction reports whether a type name is one that does not by itself
// determine the backing storage - a union or overlay filesystem, an in-memory one, or a
// user-space (FUSE) one - and returns the reason when it is. Such a path is Undetermined,
// never Local, however ordinary the files under it look, because what is underneath may be
// anything at all, including a NAS. It is checked BEFORE the enumerations, so a type that
// ever appeared in both places still cannot be waved through.
func NotLocalByConstruction(typeName string) string {
	switch typeName {
	case "overlay", "overlayfs", "aufs", "unionfs":
		return "a union or overlay filesystem does not say what storage is underneath it"
	case "tmpfs", "ramfs":
		return "an in-memory filesystem is not durable storage"
	case "fuse", "fuseblk":
		return "a user-space (FUSE) filesystem does not say what storage is underneath it"
	}
	if strings.HasPrefix(typeName, "fuse.") {
		return "a user-space (FUSE) filesystem does not say what storage is underneath it"
	}
	return ""
}

// ClassifyType applies the enumerations above to one lookup result. It is the ONLY place a
// class is decided, so every lookup in the program lands on the same answer for the same
// type name. The rule, exactly: Local if and only if recognisedLocal carries the type and
// it is not not-local by construction; NonLocal if networkBacked carries it; Undetermined
// in every other case, including a lookup that errored or returned nothing.
func ClassifyType(typeName string, err error) Classification {
	if err != nil {
		return Classification{Class: Undetermined, Type: typeName, Err: err,
			Reason: fmt.Sprintf("the filesystem-type lookup failed: %v", err)}
	}
	t := strings.TrimSpace(typeName)
	switch {
	case t == "":
		return Classification{Class: Undetermined, Reason: "the platform reported no filesystem type"}
	case NotLocalByConstruction(t) != "":
		return Classification{Class: Undetermined, Type: t,
			Reason: fmt.Sprintf("filesystem type %q is not evidence of local storage: %s", t, NotLocalByConstruction(t))}
	case recognisedLocal[t]:
		return Classification{Class: Local, Type: t}
	case networkBacked[t]:
		return Classification{Class: NonLocal, Type: t}
	default:
		return Classification{Class: Undetermined, Type: t,
			Reason: fmt.Sprintf("filesystem type %q is not one this build recognises as local", t)}
	}
}

// Lookup is the SEAM. It returns the filesystem type NAME of the storage path is on.
// Production wires LookupType (a real statfs); a test substitutes a function returning
// "ext4", "nfs", an unrecognised string or an error, which is the only way to exercise the
// network paths on a gate that has no network mount. A substituted Lookup does NOT bypass
// the enumeration - it supplies a type NAME and ClassifyType still decides - so a suite
// built entirely on substituted lookups still fails against an emptied recognisedLocal.
type Lookup func(path string) (string, error)

// Of classifies the storage path is on using lookup, defaulting to the real one when
// lookup is nil.
func Of(lookup Lookup, path string) Classification {
	if lookup == nil {
		lookup = LookupType
	}
	return ClassifyType(lookup(path))
}

// unknownType renders a filesystem magic this build has no name for. It is deliberately a
// NAME rather than an empty string: "the platform told us something we do not recognise"
// and "the platform told us nothing" are different facts, and an operator wants the number.
func unknownType(magic int64) string { return fmt.Sprintf("unknown(0x%x)", uint64(magic)) }
