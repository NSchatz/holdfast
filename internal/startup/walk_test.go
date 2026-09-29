package startup

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/NSchatz/holdfast/internal/config"
)

// enumerated mirrors exactly what the scan does with the walk's coverage: it
// lists the directories the walk traversed SUCCESSFULLY, with no recursion of
// its own, and takes the media files by name. It is the observable form of "no
// source is enumerated from a directory this run's startup walk did not
// traverse" (AC2f1), and the engine's own enumerate() is proved to do the same
// thing in internal/engine.
func enumerated(f *fakeFS, res Result) []string {
	var out []string
	for _, dir := range res.Coverage {
		ents, err := f.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range ents {
			if e.IsDir {
				continue
			}
			if mediaByExt(e.Name) {
				out = append(out, filepath.Join(dir, e.Name))
			}
		}
	}
	sort.Strings(out)
	return out
}

func wantEnumerated(t *testing.T, f *fakeFS, res Result, want ...string) {
	t.Helper()
	got := enumerated(f, res)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sources enumerated = %v, want %v (coverage %v)", got, want, res.Coverage)
	}
}

func recordPaths(res Result) []string {
	var out []string
	for _, r := range res.Records {
		out = append(out, r.Path)
	}
	sort.Strings(out)
	return out
}

// TestWalk_IsExhaustiveOverSubMountsAtAnyDepth: a mounted filesystem beneath a
// root is found at ANY depth, whether or not a source lies on it and whether or
// not it sits directly under the root, and the decision accounts for all of them
// (AC2b, AC2d).
func TestWalk_IsExhaustiveOverSubMountsAtAnyDepth(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mount("/srv/media/tv", "ext4")
	f.mount("/srv/media/tv/archive", "nfs")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	want := []string{"/srv/media", "/srv/media/tv", "/srv/media/tv/archive", "/var/state"}
	if got := recordPaths(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %v, want %v", got, want)
	}
	// The mount two levels down is named BOTH as the walk reached it and in
	// resolved form, and it is what refuses the run.
	arch, _ := recordFor(res, "/srv/media/tv/archive")
	if arch.Kind != KindMount || arch.Class != NonLocal || arch.Resolved != "/srv/media/tv/archive" {
		t.Fatalf("archive record = %+v, want a non-local mount record naming its resolved form", arch)
	}
	if res.Start || res.Row != 4 || !hasCause(res, CauseNotLocal, "/srv/media/tv/archive") {
		t.Fatalf("a network sub-mount two levels down did not refuse the run: row %d, causes %+v", res.Row, res.Causes)
	}
}

// TestWalk_MountBeneathARootRefusesWithoutAnOptInAndStartsWithOne, and an opt-in
// naming only the ROOT does not cover a mount beneath it: an opt-in covers the
// paths it names and no others (AC2c, AC2d1, AC8, AC9, AC10).
func TestWalk_MountBeneathARootRefusesWithoutAnOptInAndStartsWithOne(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mount("/srv/media/tv", "nfs")
		f.mkfile("/srv/media/tv/Show.mkv")
		f.mkdir("/var/state")
		return f
	}

	t.Run("no opt-in refuses and prints the declaration", func(t *testing.T) {
		f := build()
		res := check(f, []string{"/srv/media"}, "/var/state")
		if res.Start {
			t.Fatal("started with an uncovered network mount beneath a library root")
		}
		cs := causesOf(res, CauseNotLocal)
		if len(cs) != 1 || cs[0].Path != "/srv/media/tv" {
			t.Fatalf("causes = %+v, want exactly one naming /srv/media/tv", cs)
		}
		if cs[0].Declaration != "/srv/media/tv" {
			t.Fatalf("declaration printed = %q, want /srv/media/tv", cs[0].Declaration)
		}
		if !strings.Contains(cs[0].Detail, "nfs") {
			t.Fatalf("the refusal did not name the detected filesystem: %q", cs[0].Detail)
		}
	})

	t.Run("an opt-in for the root alone does not cover the mount", func(t *testing.T) {
		f := build()
		res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media")
		if res.Start {
			t.Fatal("an opt-in naming only the root covered a mount beneath it")
		}
		if !hasCause(res, CauseNotLocal, "/srv/media/tv") {
			t.Fatalf("causes = %+v, want the uncovered mount named", res.Causes)
		}
	})

	t.Run("an opt-in naming the mount starts the run and records the reduced guarantee", func(t *testing.T) {
		f := build()
		res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media/tv")
		if !res.Start || res.Row != rowStart {
			t.Fatalf("row = %d, start = %v, want %d/true: %+v", res.Row, res.Start, rowStart, res.Causes)
		}
		if !hasNotice(res, NoticeReducedGuarantee, "/srv/media/tv") {
			t.Fatalf("no reduced-guarantee record for the covered mount: %+v", res.Notices)
		}
		if n := noticesOf(res, NoticeReducedGuarantee); len(n) != 1 {
			t.Fatalf("reduced-guarantee records = %+v, want exactly one (nothing else earns it)", n)
		}
		wantEnumerated(t, f, res, "/srv/media/tv/Show.mkv")
	})
}

// TestWalk_BindOfASiblingIsRecordedAtTheSecondMountPointOnly: distinctness is by
// resolved MOUNT POINT and never by backing device, and a directory on the same
// mounted filesystem as the root gets no record of its own (AC2d1).
func TestWalk_BindOfASiblingIsRecordedAtTheSecondMountPointOnly(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media/film")
	f.mkfile("/srv/media/film/Film.mkv")
	f.bind("/srv/media/tv", "/srv/media/film")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	want := []string{"/srv/media", "/srv/media/tv", "/var/state"}
	if got := recordPaths(res); !reflect.DeepEqual(got, want) {
		t.Fatalf("records = %v, want %v (exactly two beneath the root: the root and the bind's mount point)", got, want)
	}
	if !res.Start {
		t.Fatalf("a local bind refused the run: %+v", res.Causes)
	}
	// One directory exposed twice is walked once, and its source is enumerated
	// once, from the spelling the walk reached first.
	if !hasNotice(res, NoticeRegionWalked, "/srv/media/tv") {
		t.Fatalf("the second exposure was not reported as undescended: %+v", res.Notices)
	}
	wantEnumerated(t, f, res, "/srv/media/film/Film.mkv")
}

