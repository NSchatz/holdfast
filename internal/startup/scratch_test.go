package startup

import (
	"bytes"
	"errors"
	"io/fs"
	"strings"
	"testing"
)

// The configured working location, in the same start-or-refuse decision as the
// library roots. Five things refuse the run and one - storage that is not local -
// deliberately does not.

// checkScratchDir runs the whole check with a scratch directory configured.
func checkScratchDir(f *fakeFS, roots []string, stateDir, scratch string, floorGB int, decls ...string) Result {
	return Run(Check{
		Roots:            roots,
		StateDir:         stateDir,
		Declarations:     decls,
		IsMediaFile:      mediaByExt,
		ScratchDir:       scratch,
		ScratchMinFreeGB: floorGB,
		Platform:         f,
	})
}

// libraryFS is the layout every case below starts from: a local root with one media
// file, a local state directory, and a scratch directory OUTSIDE the root.
func libraryFS() *fakeFS {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mkfile("/srv/media/Film.mkv")
	f.mkdir("/var/state")
	f.mkdir("/mnt/scratch")
	return f
}

// refusalText renders the operator-facing account, which is what several criteria
// are actually about: naming the path, the cause and the remedy.
func refusalText(res Result) string {
	var b bytes.Buffer
	res.WriteRefusal(&b)
	return b.String()
}

// assertRefused checks the shape every scratch refusal owes: it decided at the
// scratch row, it is a refusal, and the account names the path and offers a remedy.
func assertRefused(t *testing.T, res Result, kind CauseKind, path string) {
	t.Helper()
	if res.Start {
		t.Fatalf("the run STARTED: %+v", res.Causes)
	}
	if res.Row != rowScratch {
		t.Fatalf("row = %d, want %d (the scratch row): %+v", res.Row, rowScratch, res.Causes)
	}
	if !hasCause(res, kind, path) {
		t.Fatalf("no %s cause for %s: %+v", kind, path, res.Causes)
	}
	text := refusalText(res)
	if !strings.Contains(text, path) {
		t.Fatalf("the account does not name the path %s:\n%s", path, text)
	}
	if !strings.Contains(text, "remedy:") {
		t.Fatalf("the account offers no remedy:\n%s", text)
	}
	if !strings.Contains(text, "nothing was encoded") {
		t.Fatalf("the account does not state that nothing was encoded:\n%s", text)
	}
}

// AC-B7: a scratch_dir that does not exist, or that exists and is not a directory,
// refuses the run - and encodes, creates, renames and removes nothing. The second
// clause is checked directly: the writability probe, the ONE thing this package can
// write with, is never even asked.
func TestScratch_MissingOrNotADirectoryRefusesAndWritesNothing(t *testing.T) {
	t.Run("does not exist", func(t *testing.T) {
		f := libraryFS()
		res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/mnt/nowhere", 0)
		assertRefused(t, res, CauseScratchMissing, "/mnt/nowhere")
		if f.probes != 0 {
			t.Fatalf("the writability probe ran %d time(s) - a refusal for a missing directory must create nothing", f.probes)
		}
		if res.ScratchProbed {
			t.Fatal("the result claims a probe ran")
		}
		rec, ok := recordFor(res, "/mnt/nowhere")
		if !ok || !rec.Missing || rec.Kind != KindScratchDir {
			t.Fatalf("the missing scratch directory is not recorded as missing: %+v (found=%v)", rec, ok)
		}
	})

	t.Run("exists but is a file", func(t *testing.T) {
		f := libraryFS()
		f.mkfile("/mnt/notadir")
		res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/mnt/notadir", 0)
		assertRefused(t, res, CauseScratchNotDir, "/mnt/notadir")
		if f.probes != 0 {
			t.Fatalf("the writability probe ran %d time(s) against a path that is not a directory", f.probes)
		}
	})

	// The control arm: the identical layout with a real directory STARTS, so the two
	// refusals above are about the path and not about configuring scratch_dir at all.
	t.Run("control: a real directory starts the run", func(t *testing.T) {
		f := libraryFS()
		res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/mnt/scratch", 0)
		if !res.Start {
			t.Fatalf("a configured, present, writable scratch directory refused the run: %+v", res.Causes)
		}
	})
}

