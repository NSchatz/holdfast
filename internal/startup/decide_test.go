package startup

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// TestDecide_TheOrderIsTotalAndTheListIsClosed drives AC10b's order directly:
// each pair of grounds is decided by the EARLIER row, and a configuration
// satisfying no refusal row starts. The report is never narrowed to the deciding
// row - every cause already established is named.
func TestDecide_TheOrderIsTotalAndTheListIsClosed(t *testing.T) {
	// rows 1 and 4 together: a malformed declaration decides, though a network
	// mount is uncovered as well.
	t.Run("rows 1 and 4: malformed decides", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mount("/srv/media/tv", "nfs")
		f.mkdir("/var/state")
		res := check(f, []string{"/srv/media"}, "/var/state", "/elsewhere/tv")
		if res.Row != 1 {
			t.Fatalf("row = %d, want 1 (malformed declaration): %+v", res.Row, res.Causes)
		}
		if !hasCause(res, CauseMalformed, "/elsewhere/tv") {
			t.Fatalf("the offending declaration was not named: %+v", res.Causes)
		}
	})

	// rows 2 and 4: a missing root decides, though the state directory is on a
	// network share.
	t.Run("rows 2 and 4: missing decides", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/mnt/nas").setType("/mnt/nas", "nfs")
		res := check(f, []string{"/srv/media"}, "/mnt/nas/state")
		if res.Row != 2 {
			t.Fatalf("row = %d, want 2 (a configured library root does not exist): %+v", res.Row, res.Causes)
		}
		if !hasCause(res, CauseMissingRoot, "/srv/media") {
			t.Fatalf("the missing root was not named: %+v", res.Causes)
		}
		if !hasCause(res, CauseNotLocal, "/mnt/nas/state") {
			t.Fatalf("the report was narrowed to the deciding row: %+v", res.Causes)
		}
	})

	// rows 3 and 4: one permission denial beside three uncovered network mounts
	// refuses at row 3 and names the denial with NO declaration AND all three
	// mounts, each with its own declaration.
	t.Run("rows 3 and 4: permission decides and every cause is still named", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mount("/srv/media/a", "nfs")
		f.mount("/srv/media/b", "nfs")
		f.mount("/srv/media/c", "cifs")
		f.mkdir("/var/state")
		f.failStat("/var/state", fs.ErrPermission)

		res := check(f, []string{"/srv/media"}, "/var/state")

		if res.Row != 3 {
			t.Fatalf("row = %d, want 3 (a checked path cannot be inspected): %+v", res.Row, res.Causes)
		}
		denials := causesOf(res, CauseDenied)
		if len(denials) != 1 || denials[0].Path != "/var/state" || denials[0].Declaration != "" {
			t.Fatalf("permission denial = %+v, want exactly one for the state directory with NO declaration", denials)
		}
		if denials[0].Remedy == "" {
			t.Fatal("a refusal no declaration can lift printed no remedy either")
		}
		notLocal := causesOf(res, CauseNotLocal)
		if len(notLocal) != 3 {
			t.Fatalf("uncovered mounts named = %d, want all 3 in the one refusal: %+v", len(notLocal), notLocal)
		}
		for _, c := range notLocal {
			if c.Declaration == "" {
				t.Fatalf("no declaration printed for %s: %+v", c.Path, c)
			}
		}
	})

	t.Run("row 5: nothing applies and the run starts", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkfile("/srv/media/Film.mkv")
		f.mkdir("/var/state")
		res := check(f, []string{"/srv/media"}, "/var/state")
		if !res.Start || res.Row != 5 {
			t.Fatalf("row = %d start = %v, want 5/true: %+v", res.Row, res.Start, res.Causes)
		}
		if len(res.Causes) != 0 {
			t.Fatalf("a started run carries causes: %+v", res.Causes)
		}
	})
}

// TestDecide_MissingRootIsNamedMissingAndNeverLocal (AC6).
func TestDecide_MissingRootIsNamedMissingAndNeverLocal(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	rec, ok := recordFor(res, "/srv/media")
	if !ok || !rec.Missing || rec.Class == Local || rec.Denied {
		t.Fatalf("record = %+v, want a missing record that is not local and not a permission denial", rec)
	}
	if res.Row != 2 {
		t.Fatalf("row = %d, want 2", res.Row)
	}
}

