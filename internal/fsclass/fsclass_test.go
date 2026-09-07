package fsclass

// The REAL classification, with NOTHING substituted.
//
// Every other test of the failed-swap contract substitutes the filesystem-type lookup,
// because the gate has no network mount and never will. That makes this file the one
// that has to exist: a suite made entirely of substituted lookups reporting "ext4" is
// GREEN against a build whose recognised-local enumeration is empty - such a build would
// classify every real library not-local, never report a source untouched, and park every
// failed swap, while passing everything else. So these tests key on TYPES, run against
// the enumerations this build actually carries, and would red on an emptied set.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnumeration_IsNotEmptyAndCarriesTheDocumentedLocalTypes pins the floor. The four
// types are not a preference: docs/docker.md's own "Known limitations" already commits,
// in the repository's shipped words, to ext4/XFS/Btrfs/ZFS as the LOCAL side of
// holdfast's durability limitation. This build classifies at least what it documents;
// the additions beside them (ext2, ext3, f2fs, jfs) are the ones docs/filesystem.md
// states, on the reasoning recorded there and in fsclass.go.
func TestEnumeration_IsNotEmptyAndCarriesTheDocumentedLocalTypes(t *testing.T) {
	local := RecognisedLocalTypes()
	if len(local) == 0 {
		t.Fatal("the recognised-local enumeration is EMPTY - such a build satisfies every " +
			"behavioural rule while never once reporting a source untouched, and parks every failed swap")
	}
	for _, want := range []string{"ext4", "xfs", "btrfs", "zfs"} {
		if !contains(local, want) {
			t.Errorf("recognised-local is missing %q; it carries %v", want, local)
		}
	}
	network := NetworkBackedTypes()
	if !contains(network, "nfs") {
		t.Errorf("network-backed does not know NFS; it carries %v", network)
	}
	// SMB/CIFS is one protocol family with several Linux client spellings; knowing any
	// of them is knowing SMB/CIFS, and every spelling the lookup can produce must be in
	// the set or a real SMB mount would classify undetermined by accident.
	for _, want := range []string{"cifs", "smb2", "smbfs"} {
		if !contains(network, want) {
			t.Errorf("network-backed is missing the SMB/CIFS spelling %q; it carries %v", want, network)
		}
	}
	// The two enumerations must not overlap: a type in both would make the class depend
	// on the order the code happens to test them in.
	for _, l := range local {
		if contains(network, l) {
			t.Errorf("%q is in BOTH enumerations", l)
		}
	}
}

// TestClassifyType_EveryRecognisedLocalTypeClassifiesLocal walks the enumeration itself
// rather than a hardcoded list, so emptying the set does not merely change an
// expectation - it removes the evidence.
func TestClassifyType_EveryRecognisedLocalTypeClassifiesLocal(t *testing.T) {
	types := RecognisedLocalTypes()
	if len(types) == 0 {
		t.Fatal("no recognised-local types to classify")
	}
	for _, typ := range types {
		got := ClassifyType(typ, nil)
		if got.Class != Local {
			t.Errorf("ClassifyType(%q) = %v, want %v", typ, got.Class, Local)
		}
		if !got.IsLocal() {
			t.Errorf("ClassifyType(%q).IsLocal() = false", typ)
		}
		if got.Type != typ {
			t.Errorf("ClassifyType(%q).Type = %q, want the type named back", typ, got.Type)
		}
	}
}

// TestClassifyType_NetworkTypesAreNonLocalAndNamed - a report that says "not local" and
// nothing else is not actionable; "your library is on nfs" is.
func TestClassifyType_NetworkTypesAreNonLocalAndNamed(t *testing.T) {
	for _, typ := range NetworkBackedTypes() {
		got := ClassifyType(typ, nil)
		if got.Class != NonLocal {
			t.Errorf("ClassifyType(%q) = %v, want %v", typ, got.Class, NonLocal)
		}
		if got.Type != typ {
			t.Errorf("ClassifyType(%q) did not name the type back (got %q)", typ, got.Type)
		}
		if got.IsLocal() {
			t.Errorf("ClassifyType(%q).IsLocal() = true", typ)
		}
		if !strings.Contains(got.String(), typ) {
			t.Errorf("ClassifyType(%q).String() = %q, want the type named", typ, got.String())
		}
	}
}