// AC-B8: a scratch_dir the running user cannot create and remove a file in refuses
// the run exactly as AC-B7 requires, naming the path and the permission cause.
func TestScratch_AnUnwritableDirectoryRefusesNamingThePermissionCause(t *testing.T) {
	f := libraryFS()
	f.failWrite("/mnt/scratch", fs.ErrPermission)
	res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/mnt/scratch", 0)
	assertRefused(t, res, CauseScratchUnwritable, "/mnt/scratch")

	text := refusalText(res)
	if !strings.Contains(text, "permission") {
		t.Fatalf("the account does not name the permission cause:\n%s", text)
	}
	if !strings.Contains(text, "create and remove") {
		t.Fatalf("the account does not say what could not be done:\n%s", text)
	}
	// The probe DID run here - the criterion is about what it found - and the
	// refusal says so rather than printing an unqualified "created nothing".
	if !res.ScratchProbed || f.probes != 1 {
		t.Fatalf("the writability probe ran %d time(s) (recorded=%v), want exactly once", f.probes, res.ScratchProbed)
	}
	if !strings.Contains(text, "probe file") {
		t.Fatalf("the account does not state what the check itself wrote:\n%s", text)
	}
}

// AC-B9: free space below scratch_min_free_gb refuses, and the account names the
// path, the free space observed, the configured floor and the remedy.
func TestScratch_FreeSpaceBelowTheFloorRefusesAndNamesTheFigures(t *testing.T) {
	const floorGB = 50
	observed := uint64(3) * gibibyteBytes

	f := libraryFS()
	f.setFree("/mnt/scratch", observed)
	res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/mnt/scratch", floorGB)
	assertRefused(t, res, CauseScratchLowSpace, "/mnt/scratch")

	text := refusalText(res)
	for _, want := range []string{"3221225472", "50 GiB", "scratch_min_free_gb"} {
		if !strings.Contains(text, want) {
			t.Errorf("the account does not name %q:\n%s", want, text)
		}
	}
	if f.probes != 0 {
		t.Fatalf("the writability probe ran %d time(s) after the free-space refusal - a refusal must create nothing", f.probes)
	}

	// The control arm: the same floor against enough space starts the run, so the
	// refusal is about the arithmetic and not about setting a floor at all.
	t.Run("control: enough space starts the run", func(t *testing.T) {
		g := libraryFS()
		g.setFree("/mnt/scratch", uint64(floorGB)*gibibyteBytes)
		if res := checkScratchDir(g, []string{"/srv/media"}, "/var/state", "/mnt/scratch", floorGB); !res.Start {
			t.Fatalf("exactly the floor refused the run: %+v", res.Causes)
		}
	})

	// A floor of 0 disables the check outright, which is the documented way to turn
	// it off - so it must not even ask.
	t.Run("a floor of 0 does not check", func(t *testing.T) {
		g := libraryFS()
		g.setFree("/mnt/scratch", 1)
		if res := checkScratchDir(g, []string{"/srv/media"}, "/var/state", "/mnt/scratch", 0); !res.Start {
			t.Fatalf("a floor of 0 still refused for want of space: %+v", res.Causes)
		}
	})

	// A lookup that cannot answer is a refusal and never a guessed number: zero
	// would refuse a device that is fine, and a large number would clear one that is
	// full.
	t.Run("a lookup that fails refuses", func(t *testing.T) {
		g := libraryFS()
		g.failFree("/mnt/scratch", errors.New("statfs exploded"))
		res := checkScratchDir(g, []string{"/srv/media"}, "/var/state", "/mnt/scratch", floorGB)
		assertRefused(t, res, CauseScratchDenied, "/mnt/scratch")
	})
}

