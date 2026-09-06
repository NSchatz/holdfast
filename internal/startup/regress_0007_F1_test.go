package startup

import "testing"

// Regression probe for S0007-holdfast-startup-1, refuter finding F1.
//
// AC3, closing sentence: "a configured library root whose lookup fails BECAUSE
// THE PATH DOES NOT EXIST is outside it too - AC6 governs it, its record names
// it missing, and it is NEVER CLASSIFIED `undetermined`."
//
// AC6: the system "SHALL name that path as missing, distinctly from permission
// denial and FROM AN UNDETERMINED TYPE".
//
// The record emitted for a missing configured library root carries
// Class == Undetermined, and Result.Log prints that field verbatim as
// `classification=undetermined`, so the operator-facing record for a path that
// simply does not exist reads exactly like the record for a path whose type
// could not be determined - the conflation AC3 and AC6 both forbid.
func TestRegress0007F1_AMissingRootIsNeverClassifiedUndetermined(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state")

	rec, ok := recordFor(res, "/srv/media")
	if !ok {
		t.Fatalf("no record for the missing root: %+v", res.Records)
	}
	if !rec.Missing {
		t.Fatalf("record = %+v, want Missing set", rec)
	}
	if rec.Class == Undetermined {
		t.Fatalf("the record for a missing configured library root is classified %q; AC3 says it is NEVER classified `undetermined` and AC6 says missing is reported distinctly from an undetermined type (record %+v)",
			rec.Class, rec)
	}
}

// The classification field is load-bearing, not cosmetic: applyDeclarations
// treats every record that is not local as a path a declaration can usefully
// cover, so a stale declaration naming a root that no longer exists is consumed
// as USEFUL and never reported. AC25 keeps a declaration that covers nothing
// visible rather than passing over it in silence; with the record correctly not
// classified `undetermined` this line would be reported instead of swallowed.
func TestRegress0007F1_ADeclarationNamingAMissingRootIsSwallowed(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv")
	f.mkdir("/var/state")

	res := check(f, []string{"/srv/media"}, "/var/state", "/srv/media")

	rec, _ := recordFor(res, "/srv/media")
	if rec.Covered {
		t.Fatalf("the missing root was marked as COVERED by an opt-in: %+v", rec)
	}
	reported := false
	for _, n := range res.Notices {
		if n.Path == "/srv/media" {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the declaration naming a root that does not exist was passed over in silence: notices %+v", res.Notices)
	}
}
