package startup

import (
	"errors"
	"io/fs"
	"testing"
)

// Regression probe for S0007-holdfast-startup-1, refuter finding F3.
//
// AC10a, last sentence: "IF a CHECKED PATH's own resolved form cannot be
// established for those reasons THEN no declaration can be compared against it:
// THE SYSTEM SHALL REFUSE THE RUN, name that path and the reason, and name no
// declaration as a remedy (AC8a) - at row 3 where the cause is that the process
// may not inspect the path or its parent (AC4), OTHERWISE CLASSIFYING IT
// `undetermined` (AC3) SO THAT ROW 4 REFUSES."
//
// The implementation classifies from the filesystem-type lookup alone and then
// records the resolved form separately, so when resolution fails for a
// non-permission reason while the type lookup succeeds with a recognised-local
// type, the record is `local`, no cause is raised, and the run STARTS with a
// checked path whose resolved form is unestablished - the one state AC10a says
// must refuse at row 4.
//
// Injected through the substitutable seam, as the Verification notes require for
// failures that have no fixture.
func TestRegress0007F3_ACheckedPathWithNoResolvedFormMustRefuse(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mount("/srv/media/vault", "ext4")
	f.mkdir("/var/state")

	res := Run(Check{
		Roots:       []string{"/srv/media"},
		StateDir:    "/var/state",
		IsMediaFile: mediaByExt,
		Platform: &resolveFailFS{fakeFS: f, at: "/srv/media/vault",
			err: errors.New("input/output error")},
	})

	rec, ok := recordFor(res, "/srv/media/vault")
	if !ok {
		t.Fatalf("no record for the mount: %+v", res.Records)
	}
	if rec.Resolved != "" {
		t.Fatalf("the resolved form was established after all (%q); this probe proves nothing", rec.Resolved)
	}
	if res.Start {
		t.Fatalf("a checked path whose resolved form could not be established started the run: record %+v, row %d, causes %+v",
			rec, res.Row, res.Causes)
	}
	if res.Row != 4 {
		t.Fatalf("row = %d, want 4 (AC10a: classify it undetermined so that row 4 refuses): %+v", res.Row, res.Causes)
	}
	if rec.Class != Undetermined {
		t.Fatalf("record = %+v, want undetermined: only a POSITIVE identification is local, and a type lookup that answers for a path holdfast cannot name is not one", rec)
	}
	// No declaration is named as a remedy: none could lift this refusal.
	for _, c := range res.Causes {
		if c.Path == "/srv/media/vault" && c.Declaration != "" {
			t.Fatalf("a declaration was offered for a path whose resolved form could not be established: %+v", c)
		}
	}
}

// The clause's OTHER limb, the same seam: where the reason resolution failed is
// that the process may not inspect the path or its parent, the refusal is the
// permission one at row 3 rather than row 4 - and it is still a refusal, still
// with no declaration named.
func TestRegress0007F3_AResolvedFormDeniedRefusesAtRow3(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mount("/srv/media/vault", "ext4")
	f.mkdir("/var/state")

	res := Run(Check{
		Roots:       []string{"/srv/media"},
		StateDir:    "/var/state",
		IsMediaFile: mediaByExt,
		Declarations: []string{
			// Even naming it exactly: an opt-in permits a run on storage that is
			// not local, never on a path holdfast cannot resolve.
			"/srv/media/vault",
		},
		Platform: &resolveFailFS{fakeFS: f, at: "/srv/media/vault", err: fs.ErrPermission},
	})

	rec, ok := recordFor(res, "/srv/media/vault")
	if !ok {
		t.Fatalf("no record for the mount: %+v", res.Records)
	}
	if rec.Resolved != "" || !rec.Denied || rec.Class == Local {
		t.Fatalf("record = %+v, want an unresolved, denied record that is not local", rec)
	}
	if res.Start {
		t.Fatalf("started with a checked path whose resolved form was denied: %+v", res.Causes)
	}
	if res.Row != 3 {
		t.Fatalf("row = %d, want 3 (AC10a: at row 3 where the cause is that the process may not inspect the path or its parent): %+v", res.Row, res.Causes)
	}
	if !hasCause(res, CauseDenied, "/srv/media/vault") {
		t.Fatalf("permission denial was not named as the cause: %+v", res.Causes)
	}
	for _, c := range res.Causes {
		if c.Path == "/srv/media/vault" && c.Declaration != "" {
			t.Fatalf("a declaration was offered for a path holdfast cannot inspect: %+v", c)
		}
	}
}

// resolveFailFS is the fakeFS with resolution alone failing at one path: the
// type lookup and the stat still succeed there, which is the split AC10a's
// "otherwise" limb legislates for.
type resolveFailFS struct {
	*fakeFS
	at  string
	err error
}

func (r *resolveFailFS) Resolve(p string) (string, error) {
	if cleanPath(p) == cleanPath(r.at) {
		return "", r.err
	}
	return r.fakeFS.Resolve(p)
}
