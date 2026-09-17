package docscheck_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NSchatz/holdfast/internal/docscheck"
)

// clauseLines builds one prose line per clause, each carrying that clause's own token.
//
// The lines are derived from the exported table rather than pasted, so a fixture cannot
// drift into asserting a token the table no longer owns - a negative fixture that has gone
// stale passes for the wrong reason, and this check exists because documentation obligations
// lapse quietly.
func clauseLines(clauses []docscheck.Clause) []string {
	out := make([]string, 0, len(clauses))
	for _, c := range clauses {
		out = append(out, "The statement says "+c.Token+", at length.")
	}
	return out
}

// statementDoc is a shipped-document fixture: a heading, the anchor, the lines given, and a
// following heading that ends the statement the way a real document does.
func statementDoc(anchor string, lines []string) string {
	return "# Fixture\n\n## Non-goals\n\n<a id=\"" + anchor + "\"></a>\n\n" +
		strings.Join(lines, "\n") + "\n\n## After\n\nProse that is not the statement.\n"
}

// writeCorpus writes each document into a temp directory and returns the paths, which is the
// corpus the check is handed. The files are real files read off a disk: the unit under test
// is the check over a corpus, and a double for the corpus would be a double for the subject.
func writeCorpus(t *testing.T, docs ...string) []string {
	t.Helper()
	dir := t.TempDir()
	var paths []string
	for i, doc := range docs {
		p := filepath.Join(dir, "doc"+string(rune('a'+i))+".md")
		if err := os.WriteFile(p, []byte(doc), 0o600); err != nil {
			t.Fatalf("write the fixture corpus: %v", err)
		}
		paths = append(paths, p)
	}
	return paths
}

// TestDynamicHDRStatement_IsCarriedByOneShippedDocument grades [AC-1] of S0106: the check
// passes only if ONE shipped document carries the dynamic-HDR statement and that single
// statement says all three of its clauses.
//
// It runs over the corpus this repository actually ships, not a fixture, because the
// obligation is about the documents an operator reads. Every other test below proves this
// one can fail.
func TestDynamicHDRStatement_IsCarriedByOneShippedDocument(t *testing.T) {
	if err := docscheck.CheckDynamicHDR(shippedMarkdown(t)); err != nil {
		t.Errorf("the shipped documentation does not carry the dynamic-HDR statement: %v\n"+
			"Dolby Vision and HDR10+ sources are skipped by a tool that deletes a source once a "+
			"replacement passes its gates, and a reader weighing that skip has to be told it is "+
			"deferred, what lifting it needs, and what keeps it deferred - all three, in one place.", err)
	}
	if len(docscheck.DynamicHDRClauses) != 3 {
		t.Errorf("the dynamic-HDR statement owes 3 clauses, the table carries %d - a clause removed from "+
			"the table is a clause the shipped text is no longer held to",
			len(docscheck.DynamicHDRClauses))
	}
}

// TestDynamicHDR_MissingClauseNamesItAndTheClosestFile grades [AC-2] of S0106: when every
// occurrence of the statement is missing one of the three clauses, the check fails, naming
// the clause that is missing and the file closest to carrying them all.
//
// One fixture per clause, each dropping exactly that clause and nothing else, so a failure
// reports the problem it is about rather than the first one the scan happens to reach.
func TestDynamicHDR_MissingClauseNamesItAndTheClosestFile(t *testing.T) {
	all := clauseLines(docscheck.DynamicHDRClauses)
	for i, dropped := range docscheck.DynamicHDRClauses {
		lines := append(append([]string{}, all[:i]...), all[i+1:]...)
		paths := writeCorpus(t, statementDoc(docscheck.AnchorDynamicHDR, lines))

		err := docscheck.CheckDynamicHDR(paths)
		if err == nil {
			t.Fatalf("a statement missing the clause %q passed: the check is not reading its own clause table",
				dropped.Token)
		}
		if !strings.Contains(err.Error(), dropped.Token) {
			t.Errorf("the failure for a dropped %q does not name that token: %v", dropped.Token, err)
		}
		if !strings.Contains(err.Error(), paths[0]) {
			t.Errorf("the failure for a dropped %q does not name %s, the file closest to carrying them all: %v",
				dropped.Token, paths[0], err)
		}
		for _, kept := range docscheck.DynamicHDRClauses {
			if kept.Token != dropped.Token && strings.Contains(err.Error(), kept.Token) {
				t.Errorf("the failure for a dropped %q also reports %q, which the fixture carries: a message "+
					"naming clauses that are present sends an editor to the wrong sentence: %v",
					dropped.Token, kept.Token, err)
			}
		}
	}
}

