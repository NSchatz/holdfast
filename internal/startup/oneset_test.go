package startup

import (
	"testing"

	"github.com/NSchatz/holdfast/internal/fsclass"
)

// There is exactly ONE enumeration of the filesystem types this build classifies
// local, and this file is what keeps that true.
//
// Two consumers ask the same question at different moments: the whole-run startup
// check here, and the swap-time / guard-time lookups the engine makes for itself
// (internal/engine, via internal/fsclass). If either grew a set of its own they could
// drift, and the drift is not cosmetic in either direction - a run that STARTED
// because startup called a path local and then parked every swap on it, or a run that
// was refused as not-local and would have reported a source untouched. So this package
// reads fsclass's enumeration rather than declaring one, and the tests below fail if a
// second declaration is ever introduced, because the two classifications would then
// disagree for the type it added or dropped.

// TestOneSet_StartupClassifiesExactlyWhatTheEngineDoes drives every type in BOTH
// enumerations, plus the by-construction cases and an unrecognised name, through the
// two classification paths and demands the same class from each.
func TestOneSet_StartupClassifiesExactlyWhatTheEngineDoes(t *testing.T) {
	var types []string
	types = append(types, fsclass.RecognisedLocalTypes()...)
	types = append(types, fsclass.NetworkBackedTypes()...)
	types = append(types,
		"overlay", "overlayfs", "aufs", "unionfs", "tmpfs", "ramfs",
		"fuse", "fuseblk", "fuse.sshfs", "sdcardfs", "squashfs", "unknown(0xdeadbeef)")

	if len(types) < 20 {
		t.Fatalf("only %d types to compare - an emptied enumeration would make this test vacuous", len(types))
	}
	for _, typ := range types {
		startupSays := classify(typ, nil)
		engineSays := fsclass.ClassifyType(typ, nil)
		if string(startupSays.Class) != string(engineSays.Class) {
			t.Errorf("%q: startup classifies %q, the engine %q - two enumerations have drifted apart",
				typ, startupSays.Class, engineSays.Class)
		}
		if startupSays.Class == Local && startupSays.Type != typ {
			t.Errorf("%q: a local classification did not name the type back (%q)", typ, startupSays.Type)
		}
	}
}

// TestOneSet_TheReportedSetIsTheEnumeration: what startup PRINTS is the set the engine
// classifies against, not a copy of it.
func TestOneSet_TheReportedSetIsTheEnumeration(t *testing.T) {
	reported := LocalTypes()
	enumeration := fsclass.RecognisedLocalTypes()
	if len(reported) == 0 {
		t.Fatal("startup reports an empty local set")
	}
	if len(reported) != len(enumeration) {
		t.Fatalf("startup reports %v; the enumeration is %v", reported, enumeration)
	}
	for i := range reported {
		if reported[i] != enumeration[i] {
			t.Fatalf("startup reports %v; the enumeration is %v", reported, enumeration)
		}
	}
	// And every reported type really does classify local through the engine's path,
	// so "reported" cannot become a list nothing consults.
	for _, typ := range reported {
		if !fsclass.ClassifyType(typ, nil).IsLocal() {
			t.Errorf("startup reports %q as local but the engine does not classify it local", typ)
		}
	}
}
