package startup

import "testing"

// Regression case for S0007-holdfast-startup-1, refuter finding F2 (advisory).
//
// The defect the finding names is a CONTRADICTION, not a lost or extra file: the
// walk reported EVERY symbolic link whose resolved target leaves the configured
// library roots as one this run "does not descend into" - a link to a FILE
// included - while the coverage-bounded scan enumerated that same link as a
// source out of a directory the walk traversed successfully. Startup told the
// operator the path was out of bounds and a job row appeared for it in the same
// run.
//
// The verdict named two remedies for it and did not choose between them: stop
// noticing file links, or exclude a noticed link from coverage. This case
// therefore pins the PROPERTY both remedies exist to restore, rather than either
// remedy's mechanics - NO path startup reports as one this run does not descend
// into is enumerated by the scan - so it stays true under either. It supersedes
// the refuter's own probe, which asserted the second remedy specifically.
//
// It is red against b8b2ad5 for the file-link shape and green after.
func TestRegress0007F2_StartupsReportAndTheScanAgreeAboutEveryPath(t *testing.T) {
	// A library root holding both shapes of link that leave the roots: one to a
	// DIRECTORY on a NAS (which the walk really would have descended, and must
	// decline and report) and one to a FILE on the same NAS (which nothing
	// descends into, and which the scan sees as an ordinary entry of a directory
	// the walk traversed).
	f := newFS().setType("/", "ext4")
	f.mkdir("/mnt/nas")
	f.setType("/mnt/nas", "nfs")
	f.mkdir("/mnt/nas/shows")
	f.mkfile("/mnt/nas/shows/Show.mkv")
	f.mkfile("/mnt/nas/Film.mkv")
	f.mkdir("/srv/media")
	f.mkfile("/srv/media/Local.mkv")
	f.symlink("/srv/media/shows", "/mnt/nas/shows")
	f.symlink("/srv/media/Film.mkv", "/mnt/nas/Film.mkv")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	if !res.Start {
		t.Fatalf("refused: %+v", res.Causes)
	}

	// The DIRECTORY link is the one AC2f is about: not descended, and reported.
	if !hasNotice(res, NoticeLinkLeavesRoots, "/srv/media/shows") {
		t.Fatalf("a link onto a directory outside the roots was not reported as one this run does not descend into: %+v", res.Notices)
	}

	// Exactly what the engine's coverage-bounded enumerate() does.
	scanned := enumerated(f, res)

	// The property: startup's report and the scan never contradict each other.
	// Whatever startup declared out of bounds, the scan does not enumerate.
	declined := map[string]bool{}
	for _, n := range res.Notices {
		switch n.Kind {
		case NoticeLinkLeavesRoots, NoticeRegionWalked, NoticeUnreadable,
			NoticeListingFailed, NoticeUnresolvable:
			declined[n.Path] = true
		}
	}
	for _, p := range scanned {
		if declined[p] {
			t.Fatalf("startup reported %s as a path this run does not descend into and the scan enumerated it anyway (enumerated %v, notices %+v)",
				p, scanned, res.Notices)
		}
		for d := range declined {
			if lexicallyBeneath(p, d) {
				t.Fatalf("startup reported %s as a path this run does not descend into and the scan enumerated %s beneath it (enumerated %v)",
					d, p, scanned)
			}
		}
	}

	// Not vacuous: the scan really did enumerate something, and the NAS
	// directory's contents are not in it.
	if len(scanned) == 0 {
		t.Fatalf("nothing was enumerated at all; this case would pass against a build that enumerates nothing (coverage %v)", res.Coverage)
	}
	for _, p := range scanned {
		if p == "/srv/media/shows/Show.mkv" {
			t.Fatalf("a source under a declined directory link was enumerated: %v", scanned)
		}
	}

	// And the same agreement about AC7's report: a root whose only media entry
	// is such a link is not called empty while the scan enumerates from it.
	if hasNotice(res, NoticeEmptyRoot, "/srv/media") {
		t.Fatalf("the root was reported present and empty while the scan enumerated %v", scanned)
	}
}

// TestRegress0007F2_ARootHoldingOnlyAFileLinkIsNotCalledEmpty isolates the AC7
// half of the same disagreement: the file link is the only media entry, so a
// build that skips it when deciding "present and empty" reports an empty library
// and then queues a file out of it.
func TestRegress0007F2_ARootHoldingOnlyAFileLinkIsNotCalledEmpty(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/mnt/nas")
	f.setType("/mnt/nas", "nfs")
	f.mkfile("/mnt/nas/Film.mkv")
	f.mkdir("/srv/media")
	f.symlink("/srv/media/Film.mkv", "/mnt/nas/Film.mkv")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	if !res.Start {
		t.Fatalf("refused: %+v", res.Causes)
	}
	scanned := enumerated(f, res)
	if len(scanned) != 1 || scanned[0] != "/srv/media/Film.mkv" {
		t.Fatalf("enumerated = %v, want exactly [/srv/media/Film.mkv]", scanned)
	}
	if hasNotice(res, NoticeEmptyRoot, "/srv/media") {
		t.Fatalf("the root was reported present and empty while the scan enumerated %v", scanned)
	}
	if hasNotice(res, NoticeLinkLeavesRoots, "/srv/media/Film.mkv") {
		t.Fatalf("a link that IS a file was reported as one this run does not descend into, while the scan enumerated it: %+v", res.Notices)
	}
}
