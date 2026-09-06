package webui

// regress_0035_F15: AC8's focus proof never asks whether the rule that draws the
// ring APPLIES. A media query around it is recorded and then only printed.
//
// AC8: "WHEN a control receives keyboard focus, THE SYSTEM SHALL draw a focus
// indicator that clears 3:1 against BOTH the control's own fill and the surface
// immediately behind it, in both themes ..."
//
// TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt resolves an outline
// per rule (outlineOf), keeps the at-rule prelude on the spec as `o.at`, and then
// uses it for one thing only:
//
//	where := o.sel
//	if o.at != "" { where = o.at + " { " + o.sel }
//
// `where` is an error-message label. The coverage loop that follows asks
//
//	if o.focus && selectorReaches(o.sel, []pageElement{el}) { covered = true }
//
// and never asks what condition `o.at` puts on that rule. So the page's single
// `:focus-visible` rule can be wrapped in ANY media query and every control on the
// page still counts as covered. Wrap it in `@media (min-width: 99999px)` and no
// control draws a focus indicator at any viewport a person has; wrap it in
// `@media (prefers-reduced-motion: reduce)` and only a user who asked for reduced
// motion gets one. Both leave every committed test in this package green.
//
// This is the shape this file already refuses TWICE elsewhere. themeTokens
// compares the light block's prelude with canonicalAt for what it IS, and its own
// comment says why: "Matching a prelude with strings.Contains is the F12 defect in
// another position: `@media (prefers-color-scheme: light) and (min-width: 99999px)`
// contains both words, reads as the live light theme, and applies to nothing." The
// AC5 sweep does the same for the reduce block. The AC8 sweep was not given that
// reader, so the criterion ordinal 3 blocked on is still graded by a test that
// cannot fail on the plainest violation of it there is: no ring at all.
//
// The shipped page is CORRECT: `:focus-visible` sits at the top level and applies
// everywhere. This file grades the PROOF, which the spec's acceptance-criteria
// preamble makes part of the criterion: "a test that passes on a mutated page has
// not graded its criterion."
//
// Fix upstream in the sweep, not the page: a rule may only count as drawing the
// indicator when the at-rule it sits under is one this reader can show holds
// unconditionally (`o.at == ""`), the way themeTokens decides which :root block is
// the live one. This file goes green when a ring that no real user agent turns on
// can no longer pass as coverage.

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const (
	f15Env = "S0035_F15_MUT"

	f15Old = "  :focus-visible { outline:var(--focus-w) solid var(--focus);\n" +
		"    outline-offset:var(--focus-offset); border-radius:var(--radius-sm); }"

	// Case "a": the ring exists only above a viewport no display has.
	f15NewA = "  @media (min-width: 99999px) {\n" + f15Old + "\n  }"

	// Case "b": the ring exists only for a user who asked for reduced motion.
	f15NewB = "  @media (prefers-reduced-motion: reduce) {\n" + f15Old + "\n  }"

	// Every committed test in this package EXCEPT the regress parents, which spawn
	// children of their own. RE2 has no negative lookahead, so "does not start with
	// TestReg" is spelled with character classes.
	f15Run = `^Test([A-QS-Z]|R[^e]|Re[^g])`

	f15Grader = "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt"
)

func init() {
	which := os.Getenv(f15Env)
	replacement := ""
	switch which {
	case "":
		return
	case "a":
		replacement = f15NewA
	case "b":
		replacement = f15NewB
	default:
		panic("regress_0035_F15: unknown case " + which)
	}
	s := string(indexHTML)
	if !strings.Contains(s, f15Old) {
		panic("regress_0035_F15: mutation anchor absent from index.html: " + f15Old)
	}
	indexHTML = []byte(strings.Replace(s, f15Old, replacement, 1))
}

func f15Child(t *testing.T, which string) (green bool, out string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", f15Run, "-test.v")
	cmd.Env = append(os.Environ(), f15Env+"="+which)
	b, err := cmd.CombinedOutput()
	out = string(b)
	if !strings.Contains(out, "=== RUN   "+f15Grader) {
		t.Fatalf("the child never ran %s, so nothing was graded\n%s", f15Grader, out)
	}
	return err == nil, out
}

func f15IsChild() bool {
	for _, v := range []string{
		"S0035_F15_MUT", "S0035_F14_MUT", "S0035_MUT",
		"S0035_F7_MUT", "S0035_F11_MUT", "S0035_F12_MUT",
	} {
		if os.Getenv(v) != "" {
			return true
		}
	}
	return false
}

func TestRegress0035_F15_AC8ARingNoUserAgentTurnsOnCountsAsCoverage(t *testing.T) {
	if f15IsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	green, out := f15Child(t, "a")
	if green {
		t.Errorf("AC8 is ungraded against a ring that never applies: every committed test "+
			"in this package stayed GREEN on a page whose only :focus-visible rule sits "+
			"inside `@media (min-width: 99999px)`, so NO control draws a focus indicator at "+
			"any viewport a person has - in either theme. The sweep keeps the at-rule "+
			"prelude on the outline spec and uses it only to label an error message; the "+
			"coverage loop asks selectorReaches(o.sel, ...) and never asks what condition "+
			"o.at puts on the rule.\nmutation: %q -> %q\n%s", f15Old, f15NewA, out)
	}
}

func TestRegress0035_F15_AC8ARingScopedToAReducedMotionPreferenceCountsAsCoverage(t *testing.T) {
	if f15IsChild() {
		t.Skip("child run: this process carries a mutated page")
	}
	green, out := f15Child(t, "b")
	if green {
		t.Errorf("AC8 is ungraded against a ring scoped to one user preference: every "+
			"committed test in this package stayed GREEN on a page whose only "+
			":focus-visible rule sits inside `@media (prefers-reduced-motion: reduce)`, so "+
			"every keyboard operator who has NOT asked for reduced motion tabs through all "+
			"five controls with no indicator at all.\nmutation: %q -> %q\n%s",
			f15Old, f15NewB, out)
	}
}
