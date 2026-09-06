package webui

// S0035 impl-gate mutation harness (refuter artifact, verdict-impl-1).
//
// The spec's acceptance-criteria preamble states the grading rule this file
// executes: "the test reds means the test fails when the property is violated,
// which a refuter checks by mutating the page and observing the red - a test
// that passes on a mutated page has not graded its criterion."
//
// Editing the committed index.html is not something a review may do, so each
// mutation is applied to the embedded bytes in a CHILD test process: when
// S0035_MUT names a case, init() rewrites indexHTML before any test runs, and
// the parent asserts the criterion's own test goes RED on that page.
//
// s0035Mutations is the positive control and PASSES: every one of these edits
// violates its criterion and the committed grader reds on it. s0035Holes holds
// the edits that do NOT red, two of them behind their own regress_0035_F<n>
// test. Any case can also be driven by hand, for example:
//
//	S0035_MUT=F1-ac9-message-never-inserted go test ./internal/webui/ \
//	  -run '^TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped$' -v

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

type mutation struct {
	criterion string // the acceptance criterion whose grader is under test
	target    string // the test function that grades it
	old, new  string // the edit applied to the embedded page
	why       string // the property the edit violates
}

var s0035Mutations = map[string]mutation{
	"ac1-rgba-literal": {
		criterion: "AC1", target: "TestTokens_EveryColourIsDeclaredInATokenBlock",
		old: "background:var(--tint-cap);", new: "background:rgba(210,153,34,.12);",
		why: "the known violation the criterion names is reintroduced at its point of use",
	},
	"ac1-named-colour": {
		criterion: "AC1", target: "TestTokens_EveryColourIsDeclaredInATokenBlock",
		old: ".nr { color:var(--muted);", new: ".nr { color:rebeccapurple;",
		why: "a CSS named colour is written at its point of use",
	},
	"ac2-bare-font-size": {
		criterion: "AC2", target: "TestTokens_TypeAndSpaceComeFromTheDeclaredScales",
		old: "font-size:var(--fs-xl);", new: "font-size:18px;",
		why: "a font-size is written as a literal instead of coming from the type scale",
	},
	"ac2-bare-padding": {
		criterion: "AC2", target: "TestTokens_TypeAndSpaceComeFromTheDeclaredScales",
		old: "padding:var(--sp-4) var(--sp-7);", new: "padding:14px 20px;",
		why: "a padding length is written as a literal instead of coming from the space scale",
	},
	"ac3-pair-below-floor": {
		criterion: "AC3", target: "TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes",
		old: "--muted:#9aa3b4;", new: "--muted:#4a5160;",
		why: "a painted pair is pushed under the 4.5:1 text floor",
	},
	"ac3-unmeasured-new-rule": {
		criterion: "AC3", target: "TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes",
		old: "  .tablewrap { overflow-x:auto; }", new: "  .mut { color:var(--bad); }\n  .tablewrap { overflow-x:auto; }",
		why: "a later change adds a coloured rule that names no surface; it must be measured or red, never invisible",
	},
	"ac3-surface-with-no-text": {
		criterion: "AC3", target: "TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes",
		old: "  .tablewrap { overflow-x:auto; }", new: "  .mut { background:var(--panel); }\n  .tablewrap { overflow-x:auto; }",
		why: "a later change paints a surface with no declared text colour on it",
	},
	"ac4-no-color-scheme": {
		criterion: "AC4", target: "TestThemes_ALightSetIsDeclaredAndColorSchemeFollowsIt",
		old: "color-scheme: light dark;", new: "color-scheme: dark;",
		why: "native controls, scrollbars and form fields stop following the page theme",
	},
	"ac4-light-set-is-dark": {
		criterion: "AC4", target: "TestThemes_ALightSetIsDeclaredAndColorSchemeFollowsIt",
		old: "      --bg:#f6f7f9;", new: "      --bg:#0f1115;",
		why: "the light theme reuses the dark page surface, so there is no light set",
	},
	"ac5-motion-survives-reduce": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "transition-duration:0.01ms !important;", new: "transition-duration:400ms !important;",
		why: "a reduce preference no longer cuts the page's transitions to no perceptible motion",
	},
	"ac5-animation-added": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "  .dot { display:inline-block;", new: "  .dot { animation:pulse 2s infinite; display:inline-block;",
		why: "a state arrives by animation, so it is not readable from a static frame",
	},
	"ac6-wide-min-width": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "min-width:96px;", new: "min-width:960px;",
		why: "an element outside .tablewrap forces the page body wider than the narrow viewport",
	},
	"ac6-controls-stay-a-row": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "    .controls { flex-direction:column; align-items:stretch; }", new: "    .controls { align-items:stretch; }",
		why: "a control row is no longer a single column at the narrow viewport",
	},
	"ac7-target-under-floor": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "--target-min:32px;", new: "--target-min:20px;",
		why: "a pointer target falls under WCAG 2.2 2.5.8's 24px floor",
	},
	"ac7-min-height-dropped": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "min-height:var(--target-min); min-width:var(--target-min);", new: "min-width:var(--target-min);",
		why: "a control rule stops holding its target in one of the two dimensions",
	},
	"ac8-ring-invisible-on-primary": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "--focus:#ffffff;", new: "--focus:#1d4f7c;",
		why: "the focus ring is the primary button's own fill, so it is invisible on the control it marks",
	},
	"ac9-no-match-row-not-raised": {
		criterion: "AC9", target: "TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped",
		old: "    setNoMatchRow(body, term !== \"\" && loaded > 0 && shown === 0);\n", new: "",
		why: "a filter term matching nothing empties the table in silence again",
	},
	"ac10-copy-reworded": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: "\"Nothing queued.\"", new: "\"Nothing here.\"",
		why: "a degraded state's load-bearing wording is reworded",
	},
	"ac10-state-illegible": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: ".nr { color:var(--muted);", new: ".nr { color:var(--line);",
		why: "the honest-absence copy is painted under the 4.5:1 text floor",
	},

	// -----------------------------------------------------------------------
	// The grader sweep (conductor ruling, 2026-09-06). Every case below is a
	// hole one of these graders had - a check conditional on the thing it was
	// checking being present, or a membership test narrower than the
	// criterion's own set - and each one is now red. They are in the POSITIVE
	// control, not in s0035Holes, because that is what "hardened" means here:
	// the edit violates the criterion and the committed grader fails on it.
	// -----------------------------------------------------------------------

	"F11-ac8-ring-removed-on-the-primary-button": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "  /* An off-screen but screen-reader-available region",
		new: "  button.primary:focus-visible { outline:none; }\n  /* An off-screen but screen-reader-available region",
		why: "a more specific :focus-visible rule switches the ring off on the one control AC8 names by hand, so a keyboard operator tabbing to Rescan sees no indicator at all",
	},
	"hard-ac8-outline-removed-without-mentioning-focus": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "button.primary { background:var(--btn-primary-bg);", new: "button.primary { outline:none; background:var(--btn-primary-bg);",
		why: "the ring is removed by a rule that never mentions focus: `button.primary` (0,1,1) outranks `:focus-visible` (0,1,0), so a sweep restricted to :focus-visible rules would not have looked at it",
	},
	"hard-ac8-ring-reaches-only-the-buttons": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "  :focus-visible { outline:", new: "  button:focus-visible { outline:",
		why: "the ring is scoped to buttons, so the two inputs - the control token and the filter - draw no indicator on keyboard focus",
	},
	"F12-ac7-target-lowered-by-id": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "  #msg { font-size:var(--fs-md);", new: "  #rescan { min-height:20px; min-width:20px; }\n  #msg { font-size:var(--fs-md);",
		why: "a pointer target is held at 20px in both dimensions by a rule that addresses it by its id, which outranks the base `button` rule and names neither of the two words the old sweep gated on",
	},
	"hard-ac7-target-lowered-by-a-logical-longhand": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "  #msg { font-size:var(--fs-md);", new: "  #rescan { min-block-size:20px; }\n  #msg { font-size:var(--fs-md);",
		why: "a pointer target's minimum height is lowered through the logical longhand, which is a minimum height by any reading",
	},
	"F13-ac7-inline-target-under-floor": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: `<button id="pause">`, new: `<button id="pause" style="min-height:20px">`,
		why: "a style= attribute holds a pointer target under the floor, outranking every rule in the stylesheet",
	},
	"F13-ac1-inline-colour-literal": {
		criterion: "AC1", target: "TestTokens_EveryColourIsDeclaredInATokenBlock",
		old: `<col style="width:19%">`, new: `<col style="width:19%;background:#8b0000">`,
		why: "a colour literal is written in a style= attribute, which the stylesheet-only sweep never read",
	},
	"F13-ac2-inline-font-size": {
		criterion: "AC2", target: "TestTokens_TypeAndSpaceComeFromTheDeclaredScales",
		old: "<footer>", new: `<footer style="font-size:9px">`,
		why: "a font-size is written as a literal in a style= attribute instead of coming from the type scale",
	},
	"F13-ac6-inline-min-width": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "<main>", new: `<main style="min-width:900px">`,
		why: "a style= attribute forces the page body 900px wide, so it scrolls sideways at the 360px viewport",
	},
	"hard-ac1-literal-inside-the-token-block": {
		criterion: "AC1", target: "TestTokens_EveryColourIsDeclaredInATokenBlock",
		old: "color-scheme: light dark;", new: "color-scheme: light dark; background:#ff0000;",
		why: "a colour literal is painted from inside :root, where only the custom properties are the token block",
	},
	"hard-ac2-literal-font-size-inside-the-token-block": {
		criterion: "AC2", target: "TestTokens_TypeAndSpaceComeFromTheDeclaredScales",
		old: "--lh-base:1.5;", new: "--lh-base:1.5; font-size:18px;",
		why: "a font-size literal is painted from inside :root, where only the custom properties are the scale",
	},
	"hard-second-stylesheet-unswept": {
		criterion: "AC1", target: "TestTokens_EveryColourIsDeclaredInATokenBlock",
		old: "</style>", new: "</style>\n<style>\n  .mut { color:#8b0000; }\n</style>",
		why: "a second <style> element carries declarations the sweeps, which read the first one, would never see",
	},
	"hard-var-reference-names-nothing": {
		criterion: "AC1", target: "TestTokens_EveryVarReferenceNamesADeclaredToken",
		old: ".chip .k { font-size:var(--fs-sm); color:var(--muted);", new: ".chip .k { font-size:var(--fs-sm); color:var(--mutedd);",
		why: "a var() names a token nothing declares, so the declaration resolves to neither a colour nor a length and every sweep built on those two readers passes over it",
	},
	"hard-ac3-colour-token-not-measurable": {
		criterion: "AC3", target: "TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes",
		old: "--muted:#9aa3b4;", new: "--muted:rgb(154,163,180);",
		why: "a colour token is written in a form this file cannot measure, so every pair it paints drops out of the derivation instead of being measured",
	},
	"hard-ac3-disabled-exemption-leaks-across-the-group": {
		criterion: "AC3", target: "TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes",
		old: ".empty { color:var(--muted);", new: "button:disabled, .empty { color:var(--line);",
		why: "WCAG's disabled-control exemption is claimed for a whole selector group, so the empty-state text is painted at 1.4:1 and goes unmeasured",
	},
	"hard-ac3-colour-that-resolves-to-no-token": {
		criterion: "AC3", target: "TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes",
		old: ".nr { color:var(--muted);", new: ".nr { color:currentColor;",
		why: "a text colour resolves to no token, so the pair the honest-absence copy paints used to leave the derivation in silence rather than being measured",
	},
	"hard-ac3-second-colour-in-the-same-declaration": {
		criterion: "AC3", target: "TestContrast_EveryPaintedPairMeetsItsFloorInBothThemes",
		old: ".nr { color:var(--muted);", new: ".nr { color:light-dark(var(--muted), var(--line));",
		why: "a second colour in the same declaration paints the honest-absence text at 1.4:1 in the light theme, and only the first one used to be measured",
	},
	"hard-ac3-control-repainted-by-id": {
		criterion: "AC3", target: "TestControls_AreIdentifiableAgainstTheSurfaceTheySitOn",
		old: "  #msg { font-size:var(--fs-md);", new: "  #rescan { background:var(--bg); border-color:var(--bg); }\n  #msg { font-size:var(--fs-md);",
		why: "a control is repainted into the surface behind it by a rule addressing it by id, so nothing at all identifies it as a control",
	},
	"hard-ac3-control-rule-that-cannot-be-resolved": {
		criterion: "AC3", target: "TestControls_AreIdentifiableAgainstTheSurfaceTheySitOn",
		old: "               background-color var(--motion-fast) ease; --paints-on:var(--bg); }",
		new: "               background-color var(--motion-fast) ease; }",
		why: "the base button rule stops naming the surface it sits on, so the control's non-text contrast cannot be resolved and used to be dropped in silence",
	},
	"hard-ac4-light-theme-never-applies": {
		criterion: "AC4", target: "TestThemes_ALightSetIsDeclaredAndColorSchemeFollowsIt",
		old: "@media (prefers-color-scheme: light) {", new: "@media (prefers-color-scheme: light) and (min-width: 99999px) {",
		why: "the light token block carries a second condition that never holds, so the page has one theme again while the block still reads as the light set",
	},
	"hard-ac4-a-colour-token-has-no-light-value": {
		criterion: "AC4", target: "TestThemes_ALightSetIsDeclaredAndColorSchemeFollowsIt",
		old: "      --btn-primary-border:#1258a0;\n", new: "",
		why: "a colour token keeps its DARK value inside the light theme, which a hand-written list of ten names never asked about",
	},
	"hard-ac5-reduce-block-never-applies": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "@media (prefers-reduced-motion: reduce) {", new: "@media (prefers-reduced-motion: reduce) and (min-width: 99999px) {",
		why: "the block that cuts the motion carries a second condition that never holds, so a reduce preference stops nothing",
	},
	"hard-ac5-reduce-block-reaches-only-a-subtree": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "    *, *::before, *::after {", new: "    .chips *, *::before, *::after {",
		why: "the reduce block reaches one subtree instead of every element, so the controls keep their transitions",
	},
	"hard-ac6-width-in-calc": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "min-width:96px;", new: "min-width:calc(1000px + var(--sp-4));",
		why: "a width expression the reader cannot evaluate used to be measured as the token inside it - 8px - and cleared the 360px ceiling",
	},
	"hard-ac6-tablewrap-exemption-leaks-across-the-group": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "  .tablewrap { overflow-x:auto; }", new: "  .tablewrap, main { min-width:960px; }",
		why: "the .tablewrap exemption is claimed for a whole selector group, so <main> forces the page body 960px wide",
	},
	"hard-ac6-logical-min-inline-size": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "min-width:96px;", new: "min-inline-size:960px;",
		why: "the minimum width is asserted through the logical longhand, which is a minimum width by any reading",
	},
	"hard-ac9-no-match-message-reaches-one-table-only": {
		criterion: "AC9", target: "TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped",
		old: `for (const body of [$("queue"), $("history")]) {`, new: `for (const body of [$("queue")]) {`,
		why: "the no-match message reaches the queue alone, so a non-matching term still empties the history table in silence",
	},
}