// AC-B10: a scratch_dir that is a library root, is beneath one, or has one beneath
// it - compared after symlink resolution, as the existing checks compare - refuses
// the run, naming both paths and the remedy.
func TestScratch_OverlappingALibraryRootRefusesNamingBothPaths(t *testing.T) {
	cases := []struct {
		name    string
		build   func() *fakeFS
		roots   []string
		scratch string
	}{
		{
			name: "the scratch directory IS a library root",
			build: func() *fakeFS {
				f := newFS().setType("/", "ext4")
				f.mkdir("/srv/media")
				f.mkfile("/srv/media/Film.mkv")
				f.mkdir("/var/state")
				return f
			},
			roots:   []string{"/srv/media"},
			scratch: "/srv/media",
		},
		{
			name: "the scratch directory is BENEATH a library root",
			build: func() *fakeFS {
				f := newFS().setType("/", "ext4")
				f.mkdir("/srv/media/work")
				f.mkfile("/srv/media/Film.mkv")
				f.mkdir("/var/state")
				return f
			},
			roots:   []string{"/srv/media"},
			scratch: "/srv/media/work",
		},
		{
			name: "a library root is BENEATH the scratch directory",
			build: func() *fakeFS {
				f := newFS().setType("/", "ext4")
				f.mkdir("/srv/media")
				f.mkfile("/srv/media/Film.mkv")
				f.mkdir("/var/state")
				return f
			},
			roots:   []string{"/srv/media"},
			scratch: "/srv",
		},
		{
			// After symlink resolution, exactly as every other path comparison in
			// this package is made - a lexical comparison alone would let a link
			// into the library through.
			name: "the scratch directory is a SYMLINK into a library root",
			build: func() *fakeFS {
				f := newFS().setType("/", "ext4")
				f.mkdir("/srv/media/work")
				f.mkfile("/srv/media/Film.mkv")
				f.mkdir("/var/state")
				f.mkdir("/mnt")
				f.symlink("/mnt/scratch", "/srv/media/work")
				return f
			},
			roots:   []string{"/srv/media"},
			scratch: "/mnt/scratch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.build()
			res := checkScratchDir(f, tc.roots, "/var/state", tc.scratch, 0)
			assertRefused(t, res, CauseScratchOverlaps, tc.scratch)
			text := refusalText(res)
			if !strings.Contains(text, tc.roots[0]) {
				t.Fatalf("the account does not name the library root %s:\n%s", tc.roots[0], text)
			}
			if f.probes != 0 {
				t.Fatalf("the writability probe ran %d time(s) inside the library after an overlap refusal", f.probes)
			}
		})
	}

	// The control arm: a sibling that merely SHARES A PREFIX with a root is not an
	// overlap, and refusing it would be refusing a perfectly good configuration.
	t.Run("control: a sibling sharing a prefix is not an overlap", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.mkfile("/srv/media/Film.mkv")
		f.mkdir("/srv/mediaX")
		f.mkdir("/var/state")
		if res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/srv/mediaX", 0); !res.Start {
			t.Fatalf("/srv/mediaX was read as overlapping /srv/media: %+v", res.Causes)
		}
	})
}

