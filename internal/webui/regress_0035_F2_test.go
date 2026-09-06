package webui

// regress_0035_F2: AC6's width sweep reads every non-pixel length as zero.
//
// AC6: "no element outside a `.tablewrap` SHALL assert a fixed `width` or a
// `min-width` greater than the narrow breakpoint, so the page body itself does
// not scroll horizontally."
//
// TestLayout_NarrowViewportIsOneColumnAndNothingForcesAWiderBody measures those
// declarations with pxOf, which returns 0 for anything not suffixed "px". A
// width in em, rem, ch, vw, vmin or cm therefore reads as zero and clears a
// `> narrow` comparison no matter how wide it is. pxOf's own comment asserts the
// opposite - "which is never the unsafe direction for the two sweeps that use
// it" - and that claim is true of the 24px target sweep (0 < 24 reds) and false
// of this one (0 > 360 is silent). The page already writes `max-width:70ch` and
// `max-width:80ch`, so em-family lengths are in this stylesheet's idiom.
//
// The shipped page is CORRECT; this test grades the PROOF, which the spec's
// acceptance-criteria preamble makes part of the criterion.
//
// Fix upstream by making the sweep refuse a length it cannot measure rather than
// scoring it 0, which is the same "resolvable or red" discipline the contrast
// derivation already uses. This file goes green when it is graded.

import (
	"os"
	"testing"
)

func TestRegress0035_F2_AC6NarrowViewportSweepMissesNonPixelWidths(t *testing.T) {
	if os.Getenv("S0035_MUT") != "" {
		t.Skip("child run")
	}
	const name = "F2-ac6-min-width-in-em"
	m := s0035Holes[name]
	green, out := runS0035Mutant(t, name, m)
	if green {
		t.Errorf("AC6 is ungraded for non-pixel lengths: %s stayed GREEN on a page where %s\nmutation: %q -> %q\n%s",
			m.target, m.why, m.old, m.new, out)
	}
}