// TestClassifyType_UnrecognisedIsUndeterminedAndThereforeNotLocal is the phase's
// fail-safe: only a POSITIVE local identification counts as local. overlayfs is the
// deliberate case - not network-backed, but not settled as host-attached-and-exclusive
// either (its layers can themselves be remote), so it falls here rather than being
// waved through.
func TestClassifyType_UnrecognisedIsUndeterminedAndThereforeNotLocal(t *testing.T) {
	for _, typ := range []string{"overlay", "overlayfs", "tmpfs", "ramfs", "fuse", "fuse.sshfs",
		"sdcardfs", "squashfs", "unknown(0xdeadbeef)"} {
		got := ClassifyType(typ, nil)
		if got.Class != Undetermined {
			t.Errorf("ClassifyType(%q) = %v, want %v", typ, got.Class, Undetermined)
		}
		if got.IsLocal() {
			t.Errorf("ClassifyType(%q) counted as local - an unrecognised type must not", typ)
		}
	}
	// A type the platform did not name at all, and a lookup that failed or was denied,
	// are both undetermined and both not local.
	if got := ClassifyType("", nil); got.Class != Undetermined || got.IsLocal() {
		t.Errorf("ClassifyType(\"\") = %v, want undetermined", got.Class)
	}
	denied := errors.New("permission denied")
	got := ClassifyType("", denied)
	if got.Class != Undetermined || got.IsLocal() {
		t.Errorf("a failed lookup classified %v, want undetermined", got.Class)
	}
	if !errors.Is(got.Err, denied) {
		t.Errorf("the lookup error was dropped: %v", got.Err)
	}
}

// TestOf_UsesTheRealLookupWhenNoneIsSubstituted proves the seam defaults to the real
// statfs rather than to a stub, and that the real lookup answers about a path that
// exists and fails about one that does not. It deliberately asserts nothing about WHICH
// type the gate's own storage is - that is the runner's business and would make the test
// a report about the CI machine.
func TestOf_UsesTheRealLookupWhenNoneIsSubstituted(t *testing.T) {
	dir := t.TempDir()
	got := Of(nil, dir)
	if got.Err != nil {
		t.Fatalf("the real lookup failed on a directory that exists: %v", got.Err)
	}
	if got.Type == "" {
		t.Error("the real lookup named no type for a path that exists")
	}
	switch got.Class {
	case Local, NonLocal, Undetermined:
	default:
		t.Errorf("the real lookup produced class %q, which is not one of the three", got.Class)
	}
	// A path that is not there: the lookup fails, and a failed lookup is undetermined
	// and therefore not local - never an error the caller has to remember to handle.
	gone := Of(nil, filepath.Join(dir, "no-such-file"))
	if gone.Class != Undetermined || gone.IsLocal() {
		t.Errorf("a lookup of a missing path classified %v, want undetermined", gone.Class)
	}
	if gone.Err == nil {
		t.Error("a lookup of a missing path reported no error")
	}
	if !errors.Is(gone.Err, os.ErrNotExist) {
		t.Errorf("unexpected lookup error for a missing path: %v", gone.Err)
	}
}

// TestOf_ASubstitutedLookupStillGoesThroughTheEnumeration is what keeps the rest of the
// suite honest: a substituted lookup supplies a type NAME, never a class, so every test
// that injects "ext4" is still asserting against this build's real enumeration.
func TestOf_ASubstitutedLookupStillGoesThroughTheEnumeration(t *testing.T) {
	fake := func(string) (string, error) { return "ext4", nil }
	if got := Of(fake, "/anywhere"); got.Class != Local {
		t.Errorf("substituted ext4 classified %v, want local", got.Class)
	}
	fakeNFS := func(string) (string, error) { return "nfs", nil }
	if got := Of(fakeNFS, "/anywhere"); got.Class != NonLocal {
		t.Errorf("substituted nfs classified %v, want non-local", got.Class)
	}
	fakeErr := func(string) (string, error) { return "", errors.New("statfs: denied") }
	if got := Of(fakeErr, "/anywhere"); got.Class != Undetermined {
		t.Errorf("a substituted failing lookup classified %v, want undetermined", got.Class)
	}
}

func contains(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