// TestDecide_PermissionDenialRefusesWhateverWasDeclared (AC4, AC8a). An opt-in
// permits a run on storage that is not local; it never permits a run on a path
// holdfast cannot inspect.
func TestDecide_PermissionDenialRefusesWhateverWasDeclared(t *testing.T) {
	t.Run("a state directory on a share the process cannot stat", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkdir("/mnt/nas/state").setType("/mnt/nas", "nfs")
		f.failStat("/mnt/nas/state", fs.ErrPermission)
		f.failType("/mnt/nas/state", fs.ErrPermission)

		res := check(f, []string{"/srv/media"}, "/mnt/nas/state", "/mnt/nas/state")

		if res.Start {
			t.Fatal("an opt-in naming exactly the unstattable path started the run")
		}
		if res.Row != 3 {
			t.Fatalf("row = %d, want 3", res.Row)
		}
		for _, c := range res.Causes {
			if c.Path == "/mnt/nas/state" && c.Declaration != "" {
				t.Fatalf("a declaration was printed for a path holdfast cannot inspect: %+v", c)
			}
		}
		var out bytes.Buffer
		res.WriteRefusal(&out)
		if strings.Contains(out.String(), ConfigKey+":") {
			t.Fatalf("the refusal offered a declaration that could not lift it:\n%s", out.String())
		}
	})

	t.Run("a root beneath a parent the process may not read is denied, never missing", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media/tv")
		f.mkdir("/var/state")
		f.failStat("/srv/media/tv", fs.ErrPermission)
		f.failType("/srv/media/tv", fs.ErrPermission)

		res := check(f, []string{"/srv/media/tv"}, "/var/state")

		if res.Row != 3 {
			t.Fatalf("row = %d, want 3: %+v", res.Row, res.Causes)
		}
		if hasCause(res, CauseMissingRoot, "/srv/media/tv") {
			t.Fatal("a path whose non-existence could not be established was reported missing")
		}
		if !hasCause(res, CauseDenied, "/srv/media/tv") {
			t.Fatalf("permission denial was not reported: %+v", res.Causes)
		}
		rec, _ := recordFor(res, "/srv/media/tv")
		if rec.Class == Local {
			t.Fatal("a path that could not be inspected was classified local")
		}
	})
}