// AC-B11: a configured scratch_dir has its storage classification REPORTED at
// startup alongside the library roots and the state directory - and a classification
// that is not local does NOT refuse, needs no allow_non_local declaration, and is not
// dressed up as a reduced guarantee.
func TestScratch_StorageIsClassifiedAndReportedAndNonLocalDoesNotRefuse(t *testing.T) {
	t.Run("reported alongside the roots and the state directory", func(t *testing.T) {
		f := libraryFS()
		res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/mnt/scratch", 0)
		if !res.Start {
			t.Fatalf("the run was refused: %+v", res.Causes)
		}
		rec, ok := recordFor(res, "/mnt/scratch")
		if !ok {
			t.Fatalf("no classification record for the scratch directory: %+v", res.Records)
		}
		if rec.Kind != KindScratchDir {
			t.Fatalf("the scratch record has kind %q, want %q", rec.Kind, KindScratchDir)
		}
		if !rec.Class.IsLocal() || rec.Type != "ext4" {
			t.Fatalf("the scratch record is %+v, want a local ext4 classification", rec)
		}
		// Alongside, not instead of: the roots and the state directory are still
		// reported in the same set.
		for _, p := range []string{"/srv/media", "/var/state"} {
			if _, ok := recordFor(res, p); !ok {
				t.Fatalf("%s lost its classification record when a scratch directory was configured", p)
			}
		}
	})

	t.Run("not local: the run starts, is reported, and needs no declaration", func(t *testing.T) {
		f := libraryFS()
		f.mount("/mnt/scratch", "nfs")
		res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/mnt/scratch", 0)
		if !res.Start {
			t.Fatalf("a non-local scratch directory REFUSED the run: %+v", res.Causes)
		}
		if hasCause(res, CauseNotLocal, "/mnt/scratch") {
			t.Fatalf("the scratch directory produced a storage-is-not-local cause: %+v", res.Causes)
		}
		rec, ok := recordFor(res, "/mnt/scratch")
		if !ok || rec.Class.IsLocal() {
			t.Fatalf("the scratch record is %+v (found=%v), want a non-local classification", rec, ok)
		}
		if !hasNotice(res, NoticeScratchNotLocal, "/mnt/scratch") {
			t.Fatalf("a non-local scratch directory was not REPORTED: %+v", res.Notices)
		}
		// It is not a reduced guarantee, and must not be reported as one: nothing
		// irreversible happens there, and a reader taught to treat this like a NAS
		// under a library root would be taught something false.
		if hasNotice(res, NoticeReducedGuarantee, "/mnt/scratch") {
			t.Fatalf("a non-local scratch directory was reported as a REDUCED guarantee: %+v", res.Notices)
		}
		detail := ""
		for _, n := range noticesOf(res, NoticeScratchNotLocal) {
			detail = n.Detail
		}
		if !strings.Contains(detail, ConfigKey) || !strings.Contains(detail, "no ") {
			t.Fatalf("the report does not say that no %s declaration is required: %q", ConfigKey, detail)
		}
	})

	// The comparison arm that gives the criterion its teeth: the SAME non-local
	// filesystem under a library ROOT still refuses. So the exemption is about the
	// scratch directory specifically and not about non-local storage having quietly
	// stopped mattering.
	t.Run("the same non-local storage under a root still refuses", func(t *testing.T) {
		f := libraryFS()
		f.mkdir("/srv/media/tv")
		f.mount("/srv/media/tv", "nfs")
		res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "/mnt/scratch", 0)
		if res.Start {
			t.Fatalf("a non-local mount beneath a library root started the run")
		}
		if res.Row != rowNotLocal {
			t.Fatalf("row = %d, want %d: %+v", res.Row, rowNotLocal, res.Causes)
		}
	})
}

// With no scratch_dir configured NOTHING about this check changes: no record, no
// notice, no cause, and above all no probe - so a configuration that predates this
// item cannot be refused by, or written to by, machinery it never asked for.
func TestScratch_AnAbsentScratchDirChangesNothing(t *testing.T) {
	f := libraryFS()
	res := checkScratchDir(f, []string{"/srv/media"}, "/var/state", "", 0)
	if !res.Start {
		t.Fatalf("the run was refused with no scratch directory configured: %+v", res.Causes)
	}
	if f.probes != 0 {
		t.Fatalf("the writability probe ran %d time(s) with no scratch directory configured", f.probes)
	}
	if res.ScratchProbed {
		t.Fatal("the result claims a probe ran")
	}
	for _, rec := range res.Records {
		if rec.Kind == KindScratchDir {
			t.Fatalf("a scratch record appeared with no scratch directory configured: %+v", rec)
		}
	}
	if len(noticesOf(res, NoticeScratchNotLocal)) != 0 {
		t.Fatalf("a scratch notice appeared with no scratch directory configured: %+v", res.Notices)
	}
	if strings.Contains(refusalText(res), "probe file") {
		t.Fatal("the refusal text mentions a probe that never ran")
	}
}