// TestWalk_OneFilesystemMountedTwiceWithDifferentDeviceNumbersIsOneRegion is the
// case a (st_dev, st_ino) region key gets WRONG: two mounts of one filesystem
// need not share a superblock, so st_dev differs while both mount points expose
// the same directory. Keyed on st_dev the walk would descend both and enumerate
// one source twice, which on the default in-place path queues a second swap
// against a source the first already replaced.
//
// The region key is therefore the FILESYSTEM identity and not the per-mount
// device number: both mount points still carry their own record and are each a
// checked path an opt-in must cover in its own right, and exactly one of them is
// descended (AC2b2, AC2d1).
func TestWalk_OneFilesystemMountedTwiceWithDifferentDeviceNumbersIsOneRegion(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mount("/export", "nfs")
		f.mkfile("/export/Film.mkv")
		f.mkdir("/srv/media")
		f.remount("/srv/media/a", "/export", "devA")
		f.remount("/srv/media/b", "/export", "devB")
		f.mkdir("/var/state")
		return f
	}

	f := build()
	res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media/a", "/srv/media/b")

	if !res.Start {
		t.Fatalf("both mount points opted in and the run was still refused: %+v", res.Causes)
	}
	for _, p := range []string{"/srv/media/a", "/srv/media/b"} {
		rec, ok := recordFor(res, p)
		if !ok || rec.Kind != KindMount || rec.Class != NonLocal {
			t.Fatalf("record for %s = %+v, want its own non-local mount record", p, rec)
		}
	}
	if !hasNotice(res, NoticeRegionWalked, "/srv/media/b") {
		t.Fatalf("the second mount point of the same filesystem was descended anyway: %+v", res.Notices)
	}
	// The whole point: the source is enumerated ONCE, under the mount point the
	// walk reached first.
	wantEnumerated(t, f, res, "/srv/media/a/Film.mkv")

	// And each mount point is a checked path in its own right: opting only the
	// first in still refuses, naming the second.
	f2 := build()
	res2 := check(f2, []string{"/srv/media"}, "/var/state", "/srv/media/a")
	if res2.Start || !hasCause(res2, CauseNotLocal, "/srv/media/b") {
		t.Fatalf("an opt-in naming one mount point covered the other: row %d, %+v", res2.Row, res2.Causes)
	}
}

// TestWalk_TerminatesOverABindLoopAndALinkCycle. The walk enters each REGION at
// most once, which is what makes it terminate for every link and mount layout -
// not links alone. A bind of a root inside itself makes /srv/media/loop/loop/...
// an unbounded chain of distinct resolved directories that link resolution does
// not collapse; every region of it is already walked. Startup completes, emits
// its records and reaches the decision, entering nothing under the loop (AC2b2).
//
// Both cases are run again with the mount table unreadable, because absent mount
// information may move a classification and must never move termination or
// coverage.
func TestWalk_TerminatesOverABindLoopAndALinkCycle(t *testing.T) {
	for _, mountInfo := range []bool{true, false} {
		name := "with mount information"
		if !mountInfo {
			name = "with mount information unreadable"
		}
		t.Run(name, func(t *testing.T) {
			f := newFS().setType("/", "ext4")
			f.mkdir("/srv/media/sub")
			f.mkfile("/srv/media/Film.mkv")
			f.mkfile("/srv/media/sub/Show.mkv")
			f.bind("/srv/media/loop", "/srv/media")        // a bind-mount loop
			f.symlink("/srv/media/sub/back", "/srv/media") // a link cycle
			f.mkdir("/var/state")
			if !mountInfo {
				f.hideMountTable()
			}

			done := make(chan Result, 1)
			go func() { done <- check(f, []string{"/srv/media"}, "/var/state") }()
			var res Result
			select {
			case res = <-done:
			case <-timeout():
				t.Fatal("startup did not complete: the walk did not terminate")
			}

			if !res.Start {
				t.Fatalf("the loop refused the run: %+v", res.Causes)
			}
			for _, dir := range res.Coverage {
				if strings.HasPrefix(dir, "/srv/media/loop") {
					t.Fatalf("the walk entered %s, beneath the bind loop", dir)
				}
			}
			if !hasNotice(res, NoticeRegionWalked, "/srv/media/loop") {
				t.Fatalf("the bind loop was not reported as undescended: %+v", res.Notices)
			}
			if !hasNotice(res, NoticeRegionWalked, "/srv/media/sub/back") {
				t.Fatalf("the link cycle was not reported as undescended: %+v", res.Notices)
			}
			wantEnumerated(t, f, res, "/srv/media/Film.mkv", "/srv/media/sub/Show.mkv")
		})
	}
}

// TestWalk_NestedBindsSkipExactlyTheAlreadyWalkedPart is AC2b2's worked example:
// /srv/media/a is a bind of /tank/films and /srv/media/b a bind of /tank, met in
// that order. A mount exposing a region the walk covered only IN PART is never
// declined whole - the walk descends it and skips exactly the walked part - so
// nothing is silently lost and nothing is enumerated twice.
func TestWalk_NestedBindsSkipExactlyTheAlreadyWalkedPart(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mount("/tank", "ext4")
	f.mkdir("/tank/films")
	f.mkfile("/tank/films/Film.mkv")
	f.mkdir("/tank/shows")
	f.mkfile("/tank/shows/Show.mkv")
	f.mkdir("/srv/media")
	f.bind("/srv/media/a", "/tank/films")
	f.bind("/srv/media/b", "/tank")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	if !res.Start {
		t.Fatalf("refused: %+v", res.Causes)
	}
	for _, p := range []string{"/srv/media/a", "/srv/media/b"} {
		if _, ok := recordFor(res, p); !ok {
			t.Fatalf("no record for mount point %s: %v", p, recordPaths(res))
		}
	}
	if !hasNotice(res, NoticeRegionWalked, "/srv/media/b/films") {
		t.Fatalf("the already-walked PART was not reported: %+v", res.Notices)
	}
	for _, dir := range res.Coverage {
		if dir == "/srv/media/b/films" {
			t.Fatal("the walk entered /srv/media/b/films, whose region it had already walked under /srv/media/a")
		}
	}
	wantEnumerated(t, f, res, "/srv/media/a/Film.mkv", "/srv/media/b/shows/Show.mkv")
}

// TestWalk_BindFromOutsideTheRootsIsDescended: where the region is unwalked, the
// walk descends it and enumerates its sources, even though everything is one
// filesystem (AC2b2).
func TestWalk_BindFromOutsideTheRootsIsDescended(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/home/user/films")
	f.mkfile("/home/user/films/Film.mkv")
	f.mkdir("/srv/media")
	f.bind("/srv/media/films", "/home/user/films")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	if !res.Start {
		t.Fatalf("refused: %+v", res.Causes)
	}
	if _, ok := recordFor(res, "/srv/media/films"); !ok {
		t.Fatalf("no record for the bind's mount point: %v", recordPaths(res))
	}
	wantEnumerated(t, f, res, "/srv/media/films/Film.mkv")
}

