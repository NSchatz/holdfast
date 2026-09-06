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

	// -----------------------------------------------------------------------
	// The second grader sweep (impl-gate ordinal 4: findings F14-F19). Same
	// class as the block above - a condition or a name matched by SUBSTRING
	// instead of read for what it is, and a value SEARCHED instead of compared -
	// and every case below is the exact shape the verdict broke the grader with,
	// now red.
	// -----------------------------------------------------------------------

	"F14-ac6-narrow-block-never-applies": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "  @media (max-width: 640px) {", new: "  @media (max-width: 640px) and (min-width: 99999px) {",
		why: "the narrow-viewport block carries a second condition that never holds, so at 360px the header, both control rows and the aggregate grid stay multi-column while the prelude still carries the max-width a regex found in it",
	},
	"F14-ac6-three-column-grid": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "    .aggs { grid-template-columns:1fr; }", new: "    .aggs { grid-template-columns:repeat(3, 1fr); }",
		why: "the aggregate cards are laid out in THREE columns at the 360px viewport, in a value that satisfies a substring search for \"1fr\" by containing it",
	},
	"hard-ac6-two-track-grid": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "    .aggs { grid-template-columns:1fr; }", new: "    .aggs { grid-template-columns:1fr 1fr; }",
		why: "the aggregate cards are two columns at the narrow viewport, written as a track list that also contains \"1fr\"",
	},
	"hard-ac6-narrow-block-under-a-user-preference": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "  @media (max-width: 640px) {", new: "  @media (max-width: 640px) and (prefers-color-scheme: dark) {",
		why: "the single-column collapse reaches only a user whose system theme is dark, so a light-theme operator on a 360px screen scrolls the page body sideways",
	},
	"hard-ac6-collapse-overridden-after-the-block": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "</style>", new: "  .aggs { grid-template-columns:repeat(2, 1fr); }\n</style>",
		why: "a later top-level rule of equal specificity overrides the narrow block's single column, so the collapse is declared and then undone - accepting ANY applying rule would let the overridden declaration stand in for the layout an operator sees",
	},
	"F15-ac8-ring-no-user-agent-turns-on": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "  :focus-visible { outline:var(--focus-w) solid var(--focus);\n    outline-offset:var(--focus-offset); border-radius:var(--radius-sm); }",
		new: "  @media (min-width: 99999px) {\n  :focus-visible { outline:var(--focus-w) solid var(--focus);\n    outline-offset:var(--focus-offset); border-radius:var(--radius-sm); }\n  }",
		why: "the page's only :focus-visible rule sits inside `@media (min-width: 99999px)`, so NO control draws a focus indicator at any viewport a person has, in either theme",
	},
	"F15-ac8-ring-only-under-a-reduced-motion-preference": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "  :focus-visible { outline:var(--focus-w) solid var(--focus);\n    outline-offset:var(--focus-offset); border-radius:var(--radius-sm); }",
		new: "  @media (prefers-reduced-motion: reduce) {\n  :focus-visible { outline:var(--focus-w) solid var(--focus);\n    outline-offset:var(--focus-offset); border-radius:var(--radius-sm); }\n  }",
		why: "the ring exists only for a user who asked for reduced motion, so every other keyboard operator tabs through all five controls with no indicator at all",
	},
	"hard-ac8-ring-only-in-the-light-theme": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "  :focus-visible { outline:var(--focus-w) solid var(--focus);\n    outline-offset:var(--focus-offset); border-radius:var(--radius-sm); }",
		new: "  @media (prefers-color-scheme: light) {\n  :focus-visible { outline:var(--focus-w) solid var(--focus);\n    outline-offset:var(--focus-offset); border-radius:var(--radius-sm); }\n  }",
		why: "the ring is declared in one theme only, and AC8 requires it drawn in BOTH - a keyboard operator on the dark page gets none",
	},
	"F19-ac8-ring-drawn-inside-the-control": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "outline-offset:var(--focus-offset);", new: "outline-offset:-8px;",
		why: "a negative offset draws the ring INSIDE the control's border box, where it never meets the surface immediately behind the control that AC8 measures it against",
	},
	"F16-ac9-no-match-text-is-a-dead-constant": {
		criterion: "AC9", target: "TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped",
		old: `const NO_MATCH_TEXT = "No loaded row matches this filter. The rows loaded here are "`,
		new: `const NO_MATCH_TEXT = "n/a";` + "\n" +
			`const NO_MATCH_TEXT_OLD = "No loaded row matches this filter. The rows loaded here are "`,
		why: "the value the no-match row is built from is \"n/a\" while AC9's two sentences survive in a constant nothing reads, so the row an operator sees on a non-matching term states neither that no loaded row matches nor that the loaded rows are capped",
	},
	"hard-ac9-row-built-from-a-literal-instead-of-the-constant": {
		criterion: "AC9", target: "TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped",
		old: `const td = mk("td", "empty", NO_MATCH_TEXT);`, new: `const td = mk("td", "empty", "n/a");`,
		why: "the render site stops reading the constant whose value the wording is asserted on, so the two halves of the proof come apart from the other end",
	},
	"F17-ac10-empty-row-renders-a-blank-cell": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: `  const td = mk("td", "empty", text);`, new: `  const td = mk("td", "empty", "");`,
		why: "emptyRow builds its cell with an EMPTY string, so an empty queue and an empty history each render a blank row and the operator is shown a table that says nothing - with both call sites reading exactly as pinned",
	},
	"hard-ac10-empty-row-built-and-never-appended": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: `  if (!q.length) qbody.appendChild(emptyRow(5, "Nothing queued."));`, new: `  if (!q.length) emptyRow(5, "Nothing queued.");`,
		why: "the empty-queue row is built and never put into the table, which is finding F1's build-then-do-not-show shape in AC10's position",
	},
	"F18-ac5-motion-behind-the-vendor-prefix": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "  .tablewrap { overflow-x:auto; }",
		new: "  @-webkit-keyframes pulse { from { opacity:0; } to { opacity:1; } }\n" +
			"  .st-encoding .dot { -webkit-animation:pulse 2s infinite; }\n" +
			"  .tablewrap { overflow-x:auto; }",
		why: "the page's motion is written entirely through the WebKit prefix, which both WebKit and Blink still honour, so a user who asked for reduced motion watches the encoding dot pulse forever while the reduce block cuts transition-duration only",
	},
	"hard-ac5-animation-behind-the-vendor-prefix": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "  .dot { display:inline-block;", new: "  .dot { -webkit-animation:pulse 2s infinite; display:inline-block;",
		why: "a state arrives by animation, spelled with the vendor prefix, so it is not readable from a static frame",
	},

	// --- impl-gate ordinal 5 (F20-F25), and the class each of them names ---------
	//
	// The four F20 cases are the same defect in four places: a degraded state that is
	// BUILT exactly as every pin requires and never SHOWN.
	"F20a-ac10-mk-renders-no-text": {
		criterion: "AC9, AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: `  if (text != null) e.textContent = text;`, new: `  if (text != null) e.textContent = "";`,
		why: "mk stops writing the text it is handed, so every degraded state built through it - `Nothing queued.`, `No history yet.`, `unavailable`, `not recorded`, `unknown` and AC9's whole no-match sentence - renders as an EMPTY cell while every pinned call site reads exactly as pinned",
	},
	"F20b-ac9-no-match-row-inserted-then-hidden": {
		criterion: "AC9", target: "TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped",
		old: "  body.appendChild(tr);", new: "  tr.hidden = true;\n  body.appendChild(tr);",
		why: "the no-match row is built in the pinned order and inserted, and is then not VISIBLE, so a term matching no loaded row still empties the table in silence",
	},
	"F20c-ac10-empty-row-inserted-then-hidden": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: `  tr.dataset.empty = "1";`, new: "  tr.dataset.empty = \"1\";\n  tr.hidden = true;",
		why: "the empty-queue and empty-history rows are built, filled and appended, and are then not visible, so an operator with an empty queue is shown a table that says nothing",
	},
	"F20d-ac10-cap-notice-written-then-hidden": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: "    el.hidden = false;", new: "    el.hidden = true;",
		why: "the row-cap notice is written into its element and then hidden, so `this view is capped` never reaches an operator and a truncated view reads as a complete one",
	},
	"hard-ac10-empty-row-hidden-by-a-shape-no-reader-models": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: `  tr.dataset.empty = "1";`, new: "  tr.dataset.empty = \"1\";\n  tr.style.cssText = \"display:none\";",
		why: "the empty-table rows are hidden through a property no hiding reader here models, which is why the whole body is pinned and not a subsequence of it",
	},
	"hard-ac9-no-match-row-hidden-by-a-shape-no-reader-models": {
		criterion: "AC9", target: "TestFilter_ANonMatchingTermSaysSoAndSaysTheLoadedRowsAreCapped",
		old: "  body.appendChild(tr);", new: "  tr.style.cssText = \"display:none\";\n  body.appendChild(tr);",
		why: "the no-match row is hidden the same way, one statement inserted into a body whose every pinned step still reads exactly as pinned",
	},
	"hard-ac10-cap-notice-never-called-for-the-history": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: `  capNote("hist-cap", h.length, sumStatuses(sum, TERMINAL_STATUSES));`, new: "",
		why: "capNote writes `this view is capped` exactly as pinned and the history never calls it, so a truncated history reads as the whole ledger - finding F1's shape one call further out than any pin reached",
	},
	"hard-ac10-honest-absence-node-never-appended": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: `  if (!isNum(j.source_bytes) || !isNum(j.output_bytes)) { td.appendChild(nrNode()); return; }`,
		new: `  if (!isNum(j.source_bytes) || !isNum(j.output_bytes)) { return; }`,
		why: "a size that was never recorded renders as an EMPTY cell instead of `not recorded`, which is the distinction between a measured verdict and a missing one that this criterion exists to keep",
	},
	"hard-ac6-exemption-claimed-by-a-substring": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "  .tablewrap { overflow-x:auto; }", new: "  .tablewrapper { min-width:960px; }\n  .tablewrap { overflow-x:auto; }",
		why: "a rule scoped to nothing claims AC6's one exemption by CONTAINING the word tablewrap, and forces the page body 960px wide at the 360px viewport",
	},
	"hard-ac10-degraded-state-hidden-by-the-stylesheet": {
		criterion: "AC10", target: "TestDegradedStates_CopySurvivesVerbatimAndStaysLegibleInBothThemes",
		old: "  .tablewrap { overflow-x:auto; }", new: "  .empty { display:none; }\n  .tablewrap { overflow-x:auto; }",
		why: "the empty-table states are built, appended and painted to their contrast floor, and the STYLESHEET takes them off the page - finding F20's shape one language to the left",
	},

	// F21: AC6's region membership, evaded by a selector GROUP and by a descendant.
	"F21a-ac6-aggregate-grid-collapse-undone-by-a-selector-group": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "</style>", new: "  .aggs, .chips { grid-template-columns:repeat(2, 1fr); }\n</style>",
		why: "a later top-level rule of equal specificity, written as a selector GROUP, lays the aggregate cards out in TWO columns at the 360px viewport",
	},
	"F21b-ac6-header-collapse-undone-by-a-selector-group": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "</style>", new: "  header, footer { flex-direction:row; }\n</style>",
		why: "a later top-level rule, written as a selector GROUP, keeps the header a ROW at the 360px viewport",
	},
	"F21c-ac6-control-rows-collapse-undone-by-a-selector-group": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "</style>", new: "  .controls, .chips { flex-direction:row; }\n</style>",
		why: "a later top-level rule, written as a selector GROUP, keeps both control rows a ROW at the 360px viewport",
	},
	"hard-ac6-collapse-undone-under-a-prelude-the-reader-cannot-evaluate": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "</style>", new: "  @media (max-width: 40em) {\n    header { flex-direction:row; }\n  }\n</style>",
		why: "the same override under a prelude that IS live at 360px in a real browser and that this reader cannot evaluate, so dropping the rule rather than reding on it would let it decide the layout unseen",
	},
	"hard-ac6-header-collapse-undone-by-a-descendant-rule": {
		criterion: "AC6", target: "TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody",
		old: "</style>", new: "  body header { flex-direction:row; }\n</style>",
		why: "the same override spelled with a descendant combinator, which OUTRANKS the region's own rule - the evasion that survives a membership fixed by comparing each part of the group for equality",
	},

	// F22: AC5's cut, undone by a later block and by one scoped to a subtree.
	"F22-ac5-reduce-cut-undone-by-a-later-reduce-block": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "</style>", new: "  @media (prefers-reduced-motion: reduce) {\n    * { transition-duration:400ms !important; }\n  }\n</style>",
		why: "a second reduce block of identical origin, importance and specificity, later in source order, restores 400ms transitions - so a user who asked for reduced motion gets the page's motion back in full",
	},
	"hard-ac5-reduce-cut-undone-by-a-prelude-that-compares-equal-to-nothing": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "</style>", new: "  @media (prefers-reduced-motion: reduce) and (min-width: 0px) {\n    * { transition-duration:400ms !important; }\n  }\n</style>",
		why: "the same restoration under a prelude that is live for the same user and compares equal to the reduce block's prelude not at all, so a sweep keyed on that equality never sees it",
	},
	"hard-ac5-important-motion-beats-the-cut-from-the-top-level": {
		criterion: "AC5", target: "TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt",
		old: "</style>", new: "  .card { transition-duration:400ms !important; }\n</style>",
		why: "an important motion declaration more specific than the reduce block's universal cut beats it for every user who asked for reduced motion, from outside any reduce block at all",
	},

	// F23: AC8's offset, declared without an outline property beside it.
	"F23-ac8-ring-pulled-inside-by-an-offset-only-rule": {
		criterion: "AC8", target: "TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt",
		old: "</style>", new: "  :focus-visible { outline-offset:-8px; }\n</style>",
		why: "a later :focus-visible rule declaring ONLY outline-offset wins the cascade and draws the ring INSIDE the control's border box, where it never meets the surface immediately behind it",
	},

	// F24: AC4's color-scheme, searched instead of compared.
	"F24-ac4-color-scheme-supports-neither-scheme": {
		criterion: "AC4", target: "TestThemes_ALightSetIsDeclaredAndColorSchemeFollowsIt",
		old: "color-scheme: light dark;", new: "color-scheme: lightdark;",
		why: "color-scheme names a single unrecognised ident, so the document opts into NEITHER scheme and native controls, scrollbars and form fields stay light inside the dark page - while one word contains both of the substrings the old check searched for",
	},

	// F25: AC7 measures what is RENDERED, and a minimum is not a rendered size.
	"F25a-ac7-target-shrunk-by-a-transform": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "  #msg { font-size:var(--fs-md);", new: "  #rescan { transform:scale(0.4); }\n  #msg { font-size:var(--fs-md);",
		why: "the Rescan button is RENDERED at 12.8 x 12.8 CSS px, half of WCAG 2.2 2.5.8's floor in both dimensions and hit area included, by a property outside the sweep's four minimum names",
	},
	"F25b-ac7-target-shrunk-by-zoom": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "  #msg { font-size:var(--fs-md);", new: "  #rescan { zoom:0.4; }\n  #msg { font-size:var(--fs-md);",
		why: "the same target, shrunk under the floor through `zoom` instead",
	},
	"hard-ac7-target-shrunk-by-the-scale-property": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "  #msg { font-size:var(--fs-md);", new: "  #rescan { scale:0.4; }\n  #msg { font-size:var(--fs-md);",
		why: "the same target, shrunk under the floor through the standalone `scale` property, which is neither a minimum nor a transform",
	},
	"hard-ac7-target-shrunk-by-an-ancestor-transform": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "  #msg { font-size:var(--fs-md);", new: "  .controls { transform:scale(0.4); }\n  #msg { font-size:var(--fs-md);",
		why: "the scale is declared on the row that CONTAINS the controls, so every button and input inside it renders at 12.8px while the rule reaches none of them",
	},
	"hard-ac7-target-scaled-by-an-unreadable-transform": {
		criterion: "AC7", target: "TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor",
		old: "  #msg { font-size:var(--fs-md);", new: "  #rescan { transform:matrix(0.4, 0, 0, 0.4, 0, 0); }\n  #msg { font-size:var(--fs-md);",
		why: "the same shrink written as a matrix, which this reader cannot evaluate - and a scale it cannot evaluate must red rather than be scored as harmless, the way an unmeasurable length already does",
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
