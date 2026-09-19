package docscheck_test

// AC-13 (S0100): the README's quick start leads with a bounded `holdfast run --file`,
// ahead of any whole-library `holdfast run`, and this check reds if that order is ever
// reversed.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NSchatz/holdfast/internal/corpus"
	"github.com/NSchatz/holdfast/internal/docscheck"
)

// TestQuickStart_LeadsWithTheBoundedRun grades AC-13 against the README this repository
// actually ships, located from the repository root rather than from a working directory.
func TestQuickStart_LeadsWithTheBoundedRun(t *testing.T) {
	root, err := corpus.RepoRoot(".")
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("read the README: %v", err)
	}
	if err := docscheck.BoundedRunLeadsQuickStart(string(b)); err != nil {
		t.Error(err)
	}
}

// TestQuickStart_TheOrderCheckBites is AC-13's anti-vacuity arm, and it is the half that
// makes the case above evidence. A grader that answered "fine" for every document would
// agree with the README whatever it said, so the three ways the order can be wrong are
// each shown REDDING it: the whole-library run leading, the run commands gone, and the
// section itself gone.
func TestQuickStart_TheOrderCheckBites(t *testing.T) {
	cases := []struct {
		name string
		doc  string
	}{
		{"the whole-library run leads", "## Quick start\n\n```bash\nholdfast run --config config.yaml\n" +
			"holdfast run --config config.yaml --file /media/tv/pilot.mkv\n```\n\n## Next\n"},
		{"no run command at all", "## Quick start\n\n```bash\nholdfast validate --config config.yaml\n```\n\n## Next\n"},
		{"no quick start section", "# holdfast\n\n## Build\n\nmake build\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := docscheck.BoundedRunLeadsQuickStart(tc.doc); err == nil {
				t.Fatal("the check passed a document whose order is wrong; it cannot grade the README either")
			}
		})
	}
}
