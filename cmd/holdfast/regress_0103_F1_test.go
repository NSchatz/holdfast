package main

// Refuter artifact for S0103-holdfast-plan-report, impl gate ordinal 1, finding F1.
//
// AC-11: "WHEN the build contains the `analyze` command and both it and `plan` run over the
// same library with the same configuration, THE SYSTEM SHALL report an identical covered
// file set and an identical per file skip reason from both."
//
// The build DOES contain `analyze`, so the trigger fires. This is the same shape of library
// the criterion's own named test uses, with ONE symbolic link carrying a source name added
// to it - the case the implementation's recorded reading says it does not satisfy.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegress0103F1_PlanAndAnalyzeDisagreeOnASymlinkedSource(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")

	// A symbolic link carrying a configured video extension, pointing at a real source in
	// the same root. A scan ENUMERATES it and then skips it at the symlinked-source guard.
	if err := os.Symlink(filepath.Join(lib, "movie.mkv"), filepath.Join(lib, "linked.mkv")); err != nil {
		t.Fatal(err)
	}

	c := analyzeJSON(t, cfgPath)
	p := planJSON(t, cfgPath)

	if p.Total.Covered.Files != c.Total.Sources.Files {
		t.Fatalf("AC-11: plan covers %d file(s), analyze counts %d source(s) over the same library "+
			"with the same configuration - the two covered sets are not identical",
			p.Total.Covered.Files, c.Total.Sources.Files)
	}
	if p.Total.Covered.Bytes != c.Total.Sources.Bytes {
		t.Fatalf("AC-11: plan covers %d byte(s), analyze counts %d",
			p.Total.Covered.Bytes, c.Total.Sources.Bytes)
	}
}
