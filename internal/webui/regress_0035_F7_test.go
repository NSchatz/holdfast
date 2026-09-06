package webui

// regress_0035_F7: AC10's `not recorded` is graded by a COMMENT, not by the render.
//
// AC10: "WHEN ... an aggregate counted no contributing row, a progress figure is
// absent ... THE SYSTEM SHALL render the wording it renders today, verbatim:
// `Nothing queued.`, `No history yet.`, `unavailable`, `not recorded`, ..."
//
// TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes asserts all
// nine strings with `strings.Contains` over the WHOLE page source, comments
// included. For eight of the nine that is still a real proof: five of them
// ("Nothing queued.", "No history yet.", "this view is capped", "reconnecting…"
// and the two authorisation messages) appear on the page exactly once, at the one
// place they are rendered, and the other two ("unknown", "unavailable") are pinned
// at their render site by an older assertion elsewhere in this file
// (`mk("span", "nr", "unknown")`, `v.appendChild(mk("span", "nr", "unavailable"))`).
//
// `not recorded` is the exception. It appears three times: in the comment directly
// above the node that renders it, in that node, and in the unrelated
// "reason not recorded" string -
//
//	// The honest "not recorded" node for a nil/absent outcome field.
//	function nrNode() { return mk("span", "nr", "not recorded"); }
//
// - and NOTHING pins the node's own text. Both assertions on the phrase
// (TestHonestCopy_ShowsModelPoolingAndWorstFrame and the AC10 grader) stay
// satisfied by the comment alone. nrNode is the honest-absence node behind every
// nil outcome field: the size pair, the VMAF pair, the encoder, the encode
// duration and an aggregate that counted no contributing row. Reword it and an
// operator reading a done row can no longer tell a figure nobody measured from one
// that measured zero - which the spec's Blast radius paragraph names as the FIRST
// of the two things this change can damage.
//
// The distinction this test asks for is one webui_test.go already draws elsewhere,
// in TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt:
//
//	// Comments stripped: a comment saying the page declares no keyframes is not a
//	// keyframes block.
//
// A comment saying "not recorded" is not a rendered "not recorded".
//
// The shipped page is CORRECT; this test grades the PROOF, which the spec's
// acceptance-criteria preamble makes part of the criterion: "a test that passes on
// a mutated page has not graded its criterion."
//
// Fix upstream by pinning the wording at its render site the way the other eight
// are pinned - assert `mk("span", "nr", "not recorded")` - or by stripping comments
// before the AC10 sweep so every one of the nine is read off the page and not off
// prose about the page. This file goes green when it is graded.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	f7Env = "S0035_F7_MUT"
	f7Old = `function nrNode() { return mk("span", "nr", "not recorded"); }`
	f7New = `function nrNode() { return mk("span", "nr", "n/a"); }`

	// Every committed test in this package EXCEPT the regress parents, which spawn
	// children of their own. RE2 has no negative lookahead, so "does not start with
	// TestReg" is spelled with character classes: TestRenderIdiom matches, every
	// TestRegress0035_* does not.
	f7Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`

	// The grader whose green is the finding. Named so the probe fails loudly if the
	// child never reached it, rather than reading "nothing ran" as "nothing red".
	f7Grader = "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes"
)

// init applies the mutation to the embedded page before any test in the child
// process runs. Package-level variables (indexHTML among them) are initialised
// before init functions, so the embed is already in place here. The S0035 mutation
// harness keys off its own variable and is inert in this child, and this init is
// inert in that one.
func init() {
	if os.Getenv(f7Env) == "" {
		return
	}
	s := string(indexHTML)
	if !strings.Contains(s, f7Old) {
		panic("regress_0035_F7: mutation anchor absent from index.html: " + f7Old)
	}
	indexHTML = []byte(strings.Replace(s, f7Old, f7New, 1))
}

func TestRegress0035_F7_AC10NotRecordedIsGradedByACommentNotByTheRender(t *testing.T) {
	if os.Getenv(f7Env) != "" || os.Getenv("S0035_MUT") != "" {
		t.Skip("child run: this process carries a mutated page")
	}
	cmd := exec.Command(os.Args[0], "-test.run", f7Run, "-test.v")
	cmd.Env = append(os.Environ(), f7Env+"=1")
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   "+f7Grader) {
		t.Fatalf("the child never ran %s, so nothing was graded\n%s", f7Grader, out)
	}
	if err == nil {
		t.Errorf("AC10 is ungraded for `not recorded`: every committed test in this package "+
			"stayed GREEN on a page whose nrNode() - the honest-absence node behind every nil "+
			"outcome field - renders \"n/a\" instead of \"not recorded\". The wording survives "+
			"only in the comment on the line above it and in the unrelated \"reason not "+
			"recorded\" string, and both assertions on it match the whole page source, comments "+
			"included.\nmutation: %q -> %q\n%s", f7Old, f7New, out)
	}
}
