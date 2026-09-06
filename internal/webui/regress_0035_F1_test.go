package webui

// regress_0035_F1: AC9's grader does not grade AC9.
//
// AC9: "WHEN a filter term is entered that matches no loaded row in a table, THE
// SYSTEM SHALL render a visible message in that table's body ... Today both
// bodies go silently blank."
//
// TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped asserts
// fourteen source substrings covering setNoMatchRow's text, its class, its
// colSpan derivation, its removal path and its call site - every line of the
// function except the one that puts the row into the table. Delete
// `body.appendChild(tr)` and the page behaves exactly as it does before this
// spec (both bodies go silently blank) while the grader stays green.
//
// The shipped page is CORRECT; this test grades the PROOF, which the spec's
// acceptance-criteria preamble makes part of the criterion: "a test that passes
// on a mutated page has not graded its criterion."
//
// Fix upstream by asserting the insertion, e.g. add `body.appendChild(tr);` to
// the substring list in that test. This file goes green when it is graded.

import (
	"os"
	"testing"
)

func TestRegress0035_F1_AC9NoMatchMessageInsertionIsUngraded(t *testing.T) {
	if os.Getenv("S0035_MUT") != "" {
		t.Skip("child run")
	}
	const name = "F1-ac9-message-never-inserted"
	m := s0035Holes[name]
	green, out := runS0035Mutant(t, name, m)
	if green {
		t.Errorf("AC9 is ungraded: %s stayed GREEN on a page where %s\nmutation: %q -> %q\n%s",
			m.target, m.why, m.old, m.new, out)
	}
}
