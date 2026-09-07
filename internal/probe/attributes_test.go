package probe

// The rename-invariant attribute record.
//
// Its whole job is to survive the one operation the swap performs. A re-stat after a
// failed rename has to be able to tell "the replacement is now at the source path" from
// "the source is still there", and it can only do that if the record taken of a file at
// ONE path still matches that file observed at ANOTHER. So anything a rename changes -
// the name, the path, the inode and link identity, ctime - is excluded by construction,
// and this test is what stops a well-meaning addition from putting one back.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAttributes_SurviveTheRenameItself is AC-level: a set that can never match at the
// destination is a defect, because it makes the applied case unreachable and turns every
// applied swap into an indeterminate one.
func TestAttributes_SurviveTheRenameItself(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, "movie.__transcoding__.mkv")
	if err := os.WriteFile(tmp, []byte("the replacement, recorded at its temp path"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := StatAttributes(tmp)
	if err != nil {
		t.Fatalf("StatAttributes: %v", err)
	}

	final := filepath.Join(dir, "movie.mkv")
	if err := os.Rename(tmp, final); err != nil {
		t.Fatal(err)
	}
	after, err := StatAttributes(final)
	if err != nil {
		t.Fatalf("StatAttributes after the rename: %v", err)
	}
	if after != before {
		t.Fatalf("the record moved with the rename: %v -> %v - the applied case would be unreachable", before, after)
	}
	if after.String() != before.String() {
		t.Errorf("the recorded spelling moved: %q -> %q", before.String(), after.String())
	}
}

// TestAttributes_MoveWhenTheCONTENTMoves is the other half: a record that never changed
// would be no guard at all. A rewrite that changes the byte count, or one that lands in a
// later mtime second, is visible.
func TestAttributes_MoveWhenTheCONTENTMoves(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(p, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := StatAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("rewritten, and a different length"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := StatAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("a rewrite of a different size was invisible to the record")
	}

	// A same-size rewrite inside the same mtime second IS invisible, and that is the
	// LOCAL residual window this build documents rather than claims to have closed.
	same := filepath.Join(dir, "same.mkv")
	if err := os.WriteFile(same, []byte("aaaaaaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := StatAttributes(same)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(same, []byte("bbbbbbbb"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Force the same whole second, which is what the guard compares.
	when := time.Unix(rec.MTimeUnix, 0)
	if err := os.Chtimes(same, when, when); err != nil {
		t.Fatal(err)
	}
	hidden, err := StatAttributes(same)
	if err != nil {
		t.Fatal(err)
	}
	if hidden != rec {
		t.Fatalf("the fixture did not reproduce the documented window: %v vs %v", rec, hidden)
	}
}

// TestAttributes_AreTheSameTextAsAJobKey - the record and the job key are one spelling,
// so a reader never has to know two formats.
func TestAttributes_AreTheSameTextAsAJobKey(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rec, err := StatAttributes(p)
	if err != nil {
		t.Fatal(err)
	}
	if rec.String() != Fingerprint(p) {
		t.Errorf("StatAttributes(%s).String() = %q, Fingerprint = %q", p, rec.String(), Fingerprint(p))
	}
}

// TestStatAttributes_ReportsTheErrorRatherThanASentinel. Fingerprint answers "0:0" for a
// file it cannot stat, which is fine for a dedup key and fatal for an observation: after
// a failed swap, "the file is not there" and "the file is there and is zero bytes at the
// epoch" decide different outcomes, and a sentinel would be a fabricated observation.
func TestStatAttributes_ReportsTheErrorRatherThanASentinel(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.mkv")
	if _, err := StatAttributes(missing); err == nil {
		t.Fatal("StatAttributes reported no error for a file that is not there")
	}
	if got := Fingerprint(missing); got != "0:0" {
		t.Errorf("Fingerprint sentinel changed to %q - the job key's behaviour is unchanged", got)
	}
}

// TestGuardVocabulary_IsWhatTheRecordSays pins the two constants a job's granularity
// record is written from: the attributes compared, and the resolution of the timestamp
// compared - which is a MEASURED duration and must parse as one.
func TestGuardVocabulary_IsWhatTheRecordSays(t *testing.T) {
	if AttributeNames != "size,mtime" {
		t.Errorf("AttributeNames = %q, want the two fields Attributes actually carries", AttributeNames)
	}
	d, err := time.ParseDuration(MTimeResolution)
	if err != nil {
		t.Fatalf("MTimeResolution %q is not a duration: %v", MTimeResolution, err)
	}
	if d != time.Second {
		t.Errorf("MTimeResolution = %v, want 1s (the whole-second mtime this build compares)", d)
	}
}
