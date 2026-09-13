package startup

import (
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// What the walk KEEPS of the tree it read.
//
// The check has always listed every directory beneath the roots and always
// tested every entry against the media predicate, and then thrown the names
// away - so the scan that follows listed each of those directories again, once
// to sweep this tool's own orphaned temps and once to enumerate the sources. The
// entries are the same entries. These cases grade what the result now carries
// out, and they grade it as the scan will read it: the same set of sources, from
// the same directories, for no further listing at all.

// fromEntries is the enumeration the SCAN can now make: over the directories the
// walk covered, from the entries the walk kept, with no listing of its own. It is
// deliberately the same shape as walk_test.go's `enumerated`, which re-lists
// through the platform, so the two can be compared over one fixture.
func fromEntries(res Result) []string {
	var out []string
	for _, dir := range res.Coverage {
		for _, e := range res.Entries[dir] {
			if e.IsDir || e.ResolvesToDir {
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

func entryNames(ents []Entry) []string {
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name)
	}
	return out
}

func entryFor(ents []Entry, name string) (Entry, bool) {
	for _, e := range ents {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// mediaFixture is one library holding every entry kind that decides something
// here: media and non-media files, this tool's own temp name, a subdirectory, a
// directory whose listing FAILS, a symbolic link to a media file inside the
// roots, one to a media file beneath no root, and one whose name is a media name
// and whose target is a DIRECTORY.
func mediaFixture() *fakeFS {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mkfile("/srv/media/Top.mkv")
	f.mkfile("/srv/media/notes.txt")
	f.mkfile("/srv/media/Half.__transcoding__.mkv")
	f.mkfile("/srv/media/season/Ep1.mkv")
	f.mkfile("/srv/media/season/Ep2.mp4")
	f.mkdir("/srv/media/empty")
	f.mkfile("/srv/media/unreadable/Never.mkv")
	f.failRead("/srv/media/unreadable", errors.New("input/output error"))
	f.symlink("/srv/media/Inside.mkv", "/srv/media/season/Ep1.mkv")
	f.mkfile("/elsewhere/Outside.mkv")
	f.symlink("/srv/media/Outside.mkv", "/elsewhere/Outside.mkv")
	f.mkdir("/elsewhere/collection")
	f.symlink("/srv/media/Collection.mkv", "/elsewhere/collection")
	f.mkdir("/var/state")
	return f
}

// TestWalk_ReturnsTheMediaFilesItSaw. The walk reads every entry of every
// directory it lists and already asks the media question of each one. It now
// hands those entries back, so the answer it reached is available to the pass
// that acts on it instead of being reached again from a second listing.
func TestWalk_ReturnsTheMediaFilesItSaw(t *testing.T) {
	f := mediaFixture()
	res := check(f, []string{"/srv/media"}, "/var/state")
	if !res.Start {
		t.Fatalf("the fixture refused the run at row %d: %+v", res.Row, res.Causes)
	}

	// One key per covered directory, and no key that is not one: what the walk
	// carries is what the walk LISTED, never a claim about anywhere else.
	covered := append([]string(nil), res.Coverage...)
	sort.Strings(covered)
	carried := make([]string, 0, len(res.Entries))
	for dir := range res.Entries {
		carried = append(carried, dir)
	}
	sort.Strings(carried)
	if !reflect.DeepEqual(carried, covered) {
		t.Fatalf("entries carried for %v, want exactly the covered directories %v", carried, covered)
	}

	// The entries of one directory, unfiltered and in name order: the sweep looks
	// for its own temp names here and the enumeration looks for source names, so
	// a result that kept only the media files would serve one and not the other.
	want := []string{"Collection.mkv", "Half.__transcoding__.mkv", "Inside.mkv", "Outside.mkv",
		"Top.mkv", "empty", "notes.txt", "season", "unreadable"}
	if got := entryNames(res.Entries["/srv/media"]); !reflect.DeepEqual(got, want) {
		t.Fatalf("the entries carried for /srv/media are %v, want %v", got, want)
	}

	// A directory that listed EMPTY carries an empty listing, not no listing. The
	// two say different things: one is evidence that holdfast looked and found
	// nothing, the other is no evidence at all.
	if ents, ok := res.Entries["/srv/media/empty"]; !ok || len(ents) != 0 {
		t.Errorf("the empty directory carries entries=%v present=%v, want a present and empty listing", ents, ok)
	}

	// A directory whose listing FAILED carries nothing, exactly as it is covered
	// by nothing: a listing that did not return is not evidence about what is in
	// it, and a source under it is enumerated by no route.
	if _, ok := res.Entries["/srv/media/unreadable"]; ok {
		t.Errorf("a directory whose listing failed carried entries; nothing may be concluded from a listing that did not return")
	}

	// Each entry says enough for a later pass to decide what it is without
	// looking again: the kind the listing reported, and - for a link - what
	// following it reaches.
	top := res.Entries["/srv/media"]
	for _, tc := range []struct {
		name          string
		isDir         bool
		resolvesToDir bool
	}{
		{"Top.mkv", false, false},
		{"notes.txt", false, false},
		{"Half.__transcoding__.mkv", false, false},
		{"season", true, true},
		{"empty", true, true},
		{"unreadable", true, true},
		{"Inside.mkv", false, false},
		{"Outside.mkv", false, false},
		// The one entry whose name and whose kind disagree. A listing calls a
		// symbolic link a non-directory whatever it points at, and this one
		// points at a directory: the walk followed it and says so.
		{"Collection.mkv", false, true},
	} {
		e, ok := entryFor(top, tc.name)
		if !ok {
			t.Errorf("%s is missing from the carried entries", tc.name)
			continue
		}
		if e.IsDir != tc.isDir || e.ResolvesToDir != tc.resolvesToDir {
			t.Errorf("%s carried IsDir=%v ResolvesToDir=%v, want %v/%v",
				tc.name, e.IsDir, e.ResolvesToDir, tc.isDir, tc.resolvesToDir)
		}
	}

	// The point of all of it: the sources the carried entries yield are the
	// sources a second listing yields, and reaching them costs no listing.
	before := f.readDirs
	got := fromEntries(res)
	if after := f.readDirs; after != before {
		t.Errorf("enumerating from the carried entries cost %d further listing(s); the walk already made them", after-before)
	}
	// enumerated() re-lists, which is what the scan used to do, so the comparison
	// is only honest with those listings excluded from the count above.
	relisted := enumerated(f, res)
	// The two agree everywhere but one path, and that path is the whole of the
	// difference: a listing reports a symbolic link as a non-directory whatever
	// it points at, so re-listing offers a DIRECTORY under a media name to a
	// pipeline that would try to transcode it. The walk followed the link and
	// carried the answer, so the same enumeration made from the entries skips it.
	if want := without(relisted, "/srv/media/Collection.mkv"); !reflect.DeepEqual(got, want) {
		t.Fatalf("the carried entries enumerate %v; re-listing the same directories enumerates %v", got, relisted)
	}
	want = []string{
		"/srv/media/Half.__transcoding__.mkv", // the fake's predicate is by extension alone
		"/srv/media/Inside.mkv",
		"/srv/media/Outside.mkv",
		"/srv/media/Top.mkv",
		"/srv/media/season/Ep1.mkv",
		"/srv/media/season/Ep2.mp4",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the carried entries enumerate %v, want %v", got, want)
	}
}

func without(paths []string, drop string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p != drop {
			out = append(out, p)
		}
	}
	return out
}

// TestWalk_CarriesNothingForAPathItDeclined. The region rule and the failure
// paths decide what this run may act on, and the entries must not go behind
// them: a directory the walk declined to re-enter, and one it entered and could
// not list, carry nothing - so no pass reading the result can enumerate from
// either, and neither becomes evidence that a file is gone.
func TestWalk_CarriesNothingForAPathItDeclined(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkfile("/srv/media/Film.mkv")
	f.bind("/srv/media/loop", "/srv/media") // the same storage under a second name
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")
	if !res.Start {
		t.Fatalf("the fixture refused the run at row %d: %+v", res.Row, res.Causes)
	}
	if !hasNotice(res, NoticeRegionWalked, "/srv/media/loop") {
		t.Fatalf("the fixture did not exercise the region rule: %+v", res.Notices)
	}
	if _, ok := res.Entries["/srv/media/loop"]; ok {
		t.Errorf("the walk carried entries for a path it declined to enter; nothing may be enumerated from it")
	}
	if got, want := fromEntries(res), []string{"/srv/media/Film.mkv"}; !reflect.DeepEqual(got, want) {
		t.Errorf("the carried entries enumerate %v, want %v - one file, under the spelling the walk reached first", got, want)
	}
}
