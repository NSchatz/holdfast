package main

// THE WITNESSES, ASSERTED RATHER THAN ASSERTED ABOUT. version.go's whole argument is that a
// plan cannot pin the published version to a literal, because no fixed value is the trigger tag
// for two distinct tags - and that is a property of the DRAW, not of the check that consumes
// it. A draw that quietly returned the same tag twice, or returned the tag this gate's own
// sample names, or returned a tag the release path refuses, would leave the check running and
// deciding nothing: exactly the silent green the gate exists to refuse.

import (
	"regexp"
	"strings"
	"testing"
)

// The shape the planning logic itself accepts (release.yml's own `grep -qE`). A witness outside
// it would be REFUSED by the plan, and a refusal tells this gate nothing about the version a
// release publishes.
var releasableTag = regexp.MustCompile(`^v0\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// Drawn tags are pairwise distinct and are never a tag this gate names elsewhere. Repeated,
// because a single draw agreeing with itself proves nothing about a random source.
func TestDrawTriggerWitnesses_ArePairwiseDistinctAndNeverATagThisGateNames(t *testing.T) {
	named := map[string]string{
		sampleTag:      "the version this repository has actually published, and the tag the real-release shape is planned with",
		samplePreTag:   "the pre-release shape's tag",
		sampleMajorTag: "the tag the planning logic must refuse",
	}
	for round := 0; round < 200; round++ {
		ws, err := drawTriggerWitnesses()
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if len(ws) < 2 {
			t.Fatalf("round %d: %d witness(es) drawn. A constant version IS the trigger tag for one tag, so a single witness refuses no constant at all", round, len(ws))
		}
		seen := map[string]bool{}
		for _, w := range ws {
			if seen[w.tag] {
				t.Fatalf("round %d: %s was drawn twice. Two witnesses that are the same tag are one witness, and one witness cannot refuse a constant", round, w.tag)
			}
			seen[w.tag] = true
			if why, ok := named[w.tag]; ok {
				t.Fatalf("round %d: drew %s, which is %s. Drawing a tag this gate names anywhere else puts the very coincidence F27 hid behind back into the question", round, w.tag, why)
			}
			if !releasableTag.MatchString(w.tag) {
				t.Fatalf("round %d: drew %q, which the planning logic's own version check refuses, so the release it triggers decides nothing about the version a release publishes", round, w.tag)
			}
			if !strings.Contains(w.what, w.tag) {
				t.Fatalf("round %d: %s is described as %q, which does not name the tag; every refusal about it has to say which tag it planned", round, w.tag, w.what)
			}
		}
	}
}

// At least one witness of each shape. A pre-release publishes under its own tag and derives
// that tag the same way, so a hold that stopped at the shapes which promote would leave the
// pre-release path's version held by nothing.
func TestDrawTriggerWitnesses_CoverBothTheShapesThatPublish(t *testing.T) {
	for round := 0; round < 50; round++ {
		ws, err := drawTriggerWitnesses()
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		plain, pre := 0, 0
		for _, w := range ws {
			if strings.Contains(strings.TrimPrefix(w.tag, "v"), "-") {
				pre++
				continue
			}
			plain++
		}
		if plain < 2 {
			t.Fatalf("round %d: %d plain version tag(s) drawn, want at least 2 - two distinct witnesses are what refuse every constant with certainty", round, plain)
		}
		if pre < 1 {
			t.Fatalf("round %d: no pre-release tag drawn; a pre-release publishes under its own tag, so its version has to be held too", round)
		}
	}
}

// Every drawn tag has to be one the planning logic ACCEPTS. A witness the plan refuses would
// red as an undecidable tag against the committed tree, which is a gate that fails on its own
// inputs rather than one that grades them.
func TestDrawVersionTag_DrawsOnlyTagsThisReleasePathAccepts(t *testing.T) {
	for round := 0; round < 500; round++ {
		for _, preRelease := range []bool{false, true} {
			tag, err := drawVersionTag(preRelease)
			if err != nil {
				t.Fatalf("round %d: %v", round, err)
			}
			if !releasableTag.MatchString(tag) {
				t.Fatalf("round %d: drew %q, which the planning logic's own version check refuses", round, tag)
			}
			// The plan decides `prerelease` from the `-` in the version, so a witness of the
			// wrong shape grades the wrong release shape.
			isPre := strings.Contains(strings.TrimPrefix(tag, "v"), "-")
			if isPre != preRelease {
				t.Fatalf("round %d: drew %q for preRelease=%v", round, tag, preRelease)
			}
		}
	}
}