// TestDecide_ARootThatCannotBeListedRefusesAtRow3ForAnyCause is the F62 reading,
// made concrete: AC4b governs a configured ROOT and AC2f1's non-refusal holds for
// a root only where the region rule DECLINED it. A root whose listing fails - for
// permission, for an I/O error, for a stale handle - refuses at row 3 and reports
// the cause it observed, while the same condition one level down starts.
func TestDecide_ARootThatCannotBeListedRefusesAtRow3ForAnyCause(t *testing.T) {
	tests := []struct {
		name      string
		fsType    string
		readErr   error
		declare   []string
		detailHas string
		detailNot string
	}{
		{
			name: "mode 000 on local storage: permission denied", fsType: "ext4",
			readErr: fs.ErrPermission, detailHas: "permission denied",
		},
		{
			name: "a local root whose listing fails with an I/O error", fsType: "ext4",
			readErr:   errors.New("input/output error"),
			detailHas: "input/output error", detailNot: "permission denied",
		},
		{
			name: "an opted-in nfs root whose listing fails with a stale handle", fsType: "nfs",
			readErr: errors.New("stale file handle"), declare: []string{"/srv/media"},
			detailHas: "stale file handle", detailNot: "permission denied",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFS().setType("/", "ext4")
			f.mkdir("/srv/media").setType("/srv/media", tc.fsType)
			f.failRead("/srv/media", tc.readErr)
			f.mkdir("/var/state")

			res := check(f, []string{"/srv/media"}, "/var/state", tc.declare...)

			if res.Start {
				t.Fatal("a configured root that could not be listed started the run: its whole subtree is unknown, which would look exactly like an empty library")
			}
			if res.Row != 3 {
				t.Fatalf("row = %d, want 3: %+v", res.Row, res.Causes)
			}
			cs := causesOf(res, CauseUnlistable)
			if len(cs) != 1 || cs[0].Path != "/srv/media" {
				t.Fatalf("causes = %+v, want one unlistable-root cause", cs)
			}
			if !strings.Contains(cs[0].Detail, tc.detailHas) {
				t.Fatalf("cause detail = %q, want it to report the cause observed (%q)", cs[0].Detail, tc.detailHas)
			}
			if tc.detailNot != "" && strings.Contains(cs[0].Detail, tc.detailNot) {
				t.Fatalf("cause detail = %q, want it NOT to say %q", cs[0].Detail, tc.detailNot)
			}
			if cs[0].Declaration != "" {
				t.Fatalf("a declaration was named as a remedy for an unlistable root: %+v", cs[0])
			}
			if hasNotice(res, NoticeEmptyRoot, "/srv/media") {
				t.Fatal("a root holdfast could not list was reported present and empty")
			}
		})
	}

	t.Run("the same condition one level down starts", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media/tv")
		f.mkfile("/srv/media/Film.mkv")
		f.failRead("/srv/media/tv", errors.New("input/output error"))
		f.mkdir("/var/state")

		res := check(f, []string{"/srv/media"}, "/var/state")

		if !res.Start {
			t.Fatalf("an unlistable directory one level down refused the run: %+v", res.Causes)
		}
		if !hasNotice(res, NoticeListingFailed, "/srv/media/tv") {
			t.Fatalf("it was not reported either: %+v", res.Notices)
		}
	})

	// AC4b's antecedent does not turn on declaration ORDER. A configured root
	// nested inside another is reached by the outer root's walk first, as an
	// ordinary subdirectory, and its region is marked ON ENTRY (AC2b2) even
	// though the listing there failed - so the region rule would otherwise
	// decline it and refuse nothing. AC2b3's rationale for that non-refusal is
	// false here ("every source under such a root was reachable through the root
	// that WAS walked" - none was reachable at all) while AC4b's antecedent holds
	// exactly: the root exists, it can be inspected, and it cannot be listed.
	// Both declaration orders refuse at row 3 and report the same observed cause.
	t.Run("a nested root that cannot be listed refuses in either declaration order", func(t *testing.T) {
		for _, order := range [][]string{
			{"/srv/media", "/srv/media/tv"},
			{"/srv/media/tv", "/srv/media"},
		} {
			t.Run(strings.Join(order, " then "), func(t *testing.T) {
				f := newFS().setType("/", "ext4")
				f.mkdir("/srv/media/tv")
				f.mkfile("/srv/media/tv/Movie.mkv")
				f.failRead("/srv/media/tv", errors.New("input/output error"))
				f.mkdir("/var/state")

				res := check(f, order, "/var/state")

				if res.Start || res.Row != 3 {
					t.Fatalf("start=%v row=%d, want a row-3 refusal: %+v", res.Start, res.Row, res.Causes)
				}
				cs := causesOf(res, CauseUnlistable)
				if len(cs) != 1 || cs[0].Path != "/srv/media/tv" {
					t.Fatalf("causes = %+v, want exactly one unlistable-root cause naming /srv/media/tv", cs)
				}
				if !strings.Contains(cs[0].Detail, "input/output error") {
					t.Fatalf("cause detail = %q, want the cause it observed", cs[0].Detail)
				}
				if cs[0].Declaration != "" {
					t.Fatalf("a declaration was named as a remedy for an unlistable root: %+v", cs[0])
				}
				if hasNotice(res, NoticeEmptyRoot, "/srv/media/tv") {
					t.Fatal("a root holdfast could not list was reported present and empty")
				}
				// AC2f1 reports the path WITH THE REASON. Whichever order put
				// the region in the entered map, no report of this path may say
				// its sources come from storage the walk covered: the walk
				// covered none of it and enumerated nothing, from anywhere.
				for _, n := range res.Notices {
					if n.Path != "/srv/media/tv" {
						continue
					}
					if strings.Contains(n.Detail, "already walked") || strings.Contains(n.Detail, "enumerated exactly once") {
						t.Fatalf("an unlistable root was reported as storage already walked elsewhere: %q", n.Detail)
					}
				}
				if got := enumerated(f, res); len(got) != 0 {
					t.Fatalf("sources enumerated = %v, want none: nothing under an unlistable root is reachable", got)
				}
			})
		}
	})

	// The other half of F62: a root the region rule DECLINED is not a listing
	// failure, refuses nothing, and no row reads on the decline itself.
	t.Run("a root declined by the region rule refuses nothing", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkfile("/srv/media/Film.mkv")
		f.bind("/srv/library", "/srv/media")
		f.mkdir("/var/state")

		res := check(f, []string{"/srv/media", "/srv/library"}, "/var/state")

		if !res.Start {
			t.Fatalf("declining a region refused the run: %+v", res.Causes)
		}
		if len(causesOf(res, CauseUnlistable)) != 0 {
			t.Fatalf("declining a region was reported as a failure to list it: %+v", res.Causes)
		}
	})
}

