package startup

import (
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/NSchatz/holdfast/internal/fsclass"
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
	// Unclassified is the ZERO Class: a checked path with no storage classification,
	// because there is no storage there to classify. Exactly one thing has none - a
	// configured library root that DOES NOT EXIST - and it is reported as missing,
	// distinctly from permission denial and NEVER as `undetermined`: a lookup that
	// failed because the path is not there says nothing about storage, and that word
	// would send the operator after a filesystem problem that does not exist. It is
	// the absence of a classification rather than a fourth one, and not local.
	Unclassified Class = ""
)

// IsLocal reports whether c is the one classification that permits a run with no
// opt-in. "Not local" is non-local OR undetermined.
func (c Class) IsLocal() bool { return c == Local }

// The set of filesystem types THIS BUILD classifies `local` lives in ONE place,
// `internal/fsclass`, and this package READS it rather than declaring a second. It has
// to: the startup check and the swap-time lookups the engine makes for itself are two
// consumers of one question, and two sets that could drift would mean a run that started
// because startup called a path local and then parked every swap on it, or the reverse.
//
// A type qualifies only if, for EVERY file on storage of that type, the storage is
// attached to the host holdfast runs on and no other host can modify that file through
// it. That is what an atomic same-filesystem rename whose failure means it did not
// happen, a stat that can see a concurrent rewrite, and a working SQLite WAL all rest
// on. Any union, overlay, in-memory or user-space (FUSE) type is deliberately absent:
// its name does not by itself say what storage is underneath
// (fsclass.NotLocalByConstruction).

// LocalTypes returns the complete set of filesystem types this build classifies `local`,
// sorted. Startup prints it and the shipped documentation states it, held in agreement
// by a test the aggregate check target runs, so two builds recognising different sets
// are distinguishable without anyone reading source.
func LocalTypes() []string { return fsclass.RecognisedLocalTypes() }

// classification is the outcome of classifying one path.
type classification struct {
	Class  Class
	Type   string // the type matched, on a local or non-local record
	Reason string // why it is undetermined
	Denied bool   // the lookup failed because the process may not inspect the path
}

// classify turns one filesystem-type lookup into a classification. The lookup's FAILURE
// modes are as load-bearing as its answers, so each stays distinct:
//
//   - the process may not inspect the path: `undetermined` FOR REPORTING, with Denied
//     set, because the permission row decides the run and no opt-in lifts it;
//   - mount information absent, unreadable or unparseable: `undetermined` for every
//     path whose type it would have settled;
//   - any other failure, no type at all, or a type this build does not recognise:
//     `undetermined` with that reason, never the name of a filesystem it did not detect.
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
	if why := fsclass.NotLocalByConstruction(t); why != "" {
		return classification{Class: Undetermined,
			Reason: fmt.Sprintf("filesystem type %q is not evidence of local storage: %s", t, why)}
	}
	if fsclass.IsRecognisedLocal(t) {
		return classification{Class: Local, Type: t}
	}
	if fsclass.IsNetworkBacked(t) {
		return classification{Class: NonLocal, Type: t}
	}
	return classification{Class: Undetermined,
		Reason: fmt.Sprintf("filesystem type %q is not one this build recognises as local", t)}
}
