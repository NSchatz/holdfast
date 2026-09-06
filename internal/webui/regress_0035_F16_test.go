package webui

// regress_0035_F16: AC9's WORDING half is a whole-page substring sweep, so a dead
// string satisfies it while the row an operator sees says something else.
//
// AC9: "WHEN a filter term is entered that matches no loaded row in a table, THE
// SYSTEM SHALL render a visible message in that table's body STATING that no loaded
// row matches the filter AND that the loaded rows are themselves capped (so 'no
// match' is never read as 'not in the library')."
//
// TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped opens with
//
//	s := string(indexHTML)
//
// - the RAW page, not pageWithoutComments - and then asks only that the two
// sentences appear SOMEWHERE in it:
//
//	"No loaded row matches this filter."
//	"themselves capped"
//
// The render site it pins is `mk("td", "empty", NO_MATCH_TEXT)`, which pins the
// NAME of the constant and never its VALUE. So the two halves are not tied
// together: move the wording into any other string on the page and set
// NO_MATCH_TEXT to "n/a", and a filter term matching no loaded row renders a row
// that states NEITHER of the two things this criterion exists to make it state -
// with every committed test in this package green. It does not even need a comment
// to hide in, so stripPageComments would not close it; the mutation below is a
// second const declaration.
//
// This is impl-gate finding F7's own class. Fix loop 2 answered F7 by pinning the
// render site AND stripping comments page-wide, and reading 13 records the
// measurement that both were needed. That fix reached AC10's grader. AC9's grader
// still reads the raw page and still grades the wording off a substring nothing
// has to render.
//
// The shipped page is CORRECT: NO_MATCH_TEXT carries both sentences and the row is
// built from it. This file grades the PROOF, which the spec's acceptance-criteria
// preamble makes part of the criterion: "a test that passes on a mutated page has
// not graded its criterion."
//
// Fix upstream in the grader: assert the two sentences at the site that RENDERS
// them - the declaration `mk("td", "empty", NO_MATCH_TEXT)` reads is the only place
// the message's text is chosen - and read the comment-stripped page while doing it,
// the way TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes
// already does for AC10's nine strings. This file goes green when a no-match row
// that says "n/a" can no longer pass.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	f16Env = "S0035_F16_MUT"

	f16Old = `const NO_MATCH_TEXT = "No loaded row matches this filter. The rows loaded here are "`

	// NO_MATCH_TEXT - the value the row is actually built from - becomes "n/a".
	// The sentences AC9 demands survive verbatim, in a constant nothing reads.
	f16New = `const NO_MATCH_TEXT = "n/a";` + "\n" +
		`const NO_MATCH_TEXT_OLD = "No loaded row matches this filter. The rows loaded here are "`

	// Every committed test in this package EXCEPT the regress parents, which spawn
	// children of their own. RE2 has no negative lookahead, so "does not start with
	// TestReg" is spelled with character classes.
	f16Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`

	f16Grader = "TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped"
)

func init() {
	if os.Getenv(f16Env) == "" {
		return
	}
	s := string(indexHTML)
	if !strings.Contains(s, f16Old) {
		panic("regress_0035_F16: mutation anchor absent from index.html: " + f16Old)
	}
	indexHTML = []byte(strings.Replace(s, f16Old, f16New, 1))
}

func f16IsChild() bool {
	for _, v := range []string{
		"S0035_F16_MUT", "S0035_F15_MUT", "S0035_F14_MUT", "S0035_MUT",
		"S0035_F7_MUT", "S0035_F11_MUT", "S0035_F12_MUT",
	} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

func TestRegress0035_F16_AC9ADeadStringSatisfiesTheWordingSweep(t *testing.T) {
	if f16IsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	cmd := exec.Command(os.Args[0], "-test.run", f16Run, "-test.v")
	cmd.Env = append(os.Environ(), f16Env+"=1")
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   "+f16Grader) {
		t.Fatalf("the child never ran %s, so nothing was graded\n%s", f16Grader, out)
	}
	if err == nil {
		t.Errorf("AC9's wording is ungraded: every committed test in this package stayed "+
			"GREEN on a page where NO_MATCH_TEXT - the value the no-match row is built "+
			"from - is \"n/a\", and the two sentences the criterion demands survive only in "+
			"a second constant nothing reads. A filter term matching no loaded row then "+
			"renders a row stating neither that no loaded row matches nor that the loaded "+
			"rows are capped, which is the reading AC9 exists to prevent. The grader reads "+
			"the RAW page (s := string(indexHTML)) and asks only that the sentences appear "+
			"somewhere in it, while pinning the render site by the constant's NAME.\n"+
			"mutation: %q -> %q\n%s", f16Old, f16New, out)
	}
}