// TestWalk_SymlinkLeavingTheLibraryIsReportedAndNotDescended, and a declaration
// naming the LINK is well formed, covers nothing and is reported unnecessary
// (AC2f, AC25).
func TestWalk_SymlinkLeavingTheLibraryIsReportedAndNotDescended(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/mnt/nas/tv")
	f.mkfile("/mnt/nas/tv/Show.mkv")
	f.setType("/mnt/nas", "nfs")
	f.mkdir("/srv/media")
	f.mkfile("/srv/media/Film.mkv")
	f.symlink("/srv/media/tv", "/mnt/nas/tv")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media/tv")

	if !res.Start {
		t.Fatalf("a link out of the library refused the run: %+v", res.Causes)
	}
	ns := noticesOf(res, NoticeLinkLeavesRoots)
	if len(ns) != 1 || ns[0].Path != "/srv/media/tv" || !strings.Contains(ns[0].Detail, "/mnt/nas/tv") {
		t.Fatalf("link report = %+v, want one naming the link and its resolved target", ns)
	}
	if _, ok := recordFor(res, "/srv/media/tv"); ok {
		t.Fatal("a declined link became a checked path")
	}
	if !hasNotice(res, NoticeUnnecessary, "/srv/media/tv") {
		t.Fatalf("the declaration naming the link was not reported unnecessary: %+v", res.Notices)
	}
	wantEnumerated(t, f, res, "/srv/media/Film.mkv")
}

// TestWalk_UnderASymlinkedRootAnInteriorLinkIntoTheSameLibraryIsDescended: the
// resolved-beneath test is applied to the ROOT as well as to the target, so a
// link onto another directory of the same library has not left it. It is
// descended rather than reported as leaving the roots, and its sources are
// enumerated exactly once (AC2f).
func TestWalk_UnderASymlinkedRootAnInteriorLinkIntoTheSameLibraryIsDescended(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/pool/media/film")
	f.mkfile("/pool/media/film/Film.mkv")
	f.mkdir("/pool/media/tv")
	f.mkfile("/pool/media/tv/Show.mkv")
	f.symlink("/pool/media/tv/alias", "/pool/media/film")
	f.symlink("/srv/media", "/pool/media")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	if !res.Start {
		t.Fatalf("refused: %+v", res.Causes)
	}
	if hasNotice(res, NoticeLinkLeavesRoots, "/srv/media/tv/alias") {
		t.Fatalf("an interior link onto the same library was reported as leaving it: %+v", res.Notices)
	}
	if !hasNotice(res, NoticeRegionWalked, "/srv/media/tv/alias") {
		t.Fatalf("the interior link was neither descended nor reported as already-walked: %+v", res.Notices)
	}
	wantEnumerated(t, f, res, "/srv/media/film/Film.mkv", "/srv/media/tv/Show.mkv")
}

// TestWalk_UnreadableDirectoryAndUnreadableLocalMountReportAndNeitherRefuses: a
// path holdfast can classify but not read is one it can take no source from, and
// that is not a refusal. A mount the walk DID find stays a checked path even when
// unreadable, so it still carries its own record (AC4a, AC2f1).
func TestWalk_UnreadableDirectoryAndUnreadableLocalMountReportAndNeitherRefuses(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mkfile("/srv/media/Film.mkv")
	f.mkdir("/srv/media/lost+found")
	f.mkfile("/srv/media/lost+found/Hidden.mkv")
	f.failRead("/srv/media/lost+found", fs.ErrPermission)
	f.mount("/srv/media/vault", "ext4")
	f.mkfile("/srv/media/vault/Vault.mkv")
	f.failRead("/srv/media/vault", fs.ErrPermission)
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	if !res.Start {
		t.Fatalf("an unreadable directory one level down refused the run: %+v", res.Causes)
	}
	for _, p := range []string{"/srv/media/lost+found", "/srv/media/vault"} {
		if !hasNotice(res, NoticeUnreadable, p) {
			t.Fatalf("%s was not reported as a directory it could not read: %+v", p, res.Notices)
		}
	}
	vault, ok := recordFor(res, "/srv/media/vault")
	if !ok || vault.Class != Local {
		t.Fatalf("the unreadable local mount lost its record: %+v", vault)
	}
	// Nothing is enumerated from either, and the run still gets its readable
	// source.
	wantEnumerated(t, f, res, "/srv/media/Film.mkv")
}

// TestWalk_DirectoryRemovedMidWalkIsReportedAndTheRunStarts (AC2f1).
func TestWalk_DirectoryRemovedMidWalkIsReportedAndTheRunStarts(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media/tv")
	f.mkfile("/srv/media/Film.mkv")
	f.mkfile("/srv/media/tv/Show.mkv")
	f.failRead("/srv/media/tv", fs.ErrNotExist) // another program renamed it mid-walk
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	if !res.Start {
		t.Fatalf("a directory that vanished mid-walk refused the run: %+v", res.Causes)
	}
	if !hasNotice(res, NoticeListingFailed, "/srv/media/tv") {
		t.Fatalf("the failure was not reported: %+v", res.Notices)
	}
	wantEnumerated(t, f, res, "/srv/media/Film.mkv")
}

// TestWalk_AListingThatFailsPartwayIsNotATraversalAndStillCountsAsEntered: a
// listing that fails PARTWAY is not a successful traversal, so the directory is
// reported and nothing is enumerated from it - AND its region stays marked as
// entered, so a later exposure of that region is declined and no failure can cost
// termination (AC2f1, AC2b2).
func TestWalk_AListingThatFailsPartwayIsNotATraversalAndStillCountsAsEntered(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media/tv")
	f.mkfile("/srv/media/tv/Show.mkv")
	f.mkfile("/srv/media/Film.mkv")
	f.failReadPartway("/srv/media/tv", errors.New("input/output error"))
	f.bind("/srv/media/z", "/srv/media/tv") // a second exposure of the same region
	f.mkdir("/var/state")

	done := make(chan Result, 1)
	go func() { done <- check(f, []string{"/srv/media"}, "/var/state") }()
	var res Result
	select {
	case res = <-done:
	case <-timeout():
		t.Fatal("startup did not complete after a mid-listing failure")
	}

	if !res.Start {
		t.Fatalf("a mid-listing failure one level down refused the run: %+v", res.Causes)
	}
	if !hasNotice(res, NoticeListingFailed, "/srv/media/tv") {
		t.Fatalf("the partial listing was not reported: %+v", res.Notices)
	}
	if !hasNotice(res, NoticeRegionWalked, "/srv/media/z") {
		t.Fatalf("the second exposure of the entered region was not declined: %+v", res.Notices)
	}
	// The entries the failed listing DID hand back are not trusted: nothing is
	// enumerated from that directory under either spelling.
	wantEnumerated(t, f, res, "/srv/media/Film.mkv")
}

