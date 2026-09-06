package webui

// regress_0035_F25: AC7's target-size sweep's membership is four property NAMES,
// which is narrower than the criterion's own set - "at least 24 CSS px in both
// dimensions" is a property of the RENDERED box, and a transform or a zoom
// shrinks it without touching any of the four.
//
// AC7: "WHEN a pointer target is RENDERED at the page's base font size, THE
// SYSTEM SHALL give it at least 24 CSS px IN BOTH DIMENSIONS (WCAG 2.2 2.5.8)."
//
// TestTargets_EveryPointerTargetClearsTheTwentyFourPixelFloor now reads the
// targets from the markup and sweeps every rule whose selector can REACH one -
// impl-gate finding F12, closed - but only for:
//
//	floorProps := []string{"min-height", "min-width", "min-block-size", "min-inline-size"}
//
// so one rule addressing the Rescan button, in the page's own idiom (`#rescan`,
// which is how finding F12's mutation reached it too):
//
//	#rescan { transform:scale(0.4); }
//
// renders a 32x32 control at 12.8 x 12.8 CSS px - half the floor, in both
// dimensions, hit area included, since a scale transform scales the box the
// pointer must land in. `zoom:0.4` does the same thing. Every committed test in
// this package stays green on either.
//
// The implementation's recorded reading for this clause
// (`just readings S0035-holdfast-dashboard-ui`, reading 19) chose to answer
// "which RULES expose the pointer targets" from the markup and to be "generous
// where it cannot decide, since over-inclusion only holds MORE rules to a
// floor". That reading is right about which rules; the sweep is then narrow
// about which DECLARATIONS, and the narrow half is the unsafe direction here.
//
// The shipped page is CORRECT: it declares no transform and no zoom anywhere.
// This file grades the PROOF, which the spec's acceptance-criteria preamble
// makes part of the criterion.
//
// Fix upstream in the sweep, not the page: on a rule that can reach a pointer
// target, refuse a `transform` / `zoom` / `scale` this reader cannot show leaves
// the rendered box at or above the floor - "resolvable or red", the discipline
// the contrast derivation and the AC6 width sweep are both already built on.
// This file goes green when a control rendered at 12.8px can no longer pass.

import "testing"

func TestRegress0035_F25_AC7TargetShrunkUnderTheFloorByATransform(t *testing.T) {
	v5Assert(t, "F25-target-shrunk-by-a-transform")
}

func TestRegress0035_F25_AC7TargetShrunkUnderTheFloorByZoom(t *testing.T) {
	v5Assert(t, "F25b-target-shrunk-by-zoom")
}
