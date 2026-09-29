package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/probe"
)

// S0177 lengthens both names holdfast writes into a library folder by a suffix. A source
// whose name, or whose directory, left the earlier name just inside the kernel's limits has
// no room for it - and those long names are exactly the ones S0155 made swap. Each
// constructor therefore writes the earlier name where the suffixed one cannot fit, and only
// there; every reader accepts both generations.

// TestWorkingNames_TheWorkingSuffixGivesWayToTheNameLimit: at NAME_MAX, and at PATH_MAX,
// the working name carries TempSuffix while it fits and is the earlier construction, exactly,
// one byte past - which is still a construction name every reader recognises.
func TestWorkingNames_TheWorkingSuffixGivesWayToTheNameLimit(t *testing.T) {
	dir := "/library/films"
	room := maxBaseName - len("."+TempMarker+".mkv"+TempSuffix)

	fits := strings.Repeat("a", room)
	p := tempPath(dir, fits, "mkv", 0)
	if !strings.HasSuffix(p, TempSuffix) || len(filepath.Base(p)) != maxBaseName {
		t.Errorf("a stem that leaves the suffixed name at exactly NAME_MAX got %q (%d bytes), want the suffixed name",
			filepath.Base(p), len(filepath.Base(p)))
	}

	over := fits + "a"
	p = tempPath(dir, over, "mkv", 0)
	if want := filepath.Join(dir, over+"."+TempMarker+".mkv"); p != want {
		t.Errorf("one byte past NAME_MAX, the working path is %q, want the earlier construction %q", p, want)
	}
	if stem, ext, ok := splitTempConstruction(filepath.Base(p)); !ok || stem != over || ext != "mkv" {
		t.Errorf("the earlier name %q is not read back as a construction name (stem %q, ext %q, ok %v)",
			filepath.Base(p), stem, ext, ok)
	}

	// PATH_MAX: a directory that leaves the earlier path at exactly the limit.
	name := "film"
	earlierTail := "/" + name + "." + TempMarker + ".mkv"
	deep := "/" + strings.Repeat("d", maxPathLen-len(earlierTail)-1)
	p = tempPath(deep, name, "mkv", 0)
	if len(p) != maxPathLen || strings.HasSuffix(p, TempSuffix) {
		t.Errorf("a directory that leaves room for the earlier path only got %d bytes ending %q, want the "+
			"earlier path at exactly %d bytes", len(p), filepath.Ext(p), maxPathLen)
	}
	shallow := deep[:len(deep)-len(TempSuffix)]
	if p = tempPath(shallow, name, "mkv", 0); !strings.HasSuffix(p, TempSuffix) || len(p) != maxPathLen {
		t.Errorf("a directory with room for the suffix got %d bytes ending %q, want the suffixed path", len(p), filepath.Ext(p))
	}
}

// TestWorkingNames_TheRetainedSuffixGivesWayToTheNameLimit: a source whose earlier retained
// name fits NAME_MAX and whose suffixed one does not is still retained - under the earlier
// name, as the same inode - where the suffixed name alone would have failed the link and
// skipped the file.
func TestWorkingNames_TheRetainedSuffixGivesWayToTheNameLimit(t *testing.T) {
	d := t.TempDir()
	// The fingerprint in a retained name is the file's own (size and mtime), so the fixture
	// is written first and its name chosen afterwards; a rename keeps both.
	tmp := filepath.Join(d, "original.mkv")
	if err := os.WriteFile(tmp, []byte("the operator's original"), 0o644); err != nil {
		t.Fatal(err)
	}
	fp := probe.Fingerprint(tmp)

	stemFor := func(n int) string { return strings.Repeat("r", n) }
	n := 1
	for len(filepath.Base(legacyRetainedPathFor(filepath.Join(d, stemFor(n+1)+".mkv"), fp))) <= maxBaseName {
		n++
	}
	src := filepath.Join(d, stemFor(n)+".mkv")
	earlier := legacyRetainedPathFor(src, fp)
	if len(filepath.Base(earlier)) > maxBaseName || len(filepath.Base(earlier+UndoSuffix)) <= maxBaseName {
		t.Fatalf("the fixture's stem (%d bytes) does not sit between the two names' limits", n)
	}
	if got := retainedPathFor(src, fp); got != earlier {
		t.Fatalf("retainedPathFor = %q, want the earlier name %q where the suffix cannot fit", got, earlier)
	}
	// And with room for the suffix, it is used.
	if short := filepath.Join(d, stemFor(n-len(UndoSuffix))+".mkv"); !strings.HasSuffix(retainedPathFor(short, fp), UndoSuffix) {
		t.Errorf("a stem with room for the suffix was not given it: %q", retainedPathFor(short, fp))
	}

	if err := os.Rename(tmp, src); err != nil {
		t.Fatal(err)
	}
	if got := probe.Fingerprint(src); got != fp {
		t.Fatalf("the rename moved the fingerprint (%s -> %s), so the names above are not this file's", fp, got)
	}
	u := NewUndoWindow(undoCfg(d, 24), nil, discardLogger())
	retained, err := u.retain(context.Background(), src, fp)
	if err != nil {
		t.Fatalf("a source whose earlier retained name fits was not retained: %v", err)
	}
	if retained != earlier || !sameFile(src, retained) {
		t.Errorf("retained at %q (same inode: %v), want the earlier name %q, linked to the source",
			retained, sameFile(src, retained), earlier)
	}
	// A second retention of the same original reuses that link rather than failing on it.
	again, err := u.retain(context.Background(), src, fp)
	if err != nil || again != retained || !sameFile(src, again) {
		t.Errorf("retaining again gave %q, %v; want the same link %q reused", again, err, retained)
	}
}