// TestWalk_CostIsBoundedByTheTreeAndNotByTheLibrary: one root of a fixed layout
// walked twice, holding ten media files on the first start and ten thousand on
// the second, emits the same SET of records - each naming its path - and does the
// same number of inspections. No media file is opened on either start, which the
// Platform interface makes structural: it has no method that opens a file at all
// (AC2e).
func TestWalk_CostIsBoundedByTheTreeAndNotByTheLibrary(t *testing.T) {
	build := func(n int) *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media/tv")
		f.mount("/srv/media/nas", "ext4")
		for i := 0; i < n; i++ {
			f.mkfile(fmt.Sprintf("/srv/media/tv/Film%05d.mkv", i))
		}
		f.mkdir("/var/state")
		return f
	}

	small, large := build(10), build(10000)
	resSmall := check(small, []string{"/srv/media"}, "/var/state")
	resLarge := check(large, []string{"/srv/media"}, "/var/state")

	if !reflect.DeepEqual(recordPaths(resSmall), recordPaths(resLarge)) {
		t.Fatalf("record set moved with the file count: %v vs %v", recordPaths(resSmall), recordPaths(resLarge))
	}
	for _, r := range resSmall.Records {
		if r.Path == "" {
			t.Fatal("a record with no path: the report would name nothing")
		}
	}
	if small.inspects != large.inspects {
		t.Fatalf("inspections = %d for ten files and %d for ten thousand: the cost is bounded by the LIBRARY, not the tree",
			small.inspects, large.inspects)
	}
	if small.types != large.types {
		t.Fatalf("type lookups = %d vs %d: the cost is bounded by the library, not the tree", small.types, large.types)
	}
}

// TestWalk_ADirectoryTraversalIsWhatMeetsALink pins AC2b1: a startup that read
// the mount table alone would satisfy none of this. With the mount table
// unreadable the walk still finds every real filesystem boundary and still meets
// the symbolic link, and the classification it can no longer make finely is the
// only thing that moves.
func TestWalk_ADirectoryTraversalIsWhatMeetsALink(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/mnt/elsewhere")
	f.mkdir("/srv/media/tv")
	f.symlink("/srv/media/tv/out", "/mnt/elsewhere")
	f.mount("/srv/media/deep", "nfs")
	f.mkdir("/var/state")
	f.hideMountTable()

	res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media/deep")

	if !hasNotice(res, NoticeLinkLeavesRoots, "/srv/media/tv/out") {
		t.Fatalf("the link was not met at all: %+v", res.Notices)
	}
	if rec, ok := recordFor(res, "/srv/media/deep"); !ok || rec.Class != NonLocal {
		t.Fatalf("a real filesystem boundary was missed without the mount table: %+v", rec)
	}
	if len(noticesOf(res, NoticeMountInfoUnavailable)) != 1 {
		t.Fatalf("unreadable mount information was not reported exactly once: %+v", res.Notices)
	}
	if !res.Start {
		t.Fatalf("refused: %+v", res.Causes)
	}
}

// TestWalk_MoreThanOneRoot is the multi-root case, in all three shapes AC2b3
// names: a root nested in another, two roots bind-mounted onto one storage, and
// an alias root that is a symlink to another root. In every one the second root
// carries its own report and its own record, its sources are enumerated exactly
// ONCE, it is never called empty, and the run starts.
func TestWalk_MoreThanOneRoot(t *testing.T) {
	t.Run("a root nested inside another", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media/tv")
		f.mkfile("/srv/media/tv/Movie.mkv")
		f.mkdir("/var/state")

		res := check(f, []string{"/srv/media", "/srv/media/tv"}, "/var/state")

		if !res.Start {
			t.Fatalf("refused: %+v", res.Causes)
		}
		for _, p := range []string{"/srv/media", "/srv/media/tv"} {
			if _, ok := recordFor(res, p); !ok {
				t.Fatalf("no record for root %s: %v", p, recordPaths(res))
			}
		}
		if !hasNotice(res, NoticeRegionWalked, "/srv/media/tv") {
			t.Fatalf("the second root was not reported as one this run did not descend: %+v", res.Notices)
		}
		if hasNotice(res, NoticeEmptyRoot, "/srv/media/tv") {
			t.Fatal("an undescended root was reported present and empty")
		}
		if hasNotice(res, NoticeEmptyRoot, "/srv/media") {
			t.Fatal("a root whose library is enumerated was reported present and empty")
		}
		wantEnumerated(t, f, res, "/srv/media/tv/Movie.mkv")
	})

	t.Run("two roots bind-mounted onto one storage", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkfile("/srv/media/Movie.mkv")
		f.bind("/srv/library", "/srv/media")
		f.mkdir("/var/state")

		res := check(f, []string{"/srv/media", "/srv/library"}, "/var/state")

		if !res.Start {
			t.Fatalf("refused: %+v", res.Causes)
		}
		if _, ok := recordFor(res, "/srv/library"); !ok {
			t.Fatalf("the declined root lost its record: %v", recordPaths(res))
		}
		if !hasNotice(res, NoticeRegionWalked, "/srv/library") {
			t.Fatalf("the declined root was not reported: %+v", res.Notices)
		}
		wantEnumerated(t, f, res, "/srv/media/Movie.mkv")
	})

	t.Run("an alias root that is a symlink to another root", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkfile("/srv/media/Movie.mkv")
		f.symlink("/srv/alias", "/srv/media")
		f.mkdir("/var/state")

		res := check(f, []string{"/srv/media", "/srv/alias"}, "/var/state")

		if !res.Start {
			t.Fatalf("refused: %+v", res.Causes)
		}
		// Two spellings of one path: the records collapse into one.
		if _, ok := recordFor(res, "/srv/alias"); ok {
			t.Fatalf("two records for one resolved path: %v", recordPaths(res))
		}
		if !hasNotice(res, NoticeRegionWalked, "/srv/alias") {
			t.Fatalf("the alias root was not reported as undescended: %+v", res.Notices)
		}
		wantEnumerated(t, f, res, "/srv/media/Movie.mkv")
	})
}

