package webui

// regress_0035_F14: AC6's single-column proof accepts a narrow-viewport block that
// applies at NO viewport, and accepts a multi-column grid because it matches the
// wanted value by substring.
//
// AC6: "WHEN the viewport is 360 CSS px wide, THE SYSTEM SHALL present the header,
// both control rows and the aggregate cards in a SINGLE COLUMN ..."
//
// TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody discharges that
// half like this:
//
//	if !strings.Contains(r.at, "@media") { continue }
//	m := maxWidthRe.FindStringSubmatch(r.at)
//	...
//	if w, ok := want[r.sel]; ok && strings.Contains(r.get(w[0]), w[1]) { delete(want, r.sel) }
//
// Two holes, both of the class this loop was opened to sweep:
//
//  1. The at-rule prelude is matched by CONTAINMENT, never for what it IS. The rest
//     of the prelude is never read, so `@media (max-width: 640px) and (min-width:
//     99999px)` still yields max-width 640, still reads as the narrow block, and
//     applies to nothing. This file already refuses exactly that shape twice, for
//     AC4 and AC5, through canonicalAt - whose own comment says "Matching a prelude
//     with strings.Contains is the F12 defect in another position ... contains both
//     words, reads as the live light theme, and applies to nothing". The AC6 sweep
//     was not given the same reader.
//
//  2. The declared value is matched by CONTAINMENT too, so
//     `grid-template-columns:repeat(3, 1fr)` contains "1fr" and satisfies a check
//     whose whole subject is that there is ONE column. Three columns at a 360px
//     viewport is the state the criterion names.
//
// On either mutated page the header, both control rows and the aggregate grid are
// NOT a single column at 360px, and every committed test in this package passes.
//
// The shipped page is CORRECT. This file grades the PROOF, which the spec's
// acceptance-criteria preamble makes part of the criterion: "a test that passes on
// a mutated page has not graded its criterion."
//
// Fix upstream in the sweep, not the page: read the prelude with canonicalAt (or
// otherwise refuse a prelude carrying a condition this reader cannot show holds at
// --bp-narrow), and compare the declared value for what it is rather than for a
// substring it happens to contain. This file goes green when a narrow-viewport
// block that never applies, and a multi-column grid, can no longer pass unread.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	f14Env = "S0035_F14_MUT"

	// Case 1: the narrow block gains a second condition that never holds.
	f14aOld = "  @media (max-width: 640px) {"
	f14aNew = "  @media (max-width: 640px) and (min-width: 99999px) {"

	// Case 2: the aggregate grid is three columns at the narrow viewport.
	f14bOld = "    .aggs { grid-template-columns:1fr; }"
	f14bNew = "    .aggs { grid-template-columns:repeat(3, 1fr); }"

	// Every committed test in this package EXCEPT the regress parents, which spawn
	// children of their own. RE2 has no negative lookahead, so "does not start with
	// TestReg" is spelled with character classes.
	f14Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`

	f14Grader = "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody"
)

func init() {
	which := os.Getenv(f14Env)
	old, new := "", ""
	switch which {
	case "":
		return
	case "a":
		old, new = f14aOld, f14aNew
	case "b":
		old, new = f14bOld, f14bNew
	default:
		panic("regress_0035_F14: unknown case " + which)
	}
	s := string(indexHTML)
	if !strings.Contains(s, old) {
		panic("regress_0035_F14: mutation anchor absent from index.html: " + old)
	}
	indexHTML = []byte(strings.Replace(s, old, new, 1))
}

func f14Child(t *testing.T, which string) (green bool, out string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", f14Run, "-test.v")
	cmd.Env = append(os.Environ(), f14Env+"="+which)
	b, err := cmd.CombinedOutput()
	out = string(b)
	if !strings.Contains(out, "=== RUN   "+f14Grader) {
		t.Fatalf("the child never ran %s, so nothing was graded\n%s", f14Grader, out)
	}
	return err == nil, out
}

func f14IsChild() bool {
	for _, v := range []string{
		"S0035_F14_MUT", "S0035_MUT", "S0035_F7_MUT", "S0035_F11_MUT", "S0035_F12_MUT",
	} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

func TestRegress0035_F14_AC6ANarrowBlockThatNeverAppliesIsUnread(t *testing.T) {
	if f14IsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	green, out := f14Child(t, "a")
	if green {
		t.Errorf("AC6's single-column half is ungraded against a narrow block that never "+
			"applies: every committed test in this package stayed GREEN on a page whose "+
			"only max-width block is `@media (max-width: 640px) and (min-width: 99999px)`, "+
			"which matches no viewport at all - so at 360px the header, both control rows "+
			"and the aggregate grid stay multi-column. The sweep tests the prelude with "+
			"strings.Contains(r.at, \"@media\") plus a max-width regex and never reads the "+
			"rest of it, which is the shape canonicalAt already refuses for AC4 and AC5.\n"+
			"mutation: %q -> %q\n%s", f14aOld, f14aNew, out)
	}
}

func TestRegress0035_F14_AC6AThreeColumnGridSatisfiesTheSingleColumnCheck(t *testing.T) {
	if f14IsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	green, out := f14Child(t, "b")
	if green {
		t.Errorf("AC6's single-column half is ungraded against a multi-column grid: every "+
			"committed test in this package stayed GREEN on a page that lays the aggregate "+
			"cards out in THREE columns at the 360px viewport, because the wanted value "+
			"\"1fr\" is looked for with strings.Contains and `repeat(3, 1fr)` contains it. "+
			"A check whose whole subject is that there is one column is satisfied by three.\n"+
			"mutation: %q -> %q\n%s", f14bOld, f14bNew, out)
	}
}