// TestDecide_ADeclinedRootIsStillACheckedPath is the F63 reading: what the region
// rule withdraws is the DECLINE as a cause, and nothing else. A declined root
// stays a checked path for every row, so a second root exposing one NFS share is
// still a path an opt-in must cover in its own right.
func TestDecide_ADeclinedRootIsStillACheckedPath(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mount("/srv/media", "nfs")
		f.mkfile("/srv/media/Film.mkv")
		f.bind("/srv/media2", "/srv/media")
		f.mkdir("/var/state")
		return f
	}

	// One declaration, naming only the first root: the declined root is still a
	// checked path, still non-local, and still uncovered.
	res := check(build(), []string{"/srv/media", "/srv/media2"}, "/var/state", "/srv/media")
	if res.Start {
		t.Fatal("a declined root was exempted from the opt-in requirement")
	}
	if res.Row != 4 {
		t.Fatalf("row = %d, want 4: %+v", res.Row, res.Causes)
	}
	cs := causesOf(res, CauseNotLocal)
	if len(cs) != 1 || cs[0].Path != "/srv/media2" || cs[0].Declaration != "/srv/media2" {
		t.Fatalf("causes = %+v, want one naming /srv/media2 with its own declaration", cs)
	}
	if !hasNotice(res, NoticeRegionWalked, "/srv/media2") {
		t.Fatalf("the declined root was not reported as undescended: %+v", res.Notices)
	}

	// Declaring both starts the run, and each carries its own reduced-guarantee
	// record.
	f2 := build()
	res2 := check(f2, []string{"/srv/media", "/srv/media2"}, "/var/state", "/srv/media", "/srv/media2")
	if !res2.Start {
		t.Fatalf("both roots declared and the run was still refused: %+v", res2.Causes)
	}
	if n := noticesOf(res2, NoticeReducedGuarantee); len(n) != 2 {
		t.Fatalf("reduced-guarantee records = %+v, want one for each checked path", n)
	}
	// And the source is still enumerated exactly once.
	wantEnumerated(t, f2, res2, "/srv/media/Film.mkv")
}

// TestDecide_AMountTheWalkFoundButCannotInspectRefusesAtRow3: a mount the walk
// FOUND is a checked path in its own right (AC2b), so AC4 reads on it exactly as
// it reads on a root or the state directory. Being unable to establish its type
// at all is a failure to INSPECT and refuses at row 3 whatever was declared -
// which is a different thing from being unable to LIST it (AC4a), where the run
// still starts.
func TestDecide_AMountTheWalkFoundButCannotInspectRefusesAtRow3(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkfile("/srv/media/Film.mkv")
		f.mount("/srv/media/vault", "ext4")
		f.failType("/srv/media/vault", fs.ErrPermission)
		f.mkdir("/var/state")
		return f
	}

	res := check(build(), []string{"/srv/media"}, "/var/state")
	if res.Start {
		t.Fatal("a checked path holdfast could not inspect started the run")
	}
	if res.Row != 3 {
		t.Fatalf("row = %d, want 3: %+v", res.Row, res.Causes)
	}
	if !hasCause(res, CauseDenied, "/srv/media/vault") {
		t.Fatalf("the denial was not reported for the mount: %+v", res.Causes)
	}
	rec, ok := recordFor(res, "/srv/media/vault")
	if !ok || rec.Class == Local {
		t.Fatalf("record = %+v, want a record that is not classified local", rec)
	}

	// No opt-in lifts it, and none is printed as though one could.
	res2 := check(build(), []string{"/srv/media"}, "/var/state", "/srv/media/vault")
	if res2.Start {
		t.Fatal("an opt-in naming the uninspectable mount started the run")
	}
	for _, c := range res2.Causes {
		if c.Path == "/srv/media/vault" && c.Declaration != "" {
			t.Fatalf("a declaration was printed for a path holdfast cannot inspect: %+v", c)
		}
	}
	// And it is NOT reported as an unnecessary declaration: the remedy is to
	// grant permission, never to delete the line.
	if hasNotice(res2, NoticeUnnecessary, "/srv/media/vault") {
		t.Fatalf("a declaration on an uninspectable path was called unnecessary: %+v", res2.Notices)
	}
}

