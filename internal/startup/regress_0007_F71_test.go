package startup

import (
	"errors"
	"io/fs"
	"testing"
)

// Regression probe for S0007-holdfast-startup-1, refuter finding F71 (impl gate,
// ordinal 2; ADVISORY - see the verdict, which records why the spec's own text
// gives this corner two answers).
//
// A configured library root NESTED INSIDE another configured library root is
// reached by the outer root's walk first, as an ordinary subdirectory. descend()
// is then called with isRoot=false, so a listing that fails there raises no
// cause; the region is marked ON ENTRY all the same (AC2b2), so when walkRoot()
// reaches the same path as a ROOT it declines it under AC2b3 and refuses
// nothing. The run STARTS on a configured library root whose whole subtree is
// unknown to it.
//
//	AC4b - IF a configured library root exists and can be inspected but CANNOT
//	BE LISTED OR ENTERED, for ANY reason - the process is not permitted to, or
//	the listing fails with an I/O error, a stale handle or any other failure -
//	THEN THE SYSTEM SHALL refuse to run through AC10b row 3, name that root,
//	report the CAUSE IT OBSERVED, and name no declaration as a remedy (AC8a).
//
// The antecedent holds exactly: the root exists, it is inspectable (its type is
// answered), and it cannot be listed. AC4b's own rationale holds too - "A root
// holdfast cannot list is one whose whole subtree is unknown to it ... starting
// would look to the operator exactly like an empty library" - while AC2b3's
// rationale for not refusing does NOT ("Coverage is unharmed ... every source
// under such a root was reachable through the root that WAS walked": here no
// source under it was reachable at all).
//
// The divergence is order-sensitive: the same storage layout with the two roots
// declared in the other order refuses at row 3, which the second case pins.
func TestRegress0007F71_ANestedRootThatCannotBeListedStartsTheRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "permission denied", err: fs.ErrPermission},
		{name: "an I/O error", err: errors.New("input/output error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFS().setType("/", "ext4")
			f.mkdir("/srv/media")
			f.mkdir("/srv/media/tv")
			f.failRead("/srv/media/tv", tc.err)
			f.mkdir("/var/state")

			// The inner root declared SECOND, so the outer root's walk reaches it
			// first. Both are configured library roots on one local filesystem.
			res := check(f, []string{"/srv/media", "/srv/media/tv"}, "/var/state")

			if res.Start {
				t.Fatalf("the run STARTED with the configured library root /srv/media/tv unlistable (%v): "+
					"AC4b says a configured root that cannot be listed FOR ANY REASON refuses at AC10b row 3. "+
					"row=%d causes=%+v notices=%+v", tc.err, res.Row, res.Causes, res.Notices)
			}
			if res.Row != 3 {
				t.Fatalf("row = %d, want 3 (AC4b through AC10b row 3): %+v", res.Row, res.Causes)
			}
			if !hasCause(res, CauseUnlistable, "/srv/media/tv") {
				t.Fatalf("the unlistable root was not named as the cause it observed: %+v", res.Causes)
			}
		})
	}
}

// The other half of the same corner, and the half no reading of the spec makes
// right: the report the declined root carries states a reason that is false.
//
//	AC2b3 - such a root SHALL be reported at startup as one this run did not
//	descend, WITH THAT REASON (AC2f1) ... Coverage is unharmed and nothing is
//	enumerated twice: every source under such a root was reachable through the
//	root that WAS walked.
//
//	AC2f1 - THEN THE SYSTEM SHALL report that path at startup WITH THE REASON.
//
// walkRoot's notice says this root "exposes storage this run already walked
// under <path>; every source under it is enumerated exactly once, from there" -
// and names the root ITSELF as the path the sources come from, because the
// entered-region map recorded that same spelling on the entry whose listing then
// failed. Nothing was walked there and nothing is enumerated from there. The
// walk's true reason is carried by a second, separate notice; an operator
// reading the first is told their library is covered when it is not.
func TestRegress0007F71_TheDeclineNoticeNamesAPathTheWalkNeverTraversed(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mkdir("/srv/media/tv")
	f.mkfile("/srv/media/tv/Movie.mkv")
	f.failRead("/srv/media/tv", fs.ErrPermission)
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media", "/srv/media/tv"}, "/var/state")

	for _, n := range noticesOf(res, NoticeRegionWalked) {
		if n.Path != "/srv/media/tv" {
			continue
		}
		// The walk traversed nothing at this path: it is in no coverage set.
		for _, dir := range res.Coverage {
			if dir == "/srv/media/tv" {
				t.Fatalf("unexpected: %s is in the coverage set, so this case proves nothing", dir)
			}
		}
		t.Fatalf("the root %s was reported as one whose sources 'are enumerated exactly once, from there' - "+
			"detail %q - while the walk traversed it nowhere (coverage %v) and enumerated %v. "+
			"AC2f1 requires the path be reported WITH THE REASON, and the reason given is not the one the walk observed.",
			n.Path, n.Detail, res.Coverage, enumerated(f, res))
	}
}
