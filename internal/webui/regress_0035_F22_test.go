package webui

// regress_0035_F22: AC5's reduce-block proof is satisfied by ANY rule that cuts
// a duration, never by the one the cascade actually keeps - so a later reduce
// block can hand every transition straight back.
//
// AC5: "IF the page declares any `transition`, `animation` or `@keyframes`, THEN
// under `@media (prefers-reduced-motion: reduce)` THE SYSTEM SHALL reduce them
// to NO PERCEPTIBLE MOTION ..."
//
// TestMotion_NoStateDependsOnItAndAReducePreferenceStopsIt discharges that half
// like this:
//
//	cut := map[string]bool{}
//	for _, r := range rules {
//	    if canonicalAt(r.at) != reduceAt || !hasUniversalPart(r.sel) { continue }
//	    ...
//	    if strings.Contains(d.value, "!important") && msOf(d.value) <= 1 {
//	        cut[motionFamily(d.prop)] = true
//	    }
//	}
//
// `cut` is set by the FIRST qualifying rule and never unset. Append a second
// reduce block to the stylesheet:
//
//	@media (prefers-reduced-motion: reduce) {
//	  * { transition-duration:400ms !important; }
//	}
//
// Same origin, same importance, same specificity (0,0,0), later in source order:
// it WINS. A user who asked for reduced motion gets the page's transitions back
// at 400ms, which is the plain violation this criterion has. `cut["transition"]`
// is still true, set by the block that no longer decides anything, and every
// committed test in this package stays green.
//
// This is the identical mistake AC6's own region loop names and refuses four
// hundred lines further down the same file: "the rule that decides is the LAST
// applying one ... Accepting ANY applying rule would let a single-column
// declaration the page overrides two lines later stand in for the layout an
// operator sees". AC5's cut was not given that reading, and the implementation's
// own recorded reading 23 states it for AC6 alone.
//
// The shipped page is CORRECT: it declares one reduce block and cuts its
// transitions to 0.01ms. This file grades the PROOF, which the spec's
// acceptance-criteria preamble makes part of the criterion: "a test that passes
// on a mutated page has not graded its criterion."
//
// Fix upstream in the sweep, not the page: take the LAST qualifying declaration
// rather than any, i.e. let a later universal reduce-block declaration reset
// `cut[fam]` when its duration is perceptible. This file goes green when a
// stylesheet whose reduce preference restores 400ms motion can no longer pass.

import "testing"

func TestRegress0035_F22_AC5ReduceCutUndoneByALaterReduceBlock(t *testing.T) {
	v5Assert(t, "F22-reduce-cut-undone-by-a-later-reduce-block")
}