// TestDeclarations_OnAPathThatCannotBeInspectedSayWhy: AC10a says no declaration
// can be COMPARED against a checked path whose resolved form cannot be
// established, and it is refused at row 3 with no declaration named. What must
// not happen is the report calling that declaration "unnecessary", which would
// tell the operator to delete the one line that names the real problem.
func TestDeclarations_OnAPathThatCannotBeInspectedSayWhy(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mkdir("/mnt/nas/state").setType("/mnt/nas", "nfs")
	f.failStat("/mnt/nas/state", fs.ErrPermission)
	f.failType("/mnt/nas/state", fs.ErrPermission)

	res := check(f, []string{"/srv/media"}, "/mnt/nas/state", "/mnt/nas/state")

	if res.Row != 3 {
		t.Fatalf("row = %d, want 3: %+v", res.Row, res.Causes)
	}
	if hasNotice(res, NoticeUnnecessary, "/mnt/nas/state") {
		t.Fatalf("a declaration naming a path holdfast cannot inspect was reported unnecessary: %+v", res.Notices)
	}
	if !hasNotice(res, NoticeUnresolvable, "/mnt/nas/state") {
		t.Fatalf("the declaration was passed over in silence instead: %+v", res.Notices)
	}
}

// TestDecide_StateDirectoryThatDoesNotExistYet (AC5, AC5a, AC10a). A fresh
// install starts: the storage the directory WOULD be created on is classified,
// the directory is not created in order to classify it, and no opt-in is
// demanded.
func TestDecide_StateDirectoryThatDoesNotExistYet(t *testing.T) {
	t.Run("over local storage a fresh install starts with no declarations at all", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkdir("/var/lib")

		res := check(f, []string{"/srv/media"}, "/var/lib/holdfast/state")

		if !res.Start {
			t.Fatalf("a fresh install was refused: %+v", res.Causes)
		}
		rec, ok := recordFor(res, "/var/lib/holdfast/state")
		if !ok || rec.Class != Local || rec.Missing {
			t.Fatalf("state directory record = %+v, want a local record that is not missing", rec)
		}
		if _, created := f.nodes["/var/lib/holdfast/state"]; created {
			t.Fatal("the state directory was created in order to classify it")
		}
		if _, created := f.nodes["/var/lib/holdfast"]; created {
			t.Fatal("an ancestor of the state directory was created in order to classify it")
		}
	})

	t.Run("over network storage an opt-in naming exactly it covers it", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkdir("/mnt/nas").setType("/mnt/nas", "nfs")

		res := check(f, []string{"/srv/media"}, "/mnt/nas/holdfast/state", "/mnt/nas/holdfast/state")

		if !res.Start {
			t.Fatalf("both sides resolve through /mnt/nas and the opt-in still did not cover it: %+v", res.Causes)
		}
		if !hasNotice(res, NoticeReducedGuarantee, "/mnt/nas/holdfast/state") {
			t.Fatalf("no reduced-guarantee record for the covered state directory: %+v", res.Notices)
		}
	})

	t.Run("over network storage with no opt-in it refuses at row 4", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkdir("/mnt/nas").setType("/mnt/nas", "nfs")

		res := check(f, []string{"/srv/media"}, "/mnt/nas/holdfast/state")

		if res.Row != 4 {
			t.Fatalf("row = %d, want 4 (never row 2: a state directory that does not exist is not a missing library root)", res.Row)
		}
		if hasCause(res, CauseMissingRoot, "/mnt/nas/holdfast/state") {
			t.Fatal("a not-yet-existing state directory was refused as a missing library root")
		}
	})

	t.Run("whose nearest existing ancestor cannot be inspected refuses at row 3", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkdir("/mnt/nas")
		f.failStat("/mnt/nas", fs.ErrPermission)

		res := check(f, []string{"/srv/media"}, "/mnt/nas/holdfast/state")

		if res.Row != 3 {
			t.Fatalf("row = %d, want 3: %+v", res.Row, res.Causes)
		}
	})
}

