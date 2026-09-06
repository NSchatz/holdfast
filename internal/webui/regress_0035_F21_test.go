package webui

// regress_0035_F21: AC6's single-column proof decides WHICH RULE lays out each
// region by comparing the whole selector group for string equality, so any rule
// written as a group overrides the collapse unseen.
//
// AC6: "WHEN the viewport is 360 CSS px wide, THE SYSTEM SHALL present the
// header, both control rows and the aggregate cards in a SINGLE COLUMN ..."
//
// TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody now reads the
// prelude for the condition it is and compares the track list for the value it
// is - impl-gate finding F14, closed. It picks the deciding rule like this:
//
//	for _, r := range rules {
//	    if r.sel != region.sel || !atAppliesAtViewportWidth(r.at, narrow) { continue }
//	    if v := r.get(region.prop); v != "" { decided, from = v, r.at }
//	}
//
// `r.sel != region.sel` is the membership test, and it is narrower than the
// criterion's own set. `.aggs { grid-template-columns:repeat(2,1fr) }` after the
// narrow block is caught - it is the committed mutation
// `hard-ac6-collapse-overridden-after-the-block`. Add one selector to the group
// and the identical override is invisible:
//
//	.aggs, .chips { grid-template-columns:repeat(2, 1fr); }
//	header, footer { flex-direction:row; }
//	.controls, .chips { flex-direction:row; }
//
// Each is a top-level rule of equal specificity, later in source order than the
// @media block (an @media adds no specificity, so source order IS the cascade
// here - the test's own comment says exactly that). Each therefore WINS at
// 360px, and each leaves `decided` holding the narrow block's value the operator
// never sees. Every committed test in this package stays green on all three.
//
// This is the same leak this very file already refuses twice, in two other
// positions and for the same stated reason. selectorParts' own comment:
// "Asking a question of the whole group as one string answers it for every part
// at once, which is exactly how an exemption written for one selector leaks onto
// another". disabledOnly and exemptTablewrap both iterate parts. The region loop
// was not given the same reader - and here the leak runs the other way, letting a
// group EVADE a check rather than claim an exemption.
//
// It also contradicts the reading the implementation recorded for this clause
// (`just readings S0035-holdfast-dashboard-ui`, reading 23): "Taken: the LAST
// rule that applies at 360px and declares the property ... Refused: ANY applying
// rule satisfying it, which ... lets a single-column declaration the page
// overrides two lines later stand in for the layout an operator sees." A rule
// written as a group applies at 360px and declares the property, and is not the
// one this code takes. The reading is right; the code does not build it.
//
// The shipped page is CORRECT: nothing overrides the narrow block. This file
// grades the PROOF, which the spec's acceptance-criteria preamble makes part of
// the criterion.
//
// Fix upstream in the sweep, not the page: decide membership with
// selectorParts / selectorReaches - "does any part of this group select the
// region" - rather than with equality on the group's text. This file goes green
// when a two-column aggregate grid at 360px can no longer pass by being written
// beside a second selector.

import "testing"

func TestRegress0035_F21_AC6AggregateGridCollapseUndoneByASelectorGroup(t *testing.T) {
	v5Assert(t, "F21a-aggs-collapse-undone-by-a-selector-group")
}

func TestRegress0035_F21_AC6HeaderCollapseUndoneByASelectorGroup(t *testing.T) {
	v5Assert(t, "F21b-header-collapse-undone-by-a-selector-group")
}

func TestRegress0035_F21_AC6ControlRowCollapseUndoneByASelectorGroup(t *testing.T) {
	v5Assert(t, "F21c-controls-collapse-undone-by-a-selector-group")
}