// TestWalk_IsDeterministic: two starts with identical configuration, an
// unchanged layout and no interactive input give the identical decision and the
// identical set of records, each identical in TEXT - so the root order and the
// spelling the traversal picks for a region reachable more than one way are both
// fixed (AC12).
func TestWalk_IsDeterministic(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media/film")
		f.mkfile("/srv/media/film/Film.mkv")
		f.bind("/srv/media/tv", "/srv/media/film")
		f.mount("/srv/media/nas", "nfs")
		f.mkdir("/srv/other")
		f.bind("/srv/other/x", "/srv/media/film")
		f.mkdir("/var/state")
		return f
	}
	roots := []string{"/srv/media", "/srv/other"}
	first := check(build(), roots, "/var/state", "/srv/media/nas")
	second := check(build(), roots, "/var/state", "/srv/media/nas")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two identical starts disagreed:\n%+v\n%+v", first, second)
	}
}

// timeout bounds a walk that is supposed to TERMINATE. Ten seconds is far
// beyond any of these layouts; a walk still going by then is not slow, it is
// looping, and the test says so rather than hanging the suite.
func timeout() <-chan time.Time { return time.After(10 * time.Second) }

// ---- S0168: the walk prunes a directory the path filters exclude whole ----------------

// recordingFS is the fake platform with every listing it serves recorded, in order: what
// "the walk never lists it" is asserted against. It records and forwards, and nothing else.
type recordingFS struct {
	*fakeFS
	listed []string
}

func (r *recordingFS) ReadDir(p string) ([]Entry, error) {
	r.listed = append(r.listed, cleanPath(p))
	return r.fakeFS.ReadDir(p)
}

// pruningCheck is the walk with the path filters' exclude half in force, and the decision
// is the real one: built by internal/config from these roots, exactly as the one
// start-or-refuse construction builds it, so no case here passes on a predicate of its own.
func pruningCheck(p Platform, cfg config.Config, decls ...string) Result {
	return Run(Check{
		Roots:        cfg.LibraryRoots,
		StateDir:     "/var/state",
		Declarations: decls,
		IsMediaFile:  mediaByExt,
		Excluded:     cfg.DirectoryExcluded(),
		Platform:     p,
	})
}

// atOrBeneath reports whether p is dir or lies beneath it.
func atOrBeneath(p, dir string) bool { return p == dir || lexicallyBeneath(p, dir) }

// TestS0168AC1_AnExcludedDirectoryAndEverythingBeneathItIsNeverListed is S0168 [AC-1]: a
// directory strictly beneath a root that an exclude pattern in force for the root it is
// assigned to reaches is not listed, nothing beneath it is listed, and it is absent from the
// coverage and from the carried listings. The second root's own filters name nothing, so a
// directory there spelled like an excluded one is walked: it is that root's patterns that
// decide. The same tree walked with no decision in force is the anti-vacuity half.
func TestS0168AC1_AnExcludedDirectoryAndEverythingBeneathItIsNeverListed(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/film/Film.mkv")
		f.mkfile("/srv/media/film/Extras/Behind.mkv")
		f.mkfile("/srv/media/film/Extras/deep/Deeper.mkv")
		f.mkfile("/srv/media/.Trash-0/Old.mkv")
		f.mkfile("/srv/media/.Trash-0/sub/Older.mkv")
		f.mkfile("/srv/media/import/New.mkv")
		f.mkfile("/srv/tv/Extras/Show.mkv")
		f.mkdir("/var/state")
		return f
	}
	cfg := config.Config{LibraryRoots: []string{"/srv/media", "/srv/tv"}, Roots: []config.Root{
		{Path: "/srv/media", Clean: "/srv/media", Filters: config.PathFilters{
			Exclude: []string{"**/Extras", "**/.Trash-*/**", "/srv/media/import"}}},
		{Path: "/srv/tv", Clean: "/srv/tv"},
	}}
	excluded := []string{"/srv/media/.Trash-0", "/srv/media/film/Extras", "/srv/media/import"}

	f := &recordingFS{fakeFS: build()}
	res := pruningCheck(f, cfg)
	if !res.Start {
		t.Fatalf("refused: row %d, %+v", res.Row, res.Causes)
	}
	for _, dir := range excluded {
		for _, l := range f.listed {
			if atOrBeneath(l, dir) {
				t.Errorf("%s was listed, and it is %s or beneath it", l, dir)
			}
		}
		for _, c := range res.Coverage {
			if atOrBeneath(c, dir) {
				t.Errorf("the coverage names %s, which is %s or beneath it", c, dir)
			}
		}
		for k := range res.Entries {
			if atOrBeneath(k, dir) {
				t.Errorf("the carried listings hold %s, which is %s or beneath it", k, dir)
			}
		}
		if !hasNotice(res, NoticeExcluded, dir) {
			t.Errorf("%s was not reported as a directory the path filters exclude: %+v", dir, res.Notices)
		}
	}
	// Nothing beneath a pruned directory is met at all, so nothing there is reported either.
	if n := noticesOf(res, NoticeExcluded); len(n) != len(excluded) {
		t.Errorf("excluded-directory reports = %+v, want exactly the %d pruned directories", n, len(excluded))
	}
	wantCoverage := []string{"/srv/media", "/srv/media/film", "/srv/tv", "/srv/tv/Extras"}
	if !reflect.DeepEqual(res.Coverage, wantCoverage) {
		t.Fatalf("coverage = %v, want %v", res.Coverage, wantCoverage)
	}
	if !reflect.DeepEqual(f.listed, wantCoverage) {
		t.Fatalf("listings = %v, want exactly the covered directories %v", f.listed, wantCoverage)
	}
	wantEnumerated(t, f.fakeFS, res, "/srv/media/film/Film.mkv", "/srv/tv/Extras/Show.mkv")

	// Anti-vacuity: with no decision in force every one of them IS listed and covered.
	plain := &recordingFS{fakeFS: build()}
	plainRes := Run(Check{Roots: cfg.LibraryRoots, StateDir: "/var/state", IsMediaFile: mediaByExt, Platform: plain})
	for _, dir := range excluded {
		if !contains(plainRes.Coverage, dir) {
			t.Fatalf("with no filter %s is not covered either (%v), so its absence above proves nothing",
				dir, plainRes.Coverage)
		}
	}
	if len(noticesOf(plainRes, NoticeExcluded)) != 0 {
		t.Fatalf("a walk with no path filter reported an excluded directory: %+v", plainRes.Notices)
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestS0168AC4_ADirectoryNoExcludePatternReachesIsWalkedExactlyAsBefore is S0168 [AC-4]: a
// directory that no exclude pattern reaches and that lies outside every include pattern is
// listed and descended exactly as it is with no decision in force - include never prunes, and
// an exclude pattern that reaches only FILES prunes nothing either. Exactly is graded as
// equality of the whole result, listing for listing.
func TestS0168AC4_ADirectoryNoExcludePatternReachesIsWalkedExactlyAsBefore(t *testing.T) {
	build := func() *recordingFS {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/movies/Film.mkv")
		f.mkfile("/srv/media/tv/Show/Ep1.mkv")
		f.mkfile("/srv/media/tv/Show/Ep1.sample.mkv")
		f.mkfile("/srv/media/music/Track.mkv")
		f.mount("/srv/media/tv/nas", "ext4")
		f.mkdir("/var/state")
		return &recordingFS{fakeFS: f}
	}
	cfg := config.Config{LibraryRoots: []string{"/srv/media"},
		IncludePaths: []string{"movies/**"},
		ExcludePaths: []string{"**/*.sample.*", "/srv/elsewhere/**"}}

	filtered, plain := build(), build()
	got := pruningCheck(filtered, cfg)
	want := Run(Check{Roots: cfg.LibraryRoots, StateDir: "/var/state", IsMediaFile: mediaByExt, Platform: plain})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("a directory no exclude pattern reaches was walked differently:\n  with the filters: %+v\n"+
			"  without:          %+v", got, want)
	}
	if !reflect.DeepEqual(filtered.listed, plain.listed) {
		t.Fatalf("listings moved: %v with the filters, %v without", filtered.listed, plain.listed)
	}
	// And the directories outside every include pattern really were descended.
	for _, dir := range []string{"/srv/media/tv", "/srv/media/tv/Show", "/srv/media/tv/nas", "/srv/media/music"} {
		if !contains(got.Coverage, dir) {
			t.Fatalf("%s, which no exclude pattern reaches, is not covered: %v", dir, got.Coverage)
		}
	}
}

