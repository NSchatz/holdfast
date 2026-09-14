package docscheck

// S0102 - the two rules that resolve a PATH rather than read a token.
//
// Consolidating documentation moves prose between files, and the two ways a move loses an
// argument are both invisible to every rule above: a link left pointing at the old place,
// and a new document nothing points at. Both are checked against the tree, so both are
// graded here with a corpus on disk rather than with a string.
//
// Every test here names the acceptance criterion it grades.

import (
	"path/filepath"
	"strings"
	"testing"
)

// satisfied is a corpus that passes every anchor rule, so a test about a link reports the
// link and nothing else.
func satisfied() string {
	return residualWindowBlock() + postureBlock() + metadataBlock() + agreeingBlocks()
}

// checkRepo runs the WHOLE gate over a fixture root, which is what `make check` runs.
func checkRepo(t *testing.T, dir string) []string {
	t.Helper()
	files, err := Corpus(dir)
	if err != nil {
		t.Fatalf("Corpus: %v", err)
	}
	problems, err := CheckRepo(dir, files)
	if err != nil {
		t.Fatalf("CheckRepo: %v", err)
	}
	return problems
}

// TestCheckRepo_AC9_ADeadRepositoryRelativeLinkFails is AC-9. The link names a path that
// is not in the tree, and the gate has to name both the file it is in and the target,
// because a reader of the failure is being asked to fix one line in one document.
//
// It goes through CheckRepo rather than CheckLinks: a rule that is right and not wired
// into the gate is a rule `make check` never runs.
func TestCheckRepo_AC9_ADeadRepositoryRelativeLinkFails(t *testing.T) {
	for _, doc := range LinkedDocs {
		t.Run(doc, func(t *testing.T) {
			files := map[string]string{
				"docs/real.md": satisfied(),
				"CLAUDE.md":    "# Brief\n\nSee [the design](docs/real.md).\n",
				"README.md":    "# holdfast\n\nWhat it is.\n",
			}
			files[doc] += "\nAnd [the missing one](docs/gone.md).\n"
			problems := checkRepo(t, writeCorpus(t, files))
			if len(problems) != 1 {
				t.Fatalf("want exactly the dead link, got %d: %v", len(problems), problems)
			}
			if !strings.Contains(problems[0], doc) || !strings.Contains(problems[0], "docs/gone.md") {
				t.Errorf("the problem does not name the file and the dead target: %q", problems[0])
			}
		})
	}
}

// TestCheckLinks_AC9_WhatIsResolvedAndWhatIsNot is the rest of AC-9, one case per rule
// about WHICH links are resolved at all.
//
// The fragment case is the one that would otherwise break the repository outright: an
// anchored cross-reference is how every statement in this corpus is cited, and resolving
// docs/design/swap.md#swap-invariant whole would report every one of them as dead. The
// absolute cases are the deliberate hole: `make check` runs on every pull request and must
// not red because a third party had a bad afternoon.
func TestCheckLinks_AC9_WhatIsResolvedAndWhatIsNot(t *testing.T) {
	cases := []struct {
		name string
		link string
		dead bool
	}{
		{"a fragment on a real file is stripped before resolving", "[x](docs/real.md#some-anchor)", false},
		{"a fragment on a missing file is still resolved", "[x](docs/gone.md#some-anchor)", true},
		{"a bare fragment names no file", "[x](#some-anchor)", false},
		{"an http link is out of the check", "[x](http://example.invalid/docs/gone.md)", false},
		{"an https link is out of the check", "[x](https://example.invalid/docs/gone.md)", false},
		{"a mailto link is out of the check", "[x](mailto:nobody@example.invalid)", false},
		{"a relative path that exists resolves", "[x](docs/real.md)", false},
		{"an image is a link like any other", "![alt](docs/gone.png)", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := writeCorpus(t, map[string]string{
				"docs/real.md": satisfied(),
				"CLAUDE.md":    "# Brief\n\n" + c.link + "\n",
			})
			files, err := Corpus(dir)
			if err != nil {
				t.Fatalf("Corpus: %v", err)
			}
			problems, err := CheckLinks(dir, files)
			if err != nil {
				t.Fatalf("CheckLinks: %v", err)
			}
			if c.dead && len(problems) != 1 {
				t.Fatalf("want the link reported dead, got %d: %v", len(problems), problems)
			}
			if !c.dead && len(problems) != 0 {
				t.Fatalf("a link this check does not resolve was reported: %v", problems)
			}
		})
	}
}

// TestCheckLinks_AC9_OnlyTheTwoEntryPointsAreResolved pins the SET the rule reads. A link
// in any other document is not resolved, so a test that wrote its fixture link into the
// wrong file would report a green that means nothing - and this is what tells the two
// apart.
func TestCheckLinks_AC9_OnlyTheTwoEntryPointsAreResolved(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs/real.md":  satisfied() + "\n[x](docs/gone.md)\n",
		"CLAUDE.md":     "# Brief\n\n[x](docs/real.md)\n",
		"README.md":     "# holdfast\n\nWhat it is.\n",
		"docs/other.md": "[x](docs/gone.md)\n",
	})
	if problems := checkRepo(t, dir); len(problems) != 0 {
		t.Fatalf("a link outside %v was resolved: %v", LinkedDocs, problems)
	}
}

// TestCheckRepo_AC10_AnOrphanedDesignDocumentFails is AC-10: a document under docs/design/
// that no line in CLAUDE.md links to. That is the specific way this consolidation can lose
// an argument - the reasoning is moved out of the file that used to carry it into a file
// nothing sends a reader to - so the gate names the document rather than the absence.
func TestCheckRepo_AC10_AnOrphanedDesignDocumentFails(t *testing.T) {
	dir := writeCorpus(t, map[string]string{
		"docs/design/linked.md":   satisfied(),
		"docs/design/orphaned.md": "# Orphaned\n\nAn argument with nothing pointing at it.\n",
		"CLAUDE.md":               "# Brief\n\n[the design](docs/design/linked.md#swap-invariant)\n",
		"README.md":               "# holdfast\n\nWhat it is.\n",
	})
	problems := checkRepo(t, dir)
	if len(problems) != 1 {
		t.Fatalf("want exactly the orphaned document, got %d: %v", len(problems), problems)
	}
	if !strings.Contains(problems[0], filepath.ToSlash(filepath.Join(DesignDir, "orphaned.md"))) {
		t.Errorf("the problem does not name the orphaned document: %q", problems[0])
	}
	if strings.Contains(problems[0], "linked.md") {
		t.Errorf("a document CLAUDE.md links to was reported as orphaned: %q", problems[0])
	}
}
