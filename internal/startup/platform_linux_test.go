//go:build linux

package startup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegionFSKey_PrefersThePublishedFilesystemIdOverThePerMountDevice is the
// unit half of the region key, and it is the reason one filesystem mounted twice
// collapses to one region: Linux derives an unpublished f_fsid from the BACKING
// device, so two mounts of one filesystem answer with the same id even where
// their device numbers differ. Keyed on st_dev alone the walk would descend both
// and enumerate one source twice.
func TestRegionFSKey_PrefersThePublishedFilesystemIdOverThePerMountDevice(t *testing.T) {
	twoMountsOfOneFilesystem := []string{
		regionFSKey(11, 22, true, 2049), // first mount, its own device number
		regionFSKey(11, 22, true, 2050), // second mount, a separate superblock
	}
	if twoMountsOfOneFilesystem[0] != twoMountsOfOneFilesystem[1] {
		t.Fatalf("two mounts of one filesystem keyed as two regions: %v", twoMountsOfOneFilesystem)
	}
	if regionFSKey(11, 22, true, 2049) == regionFSKey(33, 44, true, 2049) {
		t.Fatal("two different filesystems collapsed into one region")
	}
	// No published id: fall back to the device number, which still tells two
	// mounted filesystems apart.
	if got := regionFSKey(0, 0, true, 2049); got != "dev:2049" {
		t.Fatalf("fallback key = %q, want the device number", got)
	}
	if regionFSKey(0, 0, false, 7) == regionFSKey(0, 0, false, 8) {
		t.Fatal("the fallback collapsed two devices")
	}
}

// TestSystemPlatform_ReadsTheRealHost keeps the platform layer from being dead
// code behind the substituted seam: the real one stats, lists, resolves,
// classifies and answers the mount-point question on a real directory.
func TestSystemPlatform_ReadsTheRealHost(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Film.mkv"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := System(nil, nil)

	info, err := p.Inspect(dir)
	if err != nil || !info.IsDir {
		t.Fatalf("Inspect(%s) = %+v, %v; want a directory", dir, info, err)
	}
	if info.DevID == "" || info.Region.FS == "" || info.Region.Node == "" {
		t.Fatalf("Inspect returned no storage identity: %+v", info)
	}
	subInfo, err := p.Inspect(sub)
	if err != nil {
		t.Fatal(err)
	}
	if subInfo.Region == info.Region {
		t.Fatal("two distinct directories of one filesystem shared a region key")
	}
	if subInfo.DevID != info.DevID {
		t.Fatal("a subdirectory of one filesystem reported a different mounted filesystem")
	}

	ents, err := p.ReadDir(dir)
	if err != nil || len(ents) != 2 {
		t.Fatalf("ReadDir = %v, %v; want the two entries", ents, err)
	}
	if got, err := p.Resolve(dir); err != nil || got == "" {
		t.Fatalf("Resolve = %q, %v", got, err)
	}
	if _, err := p.Inspect(filepath.Join(dir, "nope")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing path reported %v, want fs.ErrNotExist - the decision reads that distinction", err)
	}
	typ, err := p.FSType(dir)
	if err != nil && !errors.Is(err, ErrMountInfoUnavailable) {
		t.Fatalf("FSType(%s) = %q, %v", dir, typ, err)
	}
	if _, err := p.MountPoint(dir); err != nil && !errors.Is(err, ErrMountInfoUnavailable) {
		t.Fatalf("MountPoint(%s) = %v", dir, err)
	}
	// The filesystem root of this container is a mount point in anyone's mount
	// table; a temp directory is not.
	if ok, err := p.MountPoint("/"); err == nil && !ok {
		t.Fatal("/ was not recognised as a mount point")
	}
}

// TestSystemPlatform_SeamsReplaceTheRealLookups: the two seams are what make
// every criterion testable on a gate with no network mount and no second
// filesystem, so they must actually take precedence.
func TestSystemPlatform_SeamsReplaceTheRealLookups(t *testing.T) {
	p := System(
		func(path string) (string, error) { return "nfs", nil },
		func(path string) (bool, error) { return true, nil },
	)
	if got, err := p.FSType("/anything"); got != "nfs" || err != nil {
		t.Fatalf("FSType = %q, %v; want the substituted lookup to answer", got, err)
	}
	if ok, err := p.MountPoint("/anything"); !ok || err != nil {
		t.Fatalf("MountPoint = %v, %v; want the substituted layout to answer", ok, err)
	}
}

