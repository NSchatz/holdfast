package webui

// regress_0035_F23: AC8's negative-offset check is CONDITIONAL on the rule that
// sets the offset also declaring an outline property - so impl-gate finding
// F19's own violation is reopened by writing it in one declaration instead of
// two.
//
// AC8: "THE SYSTEM SHALL draw a focus indicator that clears 3:1 against BOTH the
// control's own fill AND THE SURFACE IMMEDIATELY BEHIND IT ..."
//
// F19 was closed by rejecting a negative outline-offset, and the implementation
// recorded the reading behind it (`just readings S0035-holdfast-dashboard-ui`,
// reading 21): "A ring at `-8px` is painted entirely inside the control's border
// box and is never adjacent to that surface, so the second half of the criterion
// would be measuring a boundary the page does not draw." Agreed. But
// TestFocus_RingClearsBothTheControlFillAndTheSurfaceBehindIt reaches that check
// only through outlineOf:
//
//	o := outlineSpec{sel: r.sel, at: r.at, offset: r.get("outline-offset")}
//	...
//	if sh := r.get("outline"); sh != "" { o.stated = true; ... }
//	if v := r.get("outline-style"); v != "" { o.stated = true; ... }
//	if v := r.get("outline-width"); v != "" { o.stated = true; ... }
//	if v := r.get("outline-color"); v != "" { o.stated = true; ... }
//
// and then, in the caller:
//
//	if !o.stated { continue }
//
// `outline-offset` never sets `stated`. So one line appended to the stylesheet -
//
//	:focus-visible { outline-offset:-8px; }
//
// - wins the cascade over the base :focus-visible rule (same specificity, later
// source order), pulls the ring 8px inside every control's border box, and is
// DISCARDED by the reader before the offset it carries is ever looked at. The
// page draws no indicator against the surface behind any control, in either
// theme, and every committed test in this package stays green.
//
// This is the class the loop was opened on stated exactly: an assertion
// conditional on the thing it checks being present. The check for a negative
// offset is inside `if o.focus { ... }`, which is inside the loop body that
// `if !o.stated { continue }` guards - so the only rules whose offset is ever
// examined are the rules that also happened to restate the outline.
//
// The shipped page is CORRECT: `--focus-offset` is 2px and no rule overrides it.
// This file grades the PROOF, which the spec's acceptance-criteria preamble
// makes part of the criterion.
//
// Fix upstream in the reader, not the page: treat `outline-offset` as a stated
// outline property (it is one), or resolve the offset that WINS for each control
// across every rule that can reach it, rather than per-rule. This file goes
// green when a ring drawn inside the control can no longer pass unread.

import "testing"

func TestRegress0035_F23_AC8RingPulledInsideByAnOffsetOnlyRule(t *testing.T) {
	v5Assert(t, "F23-ring-pulled-inside-by-an-offset-only-rule")
}
