//go:build linux

package startup

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression probe for S0007-holdfast-startup-1, refuter finding F70 (impl gate,
// ordinal 2).
//
// THE PRODUCTION filesystem-type lookup classifies the storage the LEXICAL
// spelling of a path sits on, not the storage the path is actually on. system
// .FSType consults the mount table FIRST and mountTable.typeFor is purely
// lexical (`e.point == p || lexicallyBeneath(p, e.point)`), with no link
// resolution on either side. /proc/self/mountinfo never lists a symlink - the
// kernel resolves paths before it mounts anything - so a path reached through a
// symbolic link matches no mount point but its own ancestors', and the type
// returned is the ancestor's. The statfs magic number, which DOES follow the
// link, is only consulted when typeFor answers "" - and typeFor can never answer
// "" while the table is readable, because "/" is a mount point in every mount
// table there is.
//
// The direction is the fail-OPEN one this whole item exists to close: a library
// root or state directory spelled through a symlink onto a NAS share is reported
// `local`, named with the LOCAL filesystem's type, and the run STARTS with no
// opt-in.
//
// Every criterion this violates reads on "the storage a path is on":
//
//	AC1 [inherited] - WHEN a library root or the state directory is not on a
//	local filesystem THE SYSTEM SHALL name the path and the detected filesystem
//	at startup and SHALL refuse to run unless the operator has explicitly opted
//	in.
//
//	Definitions/Classification - `local` (the platform positively identifies THE
//	STORAGE as a recognised-local type).
//
// The spec contemplates symlinked roots as a first-class shape rather than an
// exotic one: AC11's own worked example is "a configured root `/srv/media` that
// is itself a symlink to `/mnt/pool/media`", and AC2f is written for links met
// under a symlinked root.
//
// No real network mount is needed and none is used: the mount TABLE is a
// fixture, read through mountInfoPath - the variable the implementation itself
// declares so its absent-and-unparseable paths have a test - while every line of
// the lookup under test is the production one.

// mountinfoFixture writes a synthetic /proc/self/mountinfo naming exactly one
// non-local mount and points the production reader at it.
func mountinfoFixture(t *testing.T, nasMount string) {
	t.Helper()
	lines := []string{
		"1 0 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw",
		fmt.Sprintf("2 1 0:42 / %s rw,relatime - nfs4 nas:/films rw", escapeMountField(nasMount)),
	}
	path := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := mountInfoPath
	mountInfoPath = path
	t.Cleanup(func() { mountInfoPath = old })
}

func escapeMountField(s string) string {
	r := strings.NewReplacer(" ", `\040`, "\t", `\011`, "\n", `\012`, `\`, `\134`)
	return r.Replace(s)
}

// realTempDir is t.TempDir() with every symbolic link already resolved, so the
// only link in play is the one the case creates.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRegress0007F70_TheTypeLookupFollowsTheLinkTheStatDoes is the unit half:
// one directory, two spellings of it, one type.
func TestRegress0007F70_TheTypeLookupFollowsTheLinkTheStatDoes(t *testing.T) {
	dir := realTempDir(t)
	nas := filepath.Join(dir, "nas")
	if err := os.Mkdir(nas, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "media")
	if err := os.Symlink(nas, link); err != nil {
		t.Fatal(err)
	}
	mountinfoFixture(t, nas)

	p := System(nil, nil)

	// Control, so the case cannot pass vacuously: the fixture IS wired in and
	// the real spelling of the same directory answers with the mount's type.
	direct, err := p.FSType(nas)
	if err != nil {
		t.Fatalf("FSType(%s) = %v", nas, err)
	}
	if direct != "nfs4" {
		t.Fatalf("FSType(%s) = %q, want nfs4 - the mount-table fixture is not wired in, so this case proves nothing", nas, direct)
	}

	// The defect. os.Stat, syscall.Statfs, rename(2) and every byte holdfast
	// writes through this path follow the link; the classification does not.
	got, err := p.FSType(link)
	if err != nil {
		t.Fatalf("FSType(%s) = %v", link, err)
	}
	if got != "nfs4" {
		t.Fatalf("FSType(%s) = %q, want nfs4: %s is a symlink to %s, which is the nfs4 mount. "+
			"The type lookup answered for the storage the LINK's spelling sits on rather than the storage the path is on, "+
			"so a library root or state directory symlinked onto a NAS share classifies `local` and the run starts with no opt-in "+
			"(AC1, Definitions/Classification).", link, got, link, nas)
	}
}

// TestRegress0007F70_ASymlinkedRootOnANetworkShareStartsWithNoOptIn is the
// operator-visible half, driven through the whole check on the real platform:
// the configured library root is a symlink onto the NAS mount, and holdfast
// reports it `local`, names the LOCAL filesystem type, demands no opt-in and
// starts.
func TestRegress0007F70_ASymlinkedRootOnANetworkShareStartsWithNoOptIn(t *testing.T) {
	dir := realTempDir(t)
	nas := filepath.Join(dir, "nas")
	if err := os.MkdirAll(filepath.Join(nas, "films"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nas, "films", "Film.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "media")
	if err := os.Symlink(nas, root); err != nil {
		t.Fatal(err)
	}
	mountinfoFixture(t, nas)

	res := Run(Check{
		Roots:       []string{root},
		StateDir:    filepath.Join(dir, "state"),
		IsMediaFile: mediaByExt,
		Platform:    System(nil, nil),
	})

	rec, ok := recordFor(res, root)
	if !ok {
		t.Fatalf("no record for the configured root: %+v", res.Records)
	}
	if rec.Class == Local {
		t.Fatalf("the configured library root %s classified %s (%s) - it is a symlink to %s, which is on the nfs4 mount. "+
			"AC1: WHEN a library root ... is not on a local filesystem THE SYSTEM SHALL name the path and the detected "+
			"filesystem at startup and SHALL refuse to run unless the operator has explicitly opted in. "+
			"record=%+v", root, rec.Class, rec.Type, nas, rec)
	}
	if res.Start {
		t.Fatalf("the run STARTED on a library root that sits on a network filesystem, with no opt-in declared "+
			"(row %d, causes %+v, records %+v)", res.Row, res.Causes, res.Records)
	}
}

// TestRegress0007F70_ABindMountUnderASymlinkedRootGetsNoRecord is the second
// limb of the same lexical/resolved mismatch: mountTable.isMountPoint is the set
// of mount points spelled literally, so a mount reached through a symlinked root
// is not recognised as one at all.
//
//	AC2d1 - WHEN one filesystem is mounted at two paths beneath a configured
//	library root - a bind mount, or the same device mounted twice - THE SYSTEM
//	SHALL emit a classification record for each of those mount points, and each
//	is a checked path an opt-in must cover in its own right.
//
// A mount whose device number differs from its parent's is still caught by the
// first limb of isMountPoint; a BIND, which shares its parent's device number,
// is visible to the mount table alone - and that table is consulted by literal
// spelling.
func TestRegress0007F70_ABindMountUnderASymlinkedRootGetsNoRecord(t *testing.T) {
	dir := realTempDir(t)
	nas := filepath.Join(dir, "nas")
	if err := os.Mkdir(nas, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "media")
	if err := os.Symlink(nas, link); err != nil {
		t.Fatal(err)
	}
	mountinfoFixture(t, nas)

	p := System(nil, nil)
	if ok, err := p.MountPoint(nas); err != nil || !ok {
		t.Fatalf("MountPoint(%s) = %v, %v; the fixture is not wired in, so this case proves nothing", nas, ok, err)
	}
	ok, err := p.MountPoint(link)
	if err != nil {
		t.Fatalf("MountPoint(%s) = %v", link, err)
	}
	if !ok {
		t.Fatalf("MountPoint(%s) = false: %s is a symlink to the mount point %s, so the walk reaching a mount by a "+
			"spelling that passes through a link emits no record for it and no opt-in is ever demanded for that mount (AC2d1, AC2b).",
			link, link, nas)
	}
}
