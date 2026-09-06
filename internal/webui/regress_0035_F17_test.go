package webui

// regress_0035_F17: AC10's render-site pins prove the CALL, not the render. The
// helper the call goes through can drop the text and the grader stays green.
//
// AC10: "WHEN the queue is empty, the history is empty ... THE SYSTEM SHALL RENDER
// the wording it renders today, verbatim: `Nothing queued.`, `No history yet.` ..."
//
// TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes pairs each of
// the nine strings with the expression that renders it, and its own comment states
// why: "the page may still carry those words somewhere, but the node that shows
// them to an operator has stopped saying them". For the two empty-table states the
// pinned expression is the CALL SITE:
//
//	{"Nothing queued.", `emptyRow(5, "Nothing queued.")`},
//	{"No history yet.", `emptyRow(7, "No history yet.")`},
//
// Nothing pins what emptyRow does with its `text` argument. Change one line of it -
//
//	const td = mk("td", "empty", text);   ->   const td = mk("td", "empty", "");
//
// - and an empty queue and an empty history both render a BLANK row: the operator
// is shown a table that says nothing at all, which is the exact honesty failure
// AC10 exists to pin, and every committed test in this package stays green. The
// wording survives on the page only as an argument nothing displays, which is the
// same distinction finding F7 drew between the page's text and its render, one
// level further down the call.
//
// It is the same shape as impl-gate finding F1 on AC9 - a node built, given its
// span, and then not shown - which fix loop 1 answered for AC9 by pinning
// setNoMatchRow's whole ORDERED construction path inside the function's own body.
// AC10's two empty-table states got no equivalent, and they are the states an
// operator reads when there is nothing to read.
//
// The shipped page is CORRECT: emptyRow puts its text in the cell. This file grades
// the PROOF, which the spec's acceptance-criteria preamble makes part of the
// criterion: "a test that passes on a mutated page has not graded its criterion."
//
// Fix upstream in the grader: pin emptyRow's own body the way setNoMatchRow's is
// pinned, so the argument provably reaches a cell that reaches the table. This file
// goes green when an empty queue that renders a blank row can no longer pass.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	f17Env = "S0035_F17_MUT"

	f17Old = `  const td = mk("td", "empty", text);
  td.colSpan = cols;
  tr.appendChild(td);
  return tr;
}`
	f17New = `  const td = mk("td", "empty", "");
  td.colSpan = cols;
  tr.appendChild(td);
  return tr;
}`

	// Every committed test in this package EXCEPT the regress parents, which spawn
	// children of their own. RE2 has no negative lookahead, so "does not start with
	// TestReg" is spelled with character classes.
	f17Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`

	f17Grader = "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes"
)

func init() {
	if os.Getenv(f17Env) == "" {
		return
	}
	s := string(indexHTML)
	if !strings.Contains(s, f17Old) {
		panic("regress_0035_F17: mutation anchor absent from index.html: " + f17Old)
	}
	indexHTML = []byte(strings.Replace(s, f17Old, f17New, 1))
}

func f17IsChild() bool {
	for _, v := range []string{
		"S0035_F17_MUT", "S0035_F16_MUT", "S0035_F15_MUT", "S0035_F14_MUT",
		"S0035_MUT", "S0035_F7_MUT", "S0035_F11_MUT", "S0035_F12_MUT",
	} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

func TestRegress0035_F17_AC10AnEmptyTableRendersABlankRowUnread(t *testing.T) {
	if f17IsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	cmd := exec.Command(os.Args[0], "-test.run", f17Run, "-test.v")
	cmd.Env = append(os.Environ(), f17Env+"=1")
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   "+f17Grader) {
		t.Fatalf("the child never ran %s, so nothing was graded\n%s", f17Grader, out)
	}
	if err == nil {
		t.Errorf("AC10 is ungraded for its two empty-table states: every committed test in "+
			"this package stayed GREEN on a page whose emptyRow builds its cell with an "+
			"EMPTY string, so an empty queue and an empty history each render a blank row "+
			"and the operator is shown a table that says nothing. `Nothing queued.` and "+
			"`No history yet.` are pinned at the CALL SITE (emptyRow(5, ...)) and nothing "+
			"pins what emptyRow does with the argument - the same build-then-do-not-show "+
			"shape finding F1 named on AC9, which was closed there by pinning "+
			"setNoMatchRow's whole ordered construction path.\nmutation: %q -> %q\n%s",
			f17Old, f17New, out)
	}
}