// s0035Holes are the edits that violate their criterion and that the committed
// grader does NOT red on.
var s0035Holes = map[string]mutation{
	"F1-ac9-message-never-inserted": {
		criterion: "AC9", target: "TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped",
		old: "\n  body.appendChild(tr);", new: "",
		why: "the no-match row is built, given its colSpan, and then never put into the table body, so a term matching no loaded row empties both tables in silence - the exact state AC9 names as today's bug",
	},
	"F2-ac6-min-width-in-em": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "min-width:96px;", new: "min-width:60em;",
		why: "an element outside .tablewrap asserts a minimum width of 60em (840px at the page's 14px base, 960px against a 16px root), far wider than the 360px narrow viewport, so the page body scrolls sideways",
	},
	// F3 is advisory and carries no failing test of its own; it lives here so the
	// verdict can name one command that reproduces it.
	"F3-ac1-named-colour-in-background-image": {
		criterion: "AC1", target: "TestTokens_EveryColourIsDeclaredInATokenBlock",
		old: "  .tablewrap { overflow-x:auto; }", new: "  .mut { background-image:linear-gradient(red, blue); }\n  .tablewrap { overflow-x:auto; }",
		why: "two CSS named colours are written at their point of use, in a property outside isColourProp's list",
	},
}

func s0035Case(name string) (mutation, bool) {
	if m, ok := s0035Mutations[name]; ok {
		return m, true
	}
	m, ok := s0035Holes[name]
	return m, ok
}

