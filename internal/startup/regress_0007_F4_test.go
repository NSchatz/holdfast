package startup

import (
	"errors"
	"io/fs"
	"testing"
)

// Regression case for S0007-holdfast-startup-1, refuter finding F4 (advisory,
// evidence: reasoning - the refuter could not construct the state through the
// production platform, and this case constructs it through the substituted one).
//
// The coupling: the engine reads a NIL Coverage as "no startup check ran, walk
// the roots recursively", which is what an Engine built without the check gets.
// Result.Coverage was appended to only when a directory was traversed
// SUCCESSFULLY, so a walk that traversed nothing handed back nil - and the bound
// meant to say "enumerate nothing" said "enumerate everything" instead. The
// failure mode is silent and it is in the one field that bounds what the run may
// destroy, so the guarantee wanted is structural: Run NEVER returns a nil
// Coverage, on any path through it.
func TestRegress0007F4_CoverageIsNeverNilSoAnEmptyBoundIsNotAnAbsentOne(t *testing.T) {
	t.Run("a walk that traversed nothing at all", func(t *testing.T) {
		// The root exists and can be inspected, and its listing fails, so the
		// walk covers nothing. (This refuses at row 3, which is what makes the
		// state hard to reach in production - but a caller reading Coverage must
		// not have to know which row fired to know what the field means.)
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.failRead("/srv/media", errors.New("input/output error"))
		f.mkdir("/var/state")

		res := check(f, []string{"/srv/media"}, "/var/state")

		if len(res.Coverage) != 0 {
			t.Fatalf("coverage = %v, want empty", res.Coverage)
		}
		if res.Coverage == nil {
			t.Fatal("a walk that traversed nothing returned a NIL coverage set; the engine reads nil as `no startup check ran, enumerate the roots recursively`, so an empty bound would silently become an absent one")
		}
	})

	t.Run("a run refused before the walk ever starts", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv")
		f.mkdir("/var/state")

		res := check(f, []string{"/srv/media"}, "/var/state")

		if res.Row != 2 {
			t.Fatalf("row = %d, want 2", res.Row)
		}
		if res.Coverage == nil {
			t.Fatal("a refused run returned a NIL coverage set")
		}
	})

	t.Run("a root the process may not inspect", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/srv/media")
		f.failStat("/srv/media", fs.ErrPermission)
		f.mkdir("/var/state")

		res := check(f, []string{"/srv/media"}, "/var/state")

		if res.Coverage == nil {
			t.Fatal("a run refused at the permission row returned a NIL coverage set")
		}
	})

	t.Run("no configured roots at all", func(t *testing.T) {
		f := newFS().setType("/", "ext4")
		f.mkdir("/var/state")

		res := check(f, nil, "/var/state")

		if res.Coverage == nil {
			t.Fatal("a check with no roots returned a NIL coverage set")
		}
	})
}