// TestDecide_PresentAndEmptyRoot (AC7).
func TestDecide_PresentAndEmptyRoot(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media/tv")
	f.mkfile("/srv/media/tv/notes.txt") // present, but nothing this run would enumerate
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	if !res.Start {
		t.Fatalf("an empty library refused the run: %+v", res.Causes)
	}
	if !hasNotice(res, NoticeEmptyRoot, "/srv/media") {
		t.Fatalf("an empty root was not reported present and empty: %+v", res.Notices)
	}
	if rec, _ := recordFor(res, "/srv/media"); rec.Missing {
		t.Fatal("an empty root was reported as missing")
	}
}

// TestDeclarations_TheMalformedTestIsSyntacticAndRunsAtStartup (AC11, AC11a).
func TestDeclarations_TheMalformedTestIsSyntacticAndRunsAtStartup(t *testing.T) {
	base := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media/tv")
		f.mkdir("/var/state")
		return f
	}
	tests := []struct {
		name      string
		decl      string
		malformed bool
	}{
		{"a configured library root", "/srv/media", false},
		{"a configured root with a trailing separator", "/srv/media/", false},
		{"a path beneath a configured root", "/srv/media/tv", false},
		{"a path beneath a root that does not exist yet", "/srv/media/not/here/yet", false},
		{"a typo beneath a root", "/srv/media/tvv", false},
		{"the state directory", "/var/state", false},
		{"a path outside every root", "/srv/medja/tv", true},
		{"a sibling with a shared prefix", "/srv/mediaX", true},
		{"a relative path", "srv/media/tv", true},
		{"an empty declaration", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := check(base(), []string{"/srv/media"}, "/var/state", tc.decl)
			got := hasCause(res, CauseMalformed, tc.decl)
			if got != tc.malformed {
				t.Fatalf("declaration %q malformed = %v, want %v: %+v", tc.decl, got, tc.malformed, res.Causes)
			}
			if tc.malformed && res.Row != 1 {
				t.Fatalf("a malformed declaration did not decide at row 1: row %d", res.Row)
			}
		})
	}

	// The check touches no storage and has no failure outcome: a declaration
	// beneath a root in mode 000 is still WELL FORMED, and the refusal that
	// follows names the unlistable ROOT, never malformedness.
	t.Run("a declaration beneath an unreadable root is well formed", func(t *testing.T) {
		f := base()
		f.failRead("/srv/media", fs.ErrPermission)
		res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media/tv")
		if hasCause(res, CauseMalformed, "/srv/media/tv") {
			t.Fatalf("the syntactic check depended on reading storage: %+v", res.Causes)
		}
		if res.Row != 3 || !hasCause(res, CauseUnlistable, "/srv/media") {
			t.Fatalf("row = %d, want 3 naming the unlistable root: %+v", res.Row, res.Causes)
		}
	})

	// A symlinked root: the configured spelling is well formed and covers the
	// path; the resolved spelling is malformed.
	t.Run("a symlinked root takes the configured spelling and rejects the resolved one", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/mnt/pool/media/tv")
		f.symlink("/srv/media", "/mnt/pool/media")
		f.mkdir("/var/state")

		if res := check(f, []string{"/srv/media"}, "/var/state", "/mnt/pool/media/tv"); !hasCause(res, CauseMalformed, "/mnt/pool/media/tv") {
			t.Fatalf("the resolved spelling was accepted: %+v", res.Causes)
		}
		if res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media/tv"); hasCause(res, CauseMalformed, "/srv/media/tv") {
			t.Fatalf("the configured spelling was rejected: %+v", res.Causes)
		}
	})
}