// TestDynamicHDR_MissingAnchorIsItsOwnFailure grades [AC-3] of S0106: when no shipped
// document carries the statement's anchor at all, the check fails saying the dynamic-HDR
// statement is missing.
//
// The fixture drops exactly one thing - the anchor - and keeps every clause, so a check that
// looked for the tokens anywhere in a document rather than under the anchor would pass it.
func TestDynamicHDR_MissingAnchorIsItsOwnFailure(t *testing.T) {
	lines := clauseLines(docscheck.DynamicHDRClauses)
	paths := writeCorpus(t, "# Fixture\n\n## Non-goals\n\n"+strings.Join(lines, "\n")+"\n")

	err := docscheck.CheckDynamicHDR(paths)
	if err == nil {
		t.Fatal("a corpus carrying the clauses but no anchor passed: the statement is anchored so that a " +
			"refusal keeps pointing at the same paragraph, and prose nobody anchored is not that statement")
	}
	if !strings.Contains(err.Error(), docscheck.AnchorDynamicHDR) {
		t.Errorf("the failure does not name the missing anchor %q: %v", docscheck.AnchorDynamicHDR, err)
	}
	if !strings.Contains(err.Error(), "MISSING") {
		t.Errorf("the failure does not say the statement is missing, which is what it is: %v", err)
	}

	// The degenerate case of the same criterion: an empty corpus carries the anchor nowhere,
	// and a check that passed it would report "documented" for a repository it never read.
	if err := docscheck.CheckDynamicHDR(nil); err == nil {
		t.Error("an empty corpus passed, which passes every statement there is")
	}
}

// TestDynamicHDR_AnchorWithNothingUnderItIsItsOwnFailure grades [AC-4] of S0106: an anchor
// present with no line of text following it before the next anchor or heading fails, because
// an anchor with nothing under it is not a statement.
//
// The fixture drops exactly one thing - the text under the anchor - and keeps the clauses in
// the document, one heading further down.
func TestDynamicHDR_AnchorWithNothingUnderItIsItsOwnFailure(t *testing.T) {
	lines := clauseLines(docscheck.DynamicHDRClauses)
	doc := "# Fixture\n\n## Non-goals\n\n<a id=\"" + docscheck.AnchorDynamicHDR + "\"></a>\n\n" +
		"## After\n\n" + strings.Join(lines, "\n") + "\n"
	paths := writeCorpus(t, doc)

	err := docscheck.CheckDynamicHDR(paths)
	if err == nil {
		t.Fatal("an anchor with nothing under it passed: the anchor is where a reader is sent, so an empty " +
			"one sends them to nothing")
	}
	if !strings.Contains(err.Error(), paths[0]) || !strings.Contains(err.Error(), docscheck.AnchorDynamicHDR) {
		t.Errorf("the failure names neither the file nor the anchor, so nobody knows where to write: %v", err)
	}
	if !strings.Contains(err.Error(), "no line of text follows") {
		t.Errorf("the failure does not say the anchor is empty, which is the repair it needs: %v", err)
	}
}

// TestDynamicHDR_ClausesSplitAcrossDocumentsIsItsOwnFailure grades [AC-5] of S0106: three
// clauses carried between two different documents and by no single statement fail, because a
// reader weighing the skip has to find all three in one place.
//
// The fixture drops exactly one thing - the clauses being together - and every clause is
// still written down somewhere, which is precisely the case a per-clause presence check over
// a whole corpus would wave through.
func TestDynamicHDR_ClausesSplitAcrossDocumentsIsItsOwnFailure(t *testing.T) {
	lines := clauseLines(docscheck.DynamicHDRClauses)
	paths := writeCorpus(t,
		statementDoc(docscheck.AnchorDynamicHDR, lines[:len(lines)-1]),
		statementDoc(docscheck.AnchorDynamicHDR, lines[len(lines)-1:]),
	)

	err := docscheck.CheckDynamicHDR(paths)
	if err == nil {
		t.Fatal("clauses split across two documents passed: each document tells a reader something the " +
			"other one leaves out, and the statement that was owed says all of it in one place")
	}
	if !strings.Contains(err.Error(), "SPLIT") {
		t.Errorf("the failure reads as an ordinary missing clause rather than as clauses that are split, "+
			"which is a different repair: %v", err)
	}
	for _, p := range paths {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("the failure does not name %s, one of the documents holding part of the statement: %v", p, err)
		}
	}
}
