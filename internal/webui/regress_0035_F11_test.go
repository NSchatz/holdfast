package webui

// regress_0035_F11: AC8's focus proof reads ONE :focus-visible rule and silently
// discards the rule that removes the ring.
//
// AC8: "WHEN a control receives keyboard focus, THE SYSTEM SHALL draw a focus
// indicator that clears 3:1 against BOTH the control's own fill and the surface
// immediately behind it, in both themes, INCLUDING THE SOLID-FILL PRIMARY BUTTON
// where an accent ring on an accent fill would be invisible."
//
// TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt walks every rule
// whose selector contains ":focus-visible" and, for each, takes the colour token
// out of `outline-color`/`outline` and the width out of `outline-width`/`outline`:
//
//	if c := colourTokens(dark, v); len(c) > 0 { focus = c[0] }
//	if w := r.get("outline-width"); w != "" { width = w } else if o := r.get("outline"); o != "" {
//	    for _, f := range strings.Fields(o) { if _, ok := measuredPx(dark, f); ok { width = f; break } }
//	}
//
// Both assignments are CONDITIONAL on the rule yielding a value. `outline:none`
// yields neither: it carries no colour token, and "none" is not a measurable
// length. So a second, MORE SPECIFIC :focus-visible rule that switches the outline
// off is read by the loop and then thrown away, and the test goes on reporting the
// contrast and the width of the base rule the cascade has already overridden.
//
// That is the failure mode loop 1's F2 named for AC6 - "an unmeasurable value
// scored as harmless" - reappearing on AC8, and the width read that loop 2's
// reading 14 added to close F9 does not reach it: F9's shape was `--focus-w:0` on
// the one rule the loop can see; this shape never touches that token.
//
// The mutation below is the one AC8 names by hand. AC8 anticipates that the
// primary button will be treated specially ("including the solid-fill primary
// button"), and `button.primary:focus-visible { outline:none; }` is how a stylesheet
// says that - it is the standard prelude to a bespoke ring, and it is one line. On
// the mutated page a keyboard operator tabbing to Rescan, the one control that
// starts a scan, sees no indicator at all, while every committed test in this
// package passes.
//
// The shipped page is CORRECT: it declares exactly one :focus-visible rule and the
// ring is drawn. This test grades the PROOF, which the spec's acceptance-criteria
// preamble makes part of the criterion: "a test that passes on a mutated page has
// not graded its criterion."
//
// Fix upstream in the sweep, not the page: resolve the outline per CONTROL rather
// than folding every :focus-visible rule into one colour and one width - or, at
// minimum, FAIL on a :focus-visible rule whose outline this reader cannot measure,
// which is the "resolvable or red" discipline the AC6 width sweep and the AC3
// contrast derivation are both already built on. This file goes green when a
// :focus-visible rule that draws no indicator can no longer pass unread.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	f11Env = "S0035_F11_MUT"

	// Inserted ahead of the .visually-hidden rule, i.e. AFTER the page's single
	// :focus-visible rule, so the cascade gives it to the primary button.
	f11Old = "  /* An off-screen but screen-reader-available region"
	f11New = "  button.primary:focus-visible { outline:none; }\n" +
		"  /* An off-screen but screen-reader-available region"

	// Every committed test in this package EXCEPT the regress parents, which spawn
	// children of their own. RE2 has no negative lookahead, so "does not start with
	// TestReg" is spelled with character classes.
	f11Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`

	f11Grader = "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt"
)

func init() {
	if os.Getenv(f11Env) == "" {
		return
	}
	s := string(indexHTML)
	if !strings.Contains(s, f11Old) {
		panic("regress_0035_F11: mutation anchor absent from index.html: " + f11Old)
	}
	indexHTML = []byte(strings.Replace(s, f11Old, f11New, 1))
}

func TestRegress0035_F11_AC8ARuleThatRemovesTheRingIsUnread(t *testing.T) {
	if os.Getenv(f11Env) != "" || os.Getenv("S0035_MUT") != "" ||
		os.Getenv("S0035_F7_MUT") != "" || os.Getenv("S0035_F12_MUT") != "" {
		t.Skip("child run: this process carries a mutated page")
	}
	cmd := exec.Command(os.Args[0], "-test.run", f11Run, "-test.v")
	cmd.Env = append(os.Environ(), f11Env+"=1")
	b, err := cmd.CombinedOutput()
	out := string(b)
	if !strings.Contains(out, "=== RUN   "+f11Grader) {
		t.Fatalf("the child never ran %s, so nothing was graded\n%s", f11Grader, out)
	}
	if err == nil {
		t.Errorf("AC8 is ungraded against a rule that removes the indicator: every committed "+
			"test in this package stayed GREEN on a page carrying "+
			"`button.primary:focus-visible { outline:none; }`, which leaves the primary "+
			"button - the control AC8 names by hand - with no focus indicator on keyboard "+
			"focus in either theme. The sweep reads that rule and discards it, because "+
			"`outline:none` yields neither a colour token nor a measurable width, and then "+
			"reports the colour and width of the base rule the cascade overrode.\n"+
			"mutation: %q -> %q\n%s", f11Old, f11New, out)
	}
}
