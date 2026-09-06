package webui

// regress_0035_F24: AC4's `color-scheme` half is graded by searching the value
// for two substrings, so a declaration that opts the document into NEITHER
// scheme passes.
//
// AC4: "The document SHALL declare `color-scheme: light dark` so native
// controls, scrollbars and form fields follow the same theme instead of staying
// light inside a dark page."
//
// TestThemes_ALightSetIsDeclaredAndColorSchemeFollowsIt discharges it like this:
//
//	if cs := root.get("color-scheme"); !strings.Contains(cs, "light") || !strings.Contains(cs, "dark") {
//
// `color-scheme: lightdark` contains "light" and contains "dark". It is a single
// <custom-ident> naming a scheme no user agent implements, so the document
// supports neither light nor dark, every native control and scrollbar and form
// field falls back to the user agent's light default, and they stay light inside
// the dark page - which is the harm this criterion states in its own words. The
// committed grader stays green, and so does every other test in this package.
//
// This is "a value SEARCHED instead of compared", which is the diagnosis the
// implementation wrote for itself two hundred lines away, in gridTrackCount:
// "Searching the value for the wanted one instead is how `repeat(3, 1fr)`
// satisfied a single-column check, by CONTAINING '1fr'". The same reading was
// applied to the track list and to the at-rule prelude, and not to this one.
//
// The shipped page is CORRECT: :root declares `color-scheme: light dark`. This
// file grades the PROOF, which the spec's acceptance-criteria preamble makes
// part of the criterion: "a test that passes on a mutated page has not graded
// its criterion."
//
// Fix upstream in the assertion, not the page: read the declaration as the ident
// LIST it is and require `light` and `dark` to each be one of its top-level
// idents (topLevelFields already exists for exactly this shape). This file goes
// green when a color-scheme supporting neither scheme can no longer pass.

import "testing"

func TestRegress0035_F24_AC4ColorSchemeSupportsNeitherSchemeAndPasses(t *testing.T) {
	v5Assert(t, "F24-color-scheme-supports-neither-scheme")
}