// TestS0168AC5_ARootAPatternNamesIsStillListedClassifiedAndWalked is S0168 [AC-5]: an exclude
// pattern that matches a configured library root - by name, by a pattern matching the
// root-relative "." itself, or as an earlier root's pattern reaching a nested root - leaves
// that root listed, classified and walked, and a root that cannot be listed still refuses at
// row 3. Pruning applies only strictly beneath a root.
func TestS0168AC5_ARootAPatternNamesIsStillListedClassifiedAndWalked(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/Top.mkv")
		f.mkfile("/srv/media/film/Film.mkv")
		f.mkdir("/var/state")
		return f
	}
	for _, tc := range []struct {
		name    string
		exclude []string
	}{
		{"an absolute pattern naming the root", []string{"/srv/media"}},
		{"a relative pattern matching the root-relative dot", []string{"**"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &recordingFS{fakeFS: build()}
			res := pruningCheck(f, config.Config{LibraryRoots: []string{"/srv/media"}, ExcludePaths: tc.exclude})
			if !res.Start || res.Row != rowStart {
				t.Fatalf("row %d, start %v: %+v", res.Row, res.Start, res.Causes)
			}
			if rec, ok := recordFor(res, "/srv/media"); !ok || rec.Kind != KindLibraryRoot || rec.Class != Local {
				t.Fatalf("the root lost its classification record: %+v (found %v)", rec, ok)
			}
			if !contains(res.Coverage, "/srv/media") || !contains(f.listed, "/srv/media") {
				t.Fatalf("the root was not listed and covered: coverage %v, listings %v", res.Coverage, f.listed)
			}
			if _, ok := res.Entries["/srv/media"]; !ok {
				t.Fatal("the root's listing was not carried")
			}
			if hasNotice(res, NoticeExcluded, "/srv/media") {
				t.Fatal("the root itself was reported excluded")
			}
			// Strictly beneath it, the pattern reaches everything: through the root.
			if !hasNotice(res, NoticeExcluded, "/srv/media/film") || contains(f.listed, "/srv/media/film") {
				t.Fatalf("a directory beneath a root the pattern names was walked: listings %v", f.listed)
			}
		})
	}

	t.Run("a root that cannot be listed still refuses at row 3", func(t *testing.T) {
		f := build()
		f.failRead("/srv/media", fs.ErrPermission)
		res := pruningCheck(f, config.Config{LibraryRoots: []string{"/srv/media"}, ExcludePaths: []string{"/srv/media"}})
		if res.Start || res.Row != rowUninspect || !hasCause(res, CauseUnlistable, "/srv/media") {
			t.Fatalf("row %d, start %v, causes %+v; want the root refused as unlistable at row 3",
				res.Row, res.Start, res.Causes)
		}
	})

	// Nested roots are refused by Validate; the walk is asked about them anyway. The outer
	// root's pattern reaches the inner root, which is still listed and walked - inside the
	// outer walk, as a nested root always is - while what lies beneath it is pruned by the
	// outer root's pattern, whose filters are the ones a scan applies there.
	t.Run("a nested root an outer root's pattern reaches", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/tv/Pilot.mkv")
		f.mkfile("/srv/media/tv/season/Ep1.mkv")
		f.mkdir("/var/state")
		rf := &recordingFS{fakeFS: f}
		res := pruningCheck(rf, config.Config{LibraryRoots: []string{"/srv/media", "/srv/media/tv"}, Roots: []config.Root{
			{Path: "/srv/media", Clean: "/srv/media", Filters: config.PathFilters{Exclude: []string{"tv"}}},
			{Path: "/srv/media/tv", Clean: "/srv/media/tv"},
		}})
		if !res.Start {
			t.Fatalf("refused: %+v", res.Causes)
		}
		if _, ok := recordFor(res, "/srv/media/tv"); !ok {
			t.Fatalf("the nested root lost its record: %v", recordPaths(res))
		}
		if !contains(res.Coverage, "/srv/media/tv") || !contains(rf.listed, "/srv/media/tv") {
			t.Fatalf("the nested root was not listed: coverage %v", res.Coverage)
		}
		if hasNotice(res, NoticeExcluded, "/srv/media/tv") || hasNotice(res, NoticeRegionExcluded, "/srv/media/tv") {
			t.Fatalf("a configured root was reported as excluded: %+v", res.Notices)
		}
		if !hasNotice(res, NoticeExcluded, "/srv/media/tv/season") || contains(rf.listed, "/srv/media/tv/season") {
			t.Fatalf("beneath the nested root the outer root's pattern did not prune: listings %v", rf.listed)
		}
	})
}

