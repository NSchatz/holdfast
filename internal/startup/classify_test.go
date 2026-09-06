package startup

import (
	"errors"
	"io/fs"
	"sort"
	"strings"
	"testing"
)

// check is the whole startup check over a substituted platform.
func check(f *fakeFS, roots []string, stateDir string, decls ...string) Result {
	return Run(Check{
		Roots:        roots,
		StateDir:     stateDir,
		Declarations: decls,
		IsMediaFile:  mediaByExt,
		Platform:     f,
	})
}

// TestClassify_EveryAnswerAndEveryFailureOfTheTypeLookup drives the substituted
// filesystem-type lookup over every answer and every failure it has: a
// recognised-local type, a network type, a type this build does not recognise,
// no type at all, a lookup that fails, and mount information that is absent,
// unreadable or unparseable. Only a POSITIVE local identification is local;
// everything else is undetermined, which is not local (AC3, AC3b).
func TestClassify_EveryAnswerAndEveryFailureOfTheTypeLookup(t *testing.T) {
	tests := []struct {
		name      string
		typeName  string
		err       error
		want      Class
		wantType  string
		reasonHas string
		denied    bool
	}{
		{name: "recognised local type", typeName: "ext4", want: Local, wantType: "ext4"},
		{name: "another recognised local type", typeName: "btrfs", want: Local, wantType: "btrfs"},
		{name: "network type", typeName: "nfs", want: NonLocal, wantType: "nfs"},
		{name: "another network type", typeName: "cifs", want: NonLocal, wantType: "cifs"},
		{name: "unrecognised type", typeName: "sdcardfs", want: Undetermined,
			reasonHas: "not one this build recognises as local"},
		{name: "no type at all", typeName: "", want: Undetermined,
			reasonHas: "reported no filesystem type"},
		{name: "lookup failed", err: errors.New("i/o error"), want: Undetermined,
			reasonHas: "lookup failed"},
		{name: "mount information unavailable", err: ErrMountInfoUnavailable, want: Undetermined,
			reasonHas: "mount information is absent"},
		{name: "lookup denied", err: fs.ErrPermission, want: Undetermined, denied: true,
			reasonHas: "not permitted to inspect"},
		// AC3b: a union or overlay filesystem, an in-memory one, and anything in
		// user space are never local - the type does not say what storage is
		// underneath it.
		{name: "overlay", typeName: "overlay", want: Undetermined, reasonHas: "union or overlay"},
		{name: "tmpfs", typeName: "tmpfs", want: Undetermined, reasonHas: "in-memory"},
		{name: "fuse", typeName: "fuse", want: Undetermined, reasonHas: "user-space"},
		{name: "a fuse. type", typeName: "fuse.sshfs", want: Undetermined, reasonHas: "user-space"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.typeName, tc.err)
			if got.Class != tc.want {
				t.Fatalf("classify(%q, %v) = %s, want %s", tc.typeName, tc.err, got.Class, tc.want)
			}
			if got.Type != tc.wantType {
				t.Fatalf("classify(%q) named type %q, want %q", tc.typeName, got.Type, tc.wantType)
			}
			if got.Denied != tc.denied {
				t.Fatalf("classify(%q, %v) denied = %v, want %v", tc.typeName, tc.err, got.Denied, tc.denied)
			}
			if tc.reasonHas != "" && !strings.Contains(got.Reason, tc.reasonHas) {
				t.Fatalf("classify(%q, %v) reason = %q, want it to contain %q", tc.typeName, tc.err, got.Reason, tc.reasonHas)
			}
			if got.Class == Undetermined && got.Reason == "" {
				t.Fatal("an undetermined classification with no reason: the report would name nothing")
			}
			if got.Class == Undetermined && got.Type != "" {
				t.Fatalf("an undetermined classification named filesystem %q it did not detect", got.Type)
			}
		})
	}
}

// TestClassify_AnUndeterminedTypeRefusesJustAsANetworkOneDoes proves the
// fail-safe end to end rather than as a unit: an unrecognised type is refused
// exactly as a detected NAS is (AC3).
func TestClassify_AnUndeterminedTypeRefusesJustAsANetworkOneDoes(t *testing.T) {
	for _, typeName := range []string{"nfs", "sdcardfs", "overlay", "tmpfs", "fuse.sshfs"} {
		t.Run(typeName, func(t *testing.T) {
			f := newFS().setType("/", "ext4")
			f.mkdir("/srv/media").setType("/srv/media", typeName)
			f.mkdir("/var/state")
			res := check(f, []string{"/srv/media"}, "/var/state")
			if res.Start {
				t.Fatalf("started on a %s library root", typeName)
			}
			if res.Row != 4 {
				t.Fatalf("row = %d, want 4 (storage is not local)", res.Row)
			}
			if !hasCause(res, CauseNotLocal, "/srv/media") {
				t.Fatalf("no not-local cause for the root: %+v", res.Causes)
			}
		})
	}
}

// TestLocalSet_IsReportedAtStartup: the complete set of types this build
// classifies local is in the result of every check, so startup can print it and
// two builds recognising different sets are distinguishable from what each
// prints (AC3a).
func TestLocalSet_IsReportedAtStartup(t *testing.T) {
	f := newFS().setType("/", "ext4")
	f.mkdir("/srv/media")
	f.mkdir("/var/state")
	res := check(f, []string{"/srv/media"}, "/var/state")

	if len(res.LocalSet) == 0 {
		t.Fatal("the reported local set is empty")
	}
	if !sort.StringsAreSorted(res.LocalSet) {
		t.Fatalf("the reported local set is not in a stable order: %v", res.LocalSet)
	}
	for _, want := range []string{"ext4", "xfs", "btrfs"} {
		if !slicesContains(res.LocalSet, want) {
			t.Fatalf("the reported local set %v omits %q", res.LocalSet, want)
		}
	}
	for _, never := range []string{"nfs", "tmpfs", "overlay", "fuse"} {
		if slicesContains(res.LocalSet, never) {
			t.Fatalf("the reported local set %v names %q, which cannot be local", res.LocalSet, never)
		}
	}
	// A local record names the type it matched.
	rec, ok := recordFor(res, "/srv/media")
	if !ok || rec.Class != Local || rec.Type != "ext4" {
		t.Fatalf("local record for the root = %+v, want class local naming ext4", rec)
	}
}

func slicesContains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
