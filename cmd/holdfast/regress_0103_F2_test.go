package main

// Refuter artifact for S0103-holdfast-plan-report, impl gate ordinal 1, finding F2.
//
// AC-10: "WHEN `plan` and the daemon's own scanning pass run over the same library with the
// same configuration, THE SYSTEM SHALL report exactly the set of files that pass covers ...
// rather than a coverage rule of its own."
//
// ProcessFile declines a source path containing a literal tab or newline before it claims,
// probes or records anything: the daemon never touches such a file. The read-only pass does
// not ask that question, so a plan reports it as a file a run WOULD transcode.
//
// Graded exactly as the criterion's own named test grades it: a dry run's terminal rows are
// the set the daemon pass covered, and the plan pass's files are the set plan covers.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRegress0103F2_PlanCoversAPathTheDaemonPassDeclines(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")

	// A real, probeable source whose NAME carries a literal tab. It is enumerated (the
	// extension matches), and ProcessFile then declines it, unrecorded.
	tabbed := filepath.Join(lib, "two\tnames.mkv")
	encodeFixture(t, tabbed, "libx264", "256x144", "yuv420p")
	if _, err := os.Stat(tabbed); err != nil {
		t.Skipf("this filesystem will not hold a tab in a file name: %v", err)
	}

	pass := planPassOver(t, cfgPath)
	planned := map[string]bool{}
	for _, f := range pass.Files {
		planned[f.Path] = true
	}
	decided := daemonPassPaths(t, cfgPath)

	if planned[tabbed] && decided[tabbed] == "" {
		t.Fatalf("AC-10: plan covers %q, which the daemon's own pass declines without claiming, "+
			"probing or recording it - plan reports a file a run would never touch\n  plan:   %v\n  daemon: %v",
			tabbed, sortedSet(planned), decided)
	}
}

// TestRegress0103F2b_PlanCallsThatPathEligible is the consequence an operator reads: the
// report does not merely cover the file, it counts it among the files a run would transcode.
func TestRegress0103F2b_PlanCallsThatPathEligible(t *testing.T) {
	cfgPath, lib, _ := planLibrary(t, "")
	tabbed := filepath.Join(lib, "two\tnames.mkv")
	encodeFixture(t, tabbed, "libx264", "256x144", "yuv420p")
	if _, err := os.Stat(tabbed); err != nil {
		t.Skipf("this filesystem will not hold a tab in a file name: %v", err)
	}

	p := planJSON(t, cfgPath)
	// planLibrary's own eligible set is 2. A third is the tab-named file the daemon declines.
	if p.Total.Eligible.Files != 2 {
		t.Fatalf("AC-10: plan reports %d eligible file(s) over a library whose daemon pass would "+
			"transcode 2 - the extra is the tab-named path ProcessFile declines", p.Total.Eligible.Files)
	}
}