// init applies the named edit to the embedded page before any test in the child
// process runs. Package-level variables (indexHTML among them) are initialised
// before init functions, so the embed is already in place here.
func init() {
	name := os.Getenv("S0035_MUT")
	if name == "" {
		return
	}
	m, ok := s0035Case(name)
	if !ok {
		panic("S0035_MUT names no case: " + name)
	}
	s := string(indexHTML)
	if !strings.Contains(s, m.old) {
		panic("mutation anchor absent from index.html: " + m.old)
	}
	indexHTML = []byte(strings.Replace(s, m.old, m.new, 1))
}

// runS0035Mutant runs the criterion's grader in a child process carrying the
// mutated page, and reports whether that grader stayed GREEN.
func runS0035Mutant(t *testing.T, name string, m mutation) (green bool, out string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run", "^"+m.target+"$", "-test.v")
	cmd.Env = append(os.Environ(), "S0035_MUT="+name)
	b, err := cmd.CombinedOutput()
	if !strings.Contains(string(b), "=== RUN   "+m.target) {
		t.Fatalf("the child never ran %s at all, so nothing was graded\n%s", m.target, b)
	}
	return err == nil, string(b)
}

// The positive control. Each of these edits violates its criterion and the
// committed grader reds on it, which is what the spec's preamble demands.
func TestRegress0035_GradersRedOnAMutatedPage(t *testing.T) {
	if os.Getenv("S0035_MUT") != "" {
		t.Skip("child run: this process carries a mutated page")
	}
	for name, m := range s0035Mutations {
		t.Run(name, func(t *testing.T) {
			green, out := runS0035Mutant(t, name, m)
			if green {
				t.Errorf("%s: %s passed on a page where %s\nmutation: %q -> %q\n%s",
					m.criterion, m.target, m.why, m.old, m.new, out)
			}
		})
	}
}