// TestS0168AC6_APrunedMountKeepsItsRecordAndItsRefusal is S0168 [AC-6]: a pruned directory
// that is itself a mount point is still inspected, still carries its classification record,
// and still refuses the run at row 4 when it is not local and no declaration covers it -
// cause for cause what the same tree decides with no filter. A mount point strictly beneath
// a pruned directory carries no record and decides nothing.
func TestS0168AC6_APrunedMountKeepsItsRecordAndItsRefusal(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/Film.mkv")
		f.mount("/srv/media/nas", "nfs")
		f.mkfile("/srv/media/nas/Remote.mkv")
		f.mkdir("/var/state")
		return f
	}
	cfg := config.Config{LibraryRoots: []string{"/srv/media"}, ExcludePaths: []string{"nas"}}

	rf := &recordingFS{fakeFS: build()}
	res := pruningCheck(rf, cfg)
	plain := check(build(), []string{"/srv/media"}, "/var/state")
	if res.Start || res.Row != rowNotLocal || !hasCause(res, CauseNotLocal, "/srv/media/nas") {
		t.Fatalf("a pruned network mount did not refuse at row 4: row %d, causes %+v", res.Row, res.Causes)
	}
	if !reflect.DeepEqual(res.Causes, plain.Causes) || !reflect.DeepEqual(res.Records, plain.Records) {
		t.Fatalf("the filter moved the decision:\n  with it: %+v %+v\n  without: %+v %+v",
			res.Records, res.Causes, plain.Records, plain.Causes)
	}
	if contains(rf.listed, "/srv/media/nas") || !hasNotice(res, NoticeExcluded, "/srv/media/nas") {
		t.Fatalf("the pruned mount was listed anyway: %v", rf.listed)
	}

	// The declaration that permits it permits it exactly as before.
	allowed := pruningCheck(build(), cfg, "/srv/media/nas")
	if !allowed.Start || !hasNotice(allowed, NoticeReducedGuarantee, "/srv/media/nas") {
		t.Fatalf("the declared pruned mount did not start with a reduced guarantee: %+v %+v",
			allowed.Causes, allowed.Notices)
	}

	t.Run("a mount beneath a pruned directory decides nothing", func(t *testing.T) {
		deep := func() *fakeFS {
			f := newFS().setType("/", "ext4")
			f.mkfile("/srv/media/Film.mkv")
			f.mount("/srv/media/.Trash-0/nfs", "nfs")
			f.mkdir("/var/state")
			return f
		}
		res := pruningCheck(deep(), config.Config{LibraryRoots: []string{"/srv/media"},
			ExcludePaths: []string{"**/.Trash-*/**"}})
		if !res.Start || res.Row != rowStart {
			t.Fatalf("a mount beneath a pruned directory decided the run: row %d, %+v", res.Row, res.Causes)
		}
		if _, ok := recordFor(res, "/srv/media/.Trash-0/nfs"); ok {
			t.Fatalf("a mount beneath a pruned directory carries a record: %v", recordPaths(res))
		}
		// Anti-vacuity: without the filter that mount is met, recorded and refuses.
		plain := check(deep(), []string{"/srv/media"}, "/var/state")
		if plain.Start || !hasCause(plain, CauseNotLocal, "/srv/media/.Trash-0/nfs") {
			t.Fatalf("without the filter the deep mount did not refuse, so this case proves nothing: %+v", plain.Causes)
		}
	})
}

// TestS0168AC7_PrunedStorageUnderAnotherSpellingIsDeclinedAndNamesTheExcludedOne is S0168
// [AC-7]: storage the walk pruned, met later under a spelling no exclude pattern reaches - a
// bind mount of the same directory, a symbolic link to it, a later configured root exposing
// it - is not descended, offers no file, refuses nothing, and is reported naming the excluded
// spelling that reached it first, never saying its sources are enumerated from there.
func TestS0168AC7_PrunedStorageUnderAnotherSpellingIsDeclinedAndNamesTheExcludedOne(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/Film.mkv")
		f.mkfile("/srv/media/.Trash-0/Old.mkv")
		f.mkdir("/var/state")
		return f
	}
	exclude := []string{"**/.Trash-*/**"}
	for _, tc := range []struct {
		name  string
		roots []string
		alias string
		lay   func(f *fakeFS)
	}{
		{"a bind mount of it", []string{"/srv/media"}, "/srv/media/zz",
			func(f *fakeFS) { f.bind("/srv/media/zz", "/srv/media/.Trash-0") }},
		{"a symbolic link to it", []string{"/srv/media"}, "/srv/media/zlink",
			func(f *fakeFS) { f.symlink("/srv/media/zlink", "/srv/media/.Trash-0") }},
		{"a later configured root exposing it", []string{"/srv/media", "/srv/alias"}, "/srv/alias",
			func(f *fakeFS) { f.bind("/srv/alias", "/srv/media/.Trash-0") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := build()
			tc.lay(f)
			rf := &recordingFS{fakeFS: f}
			res := pruningCheck(rf, config.Config{LibraryRoots: tc.roots, ExcludePaths: exclude})
			if !res.Start || res.Row != rowStart {
				t.Fatalf("a second spelling of pruned storage decided the run: row %d, %+v", res.Row, res.Causes)
			}
			for _, l := range rf.listed {
				if atOrBeneath(l, tc.alias) || atOrBeneath(l, "/srv/media/.Trash-0") {
					t.Fatalf("%s was listed: the pruned storage was walked under some spelling", l)
				}
			}
			wantEnumerated(t, f, res, "/srv/media/Film.mkv")

			var got *Notice
			for _, n := range noticesOf(res, NoticeRegionExcluded) {
				if n.Path == tc.alias {
					n := n
					got = &n
				}
			}
			if got == nil {
				t.Fatalf("%s was not reported as a spelling of excluded storage: %+v", tc.alias, res.Notices)
			}
			if !strings.Contains(got.Detail, "/srv/media/.Trash-0") {
				t.Errorf("the report does not name the excluded spelling that reached it first: %q", got.Detail)
			}
			if strings.Contains(got.Detail, "enumerated exactly once") || strings.Contains(got.Detail, "from there") {
				t.Errorf("the report says the sources are enumerated from the excluded spelling: %q", got.Detail)
			}
			if hasNotice(res, NoticeRegionWalked, tc.alias) || hasNotice(res, NoticeEmptyRoot, tc.alias) {
				t.Errorf("the second spelling was also reported as walked storage or an empty root: %+v", res.Notices)
			}
		})
	}
}