// TestDeclarations_PrintedDeclarationsRoundTrip (AC11b): what holdfast prints is
// what its own syntactic check accepts, and adding exactly that text starts the
// run when it was the only uncovered path.
func TestDeclarations_PrintedDeclarationsRoundTrip(t *testing.T) {
	build := func() *fakeFS {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mount("/srv/media/tv", "nfs")
		f.mkdir("/mnt/nas").setType("/mnt/nas", "nfs")
		return f
	}
	// Both a mount beneath a root and the state directory, which is spelled as
	// the absolute path the configuration interpretation produces.
	first := check(build(), []string{"/srv/media"}, "/mnt/nas/holdfast/state")
	if first.Start {
		t.Fatal("started with two uncovered paths")
	}
	var printed []string
	var out bytes.Buffer
	first.WriteRefusal(&out)
	for _, line := range strings.Split(out.String(), "\n") {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, "- /") {
			printed = append(printed, strings.TrimPrefix(t, "- "))
		}
	}
	if len(printed) != 2 {
		t.Fatalf("declarations printed = %v, want one for each uncovered path:\n%s", printed, out.String())
	}

	second := check(build(), []string{"/srv/media"}, "/mnt/nas/holdfast/state", printed...)
	if len(causesOf(second, CauseMalformed)) != 0 {
		t.Fatalf("holdfast printed a declaration its own check calls malformed: %+v", second.Causes)
	}
	if !second.Start {
		t.Fatalf("feeding back exactly what was printed did not start the run: %+v", second.Causes)
	}
}

// TestDeclarations_UnnecessaryIsReportedRatherThanRefusedOrSilent (AC25).
func TestDeclarations_UnnecessaryIsReportedRatherThanRefusedOrSilent(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media/tv")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state",
		"/srv/media",      // a checked path classified local
		"/srv/media/tv",   // well formed, beneath a root, no mount there
		"/srv/media/typo", // a typo beneath a root: well formed, covers nothing
	)

	if !res.Start {
		t.Fatalf("an unnecessary declaration refused the run: %+v", res.Causes)
	}
	for _, p := range []string{"/srv/media", "/srv/media/tv", "/srv/media/typo"} {
		if !hasNotice(res, NoticeUnnecessary, p) {
			t.Fatalf("%s was not reported unnecessary: %+v", p, res.Notices)
		}
	}
	if n := noticesOf(res, NoticeReducedGuarantee); len(n) != 0 {
		t.Fatalf("an unnecessary declaration earned a reduced-guarantee record: %+v", n)
	}
}

// TestDeclarations_ComparedInResolvedFormOnBothSides (AC10a).
func TestDeclarations_ComparedInResolvedFormOnBothSides(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mount("/srv/media", "nfs")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media/")

	if !res.Start {
		t.Fatalf("a trailing separator defeated the comparison: %+v", res.Causes)
	}
	n := 0
	for _, rec := range res.Records {
		if rec.Path == "/srv/media" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("records for the root = %d, want exactly one", n)
	}
}

// TestDecide_ClassificationIsNotCached (AC12a): every start classifies afresh.
func TestDecide_ClassificationIsNotCached(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media").setType("/srv/media", "nfs")
	f.mkdir("/var/state")

	first := check(f, []string{"/srv/media"}, "/var/state")
	if rec, _ := recordFor(first, "/srv/media"); rec.Class != NonLocal {
		t.Fatalf("first start classified %s, want non-local", rec.Class)
	}

	f.setType("/srv/media", "ext4") // moved onto local disk between starts
	second := check(f, []string{"/srv/media"}, "/var/state")
	if rec, _ := recordFor(second, "/srv/media"); rec.Class != Local {
		t.Fatalf("second start classified %s, want local: a classification was reused across starts", rec.Class)
	}
	if !second.Start {
		t.Fatalf("the second start was refused on a stale classification: %+v", second.Causes)
	}
}
