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