// TestS0168AC10_ARootWhoseOnlySourcesArePrunedIsPresentAndEmpty is S0168 [AC-10]: a present,
// local root whose only source-named files lie beneath pruned directories is reported present
// and empty - the report's own words, "holds no media file this run would enumerate", are
// then true of it - and the run still starts.
func TestS0168AC10_ARootWhoseOnlySourcesArePrunedIsPresentAndEmpty(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/.Trash-0/Old.mkv")
		f.mkfile("/srv/media/film/Extras/Behind.mkv")
		f.mkfile("/srv/media/film/notes.txt")
		f.mkdir("/var/state")
		return f
	}
	res := pruningCheck(build(), config.Config{LibraryRoots: []string{"/srv/media"},
		ExcludePaths: []string{"**/.Trash-*/**", "**/Extras"}})
	if !res.Start || res.Row != rowStart {
		t.Fatalf("refused: row %d, %+v", res.Row, res.Causes)
	}
	if !hasNotice(res, NoticeEmptyRoot, "/srv/media") {
		t.Fatalf("a root whose only sources are pruned was not reported present and empty: %+v", res.Notices)
	}
	// Anti-vacuity: the same root with no filter holds media and is not called empty.
	if plain := check(build(), []string{"/srv/media"}, "/var/state"); hasNotice(plain, NoticeEmptyRoot, "/srv/media") {
		t.Fatal("the fixture's root is reported empty with no filter at all, so this case proves nothing")
	}
}

// TestS0168AC12_AMalformedPatternListsTheDirectory is S0168 [AC-12]: where matching an
// exclude pattern against a directory cannot decide - a malformed pattern reaching the walk
// without passing config validation - the directory is listed rather than pruned. The pattern
// is one the pinned matcher answers YES for, so a walk that trusted the answer would prune.
func TestS0168AC12_AMalformedPatternListsTheDirectory(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkfile("/srv/media/.Trash-0/Old.mkv")
	f.mkfile("/srv/media/film/Extras/Behind.mkv")
	f.mkdir("/var/state")
	rf := &recordingFS{fakeFS: f}
	res := pruningCheck(rf, config.Config{LibraryRoots: []string{"/srv/media"},
		ExcludePaths: []string{"{.Trash-0,[}", "**/Extras"}})
	if !contains(rf.listed, "/srv/media/.Trash-0") || !contains(res.Coverage, "/srv/media/.Trash-0") {
		t.Fatalf("a directory only a malformed pattern names was pruned: listings %v", rf.listed)
	}
	if hasNotice(res, NoticeExcluded, "/srv/media/.Trash-0") {
		t.Fatal("a directory only a malformed pattern names was reported excluded")
	}
	// The well-formed pattern beside it still prunes, so the listing above is the guard.
	if !hasNotice(res, NoticeExcluded, "/srv/media/film/Extras") || contains(rf.listed, "/srv/media/film/Extras") {
		t.Fatalf("the well-formed pattern beside it did not prune: listings %v", rf.listed)
	}
}

// TestS0168AC13_AFailureNoPatternCoversIsReportedAsBefore is S0168 [AC-13]: a directory no
// exclude pattern reaches that cannot be listed is reported with the same notice kind and
// detail as with no filter configured - permission denial as directory-could-not-be-read, any
// other failure as directory-could-not-be-traversed - so pruning hides no failure the filters
// do not cover. The excluded counterpart, which is never listed and so never fails, is
// reported as excluded, including when even inspecting it fails.
func TestS0168AC13_AFailureNoPatternCoversIsReportedAsBefore(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkfile("/srv/media/Film.mkv")
		f.mkfile("/srv/media/locked/Hidden.mkv")
		f.failRead("/srv/media/locked", fs.ErrPermission)
		f.mkfile("/srv/media/broken/Gone.mkv")
		f.failRead("/srv/media/broken", errors.New("input/output error"))
		f.mkfile("/srv/media/unstatable/Lost.mkv")
		f.failStat("/srv/media/unstatable", fs.ErrPermission)
		f.mkfile("/srv/media/.Trash-0/Old.mkv")
		f.failRead("/srv/media/.Trash-0", fs.ErrPermission)
		f.mkfile("/srv/media/.Trash-1000/Old.mkv")
		f.failStat("/srv/media/.Trash-1000", fs.ErrPermission)
		f.mkdir("/var/state")
		return f
	}
	res := pruningCheck(build(), config.Config{LibraryRoots: []string{"/srv/media"},
		ExcludePaths: []string{"**/.Trash-*/**"}})
	plain := check(build(), []string{"/srv/media"}, "/var/state")

	for _, tc := range []struct {
		path string
		kind NoticeKind
	}{
		{"/srv/media/locked", NoticeUnreadable},
		{"/srv/media/broken", NoticeListingFailed},
		{"/srv/media/unstatable", NoticeUnreadable},
	} {
		want := noticeAt(plain, tc.path)
		got := noticeAt(res, tc.path)
		if want == nil || want.Kind != tc.kind {
			t.Fatalf("with no filter %s is reported as %+v, want %s: the fixture proves nothing", tc.path, want, tc.kind)
		}
		if got == nil || *got != *want {
			t.Errorf("%s: reported %+v with the filter in force, %+v without", tc.path, got, want)
		}
	}
	for _, dir := range []string{"/srv/media/.Trash-0", "/srv/media/.Trash-1000"} {
		if n := noticeAt(res, dir); n == nil || n.Kind != NoticeExcluded {
			t.Errorf("%s is reported as %+v, want it reported as excluded", dir, n)
		}
		if n := noticeAt(plain, dir); n == nil || (n.Kind != NoticeUnreadable && n.Kind != NoticeListingFailed) {
			t.Errorf("with no filter %s is reported as %+v; the fixture does not fail it", dir, n)
		}
	}
	if !res.Start {
		t.Fatalf("refused: %+v", res.Causes)
	}
}

// noticeAt is the one notice naming path, or nil when there is none or more than one.
func noticeAt(res Result, path string) *Notice {
	var found *Notice
	for i := range res.Notices {
		if res.Notices[i].Path == path {
			if found != nil {
				return nil
			}
			found = &res.Notices[i]
		}
	}
	return found
}