// TestMountInfo_ParsesRealLinesAndRefusesADamagedTable. A half-read mount table
// would answer "not a mount point" for everything after the damage, so an
// unparseable file is an ERROR that classifies undetermined rather than a short
// list that silently answers no.
func TestMountInfo_ParsesRealLinesAndRefusesADamagedTable(t *testing.T) {
	good := strings.Join([]string{
		"23 28 0:21 / /proc rw,nosuid,nodev,noexec,relatime shared:12 - proc proc rw",
		"29 28 0:24 / /srv/my\\040media rw,relatime shared:15 - ext4 /dev/sdb1 rw",
		"31 29 0:25 /films /srv/my\\040media/tv rw,relatime - nfs4 nas:/films rw",
	}, "\n") + "\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	tbl, err := readMountInfo(path)
	if err != nil {
		t.Fatalf("readMountInfo: %v", err)
	}
	if !tbl.isMountPoint("/srv/my media") {
		t.Fatal("an escaped mount point was not unescaped")
	}
	if tbl.isMountPoint("/srv/my media/films") {
		t.Fatal("a path that is not a mount point was reported as one")
	}
	if got := tbl.typeFor("/srv/my media/tv/season1"); got != "nfs4" {
		t.Fatalf("typeFor under the deepest mount = %q, want nfs4", got)
	}
	if got := tbl.typeFor("/srv/my media/other"); got != "ext4" {
		t.Fatalf("typeFor = %q, want ext4", got)
	}

	bad := filepath.Join(dir, "damaged")
	if err := os.WriteFile(bad, []byte("23 28 0:21 / /proc rw\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readMountInfo(bad); err == nil {
		t.Fatal("a damaged mount table parsed cleanly; every path after the damage would silently answer no")
	}
	if _, err := readMountInfo(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("an absent mount table parsed cleanly")
	}
}

// TestSystemPlatform_TheStateDirectoryOnASymlinkedShareIsClassifiedByItsStorage
// is AC1's other subject - "a library root OR THE STATE DIRECTORY" - driven
// through the real platform layer, in the shape the research file calls the
// ordinary homelab reflex: state_dir pointed at a NAS share, here through a
// symbolic link, and not yet created. It is classified by the storage it WOULD
// be created on (Definitions), which is the longest existing prefix of its
// RESOLVED form, so the type lookup has to follow the link there too - or
// holdfast reports `local`, demands no opt-in, and opens a SQLite WAL on a
// network filesystem.
//
// The mount TABLE is a fixture read through mountInfoPath; every line of the
// lookup under test is production.
func TestSystemPlatform_TheStateDirectoryOnASymlinkedShareIsClassifiedByItsStorage(t *testing.T) {
	dir := realTempDir(t)
	nas := filepath.Join(dir, "nas")
	root := filepath.Join(dir, "media")
	for _, d := range []string{nas, root} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(dir, "share")
	if err := os.Symlink(nas, link); err != nil {
		t.Fatal(err)
	}
	mountinfoFixture(t, nas)

	stateDir := filepath.Join(link, "holdfast", "state")
	res := Run(Check{
		Roots:       []string{root},
		StateDir:    stateDir,
		IsMediaFile: mediaByExt,
		Platform:    System(nil, nil),
	})

	// Control: the library root, on the same temp filesystem but NOT through the
	// link, still classifies local - so this case is about the link and not about
	// the fixture blanketing everything.
	if rec, ok := recordFor(res, root); !ok || rec.Class != Local {
		t.Fatalf("the library root %s classified %+v, want local: the fixture is not wired in as intended", root, rec)
	}
	rec, ok := recordFor(res, stateDir)
	if !ok {
		t.Fatalf("no record for the state directory: %+v", res.Records)
	}
	if rec.Class != NonLocal || rec.Type != "nfs4" {
		t.Fatalf("the state directory %s classified %s (%s), want non-local (nfs4): it does not exist yet and the storage "+
			"it would be created on is %s, the nfs4 mount. record=%+v", stateDir, rec.Class, rec.Type, nas, rec)
	}
	if res.Start {
		t.Fatalf("the run started with the job store bound for a network filesystem, no opt-in declared: %+v", res.Causes)
	}
	if _, err := os.Stat(filepath.Join(nas, "holdfast")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused run left something behind on the state directory's storage: %v", err)
	}

	// AC11b: the declaration printed is one the malformed check accepts, and
	// feeding exactly it back starts the run against the same storage.
	decl := ""
	for _, c := range res.Causes {
		if c.Path == stateDir {
			decl = c.Declaration
		}
	}
	if decl == "" {
		t.Fatalf("no declaration was printed for the state directory: %+v", res.Causes)
	}
	again := Run(Check{
		Roots:        []string{root},
		StateDir:     stateDir,
		Declarations: []string{decl},
		IsMediaFile:  mediaByExt,
		Platform:     System(nil, nil),
	})
	if !again.Start {
		t.Fatalf("the declaration holdfast printed (%q) did not start the run: %+v", decl, again.Causes)
	}
}

// TestSystemFSType_ReportsMountInfoUnavailableRatherThanGuessing: where only the
// mount table could have settled a type, its absence is reported as such, which
// classifies the path undetermined and refuses the run unless it is opted in.
func TestSystemFSType_ReportsMountInfoUnavailableRatherThanGuessing(t *testing.T) {
	old := mountInfoPath
	mountInfoPath = filepath.Join(t.TempDir(), "absent")
	defer func() { mountInfoPath = old }()

	p := System(nil, nil)
	// A path whose magic number IS known still answers without the table (this
	// is why an ordinary local host starts with no /proc mounted).
	if _, err := p.MountPoint("/"); !errors.Is(err, ErrMountInfoUnavailable) {
		t.Fatalf("MountPoint with no mount table = %v, want ErrMountInfoUnavailable", err)
	}
	typ, err := p.FSType(t.TempDir())
	if err != nil && !errors.Is(err, ErrMountInfoUnavailable) {
		t.Fatalf("FSType = %q, %v; want either a magic-derived name or ErrMountInfoUnavailable", typ, err)
	}
	if err == nil && typ == "" {
		t.Fatal("FSType answered with no type and no error")
	}
}
